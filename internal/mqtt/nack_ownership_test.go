package mqtt

import (
	"bytes"
	"testing"

	log "github.com/sirupsen/logrus"
	meshtastic "github.com/whereiskurt/meshtk/protos/meshtastic/generated"
	"google.golang.org/protobuf/proto"
)

// fakeMsg is a minimal paho mqtt.Message for driving dispatcher directly.
type fakeMsg struct {
	topic   string
	payload []byte
}

func (m *fakeMsg) Duplicate() bool   { return false }
func (m *fakeMsg) Qos() byte         { return 0 }
func (m *fakeMsg) Retained() bool    { return false }
func (m *fakeMsg) Topic() string     { return m.topic }
func (m *fakeMsg) MessageID() uint16 { return 0 }
func (m *fakeMsg) Payload() []byte   { return m.payload }
func (m *fakeMsg) Ack()              {}

func pkiEnvelopeBytes(t *testing.T, from, to, id uint32) []byte {
	t.Helper()
	envelope := &meshtastic.ServiceEnvelope{
		Packet: &meshtastic.MeshPacket{
			From:         from,
			To:           to,
			Id:           id,
			PkiEncrypted: true,
			PayloadVariant: &meshtastic.MeshPacket_Encrypted{
				Encrypted: []byte{0xde, 0xad, 0xbe, 0xef},
			},
		},
		GatewayId: "!43542406",
		ChannelId: "dc.run",
	}
	raw, err := proto.Marshal(envelope)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	return raw
}

func newNackTestClient(nodes *NodeDB) (*MqttClient, *int) {
	logger := log.New()
	logger.SetOutput(&bytes.Buffer{})
	nacks := 0
	c := &MqttClient{
		log:   logger,
		nodes: nodes,
	}
	c.nackHandler = func(to, from uint32, requestId uint32) { nacks++ }
	return c, &nacks
}

// A PKI DM whose recipient is NOT one of this client's nodes must draw NO
// routing response. Every fleet client sees every PKI publish; before the
// ownership gate each non-recipient client NACKed the DM it (correctly)
// couldn't decrypt — ~70 NACKs racing the one real ACK, so the sender's app
// marked delivered DMs as failed.
func TestPKINackSuppressedWhenRecipientNotOwned(t *testing.T) {
	const sender = uint32(0x43542406)
	const recipient = uint32(0x5327507d)     // a real human radio, not ours
	c, nacks := newNackTestClient(&NodeDB{}) // this client owns no nodes

	c.dispatcher(nil, &fakeMsg{
		topic:   "msh/US/2/e/PKI/!43542406",
		payload: pkiEnvelopeBytes(t, sender, recipient, 12345),
	})

	if *nacks != 0 {
		t.Fatalf("non-recipient client sent %d NACK(s); must stay silent", *nacks)
	}
}

// When this client DOES own the recipient but decrypt still fails (here: the
// owned node has no private key, standing in for a sender-key resolution
// failure), the nodeinfo-retransmit NACK clue must still fire.
func TestPKINackStillFiresWhenRecipientOwned(t *testing.T) {
	const sender = uint32(0x43542406)
	const recipient = uint32(0x1555f041) // goldstein — ours, but with no privkey
	c, nacks := newNackTestClient(&NodeDB{recipient: {From: recipient}})

	c.dispatcher(nil, &fakeMsg{
		topic:   "msh/US/2/e/PKI/!43542406",
		payload: pkiEnvelopeBytes(t, sender, recipient, 12345),
	})

	if *nacks != 1 {
		t.Fatalf("owned-recipient decrypt failure sent %d NACK(s); want exactly 1", *nacks)
	}
}
