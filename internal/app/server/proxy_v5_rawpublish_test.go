package server

import (
	"bytes"
	"strings"
	"testing"

	v5 "github.com/eclipse/paho.golang/packets"
	meshtastic "github.com/whereiskurt/meshtk/protos/meshtastic/generated"
	"google.golang.org/protobuf/proto"
)

// The whole point of this file is the CONTRAST between two parsers. paho.golang's
// Properties.Unpack hard-errors on any property id outside its table, so three
// client-controlled bytes spliced into the properties block used to buy a
// permanent exemption from every inspection the proxy performs (CR-04, verifier
// PROBE-A). parseV5PublishFrame reads only what the MQTT 5.0 wire format
// guarantees is skippable without knowing any property id, so no property table
// can gate inspection ever again.
//
// unmodelledPropertyPublish is the recorded CR-04 fixture:
//
//	30       PUBLISH, QoS 0
//	0a       remaining length 10
//	0003 616263   topic "abc"
//	02       property block length 2
//	7f00     property id 0x7f -- NOT in paho.golang's table
//	6869     payload "hi"
const unmodelledPropertyPublish = "300a0003616263027f006869"

// rawPubTopic is a realistic Meshtastic topic, so the parser is exercised with
// the length the fleet actually sends rather than a 3-byte toy.
const rawPubTopic = "msh/US/2/e/dc.run/!435990e4"

// codecPublishFrame builds a fixture with the REAL paho.golang encoder. Checking
// the hand parser against another hand-rolled writer would only prove the two
// agree with each other; checking it against the codec proves it agrees with the
// wire format.
func codecPublishFrame(t *testing.T, mutate func(p *v5.Publish)) []byte {
	t.Helper()
	cp := v5.NewControlPacket(v5.PUBLISH)
	p := cp.Content.(*v5.Publish)
	p.Topic = rawPubTopic
	p.Properties = &v5.Properties{}
	p.Payload = []byte("meshtastic-payload-bytes")
	mutate(p)

	var buf bytes.Buffer
	if _, err := cp.WriteTo(&buf); err != nil {
		t.Fatalf("encode PUBLISH: %v", err)
	}
	return buf.Bytes()
}

// codecView re-reads a fixture through the codec so the hand parser's answers can
// be compared against the encoder's own idea of what it wrote.
func codecView(t *testing.T, frame []byte) *v5.Publish {
	t.Helper()
	pk, err := v5.ReadPacket(bytes.NewReader(frame))
	if err != nil {
		t.Fatalf("codec cannot read the fixture it produced: %v", err)
	}
	p, ok := pk.Content.(*v5.Publish)
	if !ok {
		t.Fatalf("fixture parsed as %T, not a PUBLISH", pk.Content)
	}
	return p
}

func assertParseMatchesCodec(t *testing.T, frame []byte, wantQoS byte) *v5RawPublish {
	t.Helper()
	want := codecView(t, frame)

	got, err := parseV5PublishFrame(frame)
	if err != nil {
		t.Fatalf("parseV5PublishFrame: %v", err)
	}
	if got.QoS != wantQoS {
		t.Errorf("QoS = %d, want %d", got.QoS, wantQoS)
	}
	if got.QoS != want.QoS {
		t.Errorf("QoS = %d, codec read %d", got.QoS, want.QoS)
	}
	if got.Topic != want.Topic {
		t.Errorf("topic = %q, codec read %q", got.Topic, want.Topic)
	}
	if !bytes.Equal(got.Payload, want.Payload) {
		t.Errorf("payload = %x, codec read %x", got.Payload, want.Payload)
	}
	if got.PayloadOffset+len(got.Payload) != len(frame) {
		t.Errorf("payload offset %d + len %d != frame len %d", got.PayloadOffset, len(got.Payload), len(frame))
	}
	return got
}

func TestParseV5PublishFrameRoundTripQoS0(t *testing.T) {
	frame := codecPublishFrame(t, func(p *v5.Publish) {
		p.QoS = 0
		expiry := uint32(300)
		p.Properties = &v5.Properties{MessageExpiry: &expiry}
	})
	assertParseMatchesCodec(t, frame, 0)
}

func TestParseV5PublishFrameRoundTripQoS1(t *testing.T) {
	frame := codecPublishFrame(t, func(p *v5.Publish) {
		p.QoS = 1
		p.PacketID = 0x1234
		expiry := uint32(300)
		p.Properties = &v5.Properties{
			MessageExpiry: &expiry,
			User:          []v5.User{{Key: "src", Value: "android"}},
		}
	})

	got := assertParseMatchesCodec(t, frame, 1)

	// The packet id is the field a QoS-blind parser would silently swallow into
	// the topic or the property length, so pin that its two bytes were skipped
	// and are still on the wire where the codec put them.
	if frame[0]>>1&0x03 != 1 {
		t.Fatalf("fixture is not QoS1: first byte %#02x", frame[0])
	}
	if got.PayloadOffset <= got.VarHeaderOffset {
		t.Fatalf("payload offset %d is not past the variable header at %d", got.PayloadOffset, got.VarHeaderOffset)
	}
	// topic length prefix (2) + topic + packet id (2) + property length varint
	// + property block -- so at minimum the packet id has to be accounted for.
	minVarHeader := 2 + len(rawPubTopic) + 2 + 1
	if got.PayloadOffset-got.VarHeaderOffset < minVarHeader {
		t.Errorf("variable header spans %d bytes, too short to have skipped the packet id (want >= %d)",
			got.PayloadOffset-got.VarHeaderOffset, minVarHeader)
	}
}

func TestParseV5PublishFrameEmptyPropertyBlock(t *testing.T) {
	frame := codecPublishFrame(t, func(p *v5.Publish) {
		p.QoS = 0
		p.Properties = &v5.Properties{}
		p.Payload = []byte("no-properties-at-all")
	})

	got := assertParseMatchesCodec(t, frame, 0)
	if string(got.Payload) != "no-properties-at-all" {
		t.Errorf("payload = %q", got.Payload)
	}
}

// This is CR-04 in one test: the codec refuses the frame, the hand parser reads
// it. If these two ever agree, the exemption is back.
func TestParseV5PublishFrameUnmodelledProperty(t *testing.T) {
	frame := mustHex(t, unmodelledPropertyPublish)

	if _, err := v5.ReadPacket(bytes.NewReader(frame)); err == nil {
		t.Fatal("the codec parsed the unmodelled-property fixture; it cannot exercise CR-04")
	}

	got, err := parseV5PublishFrame(frame)
	if err != nil {
		t.Fatalf("hand parser failed on a frame the wire format permits: %v", err)
	}
	if got.Topic != "abc" {
		t.Errorf("topic = %q, want %q", got.Topic, "abc")
	}
	if string(got.Payload) != "hi" {
		t.Errorf("payload = %q, want %q", got.Payload, "hi")
	}
	if got.QoS != 0 {
		t.Errorf("QoS = %d, want 0", got.QoS)
	}
	if got.VarHeaderOffset != 2 {
		t.Errorf("VarHeaderOffset = %d, want 2", got.VarHeaderOffset)
	}
	if got.PayloadOffset != 10 {
		t.Errorf("PayloadOffset = %d, want 10", got.PayloadOffset)
	}
}

// An empty topic is the dangerous case (it blinds every topic rule while the
// broker resolves an alias normally), but the parser does not get to decide
// that -- it reports, the caller Blocks.
func TestParseV5PublishFrameEmptyTopic(t *testing.T) {
	frame := codecPublishFrame(t, func(p *v5.Publish) {
		p.QoS = 0
		p.Topic = ""
		p.Payload = []byte("aliased")
	})

	got, err := parseV5PublishFrame(frame)
	if err != nil {
		t.Fatalf("parser rejected an empty topic instead of reporting it: %v", err)
	}
	if got.Topic != "" {
		t.Errorf("topic = %q, want empty", got.Topic)
	}
	if string(got.Payload) != "aliased" {
		t.Errorf("payload = %q", got.Payload)
	}
}

func TestParseV5PublishFrameTruncated(t *testing.T) {
	cases := []struct {
		name  string
		frame string
	}{
		// remaining length 4, topic declares 5 bytes but only 2 follow
		{"inside the topic", "30040005" + "6162"},
		// QoS1, topic "abc", then a single packet-id byte where two are required
		{"inside the packet id", "3206" + "0003616263" + "12"},
		// topic "abc" then a property-length varint that never terminates
		{"inside the property length varint", "3006" + "0003616263" + "80"},
		// property block declares 5 bytes, only 2 follow
		{"inside the property block", "3008" + "0003616263" + "05" + "7f00"},
		// declared remaining length disagrees with the bytes actually present
		{"declared length overruns the frame", "30ff" + "0003616263" + "00" + "6869"},
		// declared remaining length is shorter than the bytes present
		{"declared length undershoots the frame", "3002" + "0003616263" + "00" + "6869"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseV5PublishFrame(mustHex(t, tc.frame))
			if err == nil {
				t.Fatalf("truncated frame parsed successfully: %+v", got)
			}
			if got != nil {
				t.Fatalf("a partial view was returned alongside the error: %+v", got)
			}
		})
	}
}

// The remaining-length field is attacker controlled and up to four varint bytes
// wide, so the cap has to be checked in the parser too -- not only in readFrame.
func TestParseV5PublishFrameOversizeRejected(t *testing.T) {
	// 262145 == maxV5PacketBytes + 1, varint-encoded as 81 80 10.
	frame := mustHex(t, "30818010")

	got, err := parseV5PublishFrame(frame)
	if err == nil {
		t.Fatalf("a remaining length above the cap was accepted: %+v", got)
	}
	if got != nil {
		t.Fatalf("a partial view was returned alongside the error: %+v", got)
	}
}

// ---------------------------------------------------------------------------
// The topic-alias walk (68-REVIEW WR-01).
//
// Every case below is built so that a WRONG SKIP WIDTH is caught rather than
// tolerated: each modelled property's value is padded with 0x7f -- an id the
// walk deliberately does not model -- and a real Topic Alias follows it. Skip
// one byte too few or too many and the walk lands on a 0x7f, gives up, and
// reports not-found-and-incomplete instead of found-and-complete. So "the alias
// was found" is itself the proof that the preceding value was skipped by exactly
// its own width.
// ---------------------------------------------------------------------------

// aliasProp is a real Topic Alias property (id 0x23, two-byte value).
var aliasProp = []byte{0x23, 0x00, 0x07}

func propBlock(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

func TestV5AliasScanWireShapes(t *testing.T) {
	cases := []struct {
		name         string
		block        []byte
		wantFound    bool
		wantComplete bool
	}{
		{
			name:         "payload format indicator (one byte)",
			block:        propBlock([]byte{propPayloadFormatIndicator, 0x7f}, aliasProp),
			wantFound:    true,
			wantComplete: true,
		},
		{
			name:         "message expiry interval (four bytes)",
			block:        propBlock([]byte{propMessageExpiryInterval, 0x7f, 0x7f, 0x7f, 0x7f}, aliasProp),
			wantFound:    true,
			wantComplete: true,
		},
		{
			name:         "content type (length-prefixed string)",
			block:        propBlock([]byte{propContentType, 0x00, 0x03, 0x7f, 0x7f, 0x7f}, aliasProp),
			wantFound:    true,
			wantComplete: true,
		},
		{
			name:         "response topic (length-prefixed string)",
			block:        propBlock([]byte{propResponseTopic, 0x00, 0x02, 0x7f, 0x7f}, aliasProp),
			wantFound:    true,
			wantComplete: true,
		},
		{
			name:         "correlation data (length-prefixed binary)",
			block:        propBlock([]byte{propCorrelationData, 0x00, 0x04, 0x7f, 0x7f, 0x7f, 0x7f}, aliasProp),
			wantFound:    true,
			wantComplete: true,
		},
		{
			// 0xff 0x7f is a TWO-byte varint (the 0x80 continuation bit is set on
			// the first byte). Consume only one and the walk lands on 0x7f.
			name:         "subscription identifier (variable byte integer)",
			block:        propBlock([]byte{propSubscriptionIdentifier, 0xff, 0x7f}, aliasProp),
			wantFound:    true,
			wantComplete: true,
		},
		{
			// The two-byte shape is the alias itself. It is DETECTED rather than
			// skipped -- the walk returns the moment it sees the id, because
			// presence is the only fact the caller needs -- so this case pins the
			// detection and the immediate return, and the trailing 0x7f proves the
			// walk really did stop there instead of reading on.
			name:         "topic alias (two bytes)",
			block:        propBlock(aliasProp, []byte{0x7f}),
			wantFound:    true,
			wantComplete: true,
		},
		{
			name:         "user property (two length-prefixed strings)",
			block:        propBlock([]byte{propUserProperty, 0x00, 0x01, 0x7f, 0x00, 0x02, 0x7f, 0x7f}, aliasProp),
			wantFound:    true,
			wantComplete: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			found, complete, stop := scanV5PublishAlias(tc.block)
			if found != tc.wantFound || complete != tc.wantComplete {
				t.Fatalf("scan(%x) = found %v, complete %v (stopped at %d); want found %v, complete %v -- the preceding value was not skipped by its own width",
					tc.block, found, complete, stop, tc.wantFound, tc.wantComplete)
			}
		})
	}
}

// A block of modelled, alias-free properties must come back not-found and
// COMPLETE -- that is the answer the caller acts on, and the one that must not
// be confused with "the walk gave up".
func TestV5AliasScanModelledAliasFreeBlockIsComplete(t *testing.T) {
	block := propBlock(
		[]byte{propPayloadFormatIndicator, 0x01},
		[]byte{propMessageExpiryInterval, 0x00, 0x00, 0x01, 0x2c},
		[]byte{propContentType, 0x00, 0x04, 'j', 's', 'o', 'n'},
		[]byte{propResponseTopic, 0x00, 0x03, 'a', '/', 'b'},
		[]byte{propCorrelationData, 0x00, 0x02, 0xde, 0xad},
		[]byte{propSubscriptionIdentifier, 0x81, 0x01},
		[]byte{propUserProperty, 0x00, 0x03, 's', 'r', 'c', 0x00, 0x07, 'a', 'n', 'd', 'r', 'o', 'i', 'd'},
	)

	found, complete, stop := scanV5PublishAlias(block)
	if found {
		t.Errorf("an alias-free block reported an alias (stopped at %d)", stop)
	}
	if !complete {
		t.Errorf("a wholly modelled block reported an incomplete walk at offset %d of %d", stop, len(block))
	}
	if stop != len(block) {
		t.Errorf("stop = %d, want the block end %d", stop, len(block))
	}
}

func TestV5AliasScanEmptyBlockIsComplete(t *testing.T) {
	found, complete, stop := scanV5PublishAlias(nil)
	if found || !complete || stop != 0 {
		t.Fatalf("scan(empty) = found %v, complete %v, stop %d; want false, true, 0", found, complete, stop)
	}
}

// The CR-04 shape, and the reason this function returns two booleans instead of
// an error: an id the walk does not model must yield not-found-and-INCOMPLETE
// with no error and no partial claim, because the CALLER must still inspect the
// frame. An error here would be a decision, and this walk does not get to make
// decisions.
func TestV5AliasScanUnmodelledIDIsIncompleteNotAnError(t *testing.T) {
	block := propBlock([]byte{propPayloadFormatIndicator, 0x01}, []byte{0x7f, 0x00}, aliasProp)

	found, complete, stop := scanV5PublishAlias(block)
	if found {
		t.Error("the walk claimed to have found an alias it could not have reached")
	}
	if complete {
		t.Error("the walk claimed completeness after meeting an id it does not model")
	}
	// The unmodelled id sits directly after the two-byte payload-format property.
	if stop != 2 {
		t.Errorf("stop = %d, want 2 (the offset of the unmodelled id byte)", stop)
	}
}

// A length prefix pointing past the end of the block is the truncated-value case.
// It must behave exactly like an unmodelled id -- incomplete, no error, and
// emphatically no slice out of range.
func TestV5AliasScanTruncatedValueIsIncompleteNotAnError(t *testing.T) {
	cases := []struct {
		name  string
		block []byte
	}{
		{"string length runs past the block end", []byte{propContentType, 0x00, 0x40, 'a', 'b'}},
		{"string length prefix itself is truncated", []byte{propResponseTopic, 0x00}},
		{"binary length runs past the block end", []byte{propCorrelationData, 0xff, 0xff, 0x01}},
		{"second string of a user property runs past the end", []byte{propUserProperty, 0x00, 0x01, 'k', 0x00, 0x09, 'v'}},
		{"fixed-width value is cut short", []byte{propMessageExpiryInterval, 0x00, 0x00}},
		{"one-byte value is missing entirely", []byte{propPayloadFormatIndicator}},
		{"subscription identifier varint never terminates", []byte{propSubscriptionIdentifier, 0x80}},
		{"the alias id is the last byte, value missing", []byte{propPayloadFormatIndicator, 0x01, propTopicAlias, 0x00}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			found, complete, stop := scanV5PublishAlias(tc.block)
			if complete {
				t.Fatalf("scan(%x) reported a complete walk over a truncated block", tc.block)
			}
			if found && tc.name != "the alias id is the last byte, value missing" {
				t.Fatalf("scan(%x) reported an alias it never reached", tc.block)
			}
			if stop < 0 || stop > len(tc.block) {
				t.Fatalf("stop = %d, outside the %d byte block", stop, len(tc.block))
			}
		})
	}
}

// parseV5PublishFrame must REPORT the walk's answers, and callers must read them
// off the parse result rather than re-deriving them from the frame.
func TestV5AliasScanReportedByParseV5PublishFrame(t *testing.T) {
	build := func(props []byte) []byte {
		varHeader := []byte{0x00, 0x03, 'a', 'b', 'c'}
		varHeader = append(varHeader, byte(len(props)))
		varHeader = append(varHeader, props...)
		body := append(append([]byte{}, varHeader...), []byte("hi")...)
		frame := []byte{0x30}
		frame = append(frame, encodeV5Varint(len(body))...)
		return append(frame, body...)
	}

	cases := []struct {
		name         string
		props        []byte
		wantFound    bool
		wantComplete bool
	}{
		{"alias before an unmodelled id", propBlock(aliasProp, []byte{0x7f, 0x00}), true, true},
		{"unmodelled id before the alias", propBlock([]byte{0x7f, 0x00}, aliasProp), false, false},
		{"modelled and alias-free", []byte{propPayloadFormatIndicator, 0x01}, false, true},
		{"no properties at all", nil, false, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			frame := build(tc.props)
			got, err := parseV5PublishFrame(frame)
			if err != nil {
				t.Fatalf("parseV5PublishFrame: %v", err)
			}
			if got.HasTopicAlias != tc.wantFound {
				t.Errorf("HasTopicAlias = %v, want %v", got.HasTopicAlias, tc.wantFound)
			}
			if got.AliasScanComplete != tc.wantComplete {
				t.Errorf("AliasScanComplete = %v, want %v", got.AliasScanComplete, tc.wantComplete)
			}
			// The walk must never disturb the framing it rides along with.
			if got.Topic != "abc" {
				t.Errorf("topic = %q, want %q", got.Topic, "abc")
			}
			if string(got.Payload) != "hi" {
				t.Errorf("payload = %q, want %q", got.Payload, "hi")
			}
			if got.PayloadOffset+len(got.Payload) != len(frame) {
				t.Errorf("payload offset %d + len %d != frame len %d", got.PayloadOffset, len(got.Payload), len(frame))
			}
		})
	}
}

func TestSpliceV5PublishPayloadIdentity(t *testing.T) {
	for _, tc := range []struct {
		name  string
		frame []byte
	}{
		{"codec-encoded QoS1", codecPublishFrame(t, func(p *v5.Publish) {
			p.QoS = 1
			p.PacketID = 0x1234
			expiry := uint32(300)
			p.Properties = &v5.Properties{
				MessageExpiry: &expiry,
				User:          []v5.User{{Key: "src", Value: "android"}},
			}
		})},
		{"unmodelled property", mustHex(t, unmodelledPropertyPublish)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, err := parseV5PublishFrame(tc.frame)
			if err != nil {
				t.Fatalf("parseV5PublishFrame: %v", err)
			}

			out, err := spliceV5PublishPayload(tc.frame, p, p.Payload)
			if err != nil {
				t.Fatalf("spliceV5PublishPayload: %v", err)
			}
			if !bytes.Equal(out, tc.frame) {
				t.Fatalf("splicing the original payload back changed the frame:\n in  %x\n out %x", tc.frame, out)
			}
		})
	}
}

// A codec round trip cannot represent the properties it refused to parse.
// Copying the byte range can, which is why the splicer beats a re-encode here.
func TestSpliceV5PublishPayloadPreservesProperties(t *testing.T) {
	frame := mustHex(t, unmodelledPropertyPublish)
	p, err := parseV5PublishFrame(frame)
	if err != nil {
		t.Fatalf("parseV5PublishFrame: %v", err)
	}
	varHeader := frame[p.VarHeaderOffset:p.PayloadOffset]

	cases := []struct {
		name        string
		payload     []byte
		wantRemLen  []byte
		description string
	}{
		{
			name:       "shorter payload",
			payload:    []byte("x"),
			wantRemLen: []byte{0x09}, // 8 variable-header bytes + 1
		},
		{
			name:       "longer payload crossing the 127-byte varint boundary",
			payload:    bytes.Repeat([]byte{0xab}, 120),
			wantRemLen: []byte{0x80, 0x01}, // 8 + 120 == 128
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := spliceV5PublishPayload(frame, p, tc.payload)
			if err != nil {
				t.Fatalf("spliceV5PublishPayload: %v", err)
			}

			if out[0] != frame[0] {
				t.Errorf("fixed-header byte = %#02x, want %#02x", out[0], frame[0])
			}
			gotRemLen := out[1 : 1+len(tc.wantRemLen)]
			if !bytes.Equal(gotRemLen, tc.wantRemLen) {
				t.Errorf("remaining length varint = %x, want %x", gotRemLen, tc.wantRemLen)
			}

			start := 1 + len(tc.wantRemLen)
			gotVarHeader := out[start : start+len(varHeader)]
			if !bytes.Equal(gotVarHeader, varHeader) {
				t.Errorf("variable header changed:\n in  %x\n out %x", varHeader, gotVarHeader)
			}
			// The unmodelled property id itself, verbatim.
			if !bytes.Contains(out, []byte{0x7f, 0x00}) {
				t.Errorf("the unmodelled property bytes were lost: %x", out)
			}

			reparsed, err := parseV5PublishFrame(out)
			if err != nil {
				t.Fatalf("the spliced frame does not parse: %v", err)
			}
			if reparsed.Topic != p.Topic {
				t.Errorf("topic drifted: %q -> %q", p.Topic, reparsed.Topic)
			}
			if !bytes.Equal(reparsed.Payload, tc.payload) {
				t.Errorf("payload = %x, want %x", reparsed.Payload, tc.payload)
			}
		})
	}
}

func TestSpliceV5PublishPayloadOversizeRejected(t *testing.T) {
	frame := mustHex(t, unmodelledPropertyPublish)
	p, err := parseV5PublishFrame(frame)
	if err != nil {
		t.Fatalf("parseV5PublishFrame: %v", err)
	}

	out, err := spliceV5PublishPayload(frame, p, bytes.Repeat([]byte{0x00}, maxV5PacketBytes))
	if err == nil {
		t.Fatalf("a spliced frame above the packet cap was accepted (%d bytes)", len(out))
	}
	if out != nil {
		t.Fatalf("bytes were returned alongside the error: %d", len(out))
	}
}

// ---------------------------------------------------------------------------
// The uplink path: what the codec refused is still inspected, what nothing can
// read is refused.
// ---------------------------------------------------------------------------

// unmodelledPropertyFrame wraps a real payload in a PUBLISH whose property block
// carries id 0x7f -- outside paho.golang's table, so v5.ReadPacket refuses the
// frame while mosquitto would accept and route it perfectly normally. That
// asymmetry IS CR-04, so every fixture built here asserts the codec really does
// refuse it; otherwise the test would silently exercise the parseable path.
func unmodelledPropertyFrame(t *testing.T, topic string, payload []byte) []byte {
	t.Helper()

	varHeader := []byte{byte(len(topic) >> 8), byte(len(topic))}
	varHeader = append(varHeader, []byte(topic)...)
	varHeader = append(varHeader, 0x02, 0x7f, 0x00) // property block: 2 bytes, id 0x7f

	body := append(append([]byte{}, varHeader...), payload...)

	frame := []byte{0x30} // PUBLISH, QoS 0
	frame = append(frame, encodeV5Varint(len(body))...)
	frame = append(frame, body...)

	if _, err := v5.ReadPacket(bytes.NewReader(frame)); err == nil {
		t.Fatal("the fixture parses cleanly; it cannot exercise the CR-04 path")
	}
	return frame
}

// variableHeader returns the bytes between the fixed header and the payload --
// topic, optional packet id and the WHOLE property block. Every one of them must
// survive a rewrite verbatim, which is the property a codec round trip cannot
// offer for a frame the codec refused to parse.
func variableHeader(t *testing.T, frame []byte) []byte {
	t.Helper()
	p, err := parseV5PublishFrame(frame)
	if err != nil {
		t.Fatalf("parseV5PublishFrame: %v", err)
	}
	return frame[p.VarHeaderOffset:p.PayloadOffset]
}

func marshalEnvelope(t *testing.T, env *meshtastic.ServiceEnvelope) []byte {
	t.Helper()
	raw, err := proto.Marshal(env)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	return raw
}

// PROBE-A inverted. The verifier published exactly this shape and watched an
// unclamped hop_limit=7 envelope reach the backend byte-identical while the same
// envelope in a parseable frame was clamped. An unmodelled property must buy
// nothing.
func TestV5UnmodelledPropertyPublishIsClamped(t *testing.T) {
	n, _ := v5PublishServer(t, "publisher")
	payload := marshalEnvelope(t, nodeInfoEnvelope(t, 7, 9))
	frame := unmodelledPropertyFrame(t, v5PubTopic, payload)

	var backend bytes.Buffer
	if !n.handleV5PublishUplink(writerConn{&backend}, v5PubAddr, frame) {
		t.Fatal("connection dropped for a packet the rules allow")
	}

	out := backend.Bytes()
	if bytes.Equal(out, frame) {
		t.Fatal("the captured frame was forwarded verbatim; the clamp never reached the wire (CR-04)")
	}

	// Locate the payload with the hand parser, never by assuming an offset --
	// the codec cannot read this frame, and neither may the assertion.
	view, err := parseV5PublishFrame(out)
	if err != nil {
		t.Fatalf("the forwarded frame does not parse: %v", err)
	}
	var env meshtastic.ServiceEnvelope
	if err := proto.Unmarshal(view.Payload, &env); err != nil {
		t.Fatalf("forwarded payload is not a ServiceEnvelope: %v", err)
	}

	if got := env.GetPacket().GetHopLimit(); got != 3 {
		t.Errorf("wire hop_limit = %d, want 3", got)
	}
	if got := env.GetPacket().GetHopStart(); got != 7 {
		t.Errorf("wire hop_start = %d, want 7 (HOP_MAX clamp)", got)
	}
	if env.GetPacket().GetDecoded().Bitfield == nil {
		t.Error("Data.bitfield lost in the remarshal; 2.8 radios would drop the packet")
	}

	if view.Topic != v5PubTopic {
		t.Errorf("forwarded topic = %q, want %q", view.Topic, v5PubTopic)
	}
	// Every unmodelled property byte survives, verbatim.
	if in, gotVH := variableHeader(t, frame), out[view.VarHeaderOffset:view.PayloadOffset]; !bytes.Equal(in, gotVH) {
		t.Errorf("variable header changed across the rewrite:\n in  %x\n out %x", in, gotVH)
	}
	if !bytes.Contains(out, []byte{0x02, 0x7f, 0x00}) {
		t.Errorf("the unmodelled property block is gone from the forwarded frame: %x", out)
	}
}

// A Block rule must fire on a frame the codec refused. An undecryptable
// encrypted payload trips BlockInvalidEncryption; before this fix the same
// envelope wrapped in an unmodelled property was relayed to the broker.
func TestV5UnmodelledPropertyPublishBlockRuleFires(t *testing.T) {
	n, logs := v5PublishServer(t, "publisher")
	payload := marshalEnvelope(t, &meshtastic.ServiceEnvelope{
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
		ChannelId: "dc.run",
	})
	frame := unmodelledPropertyFrame(t, v5PubTopic, payload)

	var backend bytes.Buffer
	if n.handleV5PublishUplink(writerConn{&backend}, v5PubAddr, frame) {
		t.Fatal("an undecryptable envelope in an unmodelled-property frame was allowed through")
	}
	if backend.Len() != 0 {
		t.Fatalf("%d bytes of a Blocked publish reached the broker", backend.Len())
	}
	if got := logs.String(); !strings.Contains(got, "BLOCK") {
		t.Fatalf("no BLOCK line was logged; got:\n%s", got)
	}
}

// A blank topic blinds every topic rule and every msh/... log line while the
// broker resolves the alias normally -- which is what makes it the dangerous
// case, and why the hand-parsed path Blocks it exactly as the parseable one does.
func TestV5UnmodelledPropertyPublishEmptyTopicBlocked(t *testing.T) {
	n, logs := v5PublishServer(t, "publisher")
	frame := unmodelledPropertyFrame(t, "", marshalEnvelope(t, nodeInfoEnvelope(t, 3, 3)))

	var backend bytes.Buffer
	if n.handleV5PublishUplink(writerConn{&backend}, v5PubAddr, frame) {
		t.Fatal("an empty-topic PUBLISH was allowed through")
	}
	if backend.Len() != 0 {
		t.Fatalf("%d bytes of an empty-topic publish reached the broker", backend.Len())
	}
	got := logs.String()
	if !strings.Contains(got, "action=BLOCK") || !strings.Contains(got, "reason=topic_alias_uplink") {
		t.Fatalf("missing the alias BLOCK log line; got:\n%s", got)
	}
}

// Inspecting a frame is not licence to churn it. A frame no rule mutated goes out
// as the bytes that came in, splicer untouched.
func TestV5UnmodelledPropertyPublishUnchangedIsByteIdentical(t *testing.T) {
	n, _ := v5PublishServer(t, "publisher")
	frame := unmodelledPropertyFrame(t, v5PubTopic, marshalEnvelope(t, nodeInfoEnvelope(t, 3, 3)))

	var backend bytes.Buffer
	if !n.handleV5PublishUplink(writerConn{&backend}, v5PubAddr, frame) {
		t.Fatal("connection dropped for a packet the rules allow")
	}
	if !bytes.Equal(backend.Bytes(), frame) {
		t.Fatalf("forwarded bytes drifted:\n in  %x\n out %x", frame, backend.Bytes())
	}
}

// Fail CLOSED. A PUBLISH whose own length prefixes contradict its bytes is one
// mosquitto would refuse too, and the 3.1.1 loop ends the connection on any
// packet its codec cannot read.
func TestV5MalformedPublishFailsClosed(t *testing.T) {
	// 30 06 | 0003 "abc" | 80  -- a property-length varint that never terminates
	frame := mustHex(t, "3006"+"0003616263"+"80")
	if _, err := v5.ReadPacket(bytes.NewReader(frame)); err == nil {
		t.Fatal("the fixture parses cleanly; it cannot exercise the hand-parse failure path")
	}
	if _, err := parseV5PublishFrame(frame); err == nil {
		t.Fatal("the fixture hand-parses cleanly; it cannot exercise the fail-closed path")
	}

	n, logs := v5PublishServer(t, "publisher")
	var backend bytes.Buffer
	if n.handleV5PublishUplink(writerConn{&backend}, v5PubAddr, frame) {
		t.Fatal("an unreadable PUBLISH was relayed instead of refused")
	}
	if backend.Len() != 0 {
		t.Fatalf("%d bytes of an unreadable publish reached the broker", backend.Len())
	}
	got := logs.String()
	if !strings.Contains(got, "action=MQTT5_PUBLISH_HEADER_FAIL") {
		t.Fatalf("missing the header-fail log line; got:\n%s", got)
	}
	if !strings.Contains(got, "BLOCK") {
		t.Fatalf("no BLOCK line was logged for a refused frame; got:\n%s", got)
	}
}

// The parseable path is the one 68-02 shipped and the one the live fleet uses;
// the new fallback must not have moved it. Codec-encoded frames still go through
// the codec on the way out (re-encode on rewrite, captured bytes otherwise).
func TestV5ParseablePublishPathUnchanged(t *testing.T) {
	t.Run("clamped and re-encoded by the codec", func(t *testing.T) {
		n, _ := v5PublishServer(t, "publisher")
		frame := v5PublishFrame(t, nodeInfoEnvelope(t, 7, 9))

		var backend bytes.Buffer
		if !n.handleV5PublishUplink(writerConn{&backend}, v5PubAddr, frame) {
			t.Fatal("connection dropped for a packet the rules allow")
		}
		out := backend.Bytes()
		if bytes.Equal(out, frame) {
			t.Fatal("the clamp never reached the wire on the parseable path")
		}

		p, env := wireEnvelope(t, out)
		if got := env.GetPacket().GetHopLimit(); got != 3 {
			t.Errorf("wire hop_limit = %d, want 3", got)
		}
		if got := env.GetPacket().GetHopStart(); got != 7 {
			t.Errorf("wire hop_start = %d, want 7", got)
		}
		if out[0] != 0x32 {
			t.Errorf("first wire byte = %#02x, want 0x32 (PUBLISH, QoS1)", out[0])
		}
		if p.PacketID != 0x1234 {
			t.Errorf("packet id = %#04x, want 0x1234", p.PacketID)
		}
		if p.Properties == nil || p.Properties.MessageExpiry == nil || *p.Properties.MessageExpiry != 300 {
			t.Error("MessageExpiry property did not survive the rewrite")
		}
	})

	t.Run("unmutated is byte-identical", func(t *testing.T) {
		n, _ := v5PublishServer(t, "publisher")
		frame := v5PublishFrame(t, nodeInfoEnvelope(t, 3, 3))

		var backend bytes.Buffer
		if !n.handleV5PublishUplink(writerConn{&backend}, v5PubAddr, frame) {
			t.Fatal("connection dropped for a packet the rules allow")
		}
		if !bytes.Equal(backend.Bytes(), frame) {
			t.Fatalf("forwarded bytes drifted:\n in  %x\n out %x", frame, backend.Bytes())
		}
	})

	t.Run("topic alias still blocked", func(t *testing.T) {
		n, logs := v5PublishServer(t, "publisher")
		frame := mustHex(t, "300b00000323000368656c6c6f")

		var backend bytes.Buffer
		if n.handleV5PublishUplink(writerConn{&backend}, v5PubAddr, frame) {
			t.Fatal("an aliased PUBLISH was allowed through")
		}
		if backend.Len() != 0 {
			t.Fatalf("%d bytes of an aliased PUBLISH reached the broker", backend.Len())
		}
		if got := logs.String(); !strings.Contains(got, "reason=topic_alias_uplink") {
			t.Fatalf("missing the alias BLOCK log line; got:\n%s", got)
		}
	})
}
