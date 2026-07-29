package server

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"strings"
	"testing"

	v5 "github.com/eclipse/paho.golang/packets"
	log "github.com/sirupsen/logrus"
)

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("bad fixture hex %q: %v", s, err)
	}
	return b
}

// readFrame is the reason the v5 path can relay packets its codec cannot parse.
// Each of these is either unparseable by paho.golang (e000) or re-encoded into
// a different, longer form by it (40021234 -> 400412340000) — so "captured
// bytes out == captured bytes in" is the property that matters, not round-trip
// through the codec.
func TestReadFrameRoundTrip(t *testing.T) {
	longPublish := func() string {
		// A PUBLISH whose remaining length needs a 2-byte varint (>127), which
		// is the case a 1-byte-length reader would silently truncate.
		topic := "msh/US/2/e/dc.run/!435990e4"
		payload := bytes.Repeat([]byte{0xa5}, 200)
		var body bytes.Buffer
		body.WriteByte(byte(len(topic) >> 8))
		body.WriteByte(byte(len(topic)))
		body.WriteString(topic)
		body.WriteByte(0x00) // empty v5 properties
		body.Write(payload)

		var frame bytes.Buffer
		frame.WriteByte(0x30)
		remLen := body.Len()
		for {
			b := byte(remLen % 128)
			remLen /= 128
			if remLen > 0 {
				b |= 0x80
			}
			frame.WriteByte(b)
			if remLen == 0 {
				break
			}
		}
		frame.Write(body.Bytes())
		return hex.EncodeToString(frame.Bytes())
	}()

	cases := []struct {
		desc    string
		raw     string
		wantTyp byte
	}{
		{"zero-length DISCONNECT (paho.golang returns EOF for this)", "e000", v5.DISCONNECT},
		{"reason-only DISCONNECT", "e00100", v5.DISCONNECT},
		{"short PUBACK (re-encode would inflate it to 400412340000)", "40021234", v5.PUBACK},
		{"PINGREQ", "c000", v5.PINGREQ},
		{"PINGRESP", "d000", v5.PINGRESP},
		{"PUBLISH with a 2-byte remaining length", longPublish, v5.PUBLISH},
	}

	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			want := mustHex(t, tc.raw)
			r := bufio.NewReader(bytes.NewReader(want))

			got, typ, err := readFrame(r)
			if err != nil {
				t.Fatalf("readFrame: %v", err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("frame = %x, want %x", got, want)
			}
			if typ != tc.wantTyp {
				t.Fatalf("packet type = %d, want %d", typ, tc.wantTyp)
			}
		})
	}

	// Back-to-back frames off ONE bufio.Reader must split at the right
	// boundaries — an off-by-one here desynchronizes the whole connection.
	t.Run("back-to-back frames split correctly", func(t *testing.T) {
		var stream bytes.Buffer
		for _, tc := range cases {
			stream.Write(mustHex(t, tc.raw))
		}
		r := bufio.NewReader(bytes.NewReader(stream.Bytes()))

		for _, tc := range cases {
			got, typ, err := readFrame(r)
			if err != nil {
				t.Fatalf("%s: readFrame: %v", tc.desc, err)
			}
			if hex.EncodeToString(got) != tc.raw {
				t.Fatalf("%s: frame = %x, want %s", tc.desc, got, tc.raw)
			}
			if typ != tc.wantTyp {
				t.Fatalf("%s: type = %d, want %d", tc.desc, typ, tc.wantTyp)
			}
		}
		if _, _, err := readFrame(r); err == nil {
			t.Fatal("readFrame succeeded past the end of the stream")
		}
	})
}

// Five attacker-controlled bytes must not buy a 256 MiB allocation. The reader
// below carries the header ONLY: if readFrame allocated and then tried to fill
// the body it would fail with an io error instead of the size error, so this
// also proves the cap is checked BEFORE the make().
func TestReadFrameRejectsOversizePacket(t *testing.T) {
	var frame bytes.Buffer
	frame.WriteByte(0x30) // PUBLISH
	remLen := maxV5PacketBytes + 1
	for {
		b := byte(remLen % 128)
		remLen /= 128
		if remLen > 0 {
			b |= 0x80
		}
		frame.WriteByte(b)
		if remLen == 0 {
			break
		}
	}

	r := bufio.NewReader(bytes.NewReader(frame.Bytes()))
	raw, _, err := readFrame(r)
	if err == nil {
		t.Fatal("oversize packet accepted")
	}
	if !strings.Contains(err.Error(), "too large") {
		t.Fatalf("error = %v, want the size cap error (an io error means the body was read first)", err)
	}
	if raw != nil {
		t.Fatalf("frame bytes returned for a rejected packet: %x", raw)
	}
}

// A 5th continuation byte is a malformed remaining length, not a bigger number.
func TestReadFrameRejectsMalformedRemainingLength(t *testing.T) {
	r := bufio.NewReader(bytes.NewReader([]byte{0x30, 0xff, 0xff, 0xff, 0xff, 0x7f}))
	if _, _, err := readFrame(r); err == nil {
		t.Fatal("5-byte remaining length accepted")
	}
}

// writeMqtt5Connack must reproduce the hand-rolled literal in
// writeMqtt5UnsupportedConnack exactly, so the two encoders stay pinned to each
// other without touching the literal (which TestWriteMqtt5UnsupportedConnackWire
// already guards).
func TestWriteMqtt5ConnackMatchesUnsupportedLiteral(t *testing.T) {
	var codec, literal bytes.Buffer

	if err := writeMqtt5Connack(writerConn{&codec}, v5.ConnackUnsupportedProtocolVersion); err != nil {
		t.Fatalf("writeMqtt5Connack: %v", err)
	}
	if err := writeMqtt5UnsupportedConnack(writerConn{&literal}); err != nil {
		t.Fatalf("writeMqtt5UnsupportedConnack: %v", err)
	}

	if !bytes.Equal(codec.Bytes(), literal.Bytes()) {
		t.Fatalf("codec bytes %x != literal bytes %x", codec.Bytes(), literal.Bytes())
	}
	if want := mustHex(t, "2003008400"); !bytes.Equal(codec.Bytes(), want) {
		t.Fatalf("wire bytes = %x, want %x", codec.Bytes(), want)
	}
}

// The AllowMQTTControl matcher dereferences ip.Raw.MQTT unconditionally, and a
// v5 InspectorPacket carries Raw.MQTT5 with Raw.MQTT nil. Without the guard
// this panics — and a panic in the proxy read loop takes down the process, not
// the connection.
func TestAllowMQTTControlNilRawMQTT(t *testing.T) {
	var rule Rule
	for _, r := range inspectRules() {
		if r.Name == "AllowMQTTControl" {
			rule = r
			break
		}
	}
	if rule.Matcher == nil {
		t.Fatal("AllowMQTTControl rule not found")
	}

	logger := log.New()
	logger.SetLevel(log.PanicLevel)

	cp := v5.NewControlPacket(v5.PUBLISH)
	ip := &InspectorPacket{
		Log:   logger,
		Track: &ConnectionInfo{SocketAddress: "203.0.113.7:50000", ProtocolVersion: 5},
		Raw:   &RawPacket{MQTT5: cp},
	}

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("AllowMQTTControl panicked on a v5 packet: %v", r)
		}
	}()

	if rule.Matcher(ip) {
		t.Fatal("AllowMQTTControl matched a packet it cannot inspect")
	}
}
