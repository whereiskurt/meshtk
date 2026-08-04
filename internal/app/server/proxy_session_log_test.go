package server

import (
	"bytes"
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/eclipse/paho.mqtt.golang/packets"
)

// v4ConnectBytesKeepalive is v4ConnectBytes with the keepalive as a knob --
// the whole point of these tests is that the negotiated value reaches the log,
// so it cannot be hardcoded at 60 the way the recover harness fixes it.
func v4ConnectBytesKeepalive(t *testing.T, clientID string, keepalive uint16) []byte {
	t.Helper()
	cp := packets.NewControlPacket(packets.Connect).(*packets.ConnectPacket)
	cp.ProtocolName = "MQTT"
	cp.ProtocolVersion = 4
	cp.CleanSession = true
	cp.Keepalive = keepalive
	cp.ClientIdentifier = clientID
	cp.UsernameFlag = true
	cp.Username = "ed270dbe5d1e"
	cp.PasswordFlag = true
	cp.Password = []byte("hunter2")

	var buf bytes.Buffer
	if err := cp.Write(&buf); err != nil {
		t.Fatalf("encode CONNECT: %v", err)
	}
	return buf.Bytes()
}

// The question these tests exist to answer, and the one the proxy could NOT
// answer during the 2026-08-04 investigation: when a session ends, did WE hang
// up on the client, or did the client vanish?
//
// A radio reconnecting every 60s looks identical in the logs either way -- a
// CONNECT, some SUBSCRIBEs, silence, another CONNECT. Distinguishing
// read_timeout (our deadline fired; the floor may be too tight) from client_eof
// (the client closed the socket; look at the device) is the whole diagnostic
// value, so each reason gets its own case.

func TestCloseReasonNamesOurOwnDeadline(t *testing.T) {
	// What SetReadDeadline produces, wrapped exactly as the net package wraps
	// it -- asserting on a bare os.ErrDeadlineExceeded would pass while the
	// production path (always a *net.OpError) still fell through to
	// read_error.
	err := &net.OpError{
		Op:  "read",
		Net: "tcp",
		Err: os.ErrDeadlineExceeded,
	}
	if got := closeReason(err); got != reasonReadTimeout {
		t.Errorf("closeReason(deadline exceeded) = %q, want %q -- a proxy-side hangup would be misreported as a client problem", got, reasonReadTimeout)
	}
}

func TestCloseReasonNamesAClientThatVanished(t *testing.T) {
	if got := closeReason(io.EOF); got != reasonClientEOF {
		t.Errorf("closeReason(io.EOF) = %q, want %q", got, reasonClientEOF)
	}
}

func TestCloseReasonTreatsTruncatedFrameAsClientEOF(t *testing.T) {
	// A client killed mid-frame (backgrounded phone) surfaces as
	// ErrUnexpectedEOF, not io.EOF. Same operational meaning: the client went
	// away, do not go hunting for a proxy bug.
	if got := closeReason(io.ErrUnexpectedEOF); got != reasonClientEOF {
		t.Errorf("closeReason(ErrUnexpectedEOF) = %q, want %q", got, reasonClientEOF)
	}
}

func TestCloseReasonNamesAResetConnection(t *testing.T) {
	err := &net.OpError{Op: "read", Net: "tcp", Err: errors.New("connection reset by peer")}
	if got := closeReason(err); got != reasonReadError {
		t.Errorf("closeReason(reset) = %q, want %q", got, reasonReadError)
	}
}

func TestCloseReasonWithoutAnErrorIsACleanDisconnect(t *testing.T) {
	// The loop exits with nil err only when the client sent a real DISCONNECT.
	// Exactly 1 of 726 sessions did that on 2026-08-04, which is itself the
	// signal -- so it must not be lumped in with the error paths.
	if got := closeReason(nil); got != reasonClientDisconnect {
		t.Errorf("closeReason(nil) = %q, want %q", got, reasonClientDisconnect)
	}
}

// A timeout that is NOT our deadline (any net.Error reporting Timeout) still
// means the read window expired, so it must classify as a timeout rather than
// falling through to the generic bucket.
func TestCloseReasonNamesAGenericNetTimeout(t *testing.T) {
	if got := closeReason(&net.OpError{Op: "read", Net: "tcp", Err: timeoutErr{}}); got != reasonReadTimeout {
		t.Errorf("closeReason(net timeout) = %q, want %q", got, reasonReadTimeout)
	}
}

type timeoutErr struct{}

func (timeoutErr) Error() string { return "i/o timeout" }
func (timeoutErr) Timeout() bool { return true }

// ---------------------------------------------------------------------------
// Emission, driven through the REAL handleProxy.
//
// The classifier tests above prove the mapping; these prove the lines actually
// reach production telemetry with the fields an operator needs. A correct
// classifier nobody calls would satisfy every test above and still leave the
// proxy exactly as undiagnosable as it was.
// ---------------------------------------------------------------------------

// sessionLogProxy runs one 3.1.1 CONNECT through handleProxy against a fake
// backend and returns everything written to the inspector log.
func sessionLogProxy(t *testing.T, keepalive uint16, clientID string) string {
	t.Helper()
	n, logs := recoverTestServer(t)
	backendAddr, _ := recoverBackend(t)
	n.Config.Server.ProxyForwardAddress = backendAddr

	clientConn, peer := net.Pipe()

	done := make(chan struct{})
	go func() {
		defer close(done)
		n.handleProxy(clientConn)
	}()

	if _, err := peer.Write(v4ConnectBytesKeepalive(t, clientID, keepalive)); err != nil {
		t.Fatalf("write CONNECT to the proxy: %v", err)
	}

	// Let the CONNECT be read and inspected before hanging up, otherwise the
	// session ends before it ever starts and SESSION_START never fires.
	time.Sleep(150 * time.Millisecond)
	peer.Close()

	awaitReturn(t, done, "handleProxy")
	return logs.String()
}

func TestSessionStartLogsTheNegotiatedKeepalive(t *testing.T) {
	// The field that was missing on 2026-08-04: with no keepalive in the log
	// there was no way to tell whether a 60s reconnect cadence was the
	// minProxyReadTimeout floor or the client's own choice.
	out := sessionLogProxy(t, 60, "meshtastic-keepalive")

	if !strings.Contains(out, "action=SESSION_START") {
		t.Fatalf("no SESSION_START line; the keepalive is still invisible in production.\nlog:\n%s", out)
	}
	if !strings.Contains(out, "keepalive=60s") {
		t.Errorf("SESSION_START does not carry keepalive=60s.\nlog:\n%s", out)
	}
	// 1.5x60 = 90s. Logging the DERIVED deadline as well as the raw keepalive
	// is the point: the floor and the multiplier both live in the proxy, so an
	// operator reading only the keepalive still has to guess when we hang up.
	if !strings.Contains(out, "read_timeout=90s") {
		t.Errorf("SESSION_START does not carry the derived read_timeout=90s.\nlog:\n%s", out)
	}
}

func TestSessionStartLogsTheFlooredReadTimeout(t *testing.T) {
	// A small keepalive is exactly the case where raw keepalive and effective
	// deadline diverge -- 1.5x10=15s, but the floor holds it at 60s. An
	// operator who saw only keepalive=10s would expect a 15s hangup and
	// misread every 60s reconnect in the log.
	out := sessionLogProxy(t, 10, "meshtastic-floored")

	if !strings.Contains(out, "keepalive=10s") {
		t.Errorf("SESSION_START does not carry the raw keepalive=10s.\nlog:\n%s", out)
	}
	if !strings.Contains(out, "read_timeout=60s") {
		t.Errorf("SESSION_START does not report the FLOORED deadline (60s); the floor is invisible.\nlog:\n%s", out)
	}
}

func TestSessionStartIsAttributable(t *testing.T) {
	// A keepalive nobody can tie to a radio is untriageable: the 2026-08-04
	// investigation turned entirely on isolating one clientID.
	out := sessionLogProxy(t, 60, "meshtastic-attributable")

	for _, want := range []string{"ip=", "clientID=meshtastic-attributable", "username=ed270dbe5d1e"} {
		if !strings.Contains(out, want) {
			t.Errorf("SESSION_START is missing %q, so the line cannot be tied to a radio.\nlog:\n%s", want, out)
		}
	}
}

func TestSessionEndLogsWhyTheSessionEnded(t *testing.T) {
	// The client hangs up without a DISCONNECT -- what a backgrounded phone
	// does, and what 725 of 726 sessions did on 2026-08-04.
	out := sessionLogProxy(t, 60, "meshtastic-eof")

	if !strings.Contains(out, "action=SESSION_END") {
		t.Fatalf("no SESSION_END line; sessions still vanish silently.\nlog:\n%s", out)
	}
	if !strings.Contains(out, "reason="+reasonClientEOF) {
		t.Errorf("SESSION_END does not report reason=%s, so a client-side hangup is still indistinguishable from a proxy-side one.\nlog:\n%s", reasonClientEOF, out)
	}
	if !strings.Contains(out, "duration=") {
		t.Errorf("SESSION_END carries no duration, so a reconnect cadence cannot be measured from one line.\nlog:\n%s", out)
	}
}

func TestSessionEndDistinguishesAPoliteDisconnect(t *testing.T) {
	// MQTT's one clean exit. It must be read off the PACKET, not off a read
	// error: the loop keeps forwarding after DISCONNECT and the socket only
	// reports EOF on the next read, so error-only classification would file
	// every well-behaved client under client_eof and destroy the signal that
	// made "1 clean disconnect in 726 sessions" worth noticing.
	n, logs := recoverTestServer(t)
	backendAddr, _ := recoverBackend(t)
	n.Config.Server.ProxyForwardAddress = backendAddr

	clientConn, peer := net.Pipe()

	done := make(chan struct{})
	go func() {
		defer close(done)
		n.handleProxy(clientConn)
	}()

	if _, err := peer.Write(v4ConnectBytesKeepalive(t, "meshtastic-polite", 60)); err != nil {
		t.Fatalf("write CONNECT: %v", err)
	}
	time.Sleep(100 * time.Millisecond)

	dp := packets.NewControlPacket(packets.Disconnect).(*packets.DisconnectPacket)
	var buf bytes.Buffer
	if err := dp.Write(&buf); err != nil {
		t.Fatalf("encode DISCONNECT: %v", err)
	}
	if _, err := peer.Write(buf.Bytes()); err != nil {
		t.Fatalf("write DISCONNECT: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	peer.Close()

	awaitReturn(t, done, "handleProxy")

	out := logs.String()
	if !strings.Contains(out, "reason="+reasonClientDisconnect) {
		t.Errorf("a client that sent DISCONNECT was not reported as %s; clean exits are indistinguishable from vanishing clients.\nlog:\n%s", reasonClientDisconnect, out)
	}
}

// ---------------------------------------------------------------------------
// v5 parity.
//
// Kurt's iOS proxy speaks 3.1.1, but Meshtastic-Android 2.8 and the meshmon
// tools speak v5 and run through an entirely separate handler. Instrumenting
// only 3.1.1 would leave exactly the same blind spot for half the fleet -- and
// it is the blind spot, not the protocol, that cost a day of investigation.
// ---------------------------------------------------------------------------

func TestV5SessionStartLogsTheNegotiatedKeepalive(t *testing.T) {
	n := newTestServerCmd(&mockAuthenticator{valid: true})
	logger, logs := captureLogger()
	n.Config.Log = logger
	n.InspectorLogger = logger
	n.LoadInspectorRules()

	s := startV5Session(t, n)
	defer s.finish()

	out := logs.String()
	if !strings.Contains(out, "action=SESSION_START") {
		t.Fatalf("no SESSION_START on the v5 path; v5 clients stay undiagnosable.\nlog:\n%s", out)
	}
	// mqttasticConnect negotiates KeepAlive=60 -> 1.5x -> 90s.
	if !strings.Contains(out, "keepalive=60s") || !strings.Contains(out, "read_timeout=90s") {
		t.Errorf("v5 SESSION_START does not carry keepalive=60s and read_timeout=90s.\nlog:\n%s", out)
	}
}

func TestV5SessionEndLogsWhyTheSessionEnded(t *testing.T) {
	n := newTestServerCmd(&mockAuthenticator{valid: true})
	logger, logs := captureLogger()
	n.Config.Log = logger
	n.InspectorLogger = logger
	n.LoadInspectorRules()

	s := startV5Session(t, n)
	s.peer.Close()
	select {
	case <-s.done:
	case <-time.After(5 * time.Second):
		t.Fatal("handleProxyV5 never returned")
	}

	out := logs.String()
	if !strings.Contains(out, "action=SESSION_END") {
		t.Fatalf("no SESSION_END on the v5 path.\nlog:\n%s", out)
	}
	if !strings.Contains(out, "reason=") || !strings.Contains(out, "duration=") {
		t.Errorf("v5 SESSION_END is missing reason/duration.\nlog:\n%s", out)
	}
}

// TestSessionEndNamesAProxySideHangup is the composition the other tests leave
// unproven: closeReason maps a deadline correctly (unit-tested above) and the
// loop emits whatever reason it is handed (integration-tested above), but
// nothing yet shows that a REAL expired proxy deadline arrives in the log as
// read_timeout.
//
// This is the single most valuable line in the whole change. It is the one that
// answers "did the 60s cadence come from our floor or from the phone?" -- the
// question that was unanswerable on 2026-08-04, and the reason the floor is a
// var rather than a const.
func TestSessionEndNamesAProxySideHangup(t *testing.T) {
	restore := minProxyReadTimeout
	minProxyReadTimeout = 200 * time.Millisecond
	t.Cleanup(func() { minProxyReadTimeout = restore })

	n, logs := recoverTestServer(t)
	backendAddr, _ := recoverBackend(t)
	n.Config.Server.ProxyForwardAddress = backendAddr

	clientConn, peer := net.Pipe()
	defer peer.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		n.handleProxy(clientConn)
	}()

	// keepalive=1 -> 1.5x -> 1.5s, but the lowered floor is not what binds
	// here; the point is simply that the deadline expires while the client
	// stays silent, exactly as an idle backgrounded phone does.
	if _, err := peer.Write(v4ConnectBytesKeepalive(t, "meshtastic-idle", 1)); err != nil {
		t.Fatalf("write CONNECT: %v", err)
	}

	// Say nothing further and let OUR deadline fire.
	awaitReturn(t, done, "handleProxy")

	out := logs.String()
	if !strings.Contains(out, "reason="+reasonReadTimeout) {
		t.Errorf("an expired PROXY deadline was not reported as %s; a server-side hangup still looks like a flapping client.\nlog:\n%s", reasonReadTimeout, out)
	}
}
