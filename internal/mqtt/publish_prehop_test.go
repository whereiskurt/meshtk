package mqtt

import (
	"crypto/cipher"
	"encoding/binary"
	"testing"

	meshtastic "github.com/whereiskurt/meshtk/protos/meshtastic/generated"
	"google.golang.org/protobuf/proto"
)

// 2.8 firmware "pre-hop drop": Router::handleReceived discards any decoded
// remote packet whose hop_start is 0 with no Data.bitfield (classified as
// pre-2.3 firmware), and any packet with hop_start < hop_limit (provably
// corrupt). Observed live 2026-07-28: a 2.8.0.dafa583 radio silently dropped
// the ENTIRE fleet — every ghost and sim — because we published hop_start=0
// with no bitfield. These tests pin the two survival guarantees: hop_start
// mirrors hop_limit, and the bitfield is present inside the (encrypted) Data.

func decodeEnvelope(t *testing.T, payload []byte) *meshtastic.MeshPacket {
	t.Helper()
	env := &meshtastic.ServiceEnvelope{}
	if err := proto.Unmarshal(payload, env); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	if env.Packet == nil {
		t.Fatal("envelope has no packet")
	}
	return env.Packet
}

func decryptData(t *testing.T, c *MqttClient, p *meshtastic.MeshPacket) *meshtastic.Data {
	t.Helper()
	enc := p.GetEncrypted()
	if enc == nil {
		t.Fatal("packet has no encrypted payload")
	}
	nonce := make([]byte, 16)
	binary.LittleEndian.PutUint32(nonce[0:], p.Id)
	binary.LittleEndian.PutUint32(nonce[8:], p.From)
	decrypted := make([]byte, len(enc))
	cipher.NewCTR(c.blockCipher, nonce).XORKeyStream(decrypted, enc)
	data := &meshtastic.Data{}
	if err := proto.Unmarshal(decrypted, data); err != nil {
		t.Fatalf("unmarshal decrypted data: %v", err)
	}
	return data
}

func assertSurvivesPreHopDrop(t *testing.T, p *meshtastic.MeshPacket, data *meshtastic.Data) {
	t.Helper()
	if p.HopStart < p.HopLimit {
		t.Errorf("hop_start (%d) < hop_limit (%d): 2.8 drops as provably corrupt", p.HopStart, p.HopLimit)
	}
	if data.Bitfield == nil {
		t.Fatal("Data.bitfield absent: 2.8 drops hop_start=0 packets without it (pre-hop drop)")
	}
	if *data.Bitfield&BitfieldOkToMqtt == 0 {
		t.Errorf("bitfield %#x missing OK_TO_MQTT bit", *data.Bitfield)
	}
}

func TestChannelPublishSurvivesPreHopDrop(t *testing.T) {
	c, fb := newRetainTestClient(t)
	if err := c.PublishMessageEncrypted(0x1555f041, 0xffffffff, "msh/US/2/e/dc.run/!1555f041",
		meshtastic.PortNum_NODEINFO_APP, []byte("user-bytes")); err != nil {
		t.Fatalf("PublishMessageEncrypted: %v", err)
	}
	if len(fb.calls) != 1 {
		t.Fatalf("publishes = %d, want 1", len(fb.calls))
	}
	p := decodeEnvelope(t, fb.calls[0].payload)
	assertSurvivesPreHopDrop(t, p, decryptData(t, c, p))
}

func TestAckPublishSurvivesPreHopDrop(t *testing.T) {
	c, fb := newRetainTestClient(t)
	if err := c.PublishACK(0x1555f041, 2, "msh/US/2/e/dc.run/!1555f041", 42, []byte{}); err != nil {
		t.Fatalf("PublishACK: %v", err)
	}
	p := decodeEnvelope(t, fb.calls[0].payload)
	assertSurvivesPreHopDrop(t, p, decryptData(t, c, p))
}

func TestPlainPublishSurvivesPreHopDrop(t *testing.T) {
	c, fb := newRetainTestClient(t)
	if err := c.PublishMessagePlain(0x1555f041, 0xffffffff, "msh/US/2/map/",
		meshtastic.PortNum_MAP_REPORT_APP, []byte("report")); err != nil {
		t.Fatalf("PublishMessagePlain: %v", err)
	}
	p := decodeEnvelope(t, fb.calls[0].payload)
	data := p.GetDecoded()
	if data == nil {
		t.Fatal("plain packet has no decoded payload")
	}
	assertSurvivesPreHopDrop(t, p, data)
}
