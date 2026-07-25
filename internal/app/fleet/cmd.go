package fleet

import (
	"context"
	"encoding/base64"
	"fmt"
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
	// RevealURL caches this unlock session's single-use flag-claim link so a
	// re-trigger re-sends the SAME link (one mint per radio per unlock — no
	// token farming). Dies with the record's 1-hour expiry. See claimlink.go.
	RevealURL string
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
	Challenge       []*FlagChallengeRuntime // per-fleet covert flag challenge (nil if none)
	OTPUnlocks      []map[uint32]*OTPUnlock // Map from radio ID to unlock info, per fleet
	OTPUnlockMux    []sync.RWMutex          // Mutex to protect the OTPUnlocks map, per fleet
	LyricsResponded []map[uint32]time.Time  // When each REQUESTER ('from') last got lyrics, per fleet (cooldown, not once-ever)
	LyricsRespMux   []sync.RWMutex          // Mutex to protect the LyricsResponded map, per fleet
	RecentReq       []map[string]time.Time  // Recent inbound DMs, for retransmit dedup, per fleet
	RecentReqMux    []sync.Mutex            // Mutex to protect the RecentReq map, per fleet

	// Pending holds one-shot replies that may have been published into a gap
	// while the recipient was disconnected, keyed by recipient node. Flushed
	// when we next see that node transmit. Process-wide, not per fleet: the
	// recipient is a radio, not a fleet member.
	Pending    map[uint32][]*pendingReply
	PendingMux sync.Mutex

	// ReplyNextAt staggers consecutive one-shot reply lines to the same
	// recipient (see sendPKIReplyReliable); keyed by recipient node num.
	ReplyNextAt    map[uint32]time.Time
	ReplyNextAtMux sync.Mutex

	// KeyResolver is the ONE process-wide authoritative pubkey resolver
	// (internal/keycache, DDB MeshRadio) shared by EVERY fleet MqttClient — the
	// fleet-wide generalization of crypto.go's pubKeyCache. Built once in
	// NewFleets; nil if the store failed to build (falls back to nodes.json).
	KeyResolver *keycache.KeyResolver
}

// FlagChallengeRuntime is the per-fleet, resolved covert-flag challenge: trigger
// phrases, the reveal template, and the DERIVED code (never the committed decoy).
// The derived code lives here and is filled into the reveal server-side — it is
// never placed in the LLM system prompt or user turn.
type FlagChallengeRuntime struct {
	Triggers       []string
	RevealTemplate string
	DerivedCode    string
}

func matchesTrigger(rt *FlagChallengeRuntime, msg string) bool {
	if rt == nil {
		return false
	}
	low := strings.ToLower(msg)
	for _, t := range rt.Triggers {
		if t != "" && strings.Contains(low, strings.ToLower(t)) {
			return true
		}
	}
	return false
}

func renderReveal(rt *FlagChallengeRuntime) string {
	return strings.ReplaceAll(rt.RevealTemplate, "%CODE%", rt.DerivedCode)
}

const BACKSTOP_GRACE_SEC = 30

// cannedRefusal is the short in-character reply sent when a guardrail blocks an
// inbound message or an LLM reply (never the offending text).
const cannedRefusal = "👻 …not touching that one."

// otpAcceptWindowEachSide is how many TOTP periods on each side of "now" the bot
// accepts an OTP for. At period=30 (which phone authenticator apps honor, unlike
// 120), ±6 gives ~3 minutes of tolerance — enough for device-clock skew plus a
// LoRa round-trip, without an excessive replay window. (The real UAT failure was
// the derivation being OFF, not the window; see NewFleets' ghostSecret fallback.)
const otpAcceptWindowEachSide = 6

func NewFleets(c *config.Config) (f *FleetCmd) {
	f = new(FleetCmd)
	f.Config = c

	// The GhostKeySecret global flag/env binding does not reliably populate
	// c.GhostKeySecret in the `fleet simulate` subcommand (MapEnvVars misses it),
	// which silently left the OTP/flag derivation OFF — the bot validated against
	// the COMMITTED decoy seed instead of the derived one. Read the env directly
	// as a fallback. Scoped to the OTP + flag-code derivation ONLY (a local var):
	// deliberately NOT applied to the node keypair path (nodes.go), so this does
	// not re-key live ghosts mid-event.
	ghostSecret := c.GhostKeySecret
	if ghostSecret == "" {
		ghostSecret = os.Getenv("MESHTK_GHOST_KEY_SECRET")
	}

	// Covert flag challenges arrive out-of-band via the SOPS-backed env blob so the
	// committed config carries no trigger/reveal/decoy. Parse failure disables all
	// challenges (empty map) rather than bricking fleet boot.
	challenges, cerr := config.ParseFlagChallenges(os.Getenv("MESHTK_FLAG_CHALLENGES"))
	if cerr != nil && c.Log != nil {
		c.Log.Errorf("⚠️ MESHTK_FLAG_CHALLENGES parse failed (challenges disabled): %v", cerr)
	}

	for i := 0; i < len(c.Fleet); i++ {
		f.Nodes = append(f.Nodes, make(internal.NodeDB))
		f.NodesMutex = append(f.NodesMutex, sync.Mutex{})
		f.OTPUnlocks = append(f.OTPUnlocks, make(map[uint32]*OTPUnlock))
		f.OTPUnlockMux = append(f.OTPUnlockMux, sync.RWMutex{})
		f.LyricsResponded = append(f.LyricsResponded, make(map[uint32]time.Time))
		f.LyricsRespMux = append(f.LyricsRespMux, sync.RWMutex{})
		f.RecentReq = append(f.RecentReq, make(map[string]time.Time))
		f.RecentReqMux = append(f.RecentReqMux, sync.Mutex{})

		otpURL := f.Config.Fleet[i].OtpUrl
		if otpURL != "" && ghostSecret != "" {
			// Same server-secret munging as the node keypairs (ApplyDerivedKey):
			// the committed OtpUrl secret is a decoy HKDF input; the handler
			// validates against the derived secret. A derivation failure is
			// fail-CLOSED (nil handler, OTP never unlocks) — never a silent
			// fallback to the committed plaintext.
			derived, err := otp.DeriveOtpUrl(ghostSecret, f.Config.Fleet[i].Id, otpURL)
			if err != nil {
				c.Log.Errorf("⚠️ OTP secret derivation failed for %s (OTP disabled): %v", f.Config.Fleet[i].Id, err)
				otpURL = ""
			} else {
				otpURL = derived
			}
		}
		if otpURL != "" {
			otpHandler, _ := otp.NewOTPHandler(otpURL)
			f.OTPHandler = append(f.OTPHandler, otpHandler)
		} else {
			f.OTPHandler = append(f.OTPHandler, nil)
		}

		// Resolve the covert flag challenge for this ghost from the env blob and
		// derive the REAL code (decoy → HKDF). Nothing is injected into the persona
		// prompt; the derived code is held on the runtime challenge and filled into
		// the reveal server-side when a trigger fires. Fail-closed: any problem →
		// nil challenge (no reveal), never a leak of the committed decoy.
		var rt *FlagChallengeRuntime
		if ch, ok := challenges[f.Config.Fleet[i].Id]; ok && ghostSecret != "" {
			derived, derr := otp.DeriveFlagCode(ghostSecret, f.Config.Fleet[i].Id, ch.CommittedCode)
			if derr != nil {
				c.Log.Errorf("⚠️ flag code derivation failed for %s (challenge disabled): %v", f.Config.Fleet[i].Id, derr)
			} else {
				rt = &FlagChallengeRuntime{Triggers: ch.Triggers, RevealTemplate: ch.RevealTemplate, DerivedCode: derived}
			}
		}
		f.Challenge = append(f.Challenge, rt)
	}

	f.Pending = make(map[uint32][]*pendingReply)
	f.ReplyNextAt = make(map[uint32]time.Time)

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

	presenceWatcherAssigned := false
	for idx := range f.Config.Fleet {
		f.initNodeDb(idx)
		f.overrideFleetKeys(idx)
		mqttClient := internal.NewMqttClient(f.Config, &f.Nodes[idx], f.FleetNodeHandler)

		// Thread the ONE shared authoritative pubkey resolver into every client so
		// the whole fleet resolves decrypt/reply keys through a single cache
		// (never per-client, never per-packet). Fallback governs miss behavior.
		mqttClient.SetKeyResolver(f.KeyResolver, f.Config.Server.KeyCache.Fallback)

		// Set up ACK handler for this fleet member. AckMode "off" wires no
		// handler at all -- the radio then exhausts its retransmissions, which
		// isolates whether our acks disturb the device (A/B experiment).
		fleetIdx := idx // Capture idx for closure
		if ackEnabled(f.Config.AckMode) {
			mqttClient.SetAckStyle(f.Config.AckMode)
			mqttClient.SetAckHandler(func(to, from uint32, requestId uint32) {
				// Find the node in the fleet
				if node, exists := f.Nodes[fleetIdx][to]; exists {
					f.publishACK(fleetIdx, node, from, requestId)
				}
			})
		} else if idx == 0 {
			f.Config.Log.Warnf("AckMode=off: fleets will NOT ack PKI DMs (radio retransmits until exhausted)")
		}

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

			// Exactly ONE chatbot fleet also watches the channel topic, to see
			// radios transmit and drive the pending-reply flush (onNodeSeen).
			// Only one: FleetNodeHandler is shared across every client, so a
			// second subscriber would duplicate every packet's work for no gain
			// and starve the small ghosts container.
			if !presenceWatcherAssigned {
				presenceWatcherAssigned = true
				dmTopics = append(dmTopics, f.Config.NodeInfo.Topic+"/+")
				f.Config.Log.Infof("Fleet[%d] %s: also watching %s for radio presence (pending-reply flush)", idx, f.Config.Fleet[idx].Id, f.Config.NodeInfo.Topic+"/+")
			}
			if err := mqttClient.ConnectAndListen(dmTopics); err != nil {
				f.Config.Log.Errorf("Fleet[%d] %s: ConnectAndListen failed, DMs will not be received: %v", idx, f.Config.Fleet[idx].Id, err)
			} else {
				f.Config.Log.Infof("Fleet[%d] %s: connected and listening for DMs on %v", idx, f.Config.Fleet[idx].Id, dmTopics)
			}
		}

		f.MqttClient = append(f.MqttClient, mqttClient)
	}

	// OTP delivery poller: drain run.human's MeshOtpPending queue and PKI-DM
	// verification codes to devices from the NodeInfo (map) node identity.
	if store := f.buildOtpStore(); store != nil && len(f.MqttClient) > 0 {
		go f.startOtpPoller(context.Background(), store, otpPollInterval)
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

// replyTopicFor builds the topic a chatbot ghost publishes its PKI reply on:
// the SAME channel base as the incoming DM (always ".../2/e/PKI"), but with the
// replying ghost's OWN gateway id, "<base>/!<ghostNum hex>".
//
// Two constraints, both learned the hard way on-wire:
//  1. Own gateway, not the sender's: reusing the incoming topic
//     (".../PKI/!<sender>") publishes on the SENDER's gateway topic, which the
//     sending device ignores as a self-echo — replies never displayed.
//  2. PKI base, not the channel base: a PKI-encrypted reply republished on the
//     channel topic (".../dc.run/!<ghost>") is dropped by the device, which
//     tries CHANNEL decryption on it. The DM must stay on the PKI base so the
//     device routes it to PKI decryption. Deriving the base from the incoming
//     DM topic (the fleet only subscribes to ".../2/e/PKI/#") keeps it there.
func replyTopicFor(incomingTopic string, ghostNum uint32) string {
	base := incomingTopic
	if i := strings.LastIndex(incomingTopic, "/"); i >= 0 {
		base = incomingTopic[:i]
	}
	return fmt.Sprintf("%s/!%08x", base, ghostNum)
}

func (n *FleetCmd) sendPKIReply(toFleetIdx int, to, from uint32, topic string, reply string) {
	replyTopic, envelope, ok := n.buildPKIReply(toFleetIdx, to, from, topic, reply)
	if !ok {
		return
	}
	if err := n.MqttClient[toFleetIdx].PublishEnvelopeBytes(replyTopic, envelope); err != nil {
		n.Config.Log.Errorf("Failed to send PKI reply: %v", err)
	} else {
		n.Config.Log.Infof("Successfully sent PKI reply from %d to %d: %s", to, from, reply)
	}
}

// buildPKIReply resolves keys and constructs the marshaled reply envelope
// WITHOUT publishing. The reliable path builds once and re-sends the same
// bytes, so every copy shares one packet id and the device's dedup makes
// repeats invisible -- re-sending fresh-built packets (new id each time) is
// what used to display duplicate reply lines.
func (n *FleetCmd) buildPKIReply(toFleetIdx int, to, from uint32, topic string, reply string) (replyTopic string, envelope []byte, ok bool) {
	// Get the nodes to access their keys
	_, toNode, _, _ := n.FindNodes(to, from)

	if toNode == nil {
		n.Config.Log.Errorf("Failed to find nodes: to=%d, from=%d", to, from)
		return "", nil, false
	}

	// The 'to' node (receiver of original message) is now the sender of the reply
	senderPrivKey := toNode.PrivKey

	// Reply-encrypt recipient key MUST come from the same authoritative resolver
	// as the decrypt site (crypto.go). Migrating only decrypt would encrypt replies
	// to a stale/poisoned nodes.json key (landmine L4).
	senderPubKeyHex, err := n.MqttClient[toFleetIdx].ResolveSenderPubKey(from)
	if err != nil {
		n.Config.Log.Errorf("failed to resolve recipient public key: %v", err)
		return "", nil, false
	}

	// The 'from' node (sender of original message) is now the receiver of the reply
	recipientPubKeyBytes, err := n.MqttClient[toFleetIdx].ParseHexKey(senderPubKeyHex)
	if err != nil {
		n.Config.Log.Errorf("Failed to parse recipient public key: %v", err)
		return "", nil, false
	}

	// n.Config.Log.Tracef("Sending PKI reply from node %d to node %d", to, from)
	// n.Config.Log.Tracef("Sender private key: %s", senderPrivKey)
	// n.Config.Log.Tracef("Recipient public key: %s", recipientPubKey)

	// Parse the hex keys
	senderPrivKeyBytes, err := n.MqttClient[toFleetIdx].ParseHexKey(senderPrivKey)
	if err != nil {
		n.Config.Log.Errorf("Failed to parse sender private key: %v", err)
		return "", nil, false
	}

	// Publish the reply on THIS ghost's OWN gateway topic, mirroring
	// publishACK/publishNodeInfo (behaviours.go). Reusing the incoming `topic`
	// puts the reply on the *sender's* gateway topic (e.g. msh/US/2/e/PKI/!<sender>),
	// which the sending device ignores as a self-echo — so chatbot replies never
	// arrived at the device even though they were published, ACKed, and correctly
	// PKI-encrypted. `to` is the replying ghost's node num. (Phase 66 field bug,
	// confirmed on-wire: republishing a real reply to !<ghost> made it appear.)
	replyTopic = replyTopicFor(topic, to)

	// Build the PKI encrypted reply
	// Note: from and to are swapped for the reply
	envelope, err = n.MqttClient[toFleetIdx].BuildPKIMessage(
		to,   // sender of reply (was receiver of original message)
		from, // receiver of reply (was sender of original message)
		meshtastic.PortNum_TEXT_MESSAGE_APP,
		[]byte(reply),
		senderPrivKeyBytes,
		recipientPubKeyBytes,
	)
	if err != nil {
		n.Config.Log.Errorf("Failed to build PKI reply: %v", err)
		return "", nil, false
	}

	return replyTopic, envelope, true
}

// One-shot chatbot replies (goldstein/dt/mudge/condor style: a single packet)
// are re-sent pkiReplyRetryCount times, pkiReplyRetrySpacing apart. The iOS
// BLE↔MQTT proxy reconnects roughly every 60s and mosquitto runs
// `persistence false` with QoS0 publishes, so anything published while a radio
// is between reconnects is silently dropped — there is no queue. Three sends
// over ~90s covers ~1.5 flapping cycles. This is exactly why ricky's ~60-message
// lyric bursts always land and single-shot repliers vanish.
//
// Two timed sends, 10s apart: the BLE link regularly eats one packet of a
// burst, and waiting for the recipient's next beacon (~60s cadence) to flush
// makes the second line of a reply feel a minute late (observed live
// 2026-07-20). The 10s retry is a fast second chance; the pending queue is the
// backstop. Copies are byte-identical (one packet id per line), so redundant
// deliveries never display -- the historical dupe storm this count was cut for
// cannot recur.
const (
	pkiReplyRetryCount   = 2
	pkiReplyRetrySpacing = 10 * time.Second
)

// sendSpread invokes send once immediately, then count-1 more times spaced
// `spacing` apart on a background goroutine so the packet handler is never
// stalled. sleep is a test seam.
func sendSpread(count int, spacing time.Duration, sleep func(time.Duration), send func()) {
	if count < 1 {
		count = 1
	}
	send()
	if count == 1 {
		return
	}
	go func() {
		for range count - 1 {
			sleep(spacing)
			send()
		}
	}()
}

// lyricsEncoreCooldown spaces full lyric performances per requester. The burst
// is ~60 messages, so unbounded repeats would re-create the channel drowning;
// once-per-lifetime (the old rule) blackholed every repeat request instead.
const lyricsEncoreCooldown = 10 * time.Minute

// requestDedupWindow collapses a radio's own retransmissions of the same DM.
// Meshtastic resends an unacked direct message ~3 times, ~8s apart, so without
// this every chatbot reply is multiplied by the retransmit count: 3 requests x
// pkiReplyRetryCount = 9 copies of each line. Observed live on 2026-07-19.
// 30s covers the ~16s retransmit span with margin; the cost is that a genuine
// repeat of the identical text inside the window gets one reply, not two.
const requestDedupWindow = 30 * time.Second

// dedupRequest reports whether key was already seen within window, and records
// it otherwise. It prunes expired entries on the way through so the map cannot
// grow without bound over a multi-day fleet lifetime.
func dedupRequest(seen map[string]time.Time, key string, now time.Time, window time.Duration) bool {
	for k, t := range seen {
		if now.Sub(t) > window {
			delete(seen, k)
		}
	}
	if t, ok := seen[key]; ok && now.Sub(t) <= window {
		return true
	}
	seen[key] = now
	return false
}

// isRetransmit guards every chatbot reply path against a device retransmit.
func (n *FleetCmd) isRetransmit(toFleetIdx int, from uint32, message string) bool {
	key := fmt.Sprintf("%d:%s", from, message)
	n.RecentReqMux[toFleetIdx].Lock()
	defer n.RecentReqMux[toFleetIdx].Unlock()
	return dedupRequest(n.RecentReq[toFleetIdx], key, time.Now(), requestDedupWindow)
}

// mosquitto runs `persistence false` and Meshtastic publishes QoS0, so a reply
// published while the recipient is between reconnects is dropped with no queue
// and no retry -- the broker will not store and forward it. The timed retry
// above fires blind and hopes to overlap a connected window; this queue instead
// waits for proof of life. Every packet a radio transmits (its own NodeInfo or
// Position on the channel) proves it is connected RIGHT NOW, so that is the
// moment to re-send anything it may have missed.
const (
	pendingReplyTTL = 10 * time.Minute
	// Two flushes, 20s cooldown: the first flush lands on the recipient's FIRST
	// beacon after a loss (~60s cadence) instead of its second, and one spare
	// remains for a second gap. Safe to raise now that flushes republish the
	// EXACT bytes of the original send -- same packet id, so the device dedups
	// and a redundant copy never displays (raising this used to multiply lines
	// on screen; observed live 2026-07-19).
	pendingMaxFlush      = 2
	pendingFlushCooldown = 20 * time.Second
	// pendingFlushSpacing separates the packets of one flush burst. Both lines
	// of a 2-line reply used to go out in the same millisecond, and the BLE link
	// regularly delivered exactly one of them.
	pendingFlushSpacing = 2 * time.Second
)

type pendingReply struct {
	fleetIdx  int
	ghost     uint32 // the replying ghost (sender)
	to        uint32 // the recipient radio
	topic     string
	text      string // for logs only; the wire copy is `envelope`
	envelope  []byte // exact marshaled ServiceEnvelope of the original send
	created   time.Time
	flushes   int
	lastFlush time.Time
}

// selectDueFlushes splits entries into those still worth keeping and those to
// re-send now. Entries are dropped once they expire or exhaust their flushes,
// so the queue drains on its own without a sweeper goroutine.
func selectDueFlushes(entries []*pendingReply, now time.Time) (kept, due []*pendingReply) {
	for _, e := range entries {
		if now.Sub(e.created) > pendingReplyTTL || e.flushes >= pendingMaxFlush {
			continue
		}
		kept = append(kept, e)
		if now.Sub(e.lastFlush) >= pendingFlushCooldown {
			e.flushes++
			e.lastFlush = now
			due = append(due, e)
		}
	}
	return kept, due
}

// queuePendingReply records a reply for redelivery on the recipient's next
// sighting. lastFlush starts at now so the cooldown is measured from the
// original send, not from the epoch.
func (n *FleetCmd) queuePendingReply(toFleetIdx int, ghost, to uint32, topic, text string, envelope []byte) {
	now := time.Now()
	n.PendingMux.Lock()
	defer n.PendingMux.Unlock()
	n.Pending[to] = append(n.Pending[to], &pendingReply{
		fleetIdx: toFleetIdx, ghost: ghost, to: to, topic: topic, text: text, envelope: envelope,
		created: now, lastFlush: now,
	})
}

// onNodeSeen is the store-and-forward trigger: any transmission from a node
// proves it is connected, so flush whatever it may have missed.
func (n *FleetCmd) onNodeSeen(node uint32) {
	n.PendingMux.Lock()
	entries, ok := n.Pending[node]
	if !ok {
		n.PendingMux.Unlock()
		return
	}
	kept, due := selectDueFlushes(entries, time.Now())
	if len(kept) == 0 {
		delete(n.Pending, node)
	} else {
		n.Pending[node] = kept
	}
	n.PendingMux.Unlock()

	if len(due) == 0 {
		return
	}
	// One goroutine, sequential sends, spaced apart: a same-millisecond burst
	// down the BLE pipe regularly loses all but one packet. Each flush is the
	// EXACT bytes of the original send (same packet id), so a copy the device
	// already has is deduped and never displays twice.
	go func() {
		for i, e := range due {
			if i > 0 {
				time.Sleep(pendingFlushSpacing)
			}
			n.Config.Log.Infof("Node %d is back; re-sending pending reply (flush %d/%d): %s", node, e.flushes, pendingMaxFlush, e.text)
			if err := n.MqttClient[e.fleetIdx].PublishEnvelopeBytes(e.topic, e.envelope); err != nil {
				n.Config.Log.Errorf("Failed to flush pending reply to %d: %v", node, err)
			}
		}
	}()
}

// numberLyric prefixes a lyric line with its 1-based sequence number
// ("01: never gonna..."). The numbers make ricky's stream a live probe for
// delivery order and gaps: any misordered or missing line is visible at a
// glance on the device.
func numberLyric(i int, text string) string {
	return fmt.Sprintf("%02d: %s", i+1, text)
}

// staggerDelay serializes sends to one recipient `spacing` apart: given the
// earliest allowed send time and now, it returns how long this send must wait
// and the next send's earliest allowed time. A zero/past nextAt sends
// immediately.
func staggerDelay(nextAt, now time.Time, spacing time.Duration) (delay time.Duration, newNextAt time.Time) {
	if now.Before(nextAt) {
		delay = nextAt.Sub(now)
	}
	return delay, now.Add(delay + spacing)
}

// sendPKIReplyReliable is the one-shot reply path: the reply envelope is built
// ONCE and every copy -- immediate, timed retry, pending flush -- republishes
// the same bytes, so all copies share one packet id and the device's dedup
// keeps repeats off the screen. Consecutive lines to the same recipient are
// staggered pendingFlushSpacing apart: a same-millisecond 2-line burst down the
// BLE pipe regularly delivered exactly one line.
// Do NOT use it from handleLyricsChat — ricky already emits ~60 messages per
// request and a 3x retry would flood the channel, re-creating the drowning
// problem that hid this bug in the first place.
func (n *FleetCmd) sendPKIReplyReliable(toFleetIdx int, to, from uint32, topic string, reply string) {
	// `to` is the replying ghost, `from` is the recipient radio (swapped for the reply).
	replyTopic, envelope, ok := n.buildPKIReply(toFleetIdx, to, from, topic, reply)
	if !ok {
		return
	}
	n.queuePendingReply(toFleetIdx, to, from, replyTopic, reply, envelope)

	send := func() {
		if err := n.MqttClient[toFleetIdx].PublishEnvelopeBytes(replyTopic, envelope); err != nil {
			n.Config.Log.Errorf("Failed to send PKI reply: %v", err)
		} else {
			n.Config.Log.Infof("Successfully sent PKI reply from %d to %d: %s", to, from, reply)
		}
	}

	n.ReplyNextAtMux.Lock()
	delay, next := staggerDelay(n.ReplyNextAt[from], time.Now(), pendingFlushSpacing)
	n.ReplyNextAt[from] = next
	n.ReplyNextAtMux.Unlock()

	if delay > 0 {
		go func() {
			time.Sleep(delay)
			sendSpread(pkiReplyRetryCount, pkiReplyRetrySpacing, time.Sleep, send)
		}()
		return
	}
	sendSpread(pkiReplyRetryCount, pkiReplyRetrySpacing, time.Sleep, send)
}

func (n *FleetCmd) FleetNodeHandler(to, from uint32, topic string, portNum meshtastic.PortNum, payload []byte) {
	// Any packet from this node -- including the broadcast NodeInfo/Position it
	// emits on reconnect -- proves it is connected right now. Flush anything we
	// published into a gap. Must run BEFORE the broadcast skip below, because
	// the reconnect beacon we rely on IS a broadcast.
	n.onNodeSeen(from)

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
	case meshtastic.PortNum_NODEINFO_APP:
		// A directed NODEINFO to a ghost is the app's "exchange user info" (sent
		// when the requester's NodeDB is missing us -- e.g. freshly wiped). Real
		// firmware answers with its user info; staying silent left the requester
		// unable to learn a ghost's details (or pubkey) on demand.
		if _, ghost, _, _ := n.FindNodes(to, from); ghost != nil {
			n.respondNodeInfo(toFleetIdx, ghost, from)
		}

	case meshtastic.PortNum_TEXT_MESSAGE_APP:
		var hasOTP = false
		message := string(payload)

		// A radio retransmits an unacked DM ~3x, ~8s apart. Each copy used to
		// start its own reply chain, so the reply count multiplied by the
		// retransmit count. Drop the duplicates before any chatbot path runs.
		if n.isRetransmit(toFleetIdx, from, message) {
			n.Config.Log.Infof("Duplicate DM from %d within %v (device retransmit), not replying again: %q", from, requestDedupWindow, message)
			return
		}

		if n.OTPHandler[toFleetIdx] != nil {
			// Accept a wide ± window (not just prev/current/next): a short-period
			// code has to survive a slow LoRa mesh round-trip before it lands here.
			codes, err := n.OTPHandler[toFleetIdx].ValidCodesWindow(otpAcceptWindowEachSide)
			if err != nil {
				n.Config.Log.Errorf("Failed to calculate TOTP: %v", err)
				return
			}
			for _, code := range codes {
				if strings.Contains(message, code) {
					hasOTP = true
					break
				}
			}
			// Diagnostic: on a miss, log what arrived vs the window's midpoint so a
			// stale/skewed code is visible (the received code and expected "current"
			// will be adjacent TOTP values a few windows apart). Debug-level only.
			if !hasOTP {
				mid := ""
				if len(codes) > 0 {
					mid = codes[len(codes)/2]
				}
				n.Config.Log.Debugf("OTP miss fleet=%d from=%d msg=%q expected(current)=%s window=±%d", toFleetIdx, from, message, mid, otpAcceptWindowEachSide)
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
			// A lyrics-only ghost (ricky) has no chatmode_unlocked/GPT config, so
			// the unlocked state used to silently swallow every follow-up DM for
			// the unlock TTL. Route those to the lyrics path instead -- its own
			// cooldown decides between an encore and the encore notice.
			if _, hasGPT := chatBotMap["chatmode_unlocked"]; !hasGPT {
				if chatBot, ok := chatBotMap["chatmode_lyrics"]; ok {
					n.handleLyricsChat(toFleetIdx, to, from, topic, chatBot.Message[0])
					return
				}
			}
			if _, ok := chatBotMap["chatmode_unlocked"]; ok {
				// presence of chatmode_unlocked marks this ghost LLM-capable.

				// INPUT guardrail on every unlocked message.
				if allowed, reason := n.guardText(context.Background(), message, guardInput); !allowed {
					n.Config.Log.Infof("guardrail blocked INPUT from %d (%s)", from, reason)
					n.sendPKIReplyReliable(toFleetIdx, to, from, topic, cannedRefusal)
					return
				}

				// Deterministic covert-flag reveal: if the player raised the trigger
				// topic, send a single-use claim link (minted once per unlock; a
				// mint failure falls back to the %CODE% static reveal — see
				// claimlink.go). The code never enters the LLM; the reveal is
				// exempt from OUTPUT guard.
				if toFleetIdx < len(n.Challenge) {
					if rt := n.Challenge[toFleetIdx]; matchesTrigger(rt, message) {
						n.Config.Log.Infof("flag trigger matched (fleet %d) from %d — sending claim reveal", toFleetIdx, from)
						n.sendFlagReveal(toFleetIdx, to, from, topic, rt)
						return
					}
				}

				// Otherwise: freeform LLM chat (Bedrock or Anthropic backup).
				n.handleLLMChat(toFleetIdx, to, from, topic, message, fleetConfig.SystemPrompt)
			}
		} else if hasOTP {
			if chatBot, ok := chatBotMap["otp_success"]; ok {
				for _, reply := range chatBot.Message {
					n.Config.Log.Infof("PKI Reply for OTP success: %+v - %s", chatBot.Type, reply)
					n.sendPKIReplyReliable(toFleetIdx, to, from, topic, reply)
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
						n.sendPKIReplyReliable(toFleetIdx, to, from, topic, reply)
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
	// Cooldown by the REQUESTER (`from`), not the ghost (`to`). Keying by `to`
	// meant only the first requester per fleet lifetime ever got lyrics. And
	// once-per-lifetime silently blackholed every repeat request (observed live
	// 2026-07-20: acked, then nothing) -- a cooldown keeps the ~60-message burst
	// from drowning the channel while letting a crowd get an encore, and the
	// blocked case ANSWERS instead of ghosting the requester.
	n.LyricsRespMux[toFleetIdx].RLock()
	last, exists := n.LyricsResponded[toFleetIdx][from]
	n.LyricsRespMux[toFleetIdx].RUnlock()

	if exists && time.Since(last) < lyricsEncoreCooldown {
		n.Config.Log.Infof("Lyrics on cooldown for requester %d (%v since last); sending encore notice", from, time.Since(last).Round(time.Second))
		n.sendPKIReply(toFleetIdx, to, from, topic, "🎤 One song per crowd every 10 minutes... come back for the encore.")
		return
	}

	// Mark this requester's showtime
	n.LyricsRespMux[toFleetIdx].Lock()
	n.LyricsResponded[toFleetIdx][from] = time.Now()
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

		for i, entry := range lyricEntries {
			select {
			case <-terminationTimer.C:
				n.Config.Log.Infof("Lyrics playback terminated after song duration")
				return
			case <-time.After(entry.timestamp - time.Since(startTime)):
				line := numberLyric(i, entry.text)
				n.sendPKIReply(toFleetIdx, to, from, topic, line)
				n.Config.Log.Debugf("Sent lyric at %v: %s", entry.timestamp, line)
			}
		}
	}()

	n.Config.Log.Infof("Started lyrics playback for %d entries over %v", len(lyricEntries), songDuration)
}

func (n *FleetCmd) handleLLMChat(toFleetIdx int, to, from uint32, topic string, userMessage string, systemPrompt string) {
	n.Config.Log.Infof("LLM chat (fleet %d) msg: %s", toFleetIdx, userMessage)
	reply, err := generateReply(context.Background(), userMessage, systemPrompt)
	if err != nil {
		n.Config.Log.Errorf("LLM generate failed: %v", err)
		n.sendPKIReply(toFleetIdx, to, from, topic, "👻 …signal lost. try again.")
		return
	}
	// OUTPUT guardrail — LLM-generated replies only (the deterministic reveal in
	// the unlocked branch is exempt so its flag code is never redacted).
	if allowed, _ := n.guardText(context.Background(), reply, guardOutput); !allowed {
		n.sendPKIReply(toFleetIdx, to, from, topic, cannedRefusal)
		return
	}
	for i, chunk := range n.splitIntoChunks(reply, 60) {
		if i == 0 {
			time.Sleep(500 * time.Millisecond)
		}
		n.sendPKIReply(toFleetIdx, to, from, topic, chunk)
		time.Sleep(500 * time.Millisecond)
	}
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
