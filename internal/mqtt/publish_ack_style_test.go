package mqtt

import (
	"testing"

	meshtastic "github.com/whereiskurt/meshtk/protos/meshtastic/generated"
	"google.golang.org/protobuf/proto"
)

// publishAndDecodeACK runs PublishACK under the given style and returns the
// MeshPacket that went to the wire.
func publishAndDecodeACK(t *testing.T, style string) *meshtastic.MeshPacket {
	t.Helper()
	c, fb := newRetainTestClient(t)
	c.SetAckStyle(style)

	routing := &meshtastic.Routing{
		Variant: &meshtastic.Routing_ErrorReason{ErrorReason: meshtastic.Routing_NONE},
	}
	routingBytes, err := proto.Marshal(routing)
	if err != nil {
		t.Fatalf("marshal routing: %v", err)
	}

	if err := c.PublishACK(0x1555f041, 0x435990e4, "msh/US/2/e/dc.run/!1555f041", 0xfe2e6551, routingBytes); err != nil {
		t.Fatalf("PublishACK: %v", err)
	}
	if len(fb.calls) != 1 {
		t.Fatalf("publishes = %d, want 1", len(fb.calls))
	}

	envelope := new(meshtastic.ServiceEnvelope)
	if err := proto.Unmarshal(fb.calls[0].payload, envelope); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	pkt := envelope.GetPacket()
	if pkt == nil {
		t.Fatal("envelope has no MeshPacket")
	}
	return pkt
}

// A faithful ack must look like real firmware output. rx_rssi/rx_snr are
// receiver-side fields a transmitting node leaves zero, via_mqtt is stamped by
// the receiving gateway on ingest (never the sender), and hop_start mirrors
// hop_limit on a fresh send -- apps compute "hops away" as hop_start-hop_limit,
// which fabricated metadata made negative.
func TestFaithfulACKHasNoFabricatedRxMetadata(t *testing.T) {
	pkt := publishAndDecodeACK(t, "faithful")

	if pkt.GetRxRssi() != 0 {
		t.Errorf("rx_rssi = %d, want 0 (receiver-side field on a fresh send)", pkt.GetRxRssi())
	}
	if pkt.GetRxSnr() != 0 {
		t.Errorf("rx_snr = %v, want 0 (receiver-side field on a fresh send)", pkt.GetRxSnr())
	}
	if pkt.GetRxTime() != 0 {
		t.Errorf("rx_time = %d, want 0 (receiver-side field on a fresh send)", pkt.GetRxTime())
	}
	if pkt.GetViaMqtt() {
		t.Error("via_mqtt set by sender; only the receiving gateway stamps it")
	}
	if pkt.GetHopStart() != pkt.GetHopLimit() {
		t.Errorf("hop_start = %d, hop_limit = %d; fresh sends set them equal so hops-away computes as 0",
			pkt.GetHopStart(), pkt.GetHopLimit())
	}
	if pkt.GetPriority() != meshtastic.MeshPacket_ACK {
		t.Errorf("priority = %v, want ACK", pkt.GetPriority())
	}
	if pkt.GetFrom() != 0x1555f041 || pkt.GetTo() != 0x435990e4 {
		t.Errorf("from/to = %08x/%08x, want 1555f041/435990e4", pkt.GetFrom(), pkt.GetTo())
	}
}

// The zero value (configs that predate AckMode) must behave as faithful, not
// silently resurrect the legacy shape.
func TestUnsetAckStyleDefaultsToFaithful(t *testing.T) {
	pkt := publishAndDecodeACK(t, "")
	if pkt.GetViaMqtt() || pkt.GetRxRssi() != 0 {
		t.Error("unset ack style produced legacy fabricated metadata; default must be faithful")
	}
	if pkt.GetHopStart() != pkt.GetHopLimit() {
		t.Error("unset ack style did not set hop_start; default must be faithful")
	}
}

// PKI replies must be faithful firmware shape too. Fabricated rx_time was
// worse than cosmetic: the receiver trusted our build-time stamp over actual
// arrival, so a pending flush delivered a minute late slotted a minute back in
// the conversation history (observed live 2026-07-20).
func TestBuildPKIMessageHasNoFabricatedRxMetadata(t *testing.T) {
	c, _ := newRetainTestClient(t)
	priv := make([]byte, 32)
	pub := make([]byte, 32)
	priv[0], pub[0] = 7, 9

	envelopeBytes, err := c.BuildPKIMessage(0x1555f041, 0x435990e4,
		meshtastic.PortNum_TEXT_MESSAGE_APP, []byte("hi"), priv, pub)
	if err != nil {
		t.Fatalf("BuildPKIMessage: %v", err)
	}
	envelope := new(meshtastic.ServiceEnvelope)
	if err := proto.Unmarshal(envelopeBytes, envelope); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	pkt := envelope.GetPacket()
	if pkt == nil {
		t.Fatal("envelope has no MeshPacket")
	}

	if pkt.GetRxTime() != 0 {
		t.Errorf("rx_time = %d, want 0; a fabricated stamp reorders conversation history on late redelivery", pkt.GetRxTime())
	}
	if pkt.GetRxRssi() != 0 || pkt.GetRxSnr() != 0 {
		t.Errorf("rx_rssi/rx_snr = %d/%v, want 0/0 (receiver-side fields)", pkt.GetRxRssi(), pkt.GetRxSnr())
	}
	if pkt.GetViaMqtt() {
		t.Error("via_mqtt set by sender; only the receiving gateway stamps it")
	}
	if pkt.GetHopStart() != pkt.GetHopLimit() {
		t.Errorf("hop_start = %d, hop_limit = %d; fresh sends set them equal", pkt.GetHopStart(), pkt.GetHopLimit())
	}
	if !pkt.GetPkiEncrypted() {
		t.Error("pki_encrypted not set on a PKI message")
	}
}

// Legacy stays available for A/B comparison and keeps its historical shape.
func TestLegacyACKKeepsHistoricalShape(t *testing.T) {
	pkt := publishAndDecodeACK(t, "legacy")
	if !pkt.GetViaMqtt() || pkt.GetRxRssi() != -2 {
		t.Errorf("legacy ack lost its historical shape: via_mqtt=%v rx_rssi=%d", pkt.GetViaMqtt(), pkt.GetRxRssi())
	}
	if pkt.GetHopStart() != 0 {
		t.Errorf("legacy ack sets hop_start=%d; historical shape leaves it unset", pkt.GetHopStart())
	}
}

// A directed user-info reply must NOT retain: retain is per topic, so it would
// displace the retained broadcast NodeInfo on the ghost's own topic.
func TestPublishNodeInfoToDoesNotRetain(t *testing.T) {
	c, fb := newRetainTestClient(t)
	if err := c.PublishNodeInfoTo(0x7bc64694, 0x435990e4, "msh/US/2/e/dc.run/!7bc64694",
		"ghost-ricky-00", "GR00", make([]byte, 32),
		meshtastic.HardwareModel_HELTEC_V3, meshtastic.Config_DeviceConfig_CLIENT); err != nil {
		t.Fatalf("PublishNodeInfoTo: %v", err)
	}
	if len(fb.calls) != 1 {
		t.Fatalf("publishes = %d, want 1", len(fb.calls))
	}
	if fb.calls[0].retain {
		t.Error("directed NodeInfo reply retained; it would displace the retained broadcast NodeInfo on this topic")
	}
}
