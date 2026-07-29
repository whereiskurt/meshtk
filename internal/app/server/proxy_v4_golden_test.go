package server

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"net"
	"strings"
	"testing"

	"github.com/eclipse/paho.mqtt.golang/packets"
	log "github.com/sirupsen/logrus"
	meshtastic "github.com/whereiskurt/meshtk/protos/meshtastic/generated"
	"google.golang.org/protobuf/proto"
)

// The MQTT v5 dual-codec work (phase 68) may not change what a 3.1.1 client
// sees. "Stability is a HARD requirement" — so this file pins the EXACT bytes
// the proxy forwards for a captured 3.1.1 session, generated from the sources
// as they stood BEFORE the v5 seam was cut. Any later edit that perturbs the
// v4 path — including the plan-68-02 extraction of logDownlink into
// logDownlinkEnvelope, or a stray re-encode in the shared rewrite helpers —
// changes one of these constants and fails here.
//
// The fixture is deliberately not a passthrough-only session: its PUBLISH
// carries a ServiceEnvelope with HopLimit 7 / HopStart 9, so RewriteHopLimit
// trips and RemarshalEnvelope rewrites the payload during the run. The golden
// therefore contains the CLAMPED, re-marshalled bytes — the whole
// rule-mutation-to-wire path is inside the fixture, not adjacent to it.
//
// Regenerating: blank a constant, run the test, paste the printed hex back.
// Doing that is only legitimate when the v4 wire behavior was MEANT to change.

const (
	// Uplink: CONNECT (creds swapped to the proxy identity) + PUBLISH (hop
	// clamped and re-marshalled) + SUBSCRIBE + PINGREQ, concatenated exactly as
	// handleProxy writes them to the backend.
	v4UplinkForwardGolden = "102f00044d51545404c2003c00116d6573687461737469632d676f6c64656e000570726f7879000970726f78797061737330" +
		"50001b6d73682f55532f322f652f64632e72756e2f2134333539393065340a1e0d41f0551515ffffffff3578563412480350" +
		"0178078801012a040b16212c120664632e72756e1a092134333539393065348218001500136d73682f55532f322f652f6463" +
		"2e72756e2f2300c000"

	// The PacketDecider outcome for each forwarded packet, in order. A silent
	// decision flip (e.g. a rule stopping/starting to match) would otherwise be
	// invisible to a bytes-only golden.
	v4UplinkDecisionsGolden = "ALLOW,ALLOW,ALLOW,ALLOW"

	// Downlink: the bytes handleBackend writes back to the client for a broker
	// PUBLISH gatewayed by a DIFFERENT connection (no self-echo suppression).
	v4DownlinkForwardGolden = "304400186d73682f55532f322f652f504b492f2131353535663034310a180d41f0551515e49059433530add22a5001880101" +
		"2a02dead1203504b491a09213135353566303431"
)

func goldenTestServer(t *testing.T, auth *mockAuthenticator) *ServerCmd {
	t.Helper()
	n := newTestServerCmd(auth)
	logger := log.New()
	logger.SetLevel(log.PanicLevel)
	n.Config.Log = logger
	n.InspectorLogger = logger
	n.LoadInspectorRules()
	return n
}

// goldenUplinkEnvelope is a PKI-encrypted ServiceEnvelope with an oversized hop
// budget (HopLimit 7 > 3, HopStart 9 > HOP_MAX 7). PKI keeps the fixture free of
// channel-key setup while still reaching an ALLOW decision, and the oversized
// hops guarantee RewriteHopLimit + RemarshalEnvelope run.
func goldenUplinkEnvelope(t *testing.T) []byte {
	t.Helper()
	env := &meshtastic.ServiceEnvelope{
		Packet: &meshtastic.MeshPacket{
			From:         0x1555f041,
			To:           0xffffffff,
			Id:           0x12345678,
			HopLimit:     7,
			HopStart:     9,
			WantAck:      true,
			PkiEncrypted: true,
			PayloadVariant: &meshtastic.MeshPacket_Encrypted{
				Encrypted: []byte{0x0b, 0x16, 0x21, 0x2c},
			},
		},
		GatewayId: "!435990e4",
		ChannelId: "dc.run",
	}
	raw, err := proto.Marshal(env)
	if err != nil {
		t.Fatalf("marshal golden envelope: %v", err)
	}
	return raw
}

// v4SessionBytes is the captured client->proxy byte stream: a 3.1.1 CONNECT
// with credentials, a meshtastic PUBLISH, a SUBSCRIBE and a PINGREQ.
func v4SessionBytes(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer

	cp := packets.NewControlPacket(packets.Connect).(*packets.ConnectPacket)
	cp.ProtocolName = "MQTT"
	cp.ProtocolVersion = 4
	cp.CleanSession = true
	cp.Keepalive = 60
	cp.ClientIdentifier = "meshtastic-golden"
	cp.UsernameFlag = true
	cp.Username = "ed270dbe5d1e"
	cp.PasswordFlag = true
	cp.Password = []byte("hunter2")
	if err := cp.Write(&buf); err != nil {
		t.Fatalf("encode CONNECT: %v", err)
	}

	pub := packets.NewControlPacket(packets.Publish).(*packets.PublishPacket)
	pub.TopicName = "msh/US/2/e/dc.run/!435990e4"
	pub.Payload = goldenUplinkEnvelope(t)
	if err := pub.Write(&buf); err != nil {
		t.Fatalf("encode PUBLISH: %v", err)
	}

	sub := packets.NewControlPacket(packets.Subscribe).(*packets.SubscribePacket)
	sub.MessageID = 0x0015
	sub.Topics = []string{"msh/US/2/e/dc.run/#"}
	sub.Qoss = []byte{0}
	if err := sub.Write(&buf); err != nil {
		t.Fatalf("encode SUBSCRIBE: %v", err)
	}

	ping := packets.NewControlPacket(packets.Pingreq).(*packets.PingreqPacket)
	if err := ping.Write(&buf); err != nil {
		t.Fatalf("encode PINGREQ: %v", err)
	}

	return buf.Bytes()
}

func decisionName(d Decision) string {
	switch d {
	case Allow:
		return "ALLOW"
	case Block:
		return "BLOCK"
	case Rewrote:
		return "REWROTE"
	case NoMatch:
		return "NOMATCH"
	case Kill:
		return "KILL"
	case Slow:
		return "SLOW"
	}
	return "UNKNOWN"
}

func assertGolden(t *testing.T, name, got, want string) {
	t.Helper()
	if want == "" {
		t.Fatalf("golden %s is empty — paste this in:\n\t%s = %q", name, name, got)
	}
	if got != want {
		t.Fatalf("%s drifted:\n got  %s\n want %s", name, got, want)
	}
}

func TestV4SessionForwardBytesGolden(t *testing.T) {
	const addr = "203.0.113.7:50000"

	mock := &mockAuthenticator{valid: true}
	n := goldenTestServer(t, mock)

	clientConn, peer := net.Pipe()
	defer clientConn.Close()
	defer peer.Close()

	logger := log.New()
	logger.SetLevel(log.PanicLevel)

	// --- uplink leg: the exact inner sequence handleProxy runs per packet ---
	reader := bufio.NewReader(bytes.NewReader(v4SessionBytes(t)))
	var forwarded bytes.Buffer
	var decisions []string

	for i := 0; ; i++ {
		packet, err := packets.ReadPacket(reader)
		if err != nil {
			break
		}

		ip := &InspectorPacket{
			Log:   logger,
			Track: &ConnectionInfo{SocketAddress: addr},
			Raw:   &RawPacket{MQTT: &packet},
		}
		ip.inspectRawPacket(n, clientConn)
		if ip.AuthRejected {
			t.Fatalf("packet %d: auth rejected with a valid-credential mock", i)
		}

		result := n.PacketDecider.Decide(ip)
		decisions = append(decisions, decisionName(result.Decision))

		var out bytes.Buffer
		if err := (*ip.Raw.MQTT).Write(&out); err != nil {
			t.Fatalf("packet %d: serialize for forward: %v", i, err)
		}
		forwarded.Write(out.Bytes())
	}

	if len(decisions) != 4 {
		t.Fatalf("read %d packets from the fixture, want 4", len(decisions))
	}
	if mock.callCount != 1 {
		t.Fatalf("Authenticator.Verify called %d times, want 1", mock.callCount)
	}

	assertGolden(t, "v4UplinkForwardGolden", hex.EncodeToString(forwarded.Bytes()), v4UplinkForwardGolden)
	assertGolden(t, "v4UplinkDecisionsGolden", strings.Join(decisions, ","), v4UplinkDecisionsGolden)

	// The client's password must never be what the backend sees.
	if bytes.Contains(forwarded.Bytes(), []byte("hunter2")) {
		t.Fatal("the client password reached the forwarded bytes")
	}

	// The uplink PUBLISH taught the connection its gateway id — that is what
	// the downlink leg's self-echo check keys off.
	if got := n.gatewayFor(addr); got != "!435990e4" {
		t.Fatalf("gatewayFor(%s) = %q, want !435990e4", addr, got)
	}

	// --- downlink leg: logDownlink's decision AND the bytes it lets through ---
	other := goldenDownlinkPublish(t, "!1555f041", 0x1555f041, 0x435990e4)
	if n.logDownlink(peer, addr, other) {
		t.Fatal("another gateway's downlink was suppressed; replies would never reach the radio")
	}
	var down bytes.Buffer
	if err := other.Write(&down); err != nil {
		t.Fatalf("serialize downlink: %v", err)
	}
	assertGolden(t, "v4DownlinkForwardGolden", hex.EncodeToString(down.Bytes()), v4DownlinkForwardGolden)

	// Same connection's own uplink echoed back by the broker: suppressed.
	own := goldenDownlinkPublish(t, "!435990e4", 0x435990e4, 0x1555f041)
	if !n.logDownlink(peer, addr, own) {
		t.Fatal("own-gateway echo was forwarded; the radio gets its own traffic back down BLE")
	}
}

func goldenDownlinkPublish(t *testing.T, gatewayID string, from, to uint32) *packets.PublishPacket {
	t.Helper()
	env := &meshtastic.ServiceEnvelope{
		Packet: &meshtastic.MeshPacket{
			From:         from,
			To:           to,
			Id:           0x2ad2ad30,
			WantAck:      true,
			PkiEncrypted: true,
			PayloadVariant: &meshtastic.MeshPacket_Encrypted{
				Encrypted: []byte{0xde, 0xad},
			},
		},
		GatewayId: gatewayID,
		ChannelId: "PKI",
	}
	payload, err := proto.Marshal(env)
	if err != nil {
		t.Fatalf("marshal downlink envelope: %v", err)
	}
	pub := packets.NewControlPacket(packets.Publish).(*packets.PublishPacket)
	pub.TopicName = "msh/US/2/e/PKI/" + gatewayID
	pub.Payload = payload
	return pub
}
