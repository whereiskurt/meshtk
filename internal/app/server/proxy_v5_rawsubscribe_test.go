package server

import (
	"bytes"
	"encoding/hex"
	"strings"
	"testing"

	v5 "github.com/eclipse/paho.golang/packets"
	"github.com/eclipse/paho.mqtt.golang/packets"
	log "github.com/sirupsen/logrus"
)

// This file pins the property-agnostic SUBSCRIBE seam (68-REVIEW WR-04).
//
// Until it landed, a v5 SUBSCRIBE paho.golang refused to parse was relayed
// WITHOUT ever building an InspectorPacket -- so it never reached
// PacketDecider, MQTT.Topics was never recorded, and the first topic Block
// rule anyone added would silently not apply to it. The exemption was bought
// with the same three client-chosen property bytes as CR-04: a client picked
// which inspection judged it by choosing bytes the codec cannot read.
//
// The assertions here are deliberately BEHAVIORAL. "Reads no property id" is
// proven by two frames that differ ONLY in their property-id bytes parsing
// identically -- never by a grep -- because a grep cannot fail when a future
// edit adds a table.

// --- fixtures ---------------------------------------------------------------

type v5SubFilter struct {
	Topic string
	Opts  byte
}

// buildV5SubscribeFrame assembles a SUBSCRIBE on the wire by hand, so a test
// can put bytes in the property block that no codec would ever emit.
func buildV5SubscribeFrame(packetID uint16, props []byte, filters []v5SubFilter) []byte {
	var body bytes.Buffer
	body.WriteByte(byte(packetID >> 8))
	body.WriteByte(byte(packetID))
	body.Write(encodeV5Varint(len(props)))
	body.Write(props)
	for _, f := range filters {
		body.WriteByte(byte(len(f.Topic) >> 8))
		body.WriteByte(byte(len(f.Topic)))
		body.WriteString(f.Topic)
		body.WriteByte(f.Opts)
	}

	var frame bytes.Buffer
	frame.WriteByte(0x82) // SUBSCRIBE, reserved flags 0010
	frame.Write(encodeV5Varint(body.Len()))
	frame.Write(body.Bytes())
	return frame.Bytes()
}

var meshFilters = []v5SubFilter{
	{Topic: v5SubTopicA, Opts: 0x00},
	{Topic: v5SubTopicB, Opts: 0x01},
}

// --- the parser -------------------------------------------------------------

func TestParseV5SubscribeFrame(t *testing.T) {
	for _, tc := range []struct {
		desc     string
		frame    []byte
		wantID   uint16
		wantSubs []string
	}{
		{
			desc:     "empty property block, one filter",
			frame:    buildV5SubscribeFrame(0x0015, nil, []v5SubFilter{{Topic: v5SubTopicA}}),
			wantID:   0x0015,
			wantSubs: []string{v5SubTopicA},
		},
		{
			desc:     "two filters in order, non-zero options",
			frame:    buildV5SubscribeFrame(0x1234, nil, meshFilters),
			wantID:   0x1234,
			wantSubs: []string{v5SubTopicA, v5SubTopicB},
		},
		{
			desc:     "modelled subscription-identifier property",
			frame:    buildV5SubscribeFrame(0x0007, []byte{0x0b, 0x05}, meshFilters),
			wantID:   0x0007,
			wantSubs: []string{v5SubTopicA, v5SubTopicB},
		},
		{
			desc:     "property id no MQTT 5.0 table defines",
			frame:    buildV5SubscribeFrame(0x0007, []byte{0x7f, 0x05}, meshFilters),
			wantID:   0x0007,
			wantSubs: []string{v5SubTopicA, v5SubTopicB},
		},
		{
			desc:     "multi-byte property block length varint",
			frame:    buildV5SubscribeFrame(0x00ff, bytes.Repeat([]byte{0x7f}, 200), meshFilters),
			wantID:   0x00ff,
			wantSubs: []string{v5SubTopicA, v5SubTopicB},
		},
		{
			desc:     "zero-length topic filter is REPORTED, not judged",
			frame:    buildV5SubscribeFrame(0x0001, nil, []v5SubFilter{{Topic: ""}}),
			wantID:   0x0001,
			wantSubs: []string{""},
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			rs, err := parseV5SubscribeFrame(tc.frame)
			if err != nil {
				t.Fatalf("parseV5SubscribeFrame(%x) = %v", tc.frame, err)
			}
			if rs.PacketID != tc.wantID {
				t.Errorf("PacketID = %#04x, want %#04x", rs.PacketID, tc.wantID)
			}
			if len(rs.Filters) != len(tc.wantSubs) {
				t.Fatalf("Filters = %q, want %q", rs.Filters, tc.wantSubs)
			}
			for i := range tc.wantSubs {
				if rs.Filters[i] != tc.wantSubs[i] {
					t.Fatalf("Filters = %q, want %q (order matters)", rs.Filters, tc.wantSubs)
				}
			}
		})
	}
}

// Every rejection is a frame whose OWN length prefixes contradict its bytes --
// the same bar action=MQTT5_PUBLISH_HEADER_FAIL already applies, and the same
// bar mosquitto applies. There is no partial view on any of them.
func TestParseV5SubscribeFrameRejects(t *testing.T) {
	good := buildV5SubscribeFrame(0x0015, nil, []v5SubFilter{{Topic: "a"}})

	for _, tc := range []struct {
		desc  string
		frame []byte
	}{
		{"empty input", nil},
		{"one byte", []byte{0x82}},
		{"not a SUBSCRIBE", mustHexNoT("300a0003616263027f006869")},
		{"remaining length disagrees with the bytes present", append(append([]byte{}, good...), 0x00)},
		{"truncated packet identifier", mustHexNoT("820112")},
		{"truncated property block length", mustHexNoT("8202" + "1234")},
		{"property block declares more than is present", mustHexNoT("8203" + "1234" + "7f")},
		{"truncated topic filter length prefix", mustHexNoT("8204" + "1234" + "00" + "00")},
		{"topic filter declares more bytes than present", mustHexNoT("8207" + "1234" + "00" + "0005" + "6162")},
		{"topic filter with no subscription-options byte", mustHexNoT("8206" + "1234" + "00" + "0001" + "61")},
		{"EMPTY FILTER LIST (malformed per MQTT 5.0)", buildV5SubscribeFrame(0x0015, nil, nil)},
		{"empty filter list behind a property block", buildV5SubscribeFrame(0x0015, []byte{0x7f, 0x05}, nil)},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			rs, err := parseV5SubscribeFrame(tc.frame)
			if err == nil {
				t.Fatalf("parseV5SubscribeFrame(%x) accepted a malformed frame: %+v", tc.frame, rs)
			}
			if rs != nil {
				t.Fatalf("a rejected frame produced a partial view: %+v", rs)
			}
		})
	}
}

// THE property-agnosticism proof, and the reason this file does not settle for a
// grep. Two frames differ in ONE byte -- the property id -- and one of them is a
// property no MQTT 5.0 table defines. The codec parses the first and refuses the
// second; the hand parser must answer identically for both, because it skips the
// block by its declared length and never looks inside.
func TestParseV5SubscribeFrameIsPropertyAgnostic(t *testing.T) {
	modelled := buildV5SubscribeFrame(0x0042, []byte{0x0b, 0x05}, meshFilters)
	unmodelled := buildV5SubscribeFrame(0x0042, []byte{0x7f, 0x05}, meshFilters)

	// The fixtures must differ in exactly the property id, or the test proves
	// nothing about property-agnosticism.
	diff := 0
	if len(modelled) != len(unmodelled) {
		t.Fatalf("fixtures differ in length (%d vs %d); they must differ only in the property id",
			len(modelled), len(unmodelled))
	}
	for i := range modelled {
		if modelled[i] != unmodelled[i] {
			diff++
		}
	}
	if diff != 1 {
		t.Fatalf("fixtures differ in %d bytes, want exactly 1 (the property id)", diff)
	}

	// And the codec must genuinely disagree about them, or the seam under test
	// is never reached in production.
	if _, err := v5.ReadPacket(bytes.NewReader(modelled)); err != nil {
		t.Fatalf("the modelled-property fixture does not parse: %v", err)
	}
	if _, err := v5.ReadPacket(bytes.NewReader(unmodelled)); err == nil {
		t.Fatal("the unmodelled-property fixture parses cleanly; it cannot exercise the hand-parse arm")
	}

	a, err := parseV5SubscribeFrame(modelled)
	if err != nil {
		t.Fatalf("modelled: %v", err)
	}
	b, err := parseV5SubscribeFrame(unmodelled)
	if err != nil {
		t.Fatalf("unmodelled: %v", err)
	}

	if a.PacketID != b.PacketID {
		t.Fatalf("packet id depends on the property id: %#04x vs %#04x", a.PacketID, b.PacketID)
	}
	if strings.Join(a.Filters, "\x00") != strings.Join(b.Filters, "\x00") {
		t.Fatalf("filter list depends on the property id: %q vs %q", a.Filters, b.Filters)
	}
	if len(a.Filters) != 2 {
		t.Fatalf("filters = %q, want both mesh filters recovered", a.Filters)
	}
}

// --- the inspector ----------------------------------------------------------

func v5RawSubServer(t *testing.T) (*ServerCmd, *bytes.Buffer) {
	t.Helper()
	n, logs := v5ParityServer(t)
	n.ConnTrack[v5ParityAddr] = &ConnectionInfo{
		SocketAddress:   v5ParityAddr,
		Username:        v5TestUsername,
		ClientID:        v5TestClientID,
		ProtocolVersion: 5,
	}
	return n, logs
}

func TestV5RawSubscribeInspectorRecordsFilters(t *testing.T) {
	n, _ := v5RawSubServer(t)

	rs, err := parseV5SubscribeFrame(buildV5SubscribeFrame(0x0015, []byte{0x7f, 0x05}, meshFilters))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	ip := n.inspectV5RawSubscribe(v5ParityAddr, rs)

	if ip.MQTT.Type != "SUBSCRIBE" {
		t.Errorf("MQTT.Type = %q, want SUBSCRIBE (the same literal inspectV5Subscribe sets)", ip.MQTT.Type)
	}
	if len(ip.MQTT.Topics) != 2 || ip.MQTT.Topics[0] != v5SubTopicA || ip.MQTT.Topics[1] != v5SubTopicB {
		t.Fatalf("MQTT.Topics = %v, want [%s %s] in filter order", ip.MQTT.Topics, v5SubTopicA, v5SubTopicB)
	}
	if ip.Raw.MQTT5RawSub == nil {
		t.Fatal("Raw.MQTT5RawSub not populated; the rules engine cannot see the packet")
	}
	// At most ONE RawPacket member is ever set -- and NEVER a synthesized
	// v5.Subscribe, which would make Raw.MQTT5 lie about provenance and let a
	// rule mutate something that never reaches the wire (meshtk#22).
	if ip.Raw.MQTT != nil || ip.Raw.MQTT5 != nil || ip.Raw.MQTT5Raw != nil {
		t.Fatalf("more than one RawPacket member is set: %+v", ip.Raw)
	}
	// SetConnTrack is load-bearing: the forwarded CONNECT carries the swapped
	// proxy identity, so without it Track.Username is empty and
	// RequireMQTTUserName Blocks a subscribe on an authenticated session.
	if ip.Track.Username != v5TestUsername {
		t.Fatalf("Track.Username = %q, want the tracked original %q", ip.Track.Username, v5TestUsername)
	}
}

// SC: the matching rule by NAME, not merely "not Blocked". AllowMQTTControl is
// FIRST among the inspect rules and short-circuits, so a decision-only assertion
// would pass whether or not the inspector ran at all.
func TestV5RawSubscribeMatchesSameRuleAsCodecAndV4(t *testing.T) {
	n, _ := v5RawSubServer(t)

	rs, err := parseV5SubscribeFrame(buildV5SubscribeFrame(0x0015, []byte{0x7f, 0x05}, meshFilters))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	rawIP := n.inspectV5RawSubscribe(v5ParityAddr, rs)
	codecIP := n.inspectV5Subscribe(v5ParityAddr, v5SubscribePacket(t))

	v4Sub := packets.NewControlPacket(packets.Subscribe).(*packets.SubscribePacket)
	v4Sub.MessageID = 0x0015
	v4Sub.Topics = []string{v5SubTopicA, v5SubTopicB}
	v4Sub.Qoss = []byte{0, 1}
	var raw packets.ControlPacket = v4Sub
	logger, _ := captureLogger()
	v4IP := &InspectorPacket{
		Log:   logger,
		Track: &ConnectionInfo{SocketAddress: v5ParityAddr, Username: v5TestUsername},
		Raw:   &RawPacket{MQTT: &raw},
	}

	rawRule := matchingRuleName(t, n.PacketDecider, rawIP)
	codecRule := matchingRuleName(t, n.PacketDecider, codecIP)
	v4Rule := matchingRuleName(t, n.PacketDecider, v4IP)

	if rawRule != codecRule {
		t.Fatalf("a hand-parsed SUBSCRIBE matched %q but a codec-parsed one matched %q", rawRule, codecRule)
	}
	if rawRule != v4Rule {
		t.Fatalf("a hand-parsed v5 SUBSCRIBE matched %q but a 3.1.1 SUBSCRIBE matched %q", rawRule, v4Rule)
	}
	if rawRule != "AllowMQTTControl" {
		t.Fatalf("matching rule = %q, want AllowMQTTControl", rawRule)
	}

	if n.PacketDecider.Decide(rawIP).Decision != n.PacketDecider.Decide(codecIP).Decision {
		t.Fatal("the decision differs between the hand-parsed and codec-parsed SUBSCRIBE paths")
	}
}

// The third AllowMQTTControl branch must not perturb the first two, and must not
// panic on a RawPacket that carries only the new member.
func TestAllowMQTTControlRawSubscribe(t *testing.T) {
	rule := allowMQTTControlRule(t)
	logger, _ := captureLogger()

	ip := &InspectorPacket{
		Log:   logger,
		Track: &ConnectionInfo{SocketAddress: v5ParityAddr, ProtocolVersion: 5},
		Raw:   &RawPacket{MQTT5RawSub: &v5RawSubscribe{PacketID: 1, Filters: []string{v5SubTopicA}}},
	}
	if !rule.Matcher(ip) {
		t.Fatal("AllowMQTTControl did not match a hand-parsed v5 SUBSCRIBE")
	}

	// The explicit neither-codec-populated tail still answers false.
	empty := &InspectorPacket{
		Log:   logger,
		Track: &ConnectionInfo{SocketAddress: v5ParityAddr},
		Raw:   &RawPacket{},
	}
	if rule.Matcher(empty) {
		t.Fatal("AllowMQTTControl matched a packet no codec populated")
	}
}

// --- the uplink arm ---------------------------------------------------------

// unparseableSubscribe is the fixture the pre-existing parity test already
// proves the codec refuses: property id 0x7f inside an otherwise well-formed
// two-filter SUBSCRIBE.
func unparseableSubscribe(t *testing.T) []byte {
	t.Helper()
	frame := buildV5SubscribeFrame(0x0015, []byte{0x7f, 0x05}, meshFilters)
	if _, err := v5.ReadPacket(bytes.NewReader(frame)); err == nil {
		t.Fatal("the fixture parses cleanly; it cannot exercise the hand-parse arm")
	}
	return frame
}

// The CAPTURED frame is what relays. The hand parse is read-only: re-encoding
// would drop the very property bytes the codec refused to read.
func TestV5RawSubscribeRelayedByteIdentical(t *testing.T) {
	n, logs := v5RawSubServer(t)
	frame := unparseableSubscribe(t)

	var backend bytes.Buffer
	var client bytes.Buffer
	if !n.handleV5SubscribeUplink(writerConn{&client}, writerConn{&backend}, v5ParityAddr, frame) {
		t.Fatal("the connection was dropped for a SUBSCRIBE the rules allow")
	}
	if !bytes.Equal(backend.Bytes(), frame) {
		t.Fatalf("relayed %x, want the captured frame %x", backend.Bytes(), frame)
	}
	if client.Len() != 0 {
		t.Fatalf("%d bytes were written to the client for an allowed SUBSCRIBE: %x", client.Len(), client.Bytes())
	}

	out := logs.String()
	if !strings.Contains(out, "action=MQTT5_PARSE_FAIL") || !strings.Contains(out, "mqtt_type=SUBSCRIBE") {
		t.Fatalf("the parse-fail signal was dropped; ops loses the only marker that this path was taken:\n%s", out)
	}
	if strings.Contains(out, "action=MQTT5_SUBSCRIBE_HEADER_FAIL") {
		t.Fatalf("a hand-parseable SUBSCRIBE was refused:\n%s", out)
	}
}

// A codec-parseable SUBSCRIBE keeps its existing behavior exactly: inspected,
// decided, relayed as captured, no parse-fail line.
func TestV5CodecSubscribeStillRelayedByteIdentical(t *testing.T) {
	n, logs := v5RawSubServer(t)
	frame := encodePacket(t, v5SubscribePacket(t))

	var backend bytes.Buffer
	var client bytes.Buffer
	if !n.handleV5SubscribeUplink(writerConn{&client}, writerConn{&backend}, v5ParityAddr, frame) {
		t.Fatal("the connection was dropped for a parseable SUBSCRIBE")
	}
	if !bytes.Equal(backend.Bytes(), frame) {
		t.Fatalf("relayed %x, want the captured frame %x", backend.Bytes(), frame)
	}
	if out := logs.String(); strings.Contains(out, "action=MQTT5_PARSE_FAIL") {
		t.Fatalf("a parseable SUBSCRIBE reported a parse failure:\n%s", out)
	}
}

// A SUBSCRIBE whose own length prefixes contradict its bytes fails CLOSED: it is
// logged, answered with the malformed-packet reason code, and the connection
// ends. This is what retires accepted risk T-68-06-05 -- and the bar is narrow
// on purpose, because mosquitto refuses these frames too.
func TestV5SubscribeHeaderFailRefused(t *testing.T) {
	for _, tc := range []struct {
		desc  string
		frame []byte
	}{
		{"length prefixes contradict the bytes", mustHexNoT("8207" + "1234" + "00" + "0005" + "6162")},
		// The empty-filter fixture needs an UNMODELLED property id to reach the
		// hand-parse arm at all: paho.golang parses an empty subscription list
		// happily (probed, not assumed), so a bare 8203001500 goes down the
		// codec path. The refusal lives in parseV5SubscribeFrame, which is the
		// hand-parsed path only -- see TestV5CodecParseableEmptyFilterListStillRelays
		// for the deliberately unchanged other half.
		{"empty filter list", buildV5SubscribeFrame(0x0015, []byte{0x7f, 0x05}, nil)},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			n, logs := v5RawSubServer(t)

			var backend bytes.Buffer
			var client bytes.Buffer
			if n.handleV5SubscribeUplink(writerConn{&client}, writerConn{&backend}, v5ParityAddr, tc.frame) {
				t.Fatal("a malformed SUBSCRIBE was accepted; the connection should end")
			}
			if backend.Len() != 0 {
				t.Fatalf("%d bytes of a refused SUBSCRIBE reached the broker: %x", backend.Len(), backend.Bytes())
			}

			// The client gets a reason code, never a silent close: a client that
			// gets no answer cannot tell refusal from a network fault and retries.
			got := client.Bytes()
			if len(got) < 3 || got[0]>>4 != v5.DISCONNECT {
				t.Fatalf("client-bound bytes = %s, want a DISCONNECT", hex.EncodeToString(got))
			}
			if got[2] != v5.DisconnectMalformedPacket {
				t.Fatalf("DISCONNECT reason = %#02x, want %#02x (malformed packet)", got[2], v5.DisconnectMalformedPacket)
			}

			if out := logs.String(); !strings.Contains(out, "action=MQTT5_SUBSCRIBE_HEADER_FAIL") {
				t.Fatalf("missing the header-fail line; got:\n%s", out)
			}
		})
	}
}

// The scope fence, asserted rather than assumed. paho.golang parses a SUBSCRIBE
// with an empty subscription list without complaint, so such a frame goes down
// the CODEC path and this plan leaves it exactly as it found it: relayed, and
// refused by mosquitto rather than by the proxy. Widening the fail-closed path
// to frames the codec reads fine is not what MQFX-04 asks for, and pinning that
// here means a future widening is a deliberate edit rather than a drift.
func TestV5CodecParseableEmptyFilterListStillRelays(t *testing.T) {
	frame := buildV5SubscribeFrame(0x0015, nil, nil)
	if _, err := v5.ReadPacket(bytes.NewReader(frame)); err != nil {
		t.Fatalf("the codec now refuses an empty filter list (%v); this test's premise moved", err)
	}

	n, logs := v5RawSubServer(t)
	var backend, client bytes.Buffer
	if !n.handleV5SubscribeUplink(writerConn{&client}, writerConn{&backend}, v5ParityAddr, frame) {
		t.Fatal("a codec-parseable SUBSCRIBE was refused; the fail-closed path widened beyond the hand parser")
	}
	if !bytes.Equal(backend.Bytes(), frame) {
		t.Fatalf("relayed %x, want the captured frame %x", backend.Bytes(), frame)
	}
	if out := logs.String(); strings.Contains(out, "action=MQTT5_SUBSCRIBE_HEADER_FAIL") {
		t.Fatalf("the header-fail path fired for a frame the codec reads fine:\n%s", out)
	}
}

// 69-03 closed WR-05 by sanitizing every client-controlled string at every
// InspectorLogger boundary. A line ADDED after that sweep reopens the whole
// finding unless it obeys the same rule, because SimpleFormatter does no
// quoting. So the rule is tested rather than trusted.
func TestV5SubscribeHeaderFailLogCannotBeForged(t *testing.T) {
	const forged = "evil\n2026-07-29 00:00:00.000 action=ALLOW, ip=10.0.0.1, username=admin, mqtt_type=SUBSCRIBE"

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
	n.ConnTrack[v5ParityAddr] = &ConnectionInfo{
		SocketAddress:   v5ParityAddr,
		Username:        v5TestUsername,
		ClientID:        forged,
		ProtocolVersion: 5,
	}

	frame := mustHexNoT("8207" + "1234" + "00" + "0005" + "6162")
	var backend, client bytes.Buffer
	if n.handleV5SubscribeUplink(writerConn{&client}, writerConn{&backend}, v5ParityAddr, frame) {
		t.Fatal("a malformed SUBSCRIBE was accepted")
	}

	out := buf.String()
	idx := strings.Index(out, "action=MQTT5_SUBSCRIBE_HEADER_FAIL")
	if idx < 0 {
		t.Fatalf("the header-fail line was never emitted; got:\n%s", out)
	}
	// From the new line's own offset to the end of the log: exactly ONE newline,
	// which is its own terminator. Two means the client id opened a record of
	// its choosing -- WR-05, reopened.
	if got := strings.Count(out[idx:], "\n"); got != 1 {
		t.Fatalf("the new line spans %d physical lines (want exactly 1):\n%s", got, out[idx:])
	}
	// Evidence of the attempt must SURVIVE inside the single record; it just
	// must not be its own line.
	if !strings.Contains(out, "username=admin") {
		t.Fatalf("the forged text was dropped rather than contained; got:\n%s", out)
	}
	// The whole frame is exactly two records: the pre-existing MQTT5_PARSE_FAIL
	// that marks the hand-parse arm, plus the new line. A hostile client id adds
	// nothing.
	if got := strings.Count(out, "\n"); got != 2 {
		t.Fatalf("one frame produced %d log lines (want 2: MQTT5_PARSE_FAIL + the header-fail line):\n%s", got, out)
	}
}

// mustHexNoT is mustHex for package-level fixtures, where there is no *testing.T
// in scope. A bad literal is a programming error, not a test failure.
func mustHexNoT(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		panic("bad fixture hex " + s + ": " + err.Error())
	}
	return b
}
