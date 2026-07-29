package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

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

// --- v5 CONNECT authentication, credential swap and alias suppression -------

const (
	v5TestClientID = "mqttastic-android-test"
	v5TestUsername = "ed270dbe5d1e"
	v5TestPassword = "hunter2"
)

func v5TestServer(t *testing.T, auth *mockAuthenticator) *ServerCmd {
	t.Helper()
	n := newTestServerCmd(auth)
	logger := log.New()
	logger.SetLevel(log.PanicLevel)
	n.Config.Log = logger
	n.InspectorLogger = logger
	return n
}

// mqttasticConnect builds a CONNECT shaped like the one Meshtastic-Android
// 2.8.0's mqttastic client sends: several v5 properties including a topic-alias
// budget, plus a User property. Everything except TopicAliasMaximum must
// survive the proxy untouched.
func mqttasticConnect(t *testing.T) (*v5.ControlPacket, *v5.Connect) {
	t.Helper()
	cp := v5.NewControlPacket(v5.CONNECT)
	c := cp.Content.(*v5.Connect)

	c.ProtocolName = "MQTT"
	c.ProtocolVersion = 5
	c.CleanStart = true
	c.KeepAlive = 60
	c.ClientID = v5TestClientID
	c.UsernameFlag = true
	c.Username = v5TestUsername
	c.PasswordFlag = true
	c.Password = []byte(v5TestPassword)

	sessionExpiry := uint32(10000)
	receiveMax := uint16(20)
	topicAliasMax := uint16(10)
	maxPacketSize := uint32(1048576)
	c.Properties = &v5.Properties{
		SessionExpiryInterval: &sessionExpiry,
		ReceiveMaximum:        &receiveMax,
		TopicAliasMaximum:     &topicAliasMax,
		MaximumPacketSize:     &maxPacketSize,
		User:                  []v5.User{{Key: "client", Value: "mqttastic"}},
	}
	return cp, c
}

func reparseConnect(t *testing.T, cp *v5.ControlPacket) ([]byte, *v5.Connect) {
	t.Helper()
	var out bytes.Buffer
	if _, err := cp.WriteTo(&out); err != nil {
		t.Fatalf("encode CONNECT: %v", err)
	}
	round, err := v5.ReadPacket(bytes.NewReader(out.Bytes()))
	if err != nil {
		t.Fatalf("re-parse CONNECT: %v", err)
	}
	c, ok := round.Content.(*v5.Connect)
	if !ok {
		t.Fatalf("re-parsed as %T, want *v5.Connect", round.Content)
	}
	return out.Bytes(), c
}

func TestV5ConnectCredSwapPreservesProperties(t *testing.T) {
	mock := &mockAuthenticator{valid: true}
	n := v5TestServer(t, mock)

	clientConn, peer := net.Pipe()
	defer clientConn.Close()
	defer peer.Close()

	cp, c := mqttasticConnect(t)
	if !n.inspectV5Connect(clientConn, "203.0.113.7:50000", c) {
		t.Fatal("valid credentials rejected")
	}

	forwarded, got := reparseConnect(t, cp)

	if got.Username != "proxy" {
		t.Errorf("forwarded username = %q, want proxy", got.Username)
	}
	if string(got.Password) != "proxypass" {
		t.Errorf("forwarded password = %q, want proxypass", got.Password)
	}
	if got.ClientID != v5TestClientID {
		t.Errorf("clientID = %q, want %q", got.ClientID, v5TestClientID)
	}
	if got.Properties.TopicAliasMaximum != nil {
		t.Errorf("TopicAliasMaximum = %d, want absent", *got.Properties.TopicAliasMaximum)
	}

	// Everything else the client negotiated must survive verbatim.
	if got.Properties.SessionExpiryInterval == nil || *got.Properties.SessionExpiryInterval != 10000 {
		t.Error("SessionExpiryInterval did not survive the swap")
	}
	if got.Properties.ReceiveMaximum == nil || *got.Properties.ReceiveMaximum != 20 {
		t.Error("ReceiveMaximum did not survive the swap")
	}
	if got.Properties.MaximumPacketSize == nil || *got.Properties.MaximumPacketSize != 1048576 {
		t.Error("MaximumPacketSize did not survive the swap")
	}
	if len(got.Properties.User) != 1 || got.Properties.User[0].Key != "client" || got.Properties.User[0].Value != "mqttastic" {
		t.Errorf("User properties did not survive the swap: %+v", got.Properties.User)
	}

	// The whole point of the swap: mosquitto must never see client creds.
	if bytes.Contains(forwarded, []byte(v5TestPassword)) {
		t.Fatal("the client password appears in the bytes forwarded to the broker")
	}
	if bytes.Contains(forwarded, []byte(v5TestUsername)) {
		t.Fatal("the client username appears in the bytes forwarded to the broker")
	}
}

// Parse -> re-encode with no edits must be byte-identical, or every "the proxy
// only changed what it meant to change" claim rests on nothing.
func TestV5ConnectNoMutationIsByteIdentical(t *testing.T) {
	cp, _ := mqttasticConnect(t)
	var in bytes.Buffer
	if _, err := cp.WriteTo(&in); err != nil {
		t.Fatalf("encode: %v", err)
	}

	round, err := v5.ReadPacket(bytes.NewReader(in.Bytes()))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var out bytes.Buffer
	if _, err := round.WriteTo(&out); err != nil {
		t.Fatalf("re-encode: %v", err)
	}

	if !bytes.Equal(in.Bytes(), out.Bytes()) {
		t.Fatalf("re-encode drifted:\n in  %x\n out %x", in.Bytes(), out.Bytes())
	}
}

func TestV5ConnackReasonCodes(t *testing.T) {
	cases := []struct {
		desc   string
		reason byte
		want   string
	}{
		{"unsupported protocol version", v5.ConnackUnsupportedProtocolVersion, "2003008400"},
		{"not authorized", v5.ConnackNotAuthorized, "2003008700"},
		{"bad authentication method", v5.ConnackBadAuthenticationMethod, "2003008c00"},
	}

	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			var buf bytes.Buffer
			if err := writeMqtt5Connack(writerConn{&buf}, tc.reason); err != nil {
				t.Fatalf("writeMqtt5Connack: %v", err)
			}
			if got := hex.EncodeToString(buf.Bytes()); got != tc.want {
				t.Fatalf("wire bytes = %s, want %s", got, tc.want)
			}
		})
	}
}

// connackFrom drains one CONNACK the inspector wrote to the client side of a
// pipe. inspectV5Connect writes before returning, so the read has to be
// concurrent with the call.
//
// io.ReadFull, not a single Read: net.Pipe is unbuffered and ControlPacket
// .WriteTo emits the packet as several Writes, so a lone Read consumes the
// first chunk and the writer blocks forever on the next one.
func connackFrom(t *testing.T, run func(net.Conn) bool) (allow bool, wire string) {
	t.Helper()
	clientConn, peer := net.Pipe()
	defer clientConn.Close()
	defer peer.Close()

	done := make(chan bool, 1)
	go func() {
		done <- run(clientConn)
	}()

	// A bare CONNACK is exactly 5 bytes: 20 03 <flags> <reason> <props-len>.
	buf := make([]byte, 5)
	if err := peer.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	if _, err := io.ReadFull(peer, buf); err != nil {
		t.Fatalf("read CONNACK: %v", err)
	}

	return <-done, hex.EncodeToString(buf)
}

func TestV5EnhancedAuthRejected(t *testing.T) {
	mock := &mockAuthenticator{valid: true}
	n := v5TestServer(t, mock)

	_, c := mqttasticConnect(t)
	c.Properties.AuthMethod = "SCRAM-SHA-1"

	allow, wire := connackFrom(t, func(conn net.Conn) bool {
		return n.inspectV5Connect(conn, "203.0.113.7:50000", c)
	})

	if allow {
		t.Fatal("enhanced-auth CONNECT allowed through")
	}
	if wire != "2003008c00" {
		t.Fatalf("CONNACK = %s, want 2003008c00 (bad authentication method)", wire)
	}
	if mock.callCount != 0 {
		t.Fatalf("Authenticator.Verify called %d times; enhanced auth must be refused before any lookup or dial", mock.callCount)
	}
}

func TestV5ConnectEmptyUsernameRejected(t *testing.T) {
	mock := &mockAuthenticator{valid: true}
	n := v5TestServer(t, mock)

	_, c := mqttasticConnect(t)
	c.Username = ""
	c.UsernameFlag = false

	allow, wire := connackFrom(t, func(conn net.Conn) bool {
		return n.inspectV5Connect(conn, "203.0.113.7:50000", c)
	})

	if allow {
		t.Fatal("empty-username CONNECT allowed through")
	}
	if wire != "2003008700" {
		t.Fatalf("CONNACK = %s, want 2003008700 (not authorized)", wire)
	}
	if mock.callCount != 0 {
		t.Fatalf("Authenticator.Verify called %d times for an empty username; must fail closed", mock.callCount)
	}
}

func TestV5ConnectInvalidCredsRejected(t *testing.T) {
	for _, tc := range []struct {
		desc string
		mock *mockAuthenticator
	}{
		{"invalid credentials", &mockAuthenticator{valid: false}},
		{"authenticator error", &mockAuthenticator{valid: false, err: fmt.Errorf("dynamodb timeout")}},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			n := v5TestServer(t, tc.mock)
			_, c := mqttasticConnect(t)

			allow, wire := connackFrom(t, func(conn net.Conn) bool {
				return n.inspectV5Connect(conn, "203.0.113.7:50000", c)
			})

			if allow {
				t.Fatal("bad credentials allowed through")
			}
			// 0x87, not 3.1.1's 0x05 (meaningless in v5) and not 0x84
			// (reserved for protocol levels above 5 -- answering it here is
			// what made mqttastic retry-loop).
			if wire != "2003008700" {
				t.Fatalf("CONNACK = %s, want 2003008700", wire)
			}
			if tc.mock.callCount != 1 {
				t.Fatalf("Verify called %d times, want 1", tc.mock.callCount)
			}
		})
	}
}

func TestV5ConnectPassthroughForwardsOriginalCreds(t *testing.T) {
	mock := &mockAuthenticator{valid: false}
	n := v5TestServer(t, mock)

	clientConn, peer := net.Pipe()
	defer clientConn.Close()
	defer peer.Close()

	cp, c := mqttasticConnect(t)
	c.Username = "ghosts" // seeded into CredCache.Passthrough by newTestServerCmd
	c.Password = []byte("ghostpass")

	if !n.inspectV5Connect(clientConn, "203.0.113.7:50000", c) {
		t.Fatal("passthrough username rejected")
	}
	if mock.callCount != 0 {
		t.Fatalf("Verify called %d times for a passthrough username, want 0", mock.callCount)
	}

	_, got := reparseConnect(t, cp)
	if got.Username != "ghosts" {
		t.Errorf("username = %q, want ghosts (passthrough forwards the original)", got.Username)
	}
	if string(got.Password) != "ghostpass" {
		t.Errorf("password = %q, want ghostpass", got.Password)
	}
	// Passthrough still may not alias: the rules engine keys off real topics.
	if got.Properties.TopicAliasMaximum != nil {
		t.Error("TopicAliasMaximum survived on the passthrough path")
	}
}

// mosquitto 2.0 advertises TopicAliasMaximum=10 by default. Left alone, the
// client may publish with an empty topic + a Topic Alias, blinding every
// topic-based rule while the broker resolves it and fans out normally.
//
// Driven through handleBackendV5 rather than a helper, so the CONNACK branch,
// the raw relay of everything else and the parse-failure fallback are all
// exercised on the real downlink loop.
func TestV5ConnackTopicAliasStripped(t *testing.T) {
	n := v5TestServer(t, &mockAuthenticator{valid: true})

	clientConn, peer := net.Pipe()
	defer clientConn.Close()
	defer peer.Close()

	var stream bytes.Buffer
	stream.Write(mustHex(t, "200900000622000a210014")) // real mosquitto 2.0.22 CONNACK
	stream.Write(mustHex(t, "200100"))                 // CONNACK the codec cannot parse
	stream.Write(mustHex(t, "d000"))                   // PINGRESP -- never parsed at all

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go n.handleBackendV5(ctx, clientConn, "203.0.113.7:50000",
		writerConn{&bytes.Buffer{}}, bufio.NewReader(bytes.NewReader(stream.Bytes())))

	if err := peer.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	read := func(t *testing.T, size int) string {
		t.Helper()
		buf := make([]byte, size)
		if _, err := io.ReadFull(peer, buf); err != nil {
			t.Fatalf("read downlink: %v", err)
		}
		return hex.EncodeToString(buf)
	}

	// TopicAliasMaximum (0x22 000a) gone; ReceiveMaximum (0x21 0014) intact.
	if got, want := read(t, 8), "2006000003210014"; got != want {
		t.Fatalf("rewritten CONNACK = %s, want %s", got, want)
	}
	// Unparseable CONNACK relays verbatim -- better than closing a connection
	// the broker just accepted.
	if got, want := read(t, 3), "200100"; got != want {
		t.Fatalf("unparseable CONNACK = %s, want it forwarded verbatim as %s", got, want)
	}
	if got, want := read(t, 2), "d000"; got != want {
		t.Fatalf("PINGRESP = %s, want %s relayed raw", got, want)
	}
}

func TestV5ConnectStampsProtocolVersion(t *testing.T) {
	const addr = "203.0.113.7:50000"
	mock := &mockAuthenticator{valid: true}
	n := v5TestServer(t, mock)

	clientConn, peer := net.Pipe()
	defer clientConn.Close()
	defer peer.Close()

	_, c := mqttasticConnect(t)
	if !n.inspectV5Connect(clientConn, addr, c) {
		t.Fatal("valid credentials rejected")
	}

	n.ConnMutex.RLock()
	info, ok := n.ConnTrack[addr]
	n.ConnMutex.RUnlock()
	if !ok {
		t.Fatal("no ConnTrack entry after CONNECT")
	}

	if info.ProtocolVersion != 5 {
		t.Errorf("ProtocolVersion = %d, want 5", info.ProtocolVersion)
	}
	// Tracked identity is the CLIENT's, not the swapped broker identity --
	// rules and logs key off who actually connected.
	if info.Username != v5TestUsername {
		t.Errorf("tracked username = %q, want the original %q", info.Username, v5TestUsername)
	}
	if info.ClientID != v5TestClientID {
		t.Errorf("tracked clientID = %q, want %q", info.ClientID, v5TestClientID)
	}
	// Password is stored hex-encoded, exactly as the 3.1.1 path does.
	if info.Password != fmt.Sprintf("%x", []byte(v5TestPassword)) {
		t.Errorf("tracked password = %q, want the hex encoding", info.Password)
	}
	if strings.Contains(info.Password, v5TestPassword) {
		t.Error("plaintext password stored in ConnTrack")
	}
}
