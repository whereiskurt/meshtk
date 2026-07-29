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
