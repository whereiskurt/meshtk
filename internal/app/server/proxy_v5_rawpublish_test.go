package server

import (
	"bytes"
	"testing"

	v5 "github.com/eclipse/paho.golang/packets"
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
