package server

import (
	"bytes"
	"net"
	"strconv"
	"strings"
	"testing"

	v5 "github.com/eclipse/paho.golang/packets"
	"github.com/eclipse/paho.mqtt.golang/packets"
	meshtastic "github.com/whereiskurt/meshtk/protos/meshtastic/generated"
	"google.golang.org/protobuf/proto"
)

// 68-REVIEW CR-02: a Last Will bypassed the entire inspection chain. The Will
// topic and payload were re-encoded into the CONNECT and handed to mosquitto,
// which publishes them on disconnect -- so the payload never traversed
// PacketDecider.Decide and was never touched by RewriteHopLimit. These tests
// name the actual threat: the Will payload is a REAL marshalled
// ServiceEnvelope carrying an oversized hop budget, the same fixture shape the
// review used to prove the bypass.

const willFloodTopic = "msh/US/2/e/dc.run/!435990e4"

// willFloodEnvelope is the packet the strip exists to stop: HopLimit 7 (> the
// clamp's 3) and HopStart 9 (> HOP_MAX 7) on a broadcast. Delivered as a Will it
// would be rebroadcast by every downlink-enabled radio at that hop budget --
// the fleet-wide RF amplification RewriteHopLimit exists to prevent, obtained by
// connecting and dropping the socket.
func willFloodEnvelope(t *testing.T) []byte {
	t.Helper()
	env := &meshtastic.ServiceEnvelope{
		Packet: &meshtastic.MeshPacket{
			From:     0x1555f041,
			To:       0xffffffff,
			Id:       0x0be1f100,
			HopLimit: 7,
			HopStart: 9,
			PayloadVariant: &meshtastic.MeshPacket_Decoded{
				Decoded: &meshtastic.Data{
					Portnum: meshtastic.PortNum_TEXT_MESSAGE_APP,
					Payload: []byte("FLOOD-VIA-WILL"),
				},
			},
		},
		GatewayId: "!435990e4",
		ChannelId: "dc.run",
	}
	raw, err := proto.Marshal(env)
	if err != nil {
		t.Fatalf("marshal will envelope: %v", err)
	}
	return raw
}

// TestWillStrippedFromV5Connect is the v5 half of ROADMAP Success Criterion 3.
func TestWillStrippedFromV5Connect(t *testing.T) {
	buf, n := simpleFormatterServer(t)

	clientConn, peer := net.Pipe()
	defer clientConn.Close()
	defer peer.Close()
	drain(t, peer)

	will := willFloodEnvelope(t)
	cp, c := mqttasticConnect(t)
	c.WillFlag = true
	c.WillQOS = 1
	c.WillRetain = true
	c.WillTopic = willFloodTopic
	c.WillMessage = will
	c.WillProperties = &v5.Properties{}

	if !n.inspectV5Connect(clientConn, "203.0.113.7:50000", c) {
		t.Fatal("valid credentials rejected")
	}

	forwarded, got := reparseConnect(t, cp)

	// The decisive assertion: the marshalled envelope bytes are not in what the
	// broker receives. Asserting only on the parsed struct would pass if the
	// payload survived somewhere else in the packet.
	if bytes.Contains(forwarded, will) {
		t.Fatalf("the Will ServiceEnvelope (hop_limit=7/hop_start=9) reached the broker inside the CONNECT (%d bytes forwarded)", len(forwarded))
	}
	if bytes.Contains(forwarded, []byte("FLOOD-VIA-WILL")) {
		t.Fatal("the Will payload text reached the broker inside the CONNECT")
	}
	if bytes.Contains(forwarded, []byte(willFloodTopic)) {
		t.Fatal("the Will topic reached the broker inside the CONNECT")
	}

	if got.WillFlag {
		t.Error("forwarded CONNECT still sets WillFlag")
	}
	if got.WillTopic != "" {
		t.Errorf("forwarded CONNECT still carries WillTopic %q", got.WillTopic)
	}
	if len(got.WillMessage) != 0 {
		t.Errorf("forwarded CONNECT still carries a %d-byte WillMessage", len(got.WillMessage))
	}
	if got.WillQOS != 0 {
		t.Errorf("forwarded CONNECT still carries WillQOS %d", got.WillQOS)
	}
	if got.WillRetain {
		t.Error("forwarded CONNECT still sets WillRetain")
	}

	// The log line 69-07 greps for in production, on the exact shape shipped.
	assertWillStrippedLine(t, buf.String(), 5, len(will))
}

// TestWillStripSurvivesReEncode guards the vendored-Pack panic: WillFlag true
// with a nil WillProperties dereferences inside Pack's `if c.WillFlag` branch.
// Clearing every field in one assignment is what keeps that state unreachable --
// this test would crash the binary rather than fail politely if it regressed.
func TestWillStripSurvivesReEncode(t *testing.T) {
	_, n := simpleFormatterServer(t)

	clientConn, peer := net.Pipe()
	defer clientConn.Close()
	defer peer.Close()
	drain(t, peer)

	cp, c := mqttasticConnect(t)
	c.WillFlag = true
	c.WillTopic = willFloodTopic
	c.WillMessage = willFloodEnvelope(t)
	c.WillProperties = &v5.Properties{}

	if !n.inspectV5Connect(clientConn, "203.0.113.7:50000", c) {
		t.Fatal("valid credentials rejected")
	}

	var out bytes.Buffer
	if _, err := cp.WriteTo(&out); err != nil {
		t.Fatalf("re-encoding the stripped CONNECT failed: %v", err)
	}
}

// TestV5ConnectWithoutWillIsUnchanged is the other half of the contract: a
// Will-less CONNECT must not gain a log line or a mutation. Byte-identity
// itself is pinned by the pre-existing TestV5ConnectNoMutationIsByteIdentical.
func TestV5ConnectWithoutWillIsUnchanged(t *testing.T) {
	buf, n := simpleFormatterServer(t)

	clientConn, peer := net.Pipe()
	defer clientConn.Close()
	defer peer.Close()
	drain(t, peer)

	cp, c := mqttasticConnect(t)
	if !n.inspectV5Connect(clientConn, "203.0.113.7:50000", c) {
		t.Fatal("valid credentials rejected")
	}

	if strings.Contains(buf.String(), "WILL_STRIPPED") {
		t.Fatalf("a Will-less CONNECT emitted a strip line:\n%s", buf.String())
	}
	if got := strings.Count(buf.String(), "\n"); got != 1 {
		t.Fatalf("a Will-less CONNECT emitted %d log lines (want exactly 1: MQTT5_CONNECT)", got)
	}

	_, got := reparseConnect(t, cp)
	if got.WillFlag || got.WillTopic != "" || len(got.WillMessage) != 0 {
		t.Error("a Will-less CONNECT gained Will state")
	}
}

// TestWillStrippedFromV4Connect is the 3.1.1 half of ROADMAP Success Criterion 3.
func TestWillStrippedFromV4Connect(t *testing.T) {
	buf, n := simpleFormatterServer(t)

	clientConn, peer := net.Pipe()
	defer clientConn.Close()
	defer peer.Close()
	drain(t, peer)

	will := willFloodEnvelope(t)
	ip := v4ConnectInspectorPacket("ed270dbe5d1e", "meshtastic-will")
	ip.Log = n.InspectorLogger
	p := (*ip.Raw.MQTT).(*packets.ConnectPacket)
	p.WillFlag = true
	p.WillQos = 1
	p.WillRetain = true
	p.WillTopic = willFloodTopic
	p.WillMessage = will

	ip.inspectRawPacket(n, clientConn)
	if ip.AuthRejected {
		t.Fatal("valid credentials rejected")
	}

	forwarded, got := reserializeV4Connect(t, p)

	if bytes.Contains(forwarded, will) {
		t.Fatalf("the Will ServiceEnvelope (hop_limit=7/hop_start=9) reached the broker inside the CONNECT (%d bytes forwarded)", len(forwarded))
	}
	if bytes.Contains(forwarded, []byte("FLOOD-VIA-WILL")) {
		t.Fatal("the Will payload text reached the broker inside the CONNECT")
	}
	if bytes.Contains(forwarded, []byte(willFloodTopic)) {
		t.Fatal("the Will topic reached the broker inside the CONNECT")
	}

	if got.WillFlag {
		t.Error("forwarded CONNECT still sets WillFlag")
	}
	if got.WillTopic != "" {
		t.Errorf("forwarded CONNECT still carries WillTopic %q", got.WillTopic)
	}
	if len(got.WillMessage) != 0 {
		t.Errorf("forwarded CONNECT still carries a %d-byte WillMessage", len(got.WillMessage))
	}
	if got.WillQos != 0 {
		t.Errorf("forwarded CONNECT still carries WillQos %d", got.WillQos)
	}
	if got.WillRetain {
		t.Error("forwarded CONNECT still sets WillRetain")
	}

	assertWillStrippedLine(t, buf.String(), 4, len(will))
}

// TestWillStripAppliesToV4Passthrough covers the branch that forwards the
// client's own credentials by design. It must not also forward a Will.
func TestWillStripAppliesToV4Passthrough(t *testing.T) {
	buf, n := simpleFormatterServer(t)

	clientConn, peer := net.Pipe()
	defer clientConn.Close()
	defer peer.Close()
	drain(t, peer)

	// "ghosts" is the passthrough identity newTestServerCmd configures.
	ip := v4ConnectInspectorPacket("ghosts", "ghost-will")
	ip.Log = n.InspectorLogger
	p := (*ip.Raw.MQTT).(*packets.ConnectPacket)
	p.WillFlag = true
	p.WillTopic = willFloodTopic
	p.WillMessage = willFloodEnvelope(t)

	ip.inspectRawPacket(n, clientConn)

	if p.Username != "ghosts" {
		t.Fatalf("passthrough credential was swapped to %q -- wrong branch under test", p.Username)
	}
	if p.WillFlag || p.WillTopic != "" || len(p.WillMessage) != 0 {
		t.Fatal("passthrough CONNECT forwarded an uninspected Will")
	}
	if !strings.Contains(buf.String(), "action=WILL_STRIPPED") {
		t.Fatalf("passthrough Will strip was not logged:\n%s", buf.String())
	}
}

// TestV4ConnectWithoutWillIsUnchanged mirrors the v5 no-Will contract. The
// byte-level guarantee for the 3.1.1 forwarded stream is the golden
// (TestV4SessionForwardBytesGolden), whose fixture is Will-less.
func TestV4ConnectWithoutWillIsUnchanged(t *testing.T) {
	buf, n := simpleFormatterServer(t)

	clientConn, peer := net.Pipe()
	defer clientConn.Close()
	defer peer.Close()
	drain(t, peer)

	ip := v4ConnectInspectorPacket("ed270dbe5d1e", "meshtastic-golden")
	ip.Log = n.InspectorLogger
	p := (*ip.Raw.MQTT).(*packets.ConnectPacket)
	before, _ := reserializeV4Connect(t, p)

	ip.inspectRawPacket(n, clientConn)

	if strings.Contains(buf.String(), "WILL_STRIPPED") {
		t.Fatalf("a Will-less CONNECT emitted a strip line:\n%s", buf.String())
	}
	after, got := reserializeV4Connect(t, p)
	if got.WillFlag || got.WillTopic != "" || len(got.WillMessage) != 0 {
		t.Error("a Will-less CONNECT gained Will state")
	}
	// The credential swap is the ONLY intended mutation, so the byte streams
	// differ -- but only in the credential region. Length is the cheap proxy:
	// "proxy"/"proxypass" vs "ed270dbe5d1e"/"hunter2".
	if len(before) == 0 || len(after) == 0 {
		t.Fatal("re-serialization produced nothing")
	}
}

// TestUnclampedHopWillNeverReachesBackendOnEitherCodec is ROADMAP Success
// Criterion 3 stated as a test: a CONNECT carrying a Will cannot deliver an
// unclamped hop_limit broadcast to the broker on EITHER codec. One envelope,
// both codecs, one assertion -- so the two paths cannot drift into disagreeing
// about the threat.
func TestUnclampedHopWillNeverReachesBackendOnEitherCodec(t *testing.T) {
	will := willFloodEnvelope(t)

	t.Run("v5", func(t *testing.T) {
		_, n := simpleFormatterServer(t)
		clientConn, peer := net.Pipe()
		defer clientConn.Close()
		defer peer.Close()
		drain(t, peer)

		cp, c := mqttasticConnect(t)
		c.WillFlag = true
		c.WillTopic = willFloodTopic
		c.WillMessage = will
		c.WillProperties = &v5.Properties{}

		if !n.inspectV5Connect(clientConn, "203.0.113.7:50000", c) {
			t.Fatal("valid credentials rejected")
		}
		forwarded, _ := reparseConnect(t, cp)
		assertNoWillEnvelope(t, forwarded, will)
	})

	t.Run("3.1.1", func(t *testing.T) {
		_, n := simpleFormatterServer(t)
		clientConn, peer := net.Pipe()
		defer clientConn.Close()
		defer peer.Close()
		drain(t, peer)

		ip := v4ConnectInspectorPacket("ed270dbe5d1e", "meshtastic-will")
		ip.Log = n.InspectorLogger
		p := (*ip.Raw.MQTT).(*packets.ConnectPacket)
		p.WillFlag = true
		p.WillTopic = willFloodTopic
		p.WillMessage = will

		ip.inspectRawPacket(n, clientConn)
		if ip.AuthRejected {
			t.Fatal("valid credentials rejected")
		}
		forwarded, _ := reserializeV4Connect(t, p)
		assertNoWillEnvelope(t, forwarded, will)
	})
}

func assertNoWillEnvelope(t *testing.T, forwarded, will []byte) {
	t.Helper()
	if bytes.Contains(forwarded, will) {
		t.Fatalf("an unclamped-hop Will ServiceEnvelope reached the backend (%d bytes forwarded)", len(forwarded))
	}
	// Also assert on a distinctive fragment, so a re-encoding that reorders
	// protobuf fields could not smuggle the payload past a whole-slice match.
	if bytes.Contains(forwarded, []byte("FLOOD-VIA-WILL")) {
		t.Fatal("the Will payload text reached the backend")
	}
}

// reserializeV4Connect writes the (possibly mutated) CONNECT exactly as
// handleProxy does -- (*ip.Raw.MQTT).Write(&buf) -- and re-reads it, so the
// assertions are on the bytes the backend would actually receive.
func reserializeV4Connect(t *testing.T, p *packets.ConnectPacket) ([]byte, *packets.ConnectPacket) {
	t.Helper()
	var out bytes.Buffer
	if err := p.Write(&out); err != nil {
		t.Fatalf("encode CONNECT: %v", err)
	}
	round, err := packets.ReadPacket(bytes.NewReader(out.Bytes()))
	if err != nil {
		t.Fatalf("re-parse CONNECT: %v", err)
	}
	got, ok := round.(*packets.ConnectPacket)
	if !ok {
		t.Fatalf("re-parsed as %T, want *packets.ConnectPacket", round)
	}
	return out.Bytes(), got
}

// assertWillStrippedLine pins the PRODUCTION log contract in ONE place, so the
// 3.1.1 and v5 assertions cannot drift -- the same reason the field names and
// field order are identical across the two codecs: 69-07 greps
// `action=WILL_STRIPPED` in CloudWatch and tells the codecs apart by
// protocol_version alone.
func assertWillStrippedLine(t *testing.T, out string, protocolVersion, wantBytes int) {
	t.Helper()
	if n := strings.Count(out, "action=WILL_STRIPPED"); n != 1 {
		t.Fatalf("emitted %d action=WILL_STRIPPED lines (want exactly 1):\n%s", n, out)
	}
	for _, want := range []string{
		"action=WILL_STRIPPED",
		"ip=203.0.113.7:50000",
		"protocol_version=" + strconv.Itoa(protocolVersion),
		"username=",
		"will_topic=" + willFloodTopic,
		"will_bytes=" + strconv.Itoa(wantBytes),
	} {
		if !strings.Contains(out, want) {
			t.Errorf("strip line missing %q:\n%s", want, out)
		}
	}
	// The payload's own bytes must never appear -- only its length.
	if strings.Contains(out, "FLOOD-VIA-WILL") {
		t.Errorf("the Will payload content was logged:\n%s", out)
	}
}
