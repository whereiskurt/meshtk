package fleet

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/whereiskurt/meshtk/internal/keycache"
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
	OTPHandler      []*otp.TOTPConfig
	OTPUnlocks      []map[uint32]*OTPUnlock // Map from radio ID to unlock info, per fleet
	OTPUnlockMux    []sync.RWMutex          // Mutex to protect the OTPUnlocks map, per fleet
	LyricsResponded []map[uint32]int        // Track which 'to' addresses have received lyrics responses, per fleet
	LyricsRespMux   []sync.RWMutex          // Mutex to protect the LyricsResponded map, per fleet

	// KeyResolver is the ONE process-wide authoritative pubkey resolver
	// (internal/keycache, DDB MeshRadio) shared by EVERY fleet MqttClient — the
	// fleet-wide generalization of crypto.go's pubKeyCache. Built once in
	// NewFleets; nil if the store failed to build (falls back to nodes.json).
	KeyResolver *keycache.KeyResolver
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
		f.LyricsResponded = append(f.LyricsResponded, make(map[uint32]int))
		f.LyricsRespMux = append(f.LyricsRespMux, sync.RWMutex{})

		otpURL := f.Config.Fleet[i].OtpUrl
		if otpURL != "" {
			otpHandler, _ := otp.NewOTPHandler(otpURL)
			f.OTPHandler = append(f.OTPHandler, otpHandler)
		} else {
			f.OTPHandler = append(f.OTPHandler, nil)
		}
	}

	f.KeyResolver = buildKeyResolver(c)

	return f
}

// buildKeyResolver constructs the ONE shared authoritative pubkey resolver for
// the whole fleet (mirrors server/cmd.go's credcache construction, but keycache
// uses GetItem, and a shorter 60-120s TTL). Returns nil on store-build failure
// so the fleet degrades to the nodes.json fallback rather than refusing to boot.
func buildKeyResolver(c *config.Config) *keycache.KeyResolver {
	kc := c.Server.KeyCache

	// NOTE: buildKeyResolver runs inside NewFleets during early bootstrap
	// (NewApp -> RegisterOsArgs -> NewFleets), BEFORE c.Log is wired, so c.Log
	// is nil here. Guard every logger call — an unconditional c.Log.*f panics
	// the whole meshtk process at startup (SIGSEGV in logrus). Runtime resolve/
	// fallback logging (crypto.go) is unaffected: c.Log is set by decrypt time.
	cache, err := keycache.NewCache(kc.TTLSecs, kc.MaxSizeMB)
	if err != nil {
		if c.Log != nil {
			c.Log.Errorf("keycache: failed to create pubkey cache (%v); falling back to nodes.json", err)
		}
		return nil
	}

	store, err := keycache.NewDynamoDBStore(kc.TableName, kc.TableRegion, kc.DynamoDBEndpoint)
	if err != nil {
		if c.Log != nil {
			c.Log.Errorf("keycache: failed to create DynamoDB store (%v); falling back to nodes.json", err)
		}
		return nil
	}

	resolver := keycache.NewKeyResolver(cache, store,
		keycache.WithNegativeTTL(time.Duration(kc.NegativeTTLSecs)*time.Second),
	)
	if c.Log != nil {
		c.Log.Infof("keycache: authoritative pubkey resolver ready (table=%s region=%s ttl=%ds fallback=%q)",
			kc.TableName, kc.TableRegion, kc.TTLSecs, kc.Fallback)
	}
	return resolver
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
		f.overrideFleetKeys(idx)
		mqttClient := internal.NewMqttClient(f.Config, &f.Nodes[idx], f.FleetNodeHandler)

		// Thread the ONE shared authoritative pubkey resolver into every client so
		// the whole fleet resolves decrypt/reply keys through a single cache
		// (never per-client, never per-packet). Fallback governs miss behavior.
		mqttClient.SetKeyResolver(f.KeyResolver, f.Config.Server.KeyCache.Fallback)

		// Set up ACK handler for this fleet member
		fleetIdx := idx // Capture idx for closure
		mqttClient.SetAckHandler(func(to, from uint32, requestId uint32) {
			// Find the node in the fleet  
			if node, exists := f.Nodes[fleetIdx][to]; exists {
				f.publishACK(fleetIdx, node, from, requestId)
			}
		})
		
		// Set up NACK handler to trigger nodeinfo request
		mqttClient.SetNackHandler(func(to, from uint32, requestId uint32) {
			// Find the node in the fleet and request nodeinfo
			if node, exists := f.Nodes[fleetIdx][to]; exists {
				f.publishNodeInfoRequest(fleetIdx, node, from)
			}
		})
		
		// Chatbot ghosts must receive incoming DMs to reply. Relying on the
		// lazy publish-time connect (whose OnConnect subscribe races and can
		// silently fail behind the meshtk proxy) leaves them publish-only, so
		// explicitly ConnectAndListen here to deterministically connect and
		// subscribe. Scope the subscription to just the PKI direct-message
		// topics (regioned + region-less) — a chatbot ghost only needs DMs, and
		// subscribing every ghost to the full firehose (#) starves the tiny
		// ghosts container. Movement-only sim fleets (rabbits) never chat, skip.
		if len(f.Config.Fleet[idx].ChatBot) > 0 {
			dmTopics := []string{"msh/+/2/e/PKI/#", "msh/2/e/PKI/#"}
			if err := mqttClient.ConnectAndListen(dmTopics); err != nil {
				f.Config.Log.Errorf("Fleet[%d] %s: ConnectAndListen failed, DMs will not be received: %v", idx, f.Config.Fleet[idx].Id, err)
			} else {
				f.Config.Log.Infof("Fleet[%d] %s: connected and listening for DMs on %v", idx, f.Config.Fleet[idx].Id, dmTopics)
			}
		}

		f.MqttClient = append(f.MqttClient, mqttClient)
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
				//f.Config.Stdout.Write([]byte(fmt.Sprintf("🚀 Fleet[%d]: MQTT connect ...\n", idx)))
				//f.Config.Stdout.Write([]byte(fmt.Sprintf("🚀 Fleet[%d]: Starting simulation ...\n", idx)))
				f.simulate(idx)
				//f.Config.Stdout.Write([]byte(fmt.Sprintf("🚀 Fleet[%d]: Simulation completed.\n", idx)))
				fleetdone <- true
				alldone <- idx
			}(idx)

			// Setup backstop timeouts to wait for the simulation to finish
			// This is to prevent the simulation from running indefinitely
			t := BACKSTOP_GRACE_SEC + f.Config.Fleet[idx].RampUpSecs + f.Config.Fleet[idx].RampSteadySecs + f.Config.Fleet[idx].RampDownSecs
			backstop := time.After(time.Duration(t) * time.Second)
			select {
			case <-fleetdone:
				//f.Config.Stdout.Write([]byte(fmt.Sprintf("🚀 Fleet[%d]: Done! Simulation completed successfully.\n", idx)))
			case <-backstop:
				//f.Config.Stdout.Write([]byte(fmt.Sprintf("🚀 Fleet[%d]: Backstopped! Simulation timeout expired after %d seconds.\n", idx, t)))
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

// ownGatewayTopic builds the MQTT topic a fleet node publishes its OWN packets
// on: "<channelBase>/!<nodeNum hex>". Every meshtk publisher (ACK, NodeInfo,
// position, and now the chatbot reply) must use the replying node's own gateway
// id here — NOT the incoming sender's topic — or the receiving device silently
// drops the packet as a self-echo (it sees its own gateway id in the topic).
func ownGatewayTopic(channelBase string, nodeNum uint32) string {
	return fmt.Sprintf("%s/!%08x", channelBase, nodeNum)
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

	// Reply-encrypt recipient key MUST come from the same authoritative resolver
	// as the decrypt site (crypto.go). Migrating only decrypt would encrypt replies
	// to a stale/poisoned nodes.json key (landmine L4).
	senderPubKeyHex, err := n.MqttClient[toFleetIdx].ResolveSenderPubKey(from)
	if err != nil {
		n.Config.Log.Errorf("failed to resolve recipient public key: %v", err)
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

	// Publish the reply on THIS ghost's OWN gateway topic, mirroring
	// publishACK/publishNodeInfo (behaviours.go). Reusing the incoming `topic`
	// puts the reply on the *sender's* gateway topic (e.g. msh/US/2/e/PKI/!<sender>),
	// which the sending device ignores as a self-echo — so chatbot replies never
	// arrived at the device even though they were published, ACKed, and correctly
	// PKI-encrypted. `to` is the replying ghost's node num. (Phase 66 field bug,
	// confirmed on-wire: republishing a real reply to !<ghost> made it appear.)
	replyTopic := ownGatewayTopic(n.Config.NodeInfo.Topic, to)

	// Send the PKI encrypted reply
	// Note: from and to are swapped for the reply
	err = n.MqttClient[toFleetIdx].PublishPKIMessage(
		to,   // sender of reply (was receiver of original message)
		from, // receiver of reply (was sender of original message)
		replyTopic,
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
				}
			}
		} else {
			if chatBot, ok := chatBotMap["otp_failure"]; ok {
				if chatBot.UnlocksChatMode {
					// Store the unlock information
					n.OTPUnlockMux[toFleetIdx].Lock()
					n.OTPUnlocks[toFleetIdx][from] = &OTPUnlock{
						UnlockTimestamp: time.Now(),
						UnlockMessage:   message,
					}
					n.OTPUnlockMux[toFleetIdx].Unlock()

					if chatBot, ok := chatBotMap["chatmode_lyrics"]; ok {
						lyrics := chatBot.Message[0]
						n.Config.Log.Infof("Lyrics set to: %s", lyrics)
						n.handleLyricsChat(toFleetIdx, to, from, topic, lyrics)
					}
				} else {
					for _, reply := range chatBot.Message {
						n.Config.Log.Infof("PKI Reply for OTP failure: %+v - %s", chatBot.Type, reply)
						n.sendPKIReply(toFleetIdx, to, from, topic, reply)
					}
				}
			}

		}

	default:
		// if n.Config.VerboseLevel == "debug" || n.Config.VerboseLevel == "trace" {
		// 	n.Config.Log.Tracef(`{to: '%v', from: '%v', topic: '%v', portNum: '%s', lkpFrom: '%v', lkpTo: '%v'}`, to, from, topic, portNum, lkpFrom, lkpTo)
		// }
	}
}

func (n *FleetCmd) handleLyricsChat(toFleetIdx int, to, from uint32, topic string, lyricsB64 string) {
	// Check if we've already responded to this 'to' address
	n.LyricsRespMux[toFleetIdx].RLock()
	count, exists := n.LyricsResponded[toFleetIdx][to]
	n.LyricsRespMux[toFleetIdx].RUnlock()
	
	if exists && count > 0 {
		n.Config.Log.Infof("Already responded to lyrics request for 'to' address %d (count: %d), skipping", to, count)
		return
	}
	
	// Mark this 'to' address as having received a response
	n.LyricsRespMux[toFleetIdx].Lock()
	n.LyricsResponded[toFleetIdx][to]++
	n.LyricsRespMux[toFleetIdx].Unlock()
	
	// Decode base64 lyrics
	lyricsBytes, err := base64.StdEncoding.DecodeString(lyricsB64)
	if err != nil {
		n.Config.Log.Errorf("Failed to decode base64 lyrics: %v", err)
		n.sendPKIReply(toFleetIdx, to, from, topic, "I'm feeling shy.")
		return
	}

	lyrics := string(lyricsBytes)
	lines := strings.Split(lyrics, "\n")

	// Parse song length from first line [length: MM:SS]
	var songDuration time.Duration
	var lyricEntries []struct {
		timestamp time.Duration
		text      string
	}

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		// Parse length header
		if strings.HasPrefix(line, "[length:") {
			lengthStr := strings.TrimSuffix(strings.TrimPrefix(line, "[length: "), "]")
			parts := strings.Split(lengthStr, ":")
			if len(parts) == 2 {
				minutes, _ := strconv.Atoi(parts[0])
				seconds, _ := strconv.Atoi(parts[1])
				songDuration = time.Duration(minutes)*time.Minute + time.Duration(seconds)*time.Second
			}
			continue
		}

		// Parse lyric lines [MM:SS.MS]text
		if strings.HasPrefix(line, "[") {
			endIdx := strings.Index(line, "]")
			if endIdx > 0 {
				timeStr := line[1:endIdx]
				text := line[endIdx+1:]

				// Parse timestamp MM:SS.MS
				parts := strings.Split(timeStr, ":")
				if len(parts) == 2 {
					minutes, _ := strconv.ParseFloat(parts[0], 64)
					seconds, _ := strconv.ParseFloat(parts[1], 64)
					timestamp := time.Duration(minutes*60+seconds) * time.Second

					lyricEntries = append(lyricEntries, struct {
						timestamp time.Duration
						text      string
					}{timestamp, text})
				}
			}
		}
	}

	if len(lyricEntries) == 0 {
		n.Config.Log.Errorf("No valid lyric entries found")
		n.sendPKIReply(toFleetIdx, to, from, topic, "It's a short one.")
		return
	}

	// Start goroutine to send lyrics at scheduled times
	go func() {
		startTime := time.Now()
		defer func() {
			n.Config.Log.Infof("Lyrics playback completed for conversation from %d to %d", from, to)
		}()

		// Create a timer for self-termination
		var terminationTimer *time.Timer
		if songDuration > 0 {
			terminationTimer = time.NewTimer(songDuration)
			defer terminationTimer.Stop()
		} else {
			// Fallback: use last lyric timestamp + 10 seconds
			lastTimestamp := lyricEntries[len(lyricEntries)-1].timestamp
			terminationTimer = time.NewTimer(lastTimestamp + 10*time.Second)
			defer terminationTimer.Stop()
		}

		for _, entry := range lyricEntries {
			select {
			case <-terminationTimer.C:
				n.Config.Log.Infof("Lyrics playback terminated after song duration")
				return
			case <-time.After(entry.timestamp - time.Since(startTime)):
				n.sendPKIReply(toFleetIdx, to, from, topic, entry.text)
				n.Config.Log.Debugf("Sent lyric at %v: %s", entry.timestamp, entry.text)
			}
		}
	}()

	n.Config.Log.Infof("Started lyrics playback for %d entries over %v", len(lyricEntries), songDuration)
}

func (n *FleetCmd) handleGPTChat(toFleetIdx int, to, from uint32, topic string, userMessage string, apiKey string, systemPrompt string) {
	n.Config.Log.Infof("Calling GPT-4 with message: %s", userMessage)

	gptResponse, err := n.callOpenAIGPT(userMessage, apiKey, systemPrompt)
	if err != nil {
		n.Config.Log.Errorf("Failed to call GPT: %v", err)
		n.sendPKIReply(toFleetIdx, to, from, topic, "Error calling GPT")
		return
	}

	chunks := n.splitIntoChunks(gptResponse, 60)

	for i, chunk := range chunks {
		if i == 0 {
			time.Sleep(500 * time.Millisecond)
		}
		n.sendPKIReply(toFleetIdx, to, from, topic, chunk)
		time.Sleep(500 * time.Millisecond)
	}
}

func (n *FleetCmd) callOpenAIGPT(message string, apiKey string, systemPrompt string) (string, error) {
	if systemPrompt == "" {
		systemPrompt = "You are a helpful assistant communicating over a mesh network. Keep responses concise and under 240 characters total."
	}

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

	apiEndpoint := "https://api.openai.com/v1/chat/completions"
	req, err := http.NewRequest("POST", apiEndpoint, bytes.NewBuffer(jsonBody))
	if err != nil {
		return "", fmt.Errorf("failed to create request: %v", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", apiKey))

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to make request: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read response: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GPT API returned status %d: %s", resp.StatusCode, string(body))
	}

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
// It attempts to break at whitespace boundaries to avoid splitting words
func (n *FleetCmd) splitIntoChunks(text string, chunkSize int) []string {
	if len(text) == 0 {
		return []string{}
	}

	var chunks []string
	remaining := text

	for len(remaining) > 0 {
		// If the remaining text fits in one chunk, add it and we're done
		if len(remaining) <= chunkSize {
			chunks = append(chunks, remaining)
			break
		}

		// Find the last whitespace within the chunk size limit
		chunkEnd := chunkSize
		for i := chunkSize - 1; i >= 0; i-- {
			if remaining[i] == ' ' || remaining[i] == '\n' || remaining[i] == '\t' {
				chunkEnd = i
				break
			}
		}

		// If no whitespace found, fall back to the original behavior
		// This handles cases where a single word is longer than chunkSize
		if chunkEnd == chunkSize {
			// Check if we're at the start and there's no whitespace at all
			// in the first chunkSize characters
			foundSpace := false
			for i := 0; i < chunkSize && i < len(remaining); i++ {
				if remaining[i] == ' ' || remaining[i] == '\n' || remaining[i] == '\t' {
					foundSpace = true
					break
				}
			}
			if !foundSpace {
				// No whitespace found, just break at chunk size
				chunks = append(chunks, remaining[:chunkSize])
				remaining = remaining[chunkSize:]
				continue
			}
		}

		// Extract the chunk and trim any trailing whitespace
		chunk := strings.TrimRight(remaining[:chunkEnd], " \n\t")
		chunks = append(chunks, chunk)

		// Move past the chunk and any leading whitespace
		remaining = strings.TrimLeft(remaining[chunkEnd:], " \n\t")
	}

	return chunks
}
