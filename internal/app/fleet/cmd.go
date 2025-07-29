package fleet

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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

type OTPUnlock struct {
	UnlockTimestamp time.Time
	UnlockMessage   string
}

type FleetCmd struct {
	Config     *config.Config
	Nodes      []internal.NodeDB
	NodesMutex []sync.Mutex
	MqttClient []*internal.MqttClient
	CmdOutput  struct {
		WasSuccess bool
	}
	OTPHandler   []*otp.TOTPConfig
	OTPUnlocks   []map[uint32]*OTPUnlock // Map from radio ID to unlock info, per fleet
	OTPUnlockMux []sync.RWMutex          // Mutex to protect the OTPUnlocks map, per fleet
}

const BACKSTOP_GRACE_SEC = 30

func NewFleets(c *config.Config) (f *FleetCmd) {
	f = new(FleetCmd)
	f.Config = c

	for i := 0; i < len(c.Fleet); i++ {
		f.Nodes = append(f.Nodes, make(internal.NodeDB))
		f.NodesMutex = append(f.NodesMutex, sync.Mutex{})
		f.OTPUnlocks = append(f.OTPUnlocks, make(map[uint32]*OTPUnlock))
		f.OTPUnlockMux = append(f.OTPUnlockMux, sync.RWMutex{})

		otpURL := f.Config.Fleet[i].OtpUrl
		if otpURL != "" {
			otpHandler, _ := otp.NewOTPHandler(otpURL)
			f.OTPHandler = append(f.OTPHandler, otpHandler)
		} else {
			f.OTPHandler = append(f.OTPHandler, nil)
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
		//n.Config.Log.Errorf("Failed to find fleet index for node %d", to)
		return
	}
	// toNode := n.Nodes[toFleetIdx][to]
	fleetConfig := n.Config.Fleet[toFleetIdx]

	switch portNum {
	case meshtastic.PortNum_TEXT_MESSAGE_APP:
		var hasOTP = false
		message := string(payload)

		if n.OTPHandler[toFleetIdx] != nil {
			totps, err := n.OTPHandler[toFleetIdx].CalculateTOTPWithAdjacentPeriods()
			if err != nil {
				n.Config.Log.Errorf("Failed to calculate TOTP: %v", err)
				return
			}
			if strings.Contains(message, totps["current"]) || strings.Contains(message, totps["previous"]) || strings.Contains(message, totps["next"]) {
				hasOTP = true
			}
		}

		// Check if the "from" radio has already unlocked OTP
		n.OTPUnlockMux[toFleetIdx].RLock()
		unlockInfo, isUnlocked := n.OTPUnlocks[toFleetIdx][from]
		n.OTPUnlockMux[toFleetIdx].RUnlock()

		// Check if unlock record is older than 1 hour and delete it if so
		if isUnlocked && time.Since(unlockInfo.UnlockTimestamp) > time.Hour {
			n.OTPUnlockMux[toFleetIdx].Lock()
			delete(n.OTPUnlocks[toFleetIdx], from)
			n.OTPUnlockMux[toFleetIdx].Unlock()
			n.Config.Log.Infof("Radio %d unlock record expired after 1 hour, removing from unlock map", from)
			isUnlocked = false
		}

		chatBotMap := make(map[string]config.ChatBot)
		for _, cb := range fleetConfig.ChatBot {
			chatBotMap[cb.Type] = cb
		}

		if isUnlocked {
			n.Config.Log.Infof("Radio %d is already unlocked (unlocked at: %v with message: %s)", from, unlockInfo.UnlockTimestamp, unlockInfo.UnlockMessage)
			if chatBot, ok := chatBotMap["chatmode_unlocked"]; ok {
				if strings.Contains(chatBot.Message[0], "`OPENAI=") {
					gptAgentUrl := strings.TrimSpace(strings.SplitN(chatBot.Message[0], "=", 2)[1])
					n.Config.Log.Infof("GPT Agent URL set to: %s", gptAgentUrl)
					if fleetConfig.OpenAIKey == "" {
						fleetConfig.OpenAIKey = os.Getenv("MESHTK_OPENAI_KEY")
					}

					if fleetConfig.OpenAIKey != "" {
						n.handleGPTChat(toFleetIdx, to, from, topic, message, fleetConfig.OpenAIKey, fleetConfig.OpenAISystemPrompt)
					} else {
						n.Config.Log.Errorf("OpenAI key not configured for fleet %d", toFleetIdx)
						n.sendPKIReply(toFleetIdx, to, from, topic, "OpenAI key not configured")
					}
				}
			}
		} else if hasOTP {
			if chatBot, ok := chatBotMap["otp_success"]; ok {
				for _, reply := range chatBot.Message {
					n.Config.Log.Infof("PKI Reply for OTP success: %+v - %s", chatBot.Type, reply)
					n.sendPKIReply(toFleetIdx, to, from, topic, reply)
				}

				if chatBot.UnlocksChatMode {
					// Store the unlock information
					n.OTPUnlockMux[toFleetIdx].Lock()
					n.OTPUnlocks[toFleetIdx][from] = &OTPUnlock{
						UnlockTimestamp: time.Now(),
						UnlockMessage:   message,
					}
					n.OTPUnlockMux[toFleetIdx].Unlock()
					n.Config.Log.Infof("Radio %d has been unlocked and stored in unlock map", from)
				}
			}
		} else {
			if chatBot, ok := chatBotMap["otp_failure"]; ok {
				for _, reply := range chatBot.Message {
					n.Config.Log.Infof("PKI Reply for OTP failure: %+v - %s", chatBot.Type, reply)
					n.sendPKIReply(toFleetIdx, to, from, topic, reply)
				}
			}
		}

	default:
		// if n.Config.VerboseLevel == "debug" || n.Config.VerboseLevel == "trace" {
		// 	n.Config.Log.Tracef(`{to: '%v', from: '%v', topic: '%v', portNum: '%s', lkpFrom: '%v', lkpTo: '%v'}`, to, from, topic, portNum, lkpFrom, lkpTo)
		// }
	}
}

// handleGPTChat calls the OpenAI GPT API and sends chunked responses back
func (n *FleetCmd) handleGPTChat(toFleetIdx int, to, from uint32, topic string, userMessage string, apiKey string, systemPrompt string) {
	n.Config.Log.Infof("Calling GPT-4 with message: %s", userMessage)

	// Call OpenAI API
	gptResponse, err := n.callOpenAIGPT(userMessage, apiKey, systemPrompt)
	if err != nil {
		n.Config.Log.Errorf("Failed to call GPT: %v", err)
		n.sendPKIReply(toFleetIdx, to, from, topic, "Error calling GPT")
		return
	}

	// Split response into 60-character chunks
	chunks := n.splitIntoChunks(gptResponse, 60)

	// Send each chunk as a separate PKI reply
	for i, chunk := range chunks {
		// Add a small delay between chunks to avoid overwhelming the mesh
		if i > 0 {
			time.Sleep(500 * time.Millisecond)
		}
		n.Config.Log.Infof("Sending GPT chunk %d/%d: %s", i+1, len(chunks), chunk)
		n.sendPKIReply(toFleetIdx, to, from, topic, chunk)
	}
}

// callOpenAIGPT makes the actual API call to OpenAI
func (n *FleetCmd) callOpenAIGPT(message string, apiKey string, systemPrompt string) (string, error) {
	// Use default system prompt if none provided
	if systemPrompt == "" {
		systemPrompt = "You are a helpful assistant communicating over a mesh network. Keep responses concise and under 240 characters total."
	}

	n.Config.Log.Debugf("Using system prompt: %s", systemPrompt)

	// Build messages array with system prompt
	messages := []map[string]string{
		{
			"role":    "system",
			"content": systemPrompt,
		},
		{
			"role":    "user",
			"content": message,
		},
	}

	// Use GPT-4o-mini for cost efficiency
	requestBody := map[string]interface{}{
		"model":       "gpt-4o-mini",
		"messages":    messages,
		"temperature": 0.8,
		"max_tokens":  150,
	}

	jsonBody, err := json.Marshal(requestBody)
	if err != nil {
		return "", fmt.Errorf("failed to marshal request: %v", err)
	}

	// Create HTTP request to OpenAI API endpoint
	apiEndpoint := "https://api.openai.com/v1/chat/completions"
	req, err := http.NewRequest("POST", apiEndpoint, bytes.NewBuffer(jsonBody))
	if err != nil {
		return "", fmt.Errorf("failed to create request: %v", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", apiKey))

	// Make the request
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to make request: %v", err)
	}
	defer resp.Body.Close()

	// Read response
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read response: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GPT API returned status %d: %s", resp.StatusCode, string(body))
	}

	// Parse response
	var response struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}

	if err := json.Unmarshal(body, &response); err != nil {
		return "", fmt.Errorf("failed to parse response: %v", err)
	}

	if response.Error.Message != "" {
		return "", fmt.Errorf("GPT API error: %s", response.Error.Message)
	}

	if len(response.Choices) == 0 {
		return "", fmt.Errorf("no response from GPT")
	}

	return strings.TrimSpace(response.Choices[0].Message.Content), nil
}

// splitIntoChunks splits a string into chunks of specified size
func (n *FleetCmd) splitIntoChunks(text string, chunkSize int) []string {
	if len(text) == 0 {
		return []string{}
	}

	var chunks []string
	for i := 0; i < len(text); i += chunkSize {
		end := i + chunkSize
		if end > len(text) {
			end = len(text)
		}
		chunks = append(chunks, text[i:end])
	}

	return chunks
}
