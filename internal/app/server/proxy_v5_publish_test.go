package server

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	v5 "github.com/eclipse/paho.golang/packets"
	log "github.com/sirupsen/logrus"
	meshtastic "github.com/whereiskurt/meshtk/protos/meshtastic/generated"
	"google.golang.org/protobuf/proto"
)

// Every assertion in this file is made against WIRE BYTES, never against the
// parsed struct. Two reasons, both learned the hard way:
//
//  1. meshtk#22 shipped a hop clamp that mutated the parsed struct and never
//     re-marshalled it onto the wire. The rule reported Rewrote, the broker got
//     the original packet, and nothing caught it for weeks.
//  2. ControlPacket.WriteTo rewrites FixedHeader.Flags for PUBLISH to
//     Type<<4|flags, so a struct assertion reads 0x32 where the first byte on
//     the wire is what actually matters.

const (
	v5PubAddr  = "203.0.113.7:50000"
	v5PubTopic = "msh/US/2/e/dc.run/!435990e4"
	v5PubGw    = "!435990e4"
)

// captureLogger returns a logger whose output can be grepped, so log-line
// assertions (action=BLOCK, action=MQTT5_PARSE_FAIL) test the actual ops
// contract rather than an internal return value.
func captureLogger() (*log.Logger, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	logger := log.New()
	logger.SetOutput(buf)
	logger.SetLevel(log.DebugLevel)
	logger.SetFormatter(&log.TextFormatter{DisableColors: true, DisableTimestamp: true})
	return logger, buf
}

// v5PublishServer is a ServerCmd with the real rule set loaded and a tracked
// connection that already passed CONNECT, which is the state every uplink
// PUBLISH arrives in.
func v5PublishServer(t *testing.T, username string) (*ServerCmd, *bytes.Buffer) {
	t.Helper()
	n := newTestServerCmd(&mockAuthenticator{valid: true})
	logger, logs := captureLogger()
	n.Config.Log = logger
	n.InspectorLogger = logger
	n.LoadInspectorRules()
	n.ConnTrack[v5PubAddr] = &ConnectionInfo{
		SocketAddress:   v5PubAddr,
		Username:        username,
		ClientID:        "mqttastic-android-test",
		ProtocolVersion: 5,
	}
	return n, logs
}

// nodeInfoEnvelope is a decoded NODEINFO ServiceEnvelope -- the highest-volume
// uplink on the real fleet, and one the rules engine judges without any
// channel-key setup in the fixture.
//
// That is now a convenience, not a hazard. This comment used to say a
// TEXT_MESSAGE could not be used here because it reached RewritePayloadString
// and dereferenced a nil cipher; that crash (68-REVIEW CR-01) is closed by
// 69-01 and is covered on BOTH codecs in rules_rewrite_test.go.
func nodeInfoEnvelope(t *testing.T, hopLimit, hopStart uint32) *meshtastic.ServiceEnvelope {
	t.Helper()
	user, err := proto.Marshal(&meshtastic.User{
		Id:        v5PubGw,
		LongName:  "DC34 test",
		ShortName: "T34",
	})
	if err != nil {
		t.Fatalf("marshal user: %v", err)
	}
	return &meshtastic.ServiceEnvelope{
		Packet: &meshtastic.MeshPacket{
			From:     0x435990e4,
			To:       0xffffffff,
			Id:       0x1234abcd,
			HopLimit: hopLimit,
			HopStart: hopStart,
			PayloadVariant: &meshtastic.MeshPacket_Decoded{
				Decoded: &meshtastic.Data{
					Portnum:  meshtastic.PortNum_NODEINFO_APP,
					Payload:  user,
					Bitfield: proto.Uint32(1),
				},
			},
		},
		GatewayId: v5PubGw,
		ChannelId: "dc.run",
	}
}

// v5PublishPacket builds the shape mqttastic actually sends: QoS1 with a packet
// id, a MessageExpiry property and a User property. All of those must survive a
// rewrite round trip.
func v5PublishPacket(t *testing.T, env *meshtastic.ServiceEnvelope) *v5.ControlPacket {
	t.Helper()
	payload, err := proto.Marshal(env)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}

	cp := v5.NewControlPacket(v5.PUBLISH)
	p := cp.Content.(*v5.Publish)
	p.Topic = v5PubTopic
	p.QoS = 1
	p.PacketID = 0x1234
	expiry := uint32(300)
	p.Properties = &v5.Properties{
		MessageExpiry: &expiry,
		User:          []v5.User{{Key: "src", Value: "android"}},
	}
	p.Payload = payload
	return cp
}

func v5PublishFrame(t *testing.T, env *meshtastic.ServiceEnvelope) []byte {
	t.Helper()
	var buf bytes.Buffer
	if _, err := v5PublishPacket(t, env).WriteTo(&buf); err != nil {
		t.Fatalf("encode PUBLISH: %v", err)
	}
	return buf.Bytes()
}

// wireEnvelope decodes the ServiceEnvelope out of an ENCODED v5 PUBLISH frame.
// Going through the wire bytes is the entire point: a struct read would pass
// even if the rewrite never reached the forwarded packet.
func wireEnvelope(t *testing.T, frame []byte) (*v5.Publish, *meshtastic.ServiceEnvelope) {
	t.Helper()
	pk, err := v5.ReadPacket(bytes.NewReader(frame))
	if err != nil {
		t.Fatalf("re-parse forwarded PUBLISH: %v", err)
	}
	p, ok := pk.Content.(*v5.Publish)
	if !ok {
		t.Fatalf("forwarded frame parsed as %T, want *v5.Publish", pk.Content)
	}
	env := &meshtastic.ServiceEnvelope{}
	if err := proto.Unmarshal(p.Payload, env); err != nil {
		t.Fatalf("unmarshal forwarded envelope: %v", err)
	}
	return p, env
}

// The hop clamp must land on the bytes the broker receives, and everything the
// client negotiated around the payload must survive the re-encode.
func TestV5PublishRewriteReachesTheWire(t *testing.T) {
	n, _ := v5PublishServer(t, "publisher")
	frame := v5PublishFrame(t, nodeInfoEnvelope(t, 7, 9))

	var backend bytes.Buffer
	if !n.handleV5PublishUplink(writerConn{&backend}, v5PubAddr, frame) {
		t.Fatal("connection dropped for a packet the rules allow")
	}

	out := backend.Bytes()
	if bytes.Equal(out, frame) {
		t.Fatal("the captured frame was forwarded verbatim; the clamp never reached the wire (meshtk#22)")
	}

	p, env := wireEnvelope(t, out)

	if got := env.GetPacket().GetHopLimit(); got != 3 {
		t.Errorf("wire hop_limit = %d, want 3", got)
	}
	if got := env.GetPacket().GetHopStart(); got != 7 {
		t.Errorf("wire hop_start = %d, want 7 (HOP_MAX clamp)", got)
	}
	if env.GetPacket().GetHopStart() < env.GetPacket().GetHopLimit() {
		t.Error("clamp produced hop_start < hop_limit: 2.8 firmware drops that as corrupt")
	}
	// 2.8 drops decoded packets whose bitfield is absent (pre-hop drop).
	if env.GetPacket().GetDecoded().Bitfield == nil {
		t.Error("Data.bitfield lost in the remarshal; 2.8 radios would drop the packet")
	}

	// The first WIRE byte, not FixedHeader.Flags: WriteTo rewrites the struct
	// field to Type<<4|flags and a struct assertion would read 0x32 either way.
	if out[0] != 0x32 {
		t.Errorf("first wire byte = %#02x, want 0x32 (PUBLISH, QoS1)", out[0])
	}
	if p.PacketID != 0x1234 {
		t.Errorf("packet id = %#04x, want 0x1234", p.PacketID)
	}
	if p.Topic != v5PubTopic {
		t.Errorf("topic = %q, want %q", p.Topic, v5PubTopic)
	}
	if p.Properties == nil || p.Properties.MessageExpiry == nil || *p.Properties.MessageExpiry != 300 {
		t.Error("MessageExpiry property did not survive the rewrite")
	}
	if p.Properties == nil || len(p.Properties.User) != 1 ||
		p.Properties.User[0].Key != "src" || p.Properties.User[0].Value != "android" {
		t.Errorf("User property did not survive the rewrite: %+v", p.Properties)
	}
}

// A packet no rule mutated must go out as the bytes that came in. Re-encoding
// unconditionally would be gratuitous wire churn on the hot path and would risk
// dropping anything the codec models imperfectly.
func TestV5PublishUnchangedIsByteIdentical(t *testing.T) {
	n, _ := v5PublishServer(t, "publisher")
	frame := v5PublishFrame(t, nodeInfoEnvelope(t, 3, 3))

	var backend bytes.Buffer
	if !n.handleV5PublishUplink(writerConn{&backend}, v5PubAddr, frame) {
		t.Fatal("connection dropped for a packet the rules allow")
	}
	if !bytes.Equal(backend.Bytes(), frame) {
		t.Fatalf("forwarded bytes drifted:\n in  %x\n out %x", frame, backend.Bytes())
	}
}

// A topic alias makes Publish.Topic empty, blinding every topic rule and every
// msh/... log line while the broker resolves the alias and fans out normally.
// 68-01 already strips TopicAliasMaximum in both directions; this guard makes a
// spec-violating client loud instead of invisible.
func TestV5TopicAliasBlocked(t *testing.T) {
	// 30 0b | 0000 (empty topic) | 03 2300 03 (TopicAlias=3) | "hello"
	const aliasFixture = "300b00000323000368656c6c6f"

	n, logs := v5PublishServer(t, "publisher")
	frame := mustHex(t, aliasFixture)

	var backend bytes.Buffer
	if n.handleV5PublishUplink(writerConn{&backend}, v5PubAddr, frame) {
		t.Fatal("an aliased PUBLISH was allowed through")
	}
	if backend.Len() != 0 {
		t.Fatalf("%d bytes of an aliased PUBLISH reached the broker", backend.Len())
	}

	got := logs.String()
	if !strings.Contains(got, "action=BLOCK") || !strings.Contains(got, "reason=topic_alias_uplink") {
		t.Fatalf("missing the alias BLOCK log line; got:\n%s", got)
	}
}

// SetConnTrack is load-bearing, not cosmetic: it swaps in the tracked
// ConnectionInfo carrying the ORIGINAL client username. RequireMQTTUserName
// Blocks on an empty Track.Username, so skipping it would Block every single
// v5 publish -- silently, since the connection had already authenticated.
func TestV5PublishFeedsDeciderWithTrackedUsername(t *testing.T) {
	n := v5TestServer(t, &mockAuthenticator{valid: true})
	n.LoadInspectorRules()

	clientConn, peer := net.Pipe()
	defer clientConn.Close()
	defer peer.Close()

	_, c := mqttasticConnect(t)
	if !n.inspectV5Connect(clientConn, v5PubAddr, c) {
		t.Fatal("valid credentials rejected")
	}
	// The CONNECT forwarded to the broker now carries the proxy identity...
	if c.Username != "proxy" {
		t.Fatalf("credential swap did not happen: %q", c.Username)
	}

	ip := n.inspectV5Publish(v5PubAddr, v5PublishPacket(t, nodeInfoEnvelope(t, 3, 3)))

	// ...but the rules engine must still judge the packet as the CLIENT.
	if ip.Track.Username != v5TestUsername {
		t.Fatalf("Track.Username = %q, want the original client username %q", ip.Track.Username, v5TestUsername)
	}
	if ip.MQTT.Type != "PUBLISH" {
		t.Errorf("MQTT.Type = %q, want PUBLISH", ip.MQTT.Type)
	}
	if len(ip.MQTT.Topics) != 1 || ip.MQTT.Topics[0] != v5PubTopic {
		t.Errorf("MQTT.Topics = %v, want [%s]", ip.MQTT.Topics, v5PubTopic)
	}
	if !ip.Meshtastic.WasUnmarshalled {
		t.Error("envelope not decoded; inspectMeshtastic never ran")
	}

	if result := n.PacketDecider.Decide(ip); result.Decision == Block {
		t.Fatalf("decider Blocked an authenticated v5 publish: %s", result.Reason)
	}
}

// Without the gateway id on the ConnTrack entry, downlink self-echo suppression
// can never match and silently degrades to off.
func TestV5PublishRemembersGateway(t *testing.T) {
	n, _ := v5PublishServer(t, "publisher")

	if got := n.gatewayFor(v5PubAddr); got != "" {
		t.Fatalf("gateway recorded before any publish: %q", got)
	}
	n.inspectV5Publish(v5PubAddr, v5PublishPacket(t, nodeInfoEnvelope(t, 3, 3)))
	if got := n.gatewayFor(v5PubAddr); got != v5PubGw {
		t.Errorf("gatewayFor = %q, want %q", got, v5PubGw)
	}
}

// --- the topic-alias guard on the hand-parsed path (68-REVIEW WR-01) --------

// propertyPublishFrame builds a QoS0 PUBLISH with a caller-chosen property
// block, and ASSERTS the codec refuses it -- otherwise the test would silently
// exercise the parseable path and prove nothing about the hand-parse arm.
func propertyPublishFrame(t *testing.T, topic string, props, payload []byte) []byte {
	t.Helper()

	varHeader := []byte{byte(len(topic) >> 8), byte(len(topic))}
	varHeader = append(varHeader, []byte(topic)...)
	varHeader = append(varHeader, encodeV5Varint(len(props))...)
	varHeader = append(varHeader, props...)

	body := append(append([]byte{}, varHeader...), payload...)
	frame := []byte{0x30} // PUBLISH, QoS 0
	frame = append(frame, encodeV5Varint(len(body))...)
	frame = append(frame, body...)

	if _, err := v5.ReadPacket(bytes.NewReader(frame)); err == nil {
		t.Fatal("the fixture parses cleanly; it cannot exercise the hand-parse arm")
	}
	return frame
}

// aliasThenUnknown and unknownThenAlias are the SAME two properties in the two
// possible orders, and the order is the whole experiment: the walk reads left to
// right, so one ordering is detectable and the other is not.
var (
	aliasThenUnknown = []byte{0x23, 0x00, 0x07, 0x7f, 0x00}
	unknownThenAlias = []byte{0x7f, 0x00, 0x23, 0x00, 0x07}
)

// WR-01, closed. The codec path Blocks on Properties.TopicAlias != nil; the
// hand-parse path skipped the property block whole and could not see it, so a
// client picked which inspection judged it by choosing property bytes. Same
// reason string on both paths, so production greps and prior evidence keep
// working.
func TestV5TopicAliasBlockedOnHandParsedPath(t *testing.T) {
	n, logs := v5PublishServer(t, "publisher")
	frame := propertyPublishFrame(t, v5PubTopic, aliasThenUnknown,
		marshalEnvelope(t, nodeInfoEnvelope(t, 3, 3)))

	var backend bytes.Buffer
	if n.handleV5PublishUplink(writerConn{&backend}, v5PubAddr, frame) {
		t.Fatal("a PUBLISH carrying a Topic Alias was allowed on the hand-parsed path (WR-01)")
	}
	if backend.Len() != 0 {
		t.Fatalf("%d bytes of an aliased PUBLISH reached the broker", backend.Len())
	}

	got := logs.String()
	if !strings.Contains(got, "action=BLOCK") || !strings.Contains(got, "reason=topic_alias_uplink") {
		t.Fatalf("missing the alias BLOCK line the codec path also emits; got:\n%s", got)
	}
	// A Block is the outcome, not an indeterminate walk: the alias came FIRST,
	// so the walk reached a conclusive answer before meeting the unmodelled id.
	if strings.Contains(got, "MQTT5_ALIAS_SCAN_INDETERMINATE") {
		t.Fatalf("a conclusive walk was reported as indeterminate; got:\n%s", got)
	}
}

// The other ordering, and the case that must NOT become a Block. An unmodelled
// id ahead of the alias defeats the walk -- so the result is honestly reported
// as indeterminate and the frame is inspected, decided and clamped exactly as it
// would have been without the walk. Degrading to a Block here would reintroduce
// CR-04's defect class with the sign flipped: a property table deciding a packet.
func TestV5AliasIndeterminateStillFullyInspected(t *testing.T) {
	n, logs := v5PublishServer(t, "publisher")
	frame := propertyPublishFrame(t, v5PubTopic, unknownThenAlias,
		marshalEnvelope(t, nodeInfoEnvelope(t, 7, 9)))

	var backend bytes.Buffer
	if !n.handleV5PublishUplink(writerConn{&backend}, v5PubAddr, frame) {
		t.Fatal("an indeterminate alias walk Blocked the connection; the walk must never decide")
	}

	out := backend.Bytes()
	if bytes.Equal(out, frame) {
		t.Fatal("the captured frame was forwarded verbatim; the clamp never reached the wire")
	}

	// Locate the payload with the hand parser: the codec cannot read this frame,
	// and neither may the assertion.
	view, err := parseV5PublishFrame(out)
	if err != nil {
		t.Fatalf("the forwarded frame does not parse: %v", err)
	}
	if view.Topic != v5PubTopic {
		t.Errorf("forwarded topic = %q, want %q", view.Topic, v5PubTopic)
	}
	var env meshtastic.ServiceEnvelope
	if err := proto.Unmarshal(view.Payload, &env); err != nil {
		t.Fatalf("forwarded payload is not a ServiceEnvelope: %v", err)
	}
	if got := env.GetPacket().GetHopLimit(); got != 3 {
		t.Errorf("wire hop_limit = %d, want 3 -- the clamp did not run", got)
	}
	if got := env.GetPacket().GetHopStart(); got != 7 {
		t.Errorf("wire hop_start = %d, want 7 (HOP_MAX clamp)", got)
	}

	// Every unmodelled property byte still survives the rewrite verbatim.
	if !bytes.Contains(out, unknownThenAlias) {
		t.Errorf("the property block was lost from the forwarded frame: %x", out)
	}

	got := logs.String()
	if n := strings.Count(got, "action=MQTT5_ALIAS_SCAN_INDETERMINATE"); n != 1 {
		t.Fatalf("one frame produced %d indeterminate lines (want exactly 1); got:\n%s", n, got)
	}
	if strings.Contains(got, "reason=topic_alias_uplink") {
		t.Fatalf("an indeterminate walk Blocked for an alias it never saw; got:\n%s", got)
	}
}

// v5PublishServerSimpleFormatter wires the InspectorLogger to the REAL
// production formatter and to its OWN buffer.
//
// Both halves matter. SimpleFormatter is a bare fmt.Sprintf("%s %s\n", ...) with
// no quoting -- logrus' TextFormatter, which every other harness in this file
// uses, quotes, and that is precisely why WR-05 shipped undetected. And
// Config.Log gets a separate logger because the "[proxy] ALLOW" line is written
// there: counting PHYSICAL LINES in the inspector log is only meaningful if the
// inspector log is the only thing in the buffer.
func v5PublishServerSimpleFormatter(t *testing.T, clientID string) (*ServerCmd, *bytes.Buffer) {
	t.Helper()
	n := newTestServerCmd(&mockAuthenticator{valid: true})
	quiet, _ := captureLogger()
	n.Config.Log = quiet

	buf := &bytes.Buffer{}
	inspector := log.New()
	inspector.SetOutput(buf)
	inspector.SetLevel(log.DebugLevel)
	inspector.SetFormatter(&SimpleFormatter{TimestampFormat: "2006-01-02 15:04:05.000"})
	n.InspectorLogger = inspector

	n.LoadInspectorRules()
	n.ConnTrack[v5PubAddr] = &ConnectionInfo{
		SocketAddress:   v5PubAddr,
		Username:        "publisher",
		ClientID:        clientID,
		ProtocolVersion: 5,
	}
	return n, buf
}

// 69-03 closed WR-05 by sanitizing every client-controlled string at every
// InspectorLogger boundary. A line ADDED after that sweep reopens it unless it
// obeys the same rule, so the rule is tested rather than trusted: the client id
// carries a newline plus a fully-formed forged BLOCK record, and the new line
// must still be exactly one physical line.
func TestV5AliasIndeterminateLogCannotBeForged(t *testing.T) {
	const forged = "evil\n2026-07-29 00:00:00.000 action=BLOCK, ip=10.0.0.1, reason=topic_alias_uplink"

	n, buf := v5PublishServerSimpleFormatter(t, forged)
	frame := propertyPublishFrame(t, v5PubTopic, unknownThenAlias,
		marshalEnvelope(t, nodeInfoEnvelope(t, 7, 9)))

	var backend bytes.Buffer
	if !n.handleV5PublishUplink(writerConn{&backend}, v5PubAddr, frame) {
		t.Fatal("connection dropped for a packet the rules allow")
	}

	out := buf.String()
	idx := strings.Index(out, "action=MQTT5_ALIAS_SCAN_INDETERMINATE")
	if idx < 0 {
		t.Fatalf("the indeterminate line was never emitted; got:\n%s", out)
	}
	// Everything from the new line to the end of the log: exactly ONE newline,
	// which is the new line's own terminator. Two means the client id opened a
	// second record -- WR-05, reopened.
	if got := strings.Count(out[idx:], "\n"); got != 1 {
		t.Fatalf("the new line spans %d physical lines (want exactly 1):\n%s", got, out[idx:])
	}
	// The forged text must SURVIVE inside the single record -- evidence of the
	// attempt is the point; it just must not be its own line.
	if !strings.Contains(out, "action=BLOCK") {
		t.Fatalf("the forged text was dropped rather than contained; got:\n%s", out)
	}
	// And the whole frame is still exactly two records: the pre-existing
	// MQTT5_PARSE_FAIL that marks the hand-parse arm, plus the new line. A
	// hostile client id adds nothing.
	if got := strings.Count(out, "\n"); got != 2 {
		t.Fatalf("one frame produced %d log lines (want 2: MQTT5_PARSE_FAIL + the new line):\n%s", got, out)
	}
}

// paho.golang's Properties.Unpack hard-errors on any property id outside its
// table, so this frame reaches the hand-parse arm. It is then INSPECTED -- topic
// recovered, envelope decoded, PacketDecider run -- and forwarded byte-identically
// only because no rule mutated it, which is a very different thing from the
// fail-open relay this test used to be named for.
//
// Formerly TestV5PublishParseFailureForwardsRaw, which advertised the posture
// 68-02 retired when it closed CR-04. Every assertion below is unchanged; only
// the name and this comment moved.
func TestV5PublishInspectedThenForwardedByteIdentical(t *testing.T) {
	// 30 0a | 0003 "abc" | 02 7f00 (property id 0x7f is not modelled) | "hi"
	const unparseable = "300a0003616263027f006869"

	frame := mustHex(t, unparseable)
	if _, err := v5.ReadPacket(bytes.NewReader(frame)); err == nil {
		t.Fatal("the fixture parses cleanly; it cannot exercise the parse-failure path")
	}

	n, logs := v5PublishServer(t, "publisher")
	var backend bytes.Buffer
	if !n.handleV5PublishUplink(writerConn{&backend}, v5PubAddr, frame) {
		t.Fatal("connection dropped over an unparseable PUBLISH")
	}
	if !bytes.Equal(backend.Bytes(), frame) {
		t.Fatalf("forwarded %x, want the captured frame %x", backend.Bytes(), frame)
	}

	got := logs.String()
	if !strings.Contains(got, "action=MQTT5_PARSE_FAIL") || !strings.Contains(got, "mqtt_type=PUBLISH") {
		t.Fatalf("missing the parse-failure log line; got:\n%s", got)
	}
}

// --- v5 downlink: logging, self-echo suppression, no re-encode --------------

// deadlineConn records SetReadDeadline calls so a test can prove WHICH socket
// the downlink loop deadlines. handleBackend once deadlined the CLIENT socket
// from the downlink side, racing the uplink loop on the same socket and tearing
// down live radios at unpredictable intervals; that must not come back on v5.
type deadlineConn struct {
	net.Conn
	mu    sync.Mutex
	calls int
}

func (d *deadlineConn) SetReadDeadline(t time.Time) error {
	d.mu.Lock()
	d.calls++
	d.mu.Unlock()
	return d.Conn.SetReadDeadline(t)
}

func (d *deadlineConn) deadlineCalls() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls
}

// v5DownlinkFrame is a broker PUBLISH carrying a ServiceEnvelope gatewayed by
// the given node.
func v5DownlinkFrame(t *testing.T, gateway string) []byte {
	t.Helper()
	env := nodeInfoEnvelope(t, 3, 3)
	env.GatewayId = gateway
	return v5PublishFrame(t, env)
}

// runBackendV5 drives the real downlink loop over a scripted backend stream and
// returns everything it wrote to the client.
//
// io.ReadFull under a deadline, never a single Read: net.Pipe is unbuffered and
// a frame can arrive as several Writes, so a lone Read consumes the first chunk
// and the writer blocks forever.
func runBackendV5(t *testing.T, n *ServerCmd, socketAddr string, stream []byte, wantBytes int) []byte {
	t.Helper()
	clientConn, peer := net.Pipe()
	defer clientConn.Close()
	defer peer.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		n.handleBackendV5(ctx, clientConn, socketAddr,
			writerConn{&bytes.Buffer{}}, bufio.NewReader(bytes.NewReader(stream)))
	}()

	got := make([]byte, wantBytes)
	if err := peer.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	if wantBytes > 0 {
		if _, err := io.ReadFull(peer, got); err != nil {
			t.Fatalf("read downlink: %v", err)
		}
	}
	return got
}

// A radio's own DMs bouncing back down its BLE pipe is pure waste on the
// flakiest link in the chain -- mosquitto has no no-local, so it echoes every
// publish back to a subscriber of the same topic.
func TestV5DownlinkSelfEchoSuppressed(t *testing.T) {
	n, _ := v5PublishServer(t, "publisher")
	n.ConnTrack[v5PubAddr].GatewayID = v5PubGw

	selfEcho := v5DownlinkFrame(t, v5PubGw)  // suppressed
	other := v5DownlinkFrame(t, "!1555f041") // forwarded

	var stream bytes.Buffer
	stream.Write(selfEcho)
	stream.Write(other)

	got := runBackendV5(t, n, v5PubAddr, stream.Bytes(), len(other))
	if !bytes.Equal(got, other) {
		t.Fatalf("client received %x, want only the other gateway's frame %x", got, other)
	}
}

// A forwarded downlink must be the CAPTURED bytes. Properties.SubscriptionIdentifier
// is modelled as a single pointer while MQTT 5.0 permits several on one PUBLISH
// (overlapping subscriptions), so re-encoding would silently drop all but one.
// The downlink path needs nothing from the packet except Payload and Topic.
func TestV5DownlinkForwardsCapturedFrame(t *testing.T) {
	n, _ := v5PublishServer(t, "publisher")
	n.ConnTrack[v5PubAddr].GatewayID = v5PubGw

	frame := v5DownlinkFrame(t, "!1555f041")
	got := runBackendV5(t, n, v5PubAddr, frame, len(frame))
	if !bytes.Equal(got, frame) {
		t.Fatalf("downlink drifted:\n in  %x\n out %x", frame, got)
	}
}

// An unparseable downlink PUBLISH is forwarded and the connection stays open --
// proven by the PINGRESP that follows it arriving too.
func TestV5DownlinkParseFailureForwardsRaw(t *testing.T) {
	n, logs := v5PublishServer(t, "publisher")

	bad := mustHex(t, "300a0003616263027f006869")
	if _, err := v5.ReadPacket(bytes.NewReader(bad)); err == nil {
		t.Fatal("the fixture parses cleanly; it cannot exercise the parse-failure path")
	}
	pingresp := mustHex(t, "d000")

	var stream bytes.Buffer
	stream.Write(bad)
	stream.Write(pingresp)

	got := runBackendV5(t, n, v5PubAddr, stream.Bytes(), len(bad)+len(pingresp))
	if !bytes.Equal(got[:len(bad)], bad) {
		t.Fatalf("unparseable downlink = %x, want it forwarded verbatim as %x", got[:len(bad)], bad)
	}
	if !bytes.Equal(got[len(bad):], pingresp) {
		t.Fatal("the connection did not survive an unparseable downlink PUBLISH")
	}

	if out := logs.String(); !strings.Contains(out, "action=MQTT5_PARSE_FAIL") ||
		!strings.Contains(out, "mqtt_type=PUBLISH_DOWNLINK") {
		t.Fatalf("missing the downlink parse-failure log line; got:\n%s", out)
	}
}

// The deadline belongs on the socket being READ. handleBackendV5 must touch
// backendConn's deadline and never conn's -- the uplink loop owns that one.
func TestV5DownlinkDeadlineOnBackendSocket(t *testing.T) {
	n, _ := v5PublishServer(t, "publisher")

	clientConn, peer := net.Pipe()
	defer clientConn.Close()
	defer peer.Close()

	client := &deadlineConn{Conn: clientConn}
	backend := &deadlineConn{Conn: writerConn{&bytes.Buffer{}}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		n.handleBackendV5(ctx, client, v5PubAddr, backend,
			bufio.NewReader(bytes.NewReader(mustHex(t, "d000"))))
	}()

	if err := peer.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	if _, err := io.ReadFull(peer, make([]byte, 2)); err != nil {
		t.Fatalf("read PINGRESP: %v", err)
	}
	<-done // the stream is exhausted, so the loop exits on its own

	if got := client.deadlineCalls(); got != 0 {
		t.Fatalf("handleBackendV5 set the CLIENT socket's read deadline %d times; that races the uplink loop and tore down live radios", got)
	}
	if got := backend.deadlineCalls(); got == 0 {
		t.Fatal("handleBackendV5 never deadlined the backend socket; a silent broker would hang the loop forever")
	}
}
