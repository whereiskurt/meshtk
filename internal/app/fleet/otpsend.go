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
	NowMs() int64
}

// processOtpQueue runs one poll pass. Item lifecycle: expired or attempt-capped
// → reap; no pubkey yet → leave queued (the radio may announce NODEINFO later);
// send OK → delete + stamp codeSentAt; send failed → bump attempts and retry
// next pass.
func processOtpQueue(ctx context.Context, deps otpSendDeps, store otpqueue.Store, logger *log.Logger) {
	items, err := store.List(ctx)
	if err != nil {
		logger.Errorf("otp: queue list failed: %v", err)
		return
	}
	if len(items) == 0 {
		return
	}

	var sent, waiting, reaped, failed int
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
	logger.Infof("otp: pass done sent=%d waiting=%d reaped=%d failed=%d", sent, waiting, reaped, failed)
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

func (d *fleetOtpDeps) SendCode(toNodeNum uint32, pubKeyHex, code string) error {
	cfg := d.f.Config
	client := d.f.MqttClient[0]

	sender, err := parseNodeID(cfg.NodeInfo.ClientId)
	if err != nil {
		return err
	}
	senderPriv, err := client.ParseHexKey(cfg.NodeInfo.PKI.PrivateKey)
	if err != nil {
		return fmt.Errorf("nodeinfo private key: %w", err)
	}
	recipientPub, err := client.ParseHexKey(pubKeyHex)
	if err != nil {
		return fmt.Errorf("recipient pubkey: %w", err)
	}

	payload := fmt.Sprintf("run.defcon.run radio verification code: %s", code)
	envelope, err := client.BuildPKIMessage(sender, toNodeNum,
		meshtastic.PortNum_TEXT_MESSAGE_APP, []byte(payload), senderPriv, recipientPub)
	if err != nil {
		return err
	}
	return client.PublishEnvelopeBytes(pkiTopicFor(cfg.NodeInfo.Topic, sender), envelope)
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
