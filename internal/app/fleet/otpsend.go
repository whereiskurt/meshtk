package fleet

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/whereiskurt/meshtk/internal/otpqueue"
	meshtastic "github.com/whereiskurt/meshtk/protos/meshtastic/generated"
)

// OTP delivery: run.human's manual radio add/resend enqueues a MeshOtpPending
// item; this poller drains the queue and PKI-DMs the verification code to the
// device from the NodeInfo (map) node identity. On success the item is deleted
// and codeSentAt is stamped on the MeshRadio row so the site shows "code sent".

const otpPollInterval = 20 * time.Second

// otpSendDeps is the seam between queue mechanics (tested with fakes) and the
// live MQTT/keycache wiring (fleetOtpDeps below).
type otpSendDeps interface {
	// ResolvePubKeyHex returns the recipient's X25519 pubkey as 0x-hex:
	// user-supplied on the item → authoritative MeshRadio row → NODEINFO-observed.
	ResolvePubKeyHex(item otpqueue.Item) (string, bool)
	SendCode(toNodeNum uint32, pubKeyHex, code string) error
	// SendWelcome publishes the welcome text immediately AND hands the
	// byte-identical envelope to the fleet's proof-of-life pending store, so a
	// radio that comes online within the pending TTL still receives it (the
	// device's packet-id dedup makes the duplicate invisible).
	SendWelcome(item otpqueue.WelcomeItem, pubKeyHex string) error
	// NodeAliveNow reports whether the radio transmitted recently. Welcomes
	// only send while true: a PKI DM published into a dead session evaporates
	// (2026-07-27 field failure — app pairing finished 16-31 min after the
	// publish, past the 10-min pending re-flush TTL, and both welcomes were
	// lost). The queue item waits, up to its 24 h life, for first contact.
	NodeAliveNow(nodeNum uint32) bool
	NowMs() int64
}

// welcomeAliveWindow: how recently a node must have transmitted to count as
// reachable for a welcome send. Generous enough to bridge broadcast gaps,
// tight enough that "alive" still means an active session.
const welcomeAliveWindow = 5 * time.Minute

// processOtpQueue runs one poll pass. Item lifecycle: expired or attempt-capped
// → reap; no pubkey yet → leave queued (the radio may announce NODEINFO later);
// send OK → delete + stamp codeSentAt; send failed → bump attempts and retry
// next pass.
func processOtpQueue(ctx context.Context, deps otpSendDeps, store otpqueue.Store, logger *log.Logger) {
	items, welcomes, err := store.List(ctx)
	if err != nil {
		logger.Errorf("otp: queue list failed: %v", err)
		return
	}
	if len(items) == 0 && len(welcomes) == 0 {
		return
	}

	var sent, welcome, waiting, reaped, failed int
	for _, it := range items {
		switch {
		case deps.NowMs()-it.CreatedAt > otpqueue.MaxAgeMs:
			reaped++
			if err := store.Delete(ctx, it.NodeID); err != nil {
				logger.Errorf("otp: reap expired %s failed: %v", it.NodeID, err)
			}
		case it.Attempts >= otpqueue.MaxAttempts:
			reaped++
			logger.Errorf("otp: giving up on %s after %d failed publishes", it.NodeID, it.Attempts)
			if err := store.Delete(ctx, it.NodeID); err != nil {
				logger.Errorf("otp: reap capped %s failed: %v", it.NodeID, err)
			}
		default:
			pubKey, ok := deps.ResolvePubKeyHex(it)
			if !ok {
				waiting++
				continue
			}
			if err := deps.SendCode(it.NodeNum, pubKey, it.Code); err != nil {
				failed++
				logger.Errorf("otp: send to %s failed (attempt %d): %v", it.NodeID, it.Attempts+1, err)
				if err := store.BumpAttempts(ctx, it.NodeID, it.Attempts+1); err != nil {
					logger.Errorf("otp: bump %s failed: %v", it.NodeID, err)
				}
				continue
			}
			sent++
			if err := store.Delete(ctx, it.NodeID); err != nil {
				logger.Errorf("otp: delete sent %s failed (device may see a duplicate code): %v", it.NodeID, err)
			}
			if err := store.MarkRadioCodeSent(ctx, it.NodeNum, deps.NowMs()); err != nil {
				logger.Errorf("otp: codeSentAt stamp for %s failed: %v", it.NodeID, err)
			}
			logger.Infof("otp: verification code delivered to %s", it.NodeID)
		}
	}

	// Welcome items: same lifecycle gates, no codeSentAt stamp (nothing reads
	// it), delivery via SendWelcome's immediate-publish + proof-of-life re-flush.
	for _, w := range welcomes {
		switch {
		case deps.NowMs()-w.CreatedAt > otpqueue.MaxAgeMs:
			reaped++
			if err := store.DeleteWelcome(ctx, w.NodeID); err != nil {
				logger.Errorf("otp: reap expired welcome %s failed: %v", w.NodeID, err)
			}
		case w.Attempts >= otpqueue.MaxAttempts:
			reaped++
			logger.Errorf("otp: giving up on welcome %s after %d failed publishes", w.NodeID, w.Attempts)
			if err := store.DeleteWelcome(ctx, w.NodeID); err != nil {
				logger.Errorf("otp: reap capped welcome %s failed: %v", w.NodeID, err)
			}
		default:
			// Hold the welcome until the radio is provably on the mesh right
			// now — first contact after flashing may be many minutes out.
			if !deps.NodeAliveNow(w.NodeNum) {
				waiting++
				continue
			}
			pubKey, ok := deps.ResolvePubKeyHex(otpqueue.Item{NodeID: w.NodeID, NodeNum: w.NodeNum})
			if !ok {
				waiting++
				continue
			}
			if err := deps.SendWelcome(w, pubKey); err != nil {
				failed++
				logger.Errorf("otp: welcome to %s failed (attempt %d): %v", w.NodeID, w.Attempts+1, err)
				if err := store.BumpWelcomeAttempts(ctx, w.NodeID, w.Attempts+1); err != nil {
					logger.Errorf("otp: bump welcome %s failed: %v", w.NodeID, err)
				}
				continue
			}
			welcome++
			if err := store.DeleteWelcome(ctx, w.NodeID); err != nil {
				logger.Errorf("otp: delete sent welcome %s failed (device may see a duplicate): %v", w.NodeID, err)
			}
			logger.Infof("otp: welcome delivered to %s", w.NodeID)
		}
	}

	logger.Infof("otp: pass done sent=%d welcome=%d waiting=%d reaped=%d failed=%d", sent, welcome, waiting, reaped, failed)
}

// pkiTopicFor derives the SENDER's gateway PKI topic from the NodeInfo channel
// topic: "msh/US/2/e/dc.run" + !dc340001 -> "msh/US/2/e/PKI/!dc340001".
// Publishing on the sender's own gateway topic is REQUIRED: devices drop
// messages arriving on their own gateway topic as self-echo (the Phase 66
// ghost-reply field bug — see buildPKIReply).
func pkiTopicFor(nodeInfoTopic string, sender uint32) string {
	base := nodeInfoTopic
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[:i]
	}
	return fmt.Sprintf("%s/PKI/!%08x", base, sender)
}

func parseNodeID(id string) (uint32, error) {
	v, err := strconv.ParseUint(strings.TrimPrefix(id, "!"), 16, 32)
	if err != nil {
		return 0, fmt.Errorf("parse node id %q: %w", id, err)
	}
	return uint32(v), nil
}

// fleetOtpDeps is the production otpSendDeps: resolves recipient keys through
// the same chain the ghosts use, and sends from the NodeInfo (map) identity
// via the first fleet client's connection.
type fleetOtpDeps struct {
	f *FleetCmd
}

func (d *fleetOtpDeps) ResolvePubKeyHex(item otpqueue.Item) (string, bool) {
	if item.PublicKey != "" {
		return item.PublicKey, true
	}
	client := d.f.MqttClient[0]
	if hexKey, err := client.ResolveSenderPubKey(item.NodeNum); err == nil && hexKey != "" {
		return hexKey, true
	}
	return client.ObservedPubKey(item.NodeNum)
}

// buildAndPublish PKI-encrypts text from the NodeInfo (map) identity to the
// device and publishes it on the sender's gateway topic. Returns everything a
// caller needs for a byte-identical re-send.
func (d *fleetOtpDeps) buildAndPublish(toNodeNum uint32, pubKeyHex, text string) (sender uint32, topic string, envelope []byte, err error) {
	cfg := d.f.Config
	client := d.f.MqttClient[0]

	sender, err = parseNodeID(cfg.NodeInfo.ClientId)
	if err != nil {
		return 0, "", nil, err
	}
	senderPriv, err := client.ParseHexKey(cfg.NodeInfo.PKI.PrivateKey)
	if err != nil {
		return 0, "", nil, fmt.Errorf("nodeinfo private key: %w", err)
	}
	recipientPub, err := client.ParseHexKey(pubKeyHex)
	if err != nil {
		return 0, "", nil, fmt.Errorf("recipient pubkey: %w", err)
	}

	envelope, err = client.BuildPKIMessage(sender, toNodeNum,
		meshtastic.PortNum_TEXT_MESSAGE_APP, []byte(text), senderPriv, recipientPub)
	if err != nil {
		return 0, "", nil, err
	}
	topic = pkiTopicFor(cfg.NodeInfo.Topic, sender)
	return sender, topic, envelope, client.PublishEnvelopeBytes(topic, envelope)
}

func (d *fleetOtpDeps) SendCode(toNodeNum uint32, pubKeyHex, code string) error {
	_, _, _, err := d.buildAndPublish(toNodeNum, pubKeyHex,
		fmt.Sprintf("run.defcon.run radio verification code: %s", code))
	return err
}

func (d *fleetOtpDeps) SendWelcome(item otpqueue.WelcomeItem, pubKeyHex string) error {
	sender, topic, envelope, err := d.buildAndPublish(item.NodeNum, pubKeyHex, item.Message)
	if err != nil {
		return err
	}
	// Proof-of-life re-flush: the radio may still be rebooting/pairing after
	// the flash. Queue the SAME bytes; the next transmission from the node
	// (within the pending TTL) re-publishes them — invisible duplicate if the
	// first copy landed (packet-id dedup), first delivery if it didn't.
	d.f.queuePendingReply(0, sender, item.NodeNum, topic, "welcome", envelope)
	return nil
}

func (d *fleetOtpDeps) NodeAliveNow(nodeNum uint32) bool {
	return d.f.lastSeenWithin(nodeNum, welcomeAliveWindow)
}

func (d *fleetOtpDeps) NowMs() int64 { return time.Now().UnixMilli() }

// buildOtpStore mirrors buildKeyResolver's config handling: same table/region/
// endpoint block, nil (with a warn) on failure so the fleet boots regardless.
func (f *FleetCmd) buildOtpStore() otpqueue.Store {
	kc := f.Config.Server.KeyCache
	if kc.TableName == "" {
		return nil
	}
	store, err := otpqueue.NewDynamoDBStore(kc.TableName, kc.TableRegion, kc.DynamoDBEndpoint)
	if err != nil {
		if f.Config.Log != nil {
			f.Config.Log.Warnf("otp: queue store unavailable, OTP delivery disabled: %v", err)
		}
		return nil
	}
	return store
}

// startOtpPoller drains the queue every pollEvery until ctx ends. A panic in a
// pass is logged and the loop continues — OTP delivery must never take down
// the ghosts.
func (f *FleetCmd) startOtpPoller(ctx context.Context, store otpqueue.Store, pollEvery time.Duration) {
	deps := &fleetOtpDeps{f: f}
	logger := f.Config.Log
	logger.Infof("otp: delivery poller started (every %v, sender %s)", pollEvery, f.Config.NodeInfo.ClientId)

	ticker := time.NewTicker(pollEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			func() {
				defer func() {
					if r := recover(); r != nil {
						logger.Errorf("otp: poll pass panicked: %v", r)
					}
				}()
				processOtpQueue(ctx, deps, store, logger)
			}()
		}
	}
}
