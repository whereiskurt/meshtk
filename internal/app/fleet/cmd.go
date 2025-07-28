package fleet

import (
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	internal "github.com/whereiskurt/meshtk/internal/mqtt"
	"github.com/whereiskurt/meshtk/pkg/otp"

	meshtastic "github.com/whereiskurt/meshtk/protos/meshtastic/generated"

	"github.com/spf13/cobra"

	"github.com/whereiskurt/meshtk/internal/app/help"
	"github.com/whereiskurt/meshtk/pkg/config"
)

type FleetCmd struct {
	Config     *config.Config
	Nodes      []internal.NodeDB
	NodesMutex []sync.Mutex
	MqttClient []*internal.MqttClient
	CmdOutput  struct {
		WasSuccess bool
	}
	OTPHandler []*otp.TOTPConfig
}

const BACKSTOP_GRACE_SEC = 30

func NewFleets(c *config.Config) (f *FleetCmd) {
	f = new(FleetCmd)
	f.Config = c

	for i := 0; i < len(c.Fleet); i++ {
		f.Nodes = append(f.Nodes, make(internal.NodeDB))
		f.NodesMutex = append(f.NodesMutex, sync.Mutex{})

		otpURL := f.Config.Fleet[i].OtpUrl
		if otpURL != "" {
			otpHandler, _ := otp.NewOTPHandler(otpURL)
			f.OTPHandler = append(f.OTPHandler, otpHandler)
		}
	}

	return f
}
func (f *FleetCmd) Help(cmd *cobra.Command, argz []string) {
	f.CmdOutput.WasSuccess = true
	fmt.Fprintln(f.Config.Stdout, help.FleetHelp(f.Config))
}

func (f *FleetCmd) Simulate(cmd *cobra.Command, argz []string) {
	f.CmdOutput.WasSuccess = true
	s := help.Render("GlobalHeader", f.Config)
	f.Config.Stdout.Write([]byte(s + "\n"))

	f.Config.Log.Trace("fleet.Simulate")
	f.Config.Log.Tracef("%+v", f.Config)

	for idx := range f.Config.Fleet {
		f.initNodeDb(idx)
		f.MqttClient = append(f.MqttClient, internal.NewMqttClient(f.Config, &f.Nodes[idx], f.FleetNodeHandler))
	}

	terminate := make(chan os.Signal, 1)

	signal.Notify(terminate, syscall.SIGINT, syscall.SIGTERM)

	alldone := make(chan int, len(f.Config.Fleet))

	// Start all of the fleet simulations
	for i := range f.Config.Fleet {
		go func(idx int) {
			fleetdone := make(chan bool)
			go func(idx int) {
				// Kick of the fleet simulation!
				f.Config.Stdout.Write([]byte(fmt.Sprintf("🚀 Fleet[%d]: MQTT connect ...\n", idx)))
				f.Config.Stdout.Write([]byte(fmt.Sprintf("🚀 Fleet[%d]: Starting simulation ...\n", idx)))
				f.simulate(idx)
				f.Config.Stdout.Write([]byte(fmt.Sprintf("🚀 Fleet[%d]: Simulation completed.\n", idx)))
				fleetdone <- true
				alldone <- idx
			}(idx)

			// Setup backstop timeouts to wait for the simulation to finish
			// This is to prevent the simulation from running indefinitely
			t := BACKSTOP_GRACE_SEC + f.Config.Fleet[idx].RampUpSecs + f.Config.Fleet[idx].RampSteadySecs + f.Config.Fleet[idx].RampDownSecs
			backstop := time.After(time.Duration(t) * time.Second)
			select {
			case <-fleetdone:
				f.Config.Stdout.Write([]byte(fmt.Sprintf("🚀 Fleet[%d]: Done! Simulation completed successfully.\n", idx)))
			case <-backstop:
				f.Config.Stdout.Write([]byte(fmt.Sprintf("🚀 Fleet[%d]: Backstopped! Simulation timeout expired after %d seconds.\n", idx, t)))
				alldone <- idx
			}
		}(i)
	}

	// Hangout until all simulations are done or we get a termination signal
	select {
	case <-terminate:
		f.Config.Stdout.Write([]byte("\nReceived termination signal (CTRL+C)...\n"))
	case <-waitForAllCompletions(alldone, len(f.Config.Fleet)):
		f.Config.Stdout.Write([]byte("✅ All simulations completed.\n"))
	}

	f.Config.Stdout.Write([]byte("\n✅ Cleanly exiting ...\n"))

	for idx := range f.Config.Fleet {
		f.flushNodeDb(idx)
	}

	f.CmdOutput.WasSuccess = true
}

func (f *FleetCmd) FindNodes(to, from uint32) (int, *internal.Node, int, *internal.Node) {
	var toNode, fromNode *internal.Node
	var toFleetIdx, fromFleetIdx int = -1, -1 // Initialize with -1 to indicate "not found"

	for fleetIdx := range f.Nodes {
		fleetNodes := f.Nodes[fleetIdx]
		for nodeID, node := range fleetNodes {
			if nodeID == to {
				toNode = node
				toFleetIdx = fleetIdx
			}
			if nodeID == from {
				fromNode = node
				fromFleetIdx = fleetIdx
			}
		}
	}

	return toFleetIdx, toNode, fromFleetIdx, fromNode
}

func waitForAllCompletions(completionChan chan int, count int) chan struct{} {
	done := make(chan struct{})
	go func() {
		for i := 0; i < count; i++ {
			<-completionChan
		}
		close(done)
	}()
	return done
}

func (n *FleetCmd) sendPKIReply(toFleetIdx int, to, from uint32, topic string, reply string) {
	// Get the nodes to access their keys
	_, toNode, _, _ := n.FindNodes(to, from)

	if toNode == nil {
		n.Config.Log.Errorf("Failed to find nodes: to=%d, from=%d", to, from)
		return
	}

	// The 'to' node (receiver of original message) is now the sender of the reply
	senderPrivKey := toNode.PrivKey

	senderPubKeyHex, err := n.MqttClient[toFleetIdx].FetchPublicKeyFromDefcon(from)
	if err != nil {
		n.Config.Log.Errorf("failed to fetch sender public key from DEFCON API: %v", err)
		return
	}

	// The 'from' node (sender of original message) is now the receiver of the reply
	recipientPubKeyBytes, err := n.MqttClient[toFleetIdx].ParseHexKey(senderPubKeyHex)
	if err != nil {
		n.Config.Log.Errorf("Failed to parse recipient public key: %v", err)
		return
	}

	// n.Config.Log.Tracef("Sending PKI reply from node %d to node %d", to, from)
	// n.Config.Log.Tracef("Sender private key: %s", senderPrivKey)
	// n.Config.Log.Tracef("Recipient public key: %s", recipientPubKey)

	// Parse the hex keys
	senderPrivKeyBytes, err := n.MqttClient[toFleetIdx].ParseHexKey(senderPrivKey)
	if err != nil {
		n.Config.Log.Errorf("Failed to parse sender private key: %v", err)
		return
	}

	// Send the PKI encrypted reply
	// Note: from and to are swapped for the reply
	err = n.MqttClient[toFleetIdx].PublishPKIMessage(
		to,   // sender of reply (was receiver of original message)
		from, // receiver of reply (was sender of original message)
		topic,
		meshtastic.PortNum_TEXT_MESSAGE_APP,
		[]byte(reply),
		senderPrivKeyBytes,
		recipientPubKeyBytes,
	)

	if err != nil {
		n.Config.Log.Errorf("Failed to send PKI reply: %v", err)
	} else {
		n.Config.Log.Infof("Successfully sent PKI reply from %d to %d: %s", to, from, reply)
	}
}

func (n *FleetCmd) FleetNodeHandler(to, from uint32, topic string, portNum meshtastic.PortNum, payload []byte) {
	// Skip broadcast message
	if to == 4294967295 {
		return
	}

	toFleetIdx, _, _, _ := n.FindNodes(to, from)
	if toFleetIdx == -1 {
		n.Config.Log.Errorf("Failed to find fleet index for node %d", to)
		return
	}
	// toNode := n.Nodes[toFleetIdx][to]
	fleetConfig := n.Config.Fleet[toFleetIdx]

	switch portNum {
	case meshtastic.PortNum_TEXT_MESSAGE_APP:
		var hasOTP = false

		// Check if OTP handler exists for this fleet
		if n.OTPHandler[toFleetIdx] == nil {
			n.Config.Log.Warnf("No OTP handler configured for fleet %d", toFleetIdx)
			return
		}

		totps, err := n.OTPHandler[toFleetIdx].CalculateTOTPWithAdjacentPeriods()
		if err != nil {
			n.Config.Log.Errorf("Failed to calculate TOTP: %v", err)
			return
		}
		message := string(payload)
		if strings.Contains(message, totps["current"]) || strings.Contains(message, totps["previous"]) || strings.Contains(message, totps["next"]) {
			hasOTP = true
		}

		chatBotMap := make(map[string]config.ChatBot)
		for _, cb := range fleetConfig.ChatBot {
			chatBotMap[cb.Type] = cb
		}

		if hasOTP {
			if chatBot, ok := chatBotMap["otp_success"]; ok {
				reply := chatBot.Message[0]
				n.Config.Log.Infof("PKI Reply for OTP success: %+v - %s", chatBot.Type, reply)
				n.sendPKIReply(toFleetIdx, to, from, topic, reply)
			}
		} else {
			if chatBot, ok := chatBotMap["otp_failure"]; ok {
				reply := chatBot.Message[0]
				n.Config.Log.Infof("PKI Reply for OTP failure: %+v - %s", chatBot.Type, reply)
				n.sendPKIReply(toFleetIdx, to, from, topic, reply)
			}
		}

	default:
		// if n.Config.VerboseLevel == "debug" || n.Config.VerboseLevel == "trace" {
		// 	n.Config.Log.Tracef(`{to: '%v', from: '%v', topic: '%v', portNum: '%s', lkpFrom: '%v', lkpTo: '%v'}`, to, from, topic, portNum, lkpFrom, lkpTo)
		// }
	}
}
