package server

import (
	"testing"

	"github.com/eclipse/paho.mqtt.golang/packets"
	log "github.com/sirupsen/logrus"
	meshtastic "github.com/whereiskurt/meshtk/protos/meshtastic/generated"
	"google.golang.org/protobuf/proto"
)

// The proxy forwards ip.Raw.MQTT bytes, so a rule that only mutates the parsed
// struct is a silent no-op — RewriteHopLimit was exactly that from its
// introduction until 2026-07-28. These tests assert the clamp lands on the
// WIRE (the forwarded PUBLISH payload), not just the struct.

func hopClampRule(t *testing.T) Rule {
	t.Helper()
	for _, r := range rewriteRules() {
		if r.Name == "RewriteHopLimit" {
			return r
		}
	}
	t.Fatal("RewriteHopLimit rule not found")
	return Rule{}
}

func newHopTestPacket(t *testing.T, hopLimit, hopStart uint32) (*InspectorPacket, *packets.PublishPacket) {
	t.Helper()
	env := &meshtastic.ServiceEnvelope{
		Packet: &meshtastic.MeshPacket{
			From:     0x1555f041,
			To:       0xffffffff,
			Id:       42,
			HopLimit: hopLimit,
			HopStart: hopStart,
			PayloadVariant: &meshtastic.MeshPacket_Encrypted{
				Encrypted: []byte{0xde, 0xad, 0xbe, 0xef},
			},
		},
		GatewayId: "!1555f041",
		ChannelId: "dc.run",
	}
	payload, err := proto.Marshal(env)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	pub := packets.NewControlPacket(packets.Publish).(*packets.PublishPacket)
	pub.TopicName = "msh/US/2/e/dc.run/!1555f041"
	pub.Payload = payload

	var cp packets.ControlPacket = pub
	logger := log.New()
	logger.SetLevel(log.PanicLevel)
	ip := &InspectorPacket{
		Log:   logger,
		Track: &ConnectionInfo{},
		Raw:   &RawPacket{MQTT: &cp, Meshtastic: env},
	}
	return ip, pub
}

func wirePacket(t *testing.T, pub *packets.PublishPacket) *meshtastic.MeshPacket {
	t.Helper()
	env := &meshtastic.ServiceEnvelope{}
	if err := proto.Unmarshal(pub.Payload, env); err != nil {
		t.Fatalf("unmarshal forwarded payload: %v", err)
	}
	return env.Packet
}

func TestHopClampReachesTheWire(t *testing.T) {
	rule := hopClampRule(t)
	ip, pub := newHopTestPacket(t, 7, 9)

	if !rule.Matcher(ip) {
		t.Fatal("rule did not fire for hop_limit=7 hop_start=9")
	}
	p := wirePacket(t, pub)
	if p.HopLimit != 3 {
		t.Errorf("wire hop_limit = %d, want 3 (clamp never reached the forwarded bytes)", p.HopLimit)
	}
	if p.HopStart != 7 {
		t.Errorf("wire hop_start = %d, want 7 (HOP_MAX clamp)", p.HopStart)
	}
	if p.HopStart < p.HopLimit {
		t.Errorf("clamp produced hop_start (%d) < hop_limit (%d): 2.8 drops this as corrupt", p.HopStart, p.HopLimit)
	}
	if string(p.GetEncrypted()) != string([]byte{0xde, 0xad, 0xbe, 0xef}) {
		t.Error("encrypted payload changed during remarshal")
	}
}

func TestHopClampLeavesSanePacketsAlone(t *testing.T) {
	rule := hopClampRule(t)
	ip, pub := newHopTestPacket(t, 3, 3)
	before := string(pub.Payload)

	if rule.Matcher(ip) {
		t.Fatal("rule fired for a sane hop_limit=3 hop_start=3 packet")
	}
	if string(pub.Payload) != before {
		t.Error("payload mutated for a packet that should pass untouched")
	}
}
