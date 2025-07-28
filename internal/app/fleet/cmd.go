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

	f.Config.Log.Tracef("Find nodes for to=%d, from=%d", to, from)

	f.Config.Log.Tracef("Total Fleets: %d", len(f.Nodes))

	for fleetIdx := range f.Nodes {
		fleetNodes := f.Nodes[fleetIdx]
		f.Config.Log.Tracef("Total Fleet Nodes: %d", len(fleetNodes))
		for nodeID, node := range fleetNodes {
			f.Config.Log.Tracef("Fleet[%d] NodeID: %d, Node: %+v", fleetIdx, nodeID, node)
			f.Config.Log.Tracef("to[%d][%d]", to, nodeID)
			f.Config.Log.Tracef("from[%d][%d]", from, nodeID)
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

func (n *FleetCmd) FleetNodeHandler(to, from uint32, topic string, portNum meshtastic.PortNum, payload []byte) {
	// Skip broadcast message
	if to == 4294967295 {
		return
	}

	toFleetIdx, _, _, _ := n.FindNodes(to, from)

	toNode := n.Nodes[toFleetIdx][to]
	fleetConfig := n.Config.Fleet[toFleetIdx]

	switch portNum {
	case meshtastic.PortNum_TEXT_MESSAGE_APP:

		totps, _ := n.OTPHandler[toFleetIdx].CalculateTOTPWithAdjacentPeriods()
		message := string(payload)
		if strings.Contains(message, totps["current"]) || strings.Contains(message, totps["previous"]) || strings.Contains(message, totps["next"]) {
			n.Config.Log.Infof("TOTP found in message for node[%s]: %s", toNode.LongName, message)
			n.Config.Log.Infof("BEGIN PKI Reply logic here: %+v", fleetConfig)

		} else {
			n.Config.Log.Infof("TOTP NOT found in message for node[%s]: %s", toNode.LongName, message)
		}
		// if err == nil {
		// 	n.Config.Log.Infof("TOTP for node[%d]: Current: %s (valid for %s more seconds)", fromFleetIdx, totps["current"], totps["remainingSeconds"])
		// 	n.Config.Log.Infof("TOTP for node[%d]: Previous: %s, Next: %s", fromFleetIdx, totps["previous"], totps["next"])
		// 	n.Config.Log.Infof("TOTP for node[%d]: Period: %s seconds, Current period started: %s", fromFleetIdx, totps["period"], totps["currentPeriodStart"])
		// } else {
		// 	n.Config.Log.Errorf("Failed to calculate TOTP: %v", err)
		// }

		// if n.Config.VerboseLevel == "debug" || n.Config.VerboseLevel == "trace" {
		// 	n.Config.Log.Tracef("FleetNodeHandler: to=%v, from=%v, topic=%s, portNum=%s, lkpFrom=%v, lkpTo=%v", to, from, topic, portNum, toFleetIdx, fromFleetIdx)
		// 	n.Config.Log.Tracef(`{to: '%v', from: '%v', topic: '%v', message: '%s', lkpFrom: '%v', lkpTo: '%v'}`, to, from, topic, payload, toFleetIdx, fromFleetIdx)
		// }

	default:
		// if n.Config.VerboseLevel == "debug" || n.Config.VerboseLevel == "trace" {
		// 	n.Config.Log.Tracef(`{to: '%v', from: '%v', topic: '%v', portNum: '%s', lkpFrom: '%v', lkpTo: '%v'}`, to, from, topic, portNum, lkpFrom, lkpTo)
		// }
	}
}
