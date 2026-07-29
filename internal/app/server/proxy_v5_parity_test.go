package server

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	v5 "github.com/eclipse/paho.golang/packets"
	"github.com/eclipse/paho.mqtt.golang/packets"
)

// This file drives the REAL handleProxyV5 read loop end to end -- a client
// socket, a real backend the proxy dials, and the frames a client actually
// sends -- and asserts on the RECORDED BYTE STREAMS on both sides. Nothing here
// asserts on struct state, for the same reason proxy_v5_publish_test.go does
// not: the failure mode this phase keeps rediscovering is code that updates a
// struct and never reaches the wire (meshtk#22).
//
// The three defects it pins:
//
//   - CR-02: the 3.1.1 loop refreshes the ConnTrack entry on EVERY packet type
//     (inspectRawPacket calls SetConnTrack in the PUBLISH, SUBSCRIBE, PINGREQ,
//     PINGRESP and default branches). The v5 loop only ever reached
//     SetConnTrack through inspectV5Publish, so a session on the normal
//     Meshtastic cadence (position ~15 min) lost its entry to the 180s reaper
//     while its keepalives sailed past untracked -- and the next publish was
//     Blocked with "Username required for MQTT".
//   - CR-03: only PUBLISH was special-cased, so a second CONNECT was forwarded
//     to mosquitto carrying the client's own plaintext credentials, and an AUTH
//     frame was relayed into an authenticated session.
//   - WR-04: a v5 SUBSCRIBE never reached PacketDecider and MQTT.Topics was
//     never recorded, so topic rules applied to 3.1.1 clients only.

const (
	v5ParityAddr       = "203.0.113.9:51000"
	v5ParityAttacker   = "attacker-username"
	v5ParityAttackerPw = "attacker-plaintext-password"
)

// syncBuf is a bytes.Buffer a test goroutine may poll while the proxy goroutine
// writes to it. Every wire assertion in this file reads through it.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) Bytes() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.b.Bytes()...)
}

// splitFrames returns every COMPLETE control packet in b, using the proxy's own
// framing reader. A trailing partial frame is simply absent, which is what makes
// this safe to poll while bytes are still arriving.
func splitFrames(b []byte) [][]byte {
	r := bufio.NewReader(bytes.NewReader(b))
	var out [][]byte
	for {
		f, _, err := readFrame(r)
		if err != nil {
			return out
		}
		out = append(out, f)
	}
}

// v5Session drives handleProxyV5 with a real dialled backend, recording every
// byte the proxy writes to the broker and every byte it writes to the client.
type v5Session struct {
	t           *testing.T
	n           *ServerCmd
	ln          net.Listener
	clientConn  net.Conn
	peer        net.Conn
	backend     *syncBuf
	client      *syncBuf
	done        chan struct{}
	backendDone chan struct{}
	clientDone  chan struct{}
}

// startV5Session brings a v5 connection up through the real loop and returns
// once the establishing CONNECT has been re-encoded onto the backend socket.
func startV5Session(t *testing.T, n *ServerCmd) *v5Session {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for fake backend: %v", err)
	}
	n.Config.Server.ProxyForwardAddress = ln.Addr().String()

	s := &v5Session{
		t:           t,
		n:           n,
		ln:          ln,
		backend:     &syncBuf{},
		client:      &syncBuf{},
		done:        make(chan struct{}),
		backendDone: make(chan struct{}),
		clientDone:  make(chan struct{}),
	}

	go func() {
		defer close(s.backendDone)
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		io.Copy(s.backend, c)
	}()

	s.clientConn, s.peer = net.Pipe()

	go func() {
		defer close(s.clientDone)
		io.Copy(s.client, s.peer)
	}()

	go func() {
		defer close(s.done)
		s.n.handleProxyV5(s.clientConn, bufio.NewReader(s.clientConn), v5ParityAddr)
	}()

	// The establishing CONNECT: valid credentials, so inspectV5Connect swaps in
	// the proxy identity and the loop is entered.
	cp, _ := mqttasticConnect(t)
	var connect bytes.Buffer
	if _, err := cp.WriteTo(&connect); err != nil {
		t.Fatalf("encode establishing CONNECT: %v", err)
	}
	s.send(connect.Bytes())
	s.waitBackendFrames(1)

	return s
}

func (s *v5Session) send(frame []byte) {
	s.t.Helper()
	if err := s.peer.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
		s.t.Fatalf("set write deadline: %v", err)
	}
	if _, err := s.peer.Write(frame); err != nil {
		s.t.Fatalf("write frame %x to the proxy: %v", frame, err)
	}
}

func waitFrames(t *testing.T, what string, buf *syncBuf, want int) [][]byte {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		frames := splitFrames(buf.Bytes())
		if len(frames) >= want {
			return frames
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d %s frames; got %d (%x)", want, what, len(frames), buf.Bytes())
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func (s *v5Session) waitBackendFrames(want int) [][]byte {
	s.t.Helper()
	return waitFrames(s.t, "backend", s.backend, want)
}

func (s *v5Session) waitClientFrames(want int) [][]byte {
	s.t.Helper()
	return waitFrames(s.t, "client", s.client, want)
}

// awaitReturn asserts the read loop tore the connection down on its own.
func (s *v5Session) awaitReturn() {
	s.t.Helper()
	select {
	case <-s.done:
	case <-time.After(5 * time.Second):
		s.t.Fatal("handleProxyV5 did not return; the frame was relayed instead of refused")
	}
}

// finish joins every goroutine, so the recorded buffers and the captured log can
// be read without racing the proxy.
func (s *v5Session) finish() {
	s.t.Helper()
	s.peer.Close()
	select {
	case <-s.done:
	case <-time.After(5 * time.Second):
		s.t.Fatal("handleProxyV5 did not return after the client socket closed")
	}
	s.clientConn.Close()
	<-s.clientDone
	s.ln.Close()
	<-s.backendDone
}

func (s *v5Session) connectTime() int64 {
	s.t.Helper()
	s.n.ConnMutex.RLock()
	defer s.n.ConnMutex.RUnlock()
	info, ok := s.n.ConnTrack[v5ParityAddr]
	if !ok {
		s.t.Fatal("no ConnTrack entry for the session")
	}
	return info.ConnectTime
}

func (s *v5Session) backdate(secondsAgo int64) {
	s.t.Helper()
	s.n.ConnMutex.Lock()
	defer s.n.ConnMutex.Unlock()
	info, ok := s.n.ConnTrack[v5ParityAddr]
	if !ok {
		s.t.Fatal("no ConnTrack entry to backdate")
	}
	info.ConnectTime = time.Now().Unix() - secondsAgo
}

// v5ParityServer is a ServerCmd with the real rule set and a greppable log.
func v5ParityServer(t *testing.T) (*ServerCmd, *bytes.Buffer) {
	t.Helper()
	n := newTestServerCmd(&mockAuthenticator{valid: true})
	logger, logs := captureLogger()
	n.Config.Log = logger
	n.InspectorLogger = logger
	n.LoadInspectorRules()
	return n, logs
}

func encodePacket(t *testing.T, cp *v5.ControlPacket) []byte {
	t.Helper()
	var buf bytes.Buffer
	if _, err := cp.WriteTo(&buf); err != nil {
		t.Fatalf("encode packet: %v", err)
	}
	return buf.Bytes()
}

// attackerConnect is a second CONNECT on an ESTABLISHED session, carrying
// credentials that are not the ones the session authenticated with. Relaying it
// hands mosquitto the client's own plaintext password -- the exact invariant the
// establishing CONNECT is re-encoded to protect.
func attackerConnect(t *testing.T) []byte {
	t.Helper()
	cp := v5.NewControlPacket(v5.CONNECT)
	c := cp.Content.(*v5.Connect)
	c.ProtocolName = "MQTT"
	c.ProtocolVersion = 5
	c.CleanStart = true
	c.KeepAlive = 60
	c.ClientID = "mqttastic-second-connect"
	c.UsernameFlag = true
	c.Username = v5ParityAttacker
	c.PasswordFlag = true
	c.Password = []byte(v5ParityAttackerPw)
	return encodePacket(t, cp)
}

// assertDisconnectReason finds the DISCONNECT the proxy wrote to the client and
// checks its reason byte. The reason code is the client-visible half of the
// refusal: a second CONNACK is illegal on an established session, so DISCONNECT
// is the spec-correct answer to a mid-session protocol violation.
func assertDisconnectReason(t *testing.T, frames [][]byte, want byte) {
	t.Helper()
	for _, f := range frames {
		if f[0]>>4 != v5.DISCONNECT {
			continue
		}
		if len(f) < 3 {
			t.Fatalf("DISCONNECT %x carries no reason code", f)
		}
		if f[2] != want {
			t.Fatalf("DISCONNECT reason = %#02x, want %#02x", f[2], want)
		}
		return
	}
	t.Fatalf("no DISCONNECT among the frames sent to the client: %s", hex.EncodeToString(bytes.Join(frames, nil)))
}

// assertRefused is the shared shape of every illegal-frame case: nothing beyond
// the establishing CONNECT reaches the broker, the client is told why, and the
// loop returns.
func assertRefused(t *testing.T, s *v5Session, logs *bytes.Buffer, establishing []byte) {
	t.Helper()

	clientFrames := s.waitClientFrames(1)
	assertDisconnectReason(t, clientFrames, v5.DisconnectProtocolError)
	s.awaitReturn()
	s.finish()

	backend := s.backend.Bytes()
	if !bytes.Equal(backend, establishing) {
		t.Fatalf("bytes reached the broker after the establishing CONNECT:\n got  %x\n want %x", backend, establishing)
	}
	if extra := backend[len(establishing):]; len(extra) != 0 {
		t.Fatalf("%d bytes of a refused frame were relayed: %x", len(extra), extra)
	}
	if out := logs.String(); !strings.Contains(out, "action=MQTT5_PROTOCOL_VIOLATION") {
		t.Fatalf("missing the protocol-violation log line; got:\n%s", out)
	}
}

// --- CR-02: every frame refreshes the tracker entry -------------------------

// The 3.1.1 loop refreshes ConnTrack on a PINGREQ (inspectRawPacket's
// PingreqPacket branch calls SetConnTrack). The v5 loop must do the same, or a
// keepalive is invisible to the reaper.
func TestV5PingreqRefreshesConnTrack(t *testing.T) {
	n, _ := v5ParityServer(t)
	s := startV5Session(t, n)

	s.backdate(1000)
	s.send(mustHex(t, "c000")) // PINGREQ
	s.waitBackendFrames(2)

	if age := time.Now().Unix() - s.connectTime(); age > 1 {
		t.Fatalf("ConnectTime is %ds old after a PINGREQ; the keepalive did not refresh the entry", age)
	}
	s.finish()
}

// PROBE-B inverted. The reaper deletes any entry with now-ConnectTime > 180,
// and the Meshtastic position cadence is ~15 minutes, so a v5 session that only
// keepalives between publishes was purged on a timer -- and its next publish was
// Blocked with "Username required for MQTT" because Track.Username came back
// empty.
func TestV5IdleSessionSurvivesReaperWindow(t *testing.T) {
	n, logs := v5ParityServer(t)
	s := startV5Session(t, n)

	s.backdate(200)
	s.send(mustHex(t, "c000")) // PINGREQ
	s.waitBackendFrames(2)

	// The reaper's OWN predicate, verbatim from SetupTracker.
	if now := time.Now().Unix(); now-s.connectTime() > 180 {
		t.Fatalf("the reaper would purge this entry (age %ds > 180) despite a keepalive", now-s.connectTime())
	}

	publish := v5PublishFrame(t, nodeInfoEnvelope(t, 3, 3))
	s.send(publish)
	frames := s.waitBackendFrames(3)
	if !bytes.Equal(frames[2], publish) {
		t.Fatalf("publish after the idle window drifted:\n got  %x\n want %x", frames[2], publish)
	}
	s.finish()

	out := logs.String()
	if strings.Contains(out, "Username required for MQTT") {
		t.Fatalf("the publish was judged with an empty username (CR-02); log:\n%s", out)
	}
	if !strings.Contains(out, "[proxy] ALLOW") || !strings.Contains(out, "user="+v5TestUsername) {
		t.Fatalf("the publish was not allowed with the tracked original username; log:\n%s", out)
	}
}

// --- CR-03: credential- and auth-bearing frames are refused -----------------

// A second CONNECT on an established session is a protocol violation AND a
// credential leak: the captured frame carries the client's own plaintext
// password, which is precisely why the establishing CONNECT is re-encoded
// rather than relayed.
func TestV5SecondConnectRefused(t *testing.T) {
	n, logs := v5ParityServer(t)
	s := startV5Session(t, n)

	establishing := s.backend.Bytes()
	s.send(attackerConnect(t))
	assertRefused(t, s, logs, establishing)

	backend := s.backend.Bytes()
	if bytes.Contains(backend, []byte(v5ParityAttackerPw)) {
		t.Fatal("the second CONNECT's password reached the broker socket")
	}
	if bytes.Contains(backend, []byte(v5ParityAttacker)) {
		t.Fatal("the second CONNECT's username reached the broker socket")
	}
}

// inspect_v5.go asserts, in a comment on the enhanced-auth refusal, that an AUTH
// packet must never be relayed into an authenticated session. Relaying it from
// the read loop broke exactly that invariant.
func TestV5AuthFrameRefused(t *testing.T) {
	n, logs := v5ParityServer(t)
	s := startV5Session(t, n)

	establishing := s.backend.Bytes()
	cp := v5.NewControlPacket(v5.AUTH)
	cp.Content.(*v5.Auth).ReasonCode = v5.AuthReauthenticate
	s.send(encodePacket(t, cp))

	assertRefused(t, s, logs, establishing)
}

// PINGRESP is server-to-client only. A client sending one is either broken or
// probing; either way it must not be handed to the broker as if the proxy were
// a server.
func TestV5ServerOnlyFrameRefused(t *testing.T) {
	n, logs := v5ParityServer(t)
	s := startV5Session(t, n)

	establishing := s.backend.Bytes()
	s.send(mustHex(t, "d000")) // PINGRESP from a client
	assertRefused(t, s, logs, establishing)
}

// --- the refusal set must stay small ----------------------------------------

// A stricter frame switch must not become a new way to tear down legitimate
// mqttastic sessions: everything outside the refusal set still relays, byte for
// byte.
func TestV5PingreqStillRelayedByteIdentical(t *testing.T) {
	n, _ := v5ParityServer(t)
	s := startV5Session(t, n)

	pingreq := mustHex(t, "c000")
	s.send(pingreq)
	frames := s.waitBackendFrames(2)
	if !bytes.Equal(frames[1], pingreq) {
		t.Fatalf("PINGREQ relayed as %x, want the captured %x", frames[1], pingreq)
	}
	s.finish()
}

// A client DISCONNECT is a normal end-of-session frame, not a violation. This
// fixture is also the zero-length form paho.golang cannot parse (it returns
// EOF), so it doubles as proof the refusal switch dispatches on the fixed-header
// type and never on a parse.
func TestV5DisconnectFrameRelayed(t *testing.T) {
	n, logs := v5ParityServer(t)
	s := startV5Session(t, n)

	disconnect := mustHex(t, "e000")
	s.send(disconnect)
	frames := s.waitBackendFrames(2)
	if !bytes.Equal(frames[1], disconnect) {
		t.Fatalf("DISCONNECT relayed as %x, want the captured %x", frames[1], disconnect)
	}
	s.finish()

	if out := logs.String(); strings.Contains(out, "action=MQTT5_PROTOCOL_VIOLATION") {
		t.Fatalf("a client DISCONNECT was treated as a protocol violation; log:\n%s", out)
	}
}

// --- WR-04: v5 SUBSCRIBE is inspected, judged and codec-independent ---------

const (
	v5SubTopicA = "msh/US/2/e/dc.run/#"
	v5SubTopicB = "msh/US/2/e/PKI/#"
)

// v5SubscribePacket is the two-filter SUBSCRIBE a Meshtastic client sends after
// CONNECT: the channel tree and the PKI tree.
func v5SubscribePacket(t *testing.T) *v5.ControlPacket {
	t.Helper()
	cp := v5.NewControlPacket(v5.SUBSCRIBE)
	s := cp.Content.(*v5.Subscribe)
	s.PacketID = 0x0015
	s.Subscriptions = []v5.SubOptions{
		{Topic: v5SubTopicA, QoS: 0},
		{Topic: v5SubTopicB, QoS: 1},
	}
	return cp
}

// matchingRuleName reproduces RuleBasedDecider.Decide's selection so a test can
// name the rule that produced a decision. DecisionResult carries only the
// Decision and the Reason, and "the same Decision" is not the assertion this
// phase needs: LoadInspectorRules puts AllowMQTTControl first among the inspect
// rules, so the decider short-circuits before RequireMQTTUserName is consulted
// and a "not Blocked" check would pass whether or not SetConnTrack ran.
func matchingRuleName(t *testing.T, d Decider, ip *InspectorPacket) string {
	t.Helper()
	rbd, ok := d.(*RuleBasedDecider)
	if !ok {
		t.Fatalf("decider is %T, want *RuleBasedDecider", d)
	}
	for _, rule := range rbd.Rules {
		if rule.Matcher(ip) && rule.Action != Rewrote {
			return rule.Name
		}
	}
	return ""
}

func allowMQTTControlRule(t *testing.T) Rule {
	t.Helper()
	for _, r := range inspectRules() {
		if r.Name == "AllowMQTTControl" {
			return r
		}
	}
	t.Fatal("AllowMQTTControl rule not found")
	return Rule{}
}

func v5ControlPacketIP(t *testing.T, typ byte) *InspectorPacket {
	t.Helper()
	logger, _ := captureLogger()
	return &InspectorPacket{
		Log:   logger,
		Track: &ConnectionInfo{SocketAddress: v5ParityAddr, ProtocolVersion: 5},
		Raw:   &RawPacket{MQTT5: v5.NewControlPacket(typ)},
	}
}

// The 3.1.1 SubscribePacket branch records MQTT.Type and MQTT.Topics so topic
// rules have something to match on. Without the v5 mirror, topic rules applied
// to 3.1.1 clients only.
func TestV5SubscribeReachesDecider(t *testing.T) {
	n, _ := v5ParityServer(t)
	n.ConnTrack[v5ParityAddr] = &ConnectionInfo{
		SocketAddress: v5ParityAddr, Username: v5TestUsername, ProtocolVersion: 5,
	}

	ip := n.inspectV5Subscribe(v5ParityAddr, v5SubscribePacket(t))

	if ip.MQTT.Type != "SUBSCRIBE" {
		t.Errorf("MQTT.Type = %q, want SUBSCRIBE", ip.MQTT.Type)
	}
	if len(ip.MQTT.Topics) != 2 || ip.MQTT.Topics[0] != v5SubTopicA || ip.MQTT.Topics[1] != v5SubTopicB {
		t.Fatalf("MQTT.Topics = %v, want [%s %s] in order", ip.MQTT.Topics, v5SubTopicA, v5SubTopicB)
	}
	if ip.Raw.MQTT5 == nil {
		t.Error("Raw.MQTT5 not populated; the rules engine cannot see the packet")
	}
}

// SC3 in one assertion: the same rule, by NAME, judges a SUBSCRIBE on both
// codecs. Asserting only that neither is Blocked would be vacuous.
func TestV5SubscribeMatchesSameRuleAsV4(t *testing.T) {
	n, _ := v5ParityServer(t)
	n.ConnTrack[v5ParityAddr] = &ConnectionInfo{
		SocketAddress: v5ParityAddr, Username: v5TestUsername, ProtocolVersion: 5,
	}

	clientConn, peer := net.Pipe()
	defer clientConn.Close()
	defer peer.Close()

	v4Sub := packets.NewControlPacket(packets.Subscribe).(*packets.SubscribePacket)
	v4Sub.MessageID = 0x0015
	v4Sub.Topics = []string{v5SubTopicA, v5SubTopicB}
	v4Sub.Qoss = []byte{0, 1}
	var raw packets.ControlPacket = v4Sub
	logger, _ := captureLogger()
	v4IP := &InspectorPacket{
		Log:   logger,
		Track: &ConnectionInfo{SocketAddress: v5ParityAddr},
		Raw:   &RawPacket{MQTT: &raw},
	}
	v4IP.inspectRawPacket(n, clientConn)

	v5IP := n.inspectV5Subscribe(v5ParityAddr, v5SubscribePacket(t))

	v4Result := n.PacketDecider.Decide(v4IP)
	v5Result := n.PacketDecider.Decide(v5IP)
	if v4Result.Decision != v5Result.Decision {
		t.Fatalf("decision differs by codec: v4 %v, v5 %v", v4Result.Decision, v5Result.Decision)
	}

	v4Rule := matchingRuleName(t, n.PacketDecider, v4IP)
	v5Rule := matchingRuleName(t, n.PacketDecider, v5IP)
	if v4Rule != v5Rule {
		t.Fatalf("a SUBSCRIBE matched %q on 3.1.1 but %q on v5; the decider is not codec-independent", v4Rule, v5Rule)
	}
	if v5Rule != "AllowMQTTControl" {
		t.Fatalf("matching rule = %q, want AllowMQTTControl", v5Rule)
	}
	if v4IP.MQTT.Type != v5IP.MQTT.Type {
		t.Errorf("MQTT.Type differs by codec: v4 %q, v5 %q", v4IP.MQTT.Type, v5IP.MQTT.Type)
	}
}

// SetConnTrack is the directly observable half of inspectV5Subscribe: the
// CONNECT forwarded to the broker carries the swapped proxy identity, so without
// it the tracked ORIGINAL username -- the one every rule and the ALLOW log line
// key off -- is missing.
func TestV5SubscribeCarriesTrackedUsername(t *testing.T) {
	n, _ := v5ParityServer(t)

	clientConn, peer := net.Pipe()
	defer clientConn.Close()
	defer peer.Close()

	_, c := mqttasticConnect(t)
	if !n.inspectV5Connect(clientConn, v5ParityAddr, c) {
		t.Fatal("valid credentials rejected")
	}
	if c.Username != "proxy" {
		t.Fatalf("credential swap did not happen: %q", c.Username)
	}

	ip := n.inspectV5Subscribe(v5ParityAddr, v5SubscribePacket(t))
	if ip.Track.Username != v5TestUsername {
		t.Fatalf("Track.Username = %q, want the original client username %q", ip.Track.Username, v5TestUsername)
	}
}

// An allowed SUBSCRIBE goes out as the CAPTURED frame. The parse is read-only:
// re-encoding would risk the same subscription-identifier round-trip hazard that
// keeps the downlink path from re-encoding.
func TestV5SubscribeRelayedByteIdentical(t *testing.T) {
	n, _ := v5ParityServer(t)
	s := startV5Session(t, n)

	subscribe := encodePacket(t, v5SubscribePacket(t))
	s.send(subscribe)
	frames := s.waitBackendFrames(2)
	if !bytes.Equal(frames[1], subscribe) {
		t.Fatalf("SUBSCRIBE relayed as %x, want the captured frame %x", frames[1], subscribe)
	}
	s.finish()
}

// paho.golang's Properties.Unpack hard-errors on any property id outside its
// table. A SUBSCRIBE carries no credentials and no topic Block rule exists, so
// relaying it loudly beats tearing down a live session -- accepted risk
// T-68-06-05.
func TestV5SubscribeParseFailureRelaysRaw(t *testing.T) {
	// 82 09 | 1234 (packet id) | 02 7f00 (property id 0x7f is not modelled)
	//       | 0001 "a" 00 (one topic filter, QoS 0)
	const unparseable = "82091234027f0000016100"

	n, logs := v5ParityServer(t)
	frame := mustHex(t, unparseable)
	if _, err := v5.ReadPacket(bytes.NewReader(frame)); err == nil {
		t.Fatal("the fixture parses cleanly; it cannot exercise the parse-failure path")
	}

	s := startV5Session(t, n)
	s.send(frame)
	pingreq := mustHex(t, "c000")
	s.send(pingreq)

	frames := s.waitBackendFrames(3)
	if !bytes.Equal(frames[1], frame) {
		t.Fatalf("unparseable SUBSCRIBE relayed as %x, want the captured %x", frames[1], frame)
	}
	// The PINGREQ arriving proves the connection survived the parse failure.
	if !bytes.Equal(frames[2], pingreq) {
		t.Fatal("the connection did not survive an unparseable SUBSCRIBE")
	}
	s.finish()

	out := logs.String()
	if !strings.Contains(out, "action=MQTT5_PARSE_FAIL") || !strings.Contains(out, "mqtt_type=SUBSCRIBE") {
		t.Fatalf("missing the SUBSCRIBE parse-failure log line; got:\n%s", out)
	}
}

// The control-packet allowlist is the FIRST inspect rule, so if it answers
// differently per codec every rule below it is reached differently per codec.
func TestAllowMQTTControlV5(t *testing.T) {
	rule := allowMQTTControlRule(t)

	for _, tc := range []struct {
		desc string
		typ  byte
		want bool
	}{
		{"SUBSCRIBE", v5.SUBSCRIBE, true},
		{"PUBACK", v5.PUBACK, true},
		{"PINGREQ", v5.PINGREQ, true},
		{"UNSUBSCRIBE", v5.UNSUBSCRIBE, true},
		{"DISCONNECT", v5.DISCONNECT, true},
		{"PUBLISH", v5.PUBLISH, false},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			if got := rule.Matcher(v5ControlPacketIP(t, tc.typ)); got != tc.want {
				t.Fatalf("AllowMQTTControl(v5 %s) = %v, want %v", tc.desc, got, tc.want)
			}
		})
	}
}

// The 3.1.1 branch is reached first and is unedited. The v4 golden pins the
// decision sequence for a whole session; this pins the matcher itself.
func TestAllowMQTTControlV4Unchanged(t *testing.T) {
	rule := allowMQTTControlRule(t)
	logger, _ := captureLogger()

	for _, tc := range []struct {
		desc   string
		packet packets.ControlPacket
		want   bool
	}{
		{"CONNECT", packets.NewControlPacket(packets.Connect), true},
		{"SUBSCRIBE", packets.NewControlPacket(packets.Subscribe), true},
		{"PUBLISH", packets.NewControlPacket(packets.Publish), false},
		{"PINGREQ", packets.NewControlPacket(packets.Pingreq), true},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			raw := tc.packet
			ip := &InspectorPacket{
				Log:   logger,
				Track: &ConnectionInfo{SocketAddress: v5ParityAddr},
				Raw:   &RawPacket{MQTT: &raw},
			}
			if got := rule.Matcher(ip); got != tc.want {
				t.Fatalf("AllowMQTTControl(3.1.1 %s) = %v, want %v", tc.desc, got, tc.want)
			}
		})
	}
}
