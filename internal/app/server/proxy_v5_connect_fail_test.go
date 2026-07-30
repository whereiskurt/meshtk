package server

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	v5 "github.com/eclipse/paho.golang/packets"
	log "github.com/sirupsen/logrus"
)

// This file pins 68-REVIEW WR-02: every v5 CONNECT failure branch used to log
// action=MQTT5_PARSE_FAIL and return, leaving handleProxy's deferred
// conn.Close() to drop the socket with NOTHING written. A client that gets no
// answer cannot distinguish a refusal from a network fault, so it hot-retries
// against a mute socket -- which is the exact 0x84 retry-loop failure mode
// Phase 68 existed to remove, reintroduced one layer down.
//
// FOUR branches are enumerated (N = 4), and each gets one test here:
//
//	1. readFrame error                          -- the frame is unreadable
//	2. first packet is not a CONNECT            -- wrong type on a fresh socket
//	3. v5.ReadPacket error                      -- an unmodelled property id
//	4. the parsed packet is not a *v5.Connect   -- defence in depth
//
// The malformed-packet reason code (0x81) is the honest answer to all four: the
// proxy DOES speak level 5, so the complaint is about the FRAME, not the
// version. Answering the unsupported-protocol-version code (0x84) to a level-5
// CONNECT is precisely what made mqttastic retry-loop.

const v5ConnectFailAddr = "203.0.113.11:52000"

// connackMalformedWire is the five-byte CONNACK the four branches must produce:
// 20 03 <ack flags 00> <reason 81> <properties length 00>.
const connackMalformedWire = "2003008100"

// unparseableV5Connect builds a level-5 CONNECT carrying property id 0x7f,
// which no MQTT 5.0 property table defines -- so paho.golang's Properties.Unpack
// hard-errors on it while peekConnectProtocolVersion still reads level 5 off the
// same bytes and routes the connection to handleProxyV5. That combination is
// what makes branch 3 genuinely reachable in production rather than theoretical
// (68-REVIEW WR-02 proved the routing detail).
func unparseableV5Connect(clientID string) []byte {
	var body bytes.Buffer
	body.Write([]byte{0x00, 0x04})
	body.WriteString("MQTT")
	body.WriteByte(0x05)           // protocol level 5
	body.WriteByte(0x02)           // connect flags: clean start
	body.Write([]byte{0x00, 0x3c}) // keepalive 60
	body.Write([]byte{0x02, 0x7f, 0x05})
	body.WriteByte(byte(len(clientID) >> 8))
	body.WriteByte(byte(len(clientID)))
	body.WriteString(clientID)

	var frame bytes.Buffer
	frame.WriteByte(0x10)
	frame.Write(encodeV5Varint(body.Len()))
	frame.Write(body.Bytes())
	return frame.Bytes()
}

// runV5ConnectFailure drives the REAL handleProxyV5 with one frame on a fresh
// socket and returns every byte the proxy wrote back to the client, in hex.
//
// The client side is drained by its own goroutine because net.Pipe is
// unbuffered and ControlPacket.WriteTo emits several Writes: a lone Read would
// consume the first chunk and the writer would block forever on the next.
func runV5ConnectFailure(t *testing.T, n *ServerCmd, frame []byte) string {
	t.Helper()

	clientConn, peer := net.Pipe()
	client := &syncBuf{}
	clientDone := make(chan struct{})
	go func() {
		defer close(clientDone)
		io.Copy(client, peer)
	}()

	done := make(chan struct{})
	go func() {
		defer close(done)
		n.handleProxyV5(clientConn, bufio.NewReader(clientConn), v5ConnectFailAddr)
	}()

	if err := peer.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set write deadline: %v", err)
	}
	if _, err := peer.Write(frame); err != nil {
		t.Fatalf("write %x to the proxy: %v", frame, err)
	}

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handleProxyV5 did not return on a CONNECT failure")
	}

	clientConn.Close()
	peer.Close()
	<-clientDone

	return hex.EncodeToString(client.Bytes())
}

func assertMalformedConnack(t *testing.T, wire string, logs *bytes.Buffer) {
	t.Helper()
	if wire == "" {
		t.Fatalf("the CONNECT failure ended in a SILENT CLOSE -- the client cannot tell a refusal from a network fault and will hot-retry (WR-02); log:\n%s", logs.String())
	}
	if wire != connackMalformedWire {
		t.Fatalf("client-bound bytes = %s, want %s (CONNACK, malformed packet)", wire, connackMalformedWire)
	}
	out := logs.String()
	if !strings.Contains(out, "action=MQTT5_PARSE_FAIL") {
		t.Fatalf("missing the parse-fail line; got:\n%s", out)
	}
	// answered= is what lets 69-07 tell an ANSWERED failure from the pre-fix
	// silent close in production telemetry, on the same one grep.
	if !strings.Contains(out, "answered=0x81") {
		t.Fatalf("the parse-fail line does not record the answered reason code; got:\n%s", out)
	}
}

// Branch 1: readFrame refuses the frame. Five continuation bytes in the
// remaining-length varint trip its 4-byte termination guard, and the socket
// stays open -- so a silent close here is a choice, not a consequence.
func TestV5ConnectFailUnreadableFrame(t *testing.T) {
	n, logs := v5ParityServer(t)
	assertMalformedConnack(t, runV5ConnectFailure(t, n, mustHexNoT("10ffffffff")), logs)
}

// Branch 2: the first packet on a fresh socket is not a CONNECT.
func TestV5ConnectFailFirstPacketNotConnect(t *testing.T) {
	n, logs := v5ParityServer(t)
	assertMalformedConnack(t, runV5ConnectFailure(t, n, mustHexNoT("c000")), logs) // PINGREQ
}

// Branch 3: the codec refuses a CONNECT it routed here itself. This is the
// branch WR-02 proved reachable: peekConnectProtocolVersion reads level 5 off
// the very bytes v5.ReadPacket then rejects.
func TestV5ConnectFailUnmodelledProperty(t *testing.T) {
	frame := unparseableV5Connect(v5TestClientID)

	if ver, ok := peekConnectProtocolVersion(bufio.NewReader(bytes.NewReader(frame))); !ok || ver != 5 {
		t.Fatalf("peekConnectProtocolVersion = (%d, %v); the fixture would never be routed to handleProxyV5", ver, ok)
	}
	if _, err := v5.ReadPacket(bytes.NewReader(frame)); err == nil {
		t.Fatal("the fixture parses cleanly; it cannot exercise the codec-failure branch")
	}

	n, logs := v5ParityServer(t)
	assertMalformedConnack(t, runV5ConnectFailure(t, n, frame), logs)
}

// Branch 4: the parsed packet is not a *v5.Connect.
//
// UNREACHABLE THROUGH THE SOCKET TODAY, and the test says so rather than
// pretending otherwise: v5.ReadPacket switches on the same fixed-header nibble
// readFrame already checked, so a 0x1_ frame always yields a *v5.Connect. The
// branch is defence in depth against that dispatch changing, and it is reached
// here directly -- with the same byte-level assertion as its three siblings --
// because a defensive branch nobody can execute is a branch nobody knows works.
func TestV5ConnectFailParsedAsAnotherType(t *testing.T) {
	// The premise, asserted rather than assumed. If this ever fails, branch 4
	// became reachable from the socket and deserves a full-loop test.
	if cp, err := v5.ReadPacket(bytes.NewReader(mustHexNoT("101000044d5154540502003c0000045465737431"))); err == nil {
		if _, ok := cp.Content.(*v5.Connect); !ok {
			t.Fatalf("a 0x1_ frame parsed as %T; branch 4 is now socket-reachable", cp.Content)
		}
	}

	n, logs := v5ParityServer(t)
	var client bytes.Buffer
	if c, ok := n.connectFromV5Packet(writerConn{&client}, v5ConnectFailAddr, v5.NewControlPacket(v5.PINGREQ)); ok || c != nil {
		t.Fatal("a PINGREQ was accepted as a CONNECT")
	}
	assertMalformedConnack(t, hex.EncodeToString(client.Bytes()), logs)
}

// The pinned answers must not move. Bad credentials, enhanced auth and level
// above 5 are fixed by TestV5ConnackReasonCodes AND by the committed
// mqtt5_probe.py regression check, so changing any of them would be a
// regression, not a fix -- and 0x84 in particular is the retry loop Phase 68
// existed to remove.
func TestV5MalformedAnswerDistinctFromThePinnedCodes(t *testing.T) {
	if v5.ConnackMalformedPacket == v5.ConnackUnsupportedProtocolVersion {
		t.Fatal("the malformed-packet code equals the unsupported-version code")
	}
	for _, pinned := range []struct {
		desc   string
		reason byte
	}{
		{"unsupported protocol version", v5.ConnackUnsupportedProtocolVersion},
		{"not authorized", v5.ConnackNotAuthorized},
		{"bad authentication method", v5.ConnackBadAuthenticationMethod},
	} {
		if pinned.reason == v5.ConnackMalformedPacket {
			t.Fatalf("the new answer collides with the pinned %q code", pinned.desc)
		}
	}

	var buf bytes.Buffer
	if err := writeMqtt5Connack(writerConn{&buf}, v5.ConnackMalformedPacket); err != nil {
		t.Fatalf("writeMqtt5Connack: %v", err)
	}
	if got := hex.EncodeToString(buf.Bytes()); got != connackMalformedWire {
		t.Fatalf("wire bytes = %s, want %s", got, connackMalformedWire)
	}
}

// 69-03 closed WR-05 by sanitizing every client-controlled string at every
// InspectorLogger boundary. EXTENDING a line is the same hazard as adding one:
// SimpleFormatter does no quoting, so one unsanitized value reopens the whole
// finding. The plainest way to keep these four lines safe is to add no client
// string to them at all -- which is what was done, and this proves it holds
// even when the client id is a fully-formed forged record.
func TestV5ConnectParseFailLineCannotBeForged(t *testing.T) {
	const forged = "evil\n2026-07-29 00:00:00.000 action=MQTT5_CONNECT, ip=10.0.0.1, username=admin, client_id=admin"

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

	runV5ConnectFailure(t, n, unparseableV5Connect(forged))

	out := buf.String()
	if !strings.Contains(out, "action=MQTT5_PARSE_FAIL") {
		t.Fatalf("the parse-fail line was never emitted; got:\n%s", out)
	}
	if got := strings.Count(out, "\n"); got != 1 {
		t.Fatalf("one CONNECT produced %d log lines (want exactly 1):\n%s", got, out)
	}
	if strings.Contains(out, "action=MQTT5_CONNECT") {
		t.Fatalf("the forged record reached the log as its own content; got:\n%s", out)
	}
}
