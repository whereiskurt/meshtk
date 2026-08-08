package server

import (
	"bytes"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/eclipse/paho.mqtt.golang/packets"
	meshtastic "github.com/whereiskurt/meshtk/protos/meshtastic/generated"
	"google.golang.org/protobuf/proto"
)

// Blocking a PACKET and ending a SESSION are different things, and the proxy
// used to conflate them: `case Block:` returned from the uplink loop, and
// handleProxy's deferred conn.Close() then hung up on the radio.
//
// What that cost in production (us-east-1, 24h to 2026-08-08): user SHA's
// radios still had MQTT uplink enabled on the DEF CON event channels, whose
// PSKs we do not hold. Every such packet tripped BlockInvalidEncryption, so
// 555 undecryptable publishes became 555 disconnects -- one every ~2.6 minutes,
// mean session life 42.8s on the native client. Six other users were in the
// same state. The radios were fine; we were hanging up on them.
//
// A radio that hears RF we cannot decrypt is not a radio that must be
// disconnected. Drop the packet, keep the session.
func TestBlockedPublishDoesNotEndTheSession(t *testing.T) {
	n, logs := recoverTestServer(t)
	backendAddr, recorded := recoverBackend(t)
	n.Config.Server.ProxyForwardAddress = backendAddr

	clientConn, peer := net.Pipe()

	done := make(chan struct{})
	go func() {
		defer close(done)
		n.handleProxy(clientConn)
	}()

	if _, err := peer.Write(v4ConnectBytesKeepalive(t, "meshtastic-blocked", 60)); err != nil {
		t.Fatalf("write CONNECT: %v", err)
	}
	time.Sleep(100 * time.Millisecond)

	if _, err := peer.Write(v4UndecryptablePublishBytes(t)); err != nil {
		t.Fatalf("write the blocked PUBLISH: %v", err)
	}
	time.Sleep(100 * time.Millisecond)

	out := logs.String()
	if !strings.Contains(out, "BLOCK") {
		t.Fatalf("the undecryptable publish was not Blocked, so this test is not exercising the bug.\nlog:\n%s", out)
	}
	if strings.Contains(out, "action=SESSION_END") {
		t.Fatalf("a Blocked packet ended the session -- this is the bug: the radio is disconnected for transmitting RF we cannot decrypt.\nlog:\n%s", out)
	}

	// The session is not merely un-logged as ended -- it still works. A
	// subsequent packet the rules allow must still reach the broker, which is
	// the only assertion that distinguishes "still connected" from "socket
	// closed but nothing said so yet".
	ping := packets.NewControlPacket(packets.Pingreq).(*packets.PingreqPacket)
	var pingBuf bytes.Buffer
	if err := ping.Write(&pingBuf); err != nil {
		t.Fatalf("encode PINGREQ: %v", err)
	}
	if _, err := peer.Write(pingBuf.Bytes()); err != nil {
		t.Fatalf("the client socket was already closed by the Block: %v", err)
	}
	time.Sleep(100 * time.Millisecond)

	if !bytes.Contains(recorded.Bytes(), pingBuf.Bytes()) {
		t.Fatalf("the PINGREQ after a Blocked packet never reached the broker; the session did not survive.\nbackend bytes: %x", recorded.Bytes())
	}

	// And the Blocked packet itself must NOT have been forwarded -- keeping the
	// session alive is not licence to relay what a rule refused.
	if bytes.Contains(recorded.Bytes(), undecryptablePayload(t)) {
		t.Error("the Blocked envelope reached the broker; dropping the packet is the half of this that must not regress")
	}

	peer.Close()
	awaitReturn(t, done, "handleProxy")

	end := logs.String()
	if !strings.Contains(end, "action=SESSION_END") {
		t.Fatalf("no SESSION_END after the client hung up.\nlog:\n%s", end)
	}
	// Not read_error: the session ended because the client went away, and
	// mislabelling it is what hid this bug for four days. Every one of SHA's
	// 555 forced disconnects was logged as reason=read_error.
	if !strings.Contains(end, "reason="+reasonClientEOF) {
		t.Errorf("SESSION_END should report %s for a client that vanished.\nlog:\n%s", reasonClientEOF, end)
	}
	if strings.Count(end, "action=SESSION_END") != 1 {
		t.Errorf("expected exactly one SESSION_END, got %d.\nlog:\n%s", strings.Count(end, "action=SESSION_END"), end)
	}
}

// v5 parity. Meshtastic-Android 2.8 speaks v5 and runs through an entirely
// separate handler, so fixing only 3.1.1 would leave half the fleet still being
// hung up on -- and the v5 loop's comment explicitly said it dropped the
// connection "exactly as handleProxy returns on a Block decision", which is how
// the bug propagated across the protocol split in the first place.
func TestV5BlockedPublishDropsThePacketNotTheSession(t *testing.T) {
	n, logs := v5PublishServer(t, "publisher")
	frame := v5PublishFrame(t, undecryptableEnvelope())

	var backend bytes.Buffer
	got := n.handleV5PublishUplink(writerConn{&backend}, v5PubAddr, frame)

	if got == uplinkForwarded {
		t.Fatal("an undecryptable envelope was relayed to the broker; the Block half of this must not regress")
	}
	if got == uplinkFatal {
		t.Fatalf("a Blocked packet returned uplinkFatal, so the v5 loop hangs up on the radio.\nlog:\n%s", logs.String())
	}
	if backend.Len() != 0 {
		t.Fatalf("%d bytes of a Blocked publish reached the broker", backend.Len())
	}
	if !strings.Contains(logs.String(), "BLOCK") {
		t.Fatalf("no BLOCK line, so this test is not exercising the rule.\nlog:\n%s", logs.String())
	}
}

// The PUBLISH/SUBSCRIBE asymmetry is a decision, not an oversight, so it gets an
// assertion. A dropped PUBLISH costs a QoS-0 packet nobody awaits; a dropped
// SUBSCRIBE would block the client forever on a SUBACK that cannot arrive,
// because the frame never reaches the broker that would answer it.
// The real rule set cannot reach this branch -- AllowMQTTControl is first and
// short-circuits every SUBSCRIBE, which is why production saw 2,813 Blocks and
// none of them a SUBSCRIBE. A block-everything decider is therefore the only
// honest way to pin the verdict, and pinning it is what stops a later rule that
// DOES block a topic filter from silently inheriting the PUBLISH behaviour.
func TestV5BlockedSubscribeStillEndsTheSession(t *testing.T) {
	n, _ := v5PublishServer(t, "publisher")
	n.PacketDecider = NewRuleBasedDecider([]Rule{{
		Name:        "BlockEverything",
		Description: "test-only: force the Block arm the real rules cannot reach",
		Matcher:     func(*InspectorPacket) bool { return true },
		Action:      Block,
		Reason:      "test",
	}})

	var client, backend bytes.Buffer
	sub := n.handleV5SubscribeUplink(writerConn{&client}, writerConn{&backend},
		v5PubAddr, buildV5SubscribeFrame(0x0015, []byte{0x00}, meshFilters))
	if sub != uplinkFatal {
		t.Errorf("a Blocked SUBSCRIBE returned %v, want uplinkFatal; the client is left waiting on a SUBACK that can never arrive", sub)
	}

	pub := n.handleV5PublishUplink(writerConn{&backend}, v5PubAddr, v5PublishFrame(t, nodeInfoEnvelope(t, 3, 3)))
	if pub != uplinkDropped {
		t.Errorf("a Blocked PUBLISH returned %v, want uplinkDropped; the same decider must not end the session on the publish path", pub)
	}
	if backend.Len() != 0 {
		t.Errorf("%d bytes reached the broker under a block-everything decider", backend.Len())
	}
}

// The other half of the contract, and the reason this is a verdict rather than
// "never drop the connection": a session whose broker link is gone cannot be
// kept. Without this, the fix would silently convert every dead backend into a
// client that publishes into a void believing it is connected.
func TestV5DeadBackendIsStillFatal(t *testing.T) {
	n, _ := v5PublishServer(t, "publisher")
	frame := v5PublishFrame(t, nodeInfoEnvelope(t, 3, 3))

	if got := n.handleV5PublishUplink(deadConn{}, v5PubAddr, frame); got != uplinkFatal {
		t.Fatalf("a dead backend returned %v, want uplinkFatal; a half-open session would be kept alive", got)
	}
}

// deadConn is a backend that refuses every write, standing in for mosquitto
// having gone away mid-session.
type deadConn struct{ net.Conn }

func (deadConn) Write([]byte) (int, error) { return 0, errors.New("backend gone") }

// undecryptableEnvelope is the un-marshalled form of undecryptablePayload, for
// the v5 fixtures that build the frame themselves.
func undecryptableEnvelope() *meshtastic.ServiceEnvelope {
	return &meshtastic.ServiceEnvelope{
		Packet: &meshtastic.MeshPacket{
			From:     0x435990e4,
			To:       0xffffffff,
			Id:       0x1234abcd,
			HopLimit: 3,
			HopStart: 3,
			PayloadVariant: &meshtastic.MeshPacket_Encrypted{
				Encrypted: []byte{0x0b, 0x16, 0x21, 0x2c, 0x37, 0x42, 0x4d, 0x58},
			},
		},
		GatewayId: v5PubGw,
		ChannelId: "DEFCONnect",
	}
}

// undecryptablePayload is a ServiceEnvelope whose packet carries an encrypted
// payload no configured channel key can open -- exactly what a radio uplinks
// when it hears RF on a channel we do not own (DEFCONnect, HackerComms,
// NodeChat). It trips BlockInvalidEncryption.
func undecryptablePayload(t *testing.T) []byte {
	t.Helper()
	env := &meshtastic.ServiceEnvelope{
		Packet: &meshtastic.MeshPacket{
			From:     0x1555f041,
			To:       0xffffffff,
			Id:       0x2ad2ad30,
			HopLimit: 3,
			HopStart: 3,
			PayloadVariant: &meshtastic.MeshPacket_Encrypted{
				Encrypted: []byte{0x0b, 0x16, 0x21, 0x2c, 0x37, 0x42, 0x4d, 0x58},
			},
		},
		GatewayId: "!1555f041",
		ChannelId: "DEFCONnect",
	}
	payload, err := proto.Marshal(env)
	if err != nil {
		t.Fatalf("marshal undecryptable envelope: %v", err)
	}
	return payload
}

// v4UndecryptablePublishBytes wraps undecryptablePayload in the 3.1.1 PUBLISH a
// radio actually sends, on the event-channel topic SHA's radios were publishing
// to when the proxy hung up on them 555 times.
func v4UndecryptablePublishBytes(t *testing.T) []byte {
	t.Helper()
	pub := packets.NewControlPacket(packets.Publish).(*packets.PublishPacket)
	pub.TopicName = "msh/US/2/e/DEFCONnect/!1555f041"
	pub.Payload = undecryptablePayload(t)

	var buf bytes.Buffer
	if err := pub.Write(&buf); err != nil {
		t.Fatalf("encode PUBLISH: %v", err)
	}
	return buf.Bytes()
}
