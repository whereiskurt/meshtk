package server

import (
	"bufio"
	"bytes"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	v5 "github.com/eclipse/paho.golang/packets"
	"github.com/eclipse/paho.mqtt.golang/packets"
	meshtastic "github.com/whereiskurt/meshtk/protos/meshtastic/generated"
	"google.golang.org/protobuf/proto"
)

// This file pins 68-REVIEW CR-01's THIRD layer: a panic while serving one
// connection must kill that connection and nothing else.
//
// The first two layers (69-01) closed one specific nil dereference. This layer
// is deliberately independent of that bug -- it must hold for the NEXT nil
// dereference nobody has found yet -- so every test here injects a panic from a
// substituted decider or a substituted socket rather than from a real defect.
//
// EVERY test asserts on the RECOVERED LOG LINE, not merely on "the call
// returned". A call can return for many reasons -- a read error, a Block
// decision, a closed pipe -- and all of them look identical from the outside.
// Only the recover writes that line, so only that line distinguishes "contained"
// from "never panicked in the first place".
//
// Each handler under test runs in its OWN goroutine, which is how production
// runs it and what makes the assertion honest: an unrecovered panic in a
// goroutine cannot be caught by the test function, so a regression here does not
// fail this test politely -- it takes the whole test binary down, exactly as it
// used to take the whole fleet down.

// recoverSentinel is the panic value every injected panic carries. It has no
// spaces or quotes so it survives logrus TextFormatter's quoting of the message
// intact, and it is distinctive enough that finding it in the log proves the
// logged panic is THIS panic and not some incidental one.
const recoverSentinel = "meshtk-recover-test-sentinel-69-02"

// panicDecider substitutes for the rules engine so a panic can be injected at
// the exact point every codec path funnels through -- PacketDecider.Decide --
// without touching rules.go or reintroducing a crashing fixture.
//
// It is armable so one server can panic on the first connection and serve the
// next one normally, which is what "the process keeps serving other connections"
// actually means. armed is atomic because the proxy goroutine reads it while the
// test goroutine disarms it.
type panicDecider struct {
	armed    atomic.Bool
	delegate Decider
}

func (d *panicDecider) Decide(packet *InspectorPacket) DecisionResult {
	if d.armed.Load() {
		panic(recoverSentinel)
	}
	if d.delegate != nil {
		return d.delegate.Decide(packet)
	}
	return DecisionResult{Decision: Allow, Reason: "panicDecider disarmed"}
}

// armPanicDecider swaps the rules engine for one that panics, keeping the real
// decider as the delegate so disarming restores production behavior rather than
// an allow-all stub.
func armPanicDecider(n *ServerCmd) *panicDecider {
	d := &panicDecider{delegate: n.PacketDecider}
	d.armed.Store(true)
	n.PacketDecider = d
	return d
}

// panicConn is a net.Conn whose Write panics, which is the injection point both
// downlink loops offer: handleBackend and handleBackendV5 both end by writing
// the packet to the CLIENT socket.
type panicConn struct {
	closed atomic.Bool
	writes atomic.Int32
}

func (c *panicConn) Write(b []byte) (int, error) {
	c.writes.Add(1)
	panic(recoverSentinel)
}

func (c *panicConn) Read(b []byte) (int, error) {
	// A downlink loop never reads the client socket; block rather than return
	// a spurious EOF that could end a loop for the wrong reason.
	select {}
}

func (c *panicConn) Close() error {
	c.closed.Store(true)
	return nil
}

func (c *panicConn) LocalAddr() net.Addr              { return panicAddr{} }
func (c *panicConn) RemoteAddr() net.Addr             { return panicAddr{} }
func (c *panicConn) SetDeadline(time.Time) error      { return nil }
func (c *panicConn) SetReadDeadline(time.Time) error  { return nil }
func (c *panicConn) SetWriteDeadline(time.Time) error { return nil }

type panicAddr struct{}

func (panicAddr) Network() string { return "tcp" }
func (panicAddr) String() string  { return "198.51.100.66:44444" }

// recoverTestServer is a ServerCmd with the real rule set and a greppable log,
// shaped like goldenTestServer but with the log captured instead of silenced.
func recoverTestServer(t *testing.T) (*ServerCmd, *bytes.Buffer) {
	t.Helper()
	n := newTestServerCmd(&mockAuthenticator{valid: true})
	logger, logs := captureLogger()
	n.Config.Log = logger
	n.InspectorLogger = logger
	n.LoadInspectorRules()
	return n, logs
}

// recoverBackend stands in for mosquitto: it accepts every connection the proxy
// dials and records the bytes it receives, so "the process kept serving" can be
// asserted on real forwarded bytes rather than on the absence of a crash.
func recoverBackend(t *testing.T) (addr string, recorded *syncBuf) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for fake backend: %v", err)
	}
	buf := &syncBuf{}
	accepted := make(chan struct{})
	go func() {
		defer close(accepted)
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				io.Copy(buf, c)
			}(c)
		}
	}()
	t.Cleanup(func() {
		ln.Close()
		<-accepted
	})
	return ln.Addr().String(), buf
}

// v4ConnectBytes is a minimal 3.1.1 CONNECT: enough for the preflight peek to
// identify protocol level 4 and fall through to the 3.1.1 codec, and enough for
// inspectRawPacket to authenticate before the decider is consulted.
func v4ConnectBytes(t *testing.T, clientID string) []byte {
	t.Helper()
	cp := packets.NewControlPacket(packets.Connect).(*packets.ConnectPacket)
	cp.ProtocolName = "MQTT"
	cp.ProtocolVersion = 4
	cp.CleanSession = true
	cp.Keepalive = 60
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

// awaitReturn fails loudly if a handler never came back. A handler that hangs is
// not containment either -- the connection would be leaked rather than closed.
func awaitReturn(t *testing.T, done <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s never returned after the injected panic", what)
	}
}

// assertRecovered is the assertion every test in this file is really making:
// the panic was contained AND it is greppable in production telemetry with the
// label, the remote address, the panic value and a stack.
//
// There is ONE implementation so the 3.1.1 and v5 assertions cannot drift apart
// -- the same reason 69-01's six-field comparison is a single shared helper.
// The action token is passed in rather than baked in, so each call site states
// the production log contract it depends on instead of hiding it three frames
// down; that token is what 69-07 greps for in prod, and proxy.go deliberately
// emits it from exactly one place.
func assertRecovered(t *testing.T, logs *bytes.Buffer, action, label string) {
	t.Helper()
	out := logs.String()

	if !strings.Contains(out, action) {
		t.Fatalf("no recovered-panic line in the log; the panic was not contained by recoverConn.\nlog:\n%s", out)
	}
	if !strings.Contains(out, "label="+label) {
		t.Errorf("recovered line does not name label=%s -- the wrong goroutine recovered.\nlog:\n%s", label, out)
	}
	if !strings.Contains(out, recoverSentinel) {
		t.Errorf("recovered line does not carry the injected panic value %q; a different panic was logged.\nlog:\n%s", recoverSentinel, out)
	}
	if !strings.Contains(out, "stack=") {
		t.Errorf("recovered line carries no stack, so the underlying bug is undiagnosable.\nlog:\n%s", out)
	}
	if !strings.Contains(out, "remote=") {
		t.Errorf("recovered line carries no remote address, so a looping client is unattributable.\nlog:\n%s", out)
	}
}

// TestPanicInDeciderDoesNotEscapeHandleProxy drives the REAL handleProxy with a
// decider that panics. Before the recover, this panic reached the top of a
// connection goroutine and killed the process -- every connected radio dropped
// because one client sent one frame.
func TestPanicInDeciderDoesNotEscapeHandleProxy(t *testing.T) {
	n, logs := recoverTestServer(t)
	decider := armPanicDecider(n)

	backendAddr, backendBytes := recoverBackend(t)
	n.Config.Server.ProxyForwardAddress = backendAddr

	clientConn, peer := net.Pipe()
	defer peer.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		n.handleProxy(clientConn)
	}()

	if _, err := peer.Write(v4ConnectBytes(t, "meshtastic-panic")); err != nil {
		t.Fatalf("write CONNECT to the proxy: %v", err)
	}

	awaitReturn(t, done, "handleProxy")
	assertRecovered(t, logs, "action=PANIC_RECOVERED", labelProxyUplink)

	// The socket must be closed, or the "recovered" connection is one nobody is
	// reading -- a leak wearing the costume of a fix.
	//
	// net.Pipe reports the peer's closure from SetReadDeadline as well as from
	// Read, so that error is expected and ignored. The distinction that matters
	// is closure vs TIMEOUT: a socket left open would also fail this read, just
	// with a timeout, and treating that as success would make the assertion
	// vacuous.
	_ = peer.SetReadDeadline(time.Now().Add(2 * time.Second))
	switch _, err := peer.Read(make([]byte, 1)); {
	case err == nil:
		t.Error("the client socket is still readable after the recover; it was not closed")
	default:
		if ne, ok := err.(net.Error); ok && ne.Timeout() {
			t.Errorf("the client socket read timed out (%v) instead of reporting closure; the socket was left open", err)
		}
	}

	// FURTHER WORK IN THE SAME PROCESS. Reaching this point already proves the
	// process survived (an unrecovered panic in that goroutine would have taken
	// the test binary with it), but proving it still SERVES is the actual
	// contract: same ServerCmd, same handler, a second connection that must
	// reach the broker.
	decider.armed.Store(false)

	survivorConn, survivorPeer := net.Pipe()
	defer survivorPeer.Close()

	survivorDone := make(chan struct{})
	go func() {
		defer close(survivorDone)
		n.handleProxy(survivorConn)
	}()

	if _, err := survivorPeer.Write(v4ConnectBytes(t, "meshtastic-survivor")); err != nil {
		t.Fatalf("write CONNECT for the survivor connection: %v", err)
	}

	frames := waitFrames(t, "backend", backendBytes, 1)
	if len(frames) < 1 {
		t.Fatal("the survivor connection never reached the backend; the proxy stopped serving")
	}
	if frames[0][0]>>4 != byte(packets.Connect) {
		t.Errorf("first backend frame is type %d, want CONNECT", frames[0][0]>>4)
	}

	survivorPeer.Close()
	survivorConn.Close()
	awaitReturn(t, survivorDone, "handleProxy (survivor)")
}

// TestPanicInDeciderDoesNotEscapeHandleProxyV5 is the same contract on the v5
// codec, driven through the REAL handleProxyV5. Both codecs share one process,
// so a recover on only one of them leaves the fleet exactly as exposed.
func TestPanicInDeciderDoesNotEscapeHandleProxyV5(t *testing.T) {
	n, logs := v5ParityServer(t)

	// Armed BEFORE the session starts: the CONNECT path never consults the
	// decider, and swapping the field while the read loop is live would be a
	// data race rather than a test.
	armPanicDecider(n)

	s := startV5Session(t, n)
	s.send(v5PublishFrame(t, nodeInfoEnvelope(t, 3, 3)))

	s.awaitReturn()
	s.finish()

	assertRecovered(t, logs, "action=PANIC_RECOVERED", labelProxyUplinkV5)
}

// TestPanicInDownlinkDoesNotEscape covers the direction a recover on the uplink
// handler CANNOT reach. Both downlink loops are spawned as their own goroutines,
// and a recover only ever catches panics from its own goroutine's stack -- so
// without a recover of their own, a panic here is still a process kill no matter
// how well guarded the uplink is.
//
// The injection point is a client socket whose Write panics, which is what both
// loops do with every packet the broker sends down.
func TestPanicInDownlinkDoesNotEscape(t *testing.T) {
	t.Run("3.1.1", func(t *testing.T) {
		n, logs := recoverTestServer(t)

		backendConn, broker := net.Pipe()
		defer broker.Close()
		defer backendConn.Close()

		client := &panicConn{}

		done := make(chan struct{})
		go func() {
			defer close(done)
			n.handleBackend(t.Context(), client, "198.51.100.66:44444", backendConn, bufio.NewReader(backendConn))
		}()

		if _, err := broker.Write(v4DownlinkBytes(t)); err != nil {
			t.Fatalf("write downlink PUBLISH: %v", err)
		}

		awaitReturn(t, done, "handleBackend")
		assertRecovered(t, logs, "action=PANIC_RECOVERED", labelProxyDownlink)

		if client.writes.Load() == 0 {
			t.Error("the downlink never wrote to the client socket; the panic came from somewhere else")
		}
		if !client.closed.Load() {
			t.Error("the client socket was not closed by the downlink recover")
		}
	})

	t.Run("v5", func(t *testing.T) {
		n, logs := recoverTestServer(t)

		backendConn, broker := net.Pipe()
		defer broker.Close()
		defer backendConn.Close()

		client := &panicConn{}

		done := make(chan struct{})
		go func() {
			defer close(done)
			n.handleBackendV5(t.Context(), client, "198.51.100.66:44444", backendConn, bufio.NewReader(backendConn))
		}()

		// A zero-length PINGRESP: neither CONNACK nor PUBLISH, so it reaches
		// the unconditional conn.Write with no parsing in between and the
		// panic is unambiguously the socket write.
		if _, err := broker.Write([]byte{byte(v5.PINGRESP) << 4, 0x00}); err != nil {
			t.Fatalf("write downlink PINGRESP: %v", err)
		}

		awaitReturn(t, done, "handleBackendV5")
		assertRecovered(t, logs, "action=PANIC_RECOVERED", labelProxyDownlinkV5)

		if client.writes.Load() == 0 {
			t.Error("the downlink never wrote to the client socket; the panic came from somewhere else")
		}
		if !client.closed.Load() {
			t.Error("the client socket was not closed by the downlink recover")
		}
	})
}

// v4DownlinkBytes is a broker->client PUBLISH carrying a real ServiceEnvelope,
// so the 3.1.1 downlink runs its normal logDownlink path before it reaches the
// socket write that panics.
func v4DownlinkBytes(t *testing.T) []byte {
	t.Helper()
	env := &meshtastic.ServiceEnvelope{
		Packet: &meshtastic.MeshPacket{
			From: 0x1555f041,
			To:   0xffffffff,
			Id:   0x2ad2ad30,
			PayloadVariant: &meshtastic.MeshPacket_Encrypted{
				Encrypted: []byte{0xde, 0xad},
			},
		},
		GatewayId: "!1555f041",
		ChannelId: "dc.run",
	}
	payload, err := proto.Marshal(env)
	if err != nil {
		t.Fatalf("marshal downlink envelope: %v", err)
	}

	pub := packets.NewControlPacket(packets.Publish).(*packets.PublishPacket)
	pub.TopicName = "msh/US/2/e/dc.run/!1555f041"
	pub.Payload = payload

	var buf bytes.Buffer
	if err := pub.Write(&buf); err != nil {
		t.Fatalf("encode downlink PUBLISH: %v", err)
	}
	return buf.Bytes()
}
