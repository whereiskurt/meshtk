package server

// End-to-end proof for the MQTT v5 dual codec: a REAL mosquitto behind a REAL
// proxy listener, driven by raw wire bytes.
//
// Everything else in this package tests a function against a fake socket. That
// is the right shape for wire assertions, but it cannot observe the things that
// actually break in production: whether the byte stream through
// StartProxyServer -> handleProxy -> handleProxyV5 is well formed, what the
// BROKER believes the protocol version and the identity to be, what real CONNACK
// properties come back, whether QoS1 round trips, and whether a zero-length
// DISCONNECT is graceful or a torn socket. Only a live broker answers those.
//
// The test is env-gated (MESHTK_E2E=1) so `go test ./...` stays hermetic, and it
// SKIPs -- never fails -- when no broker is reachable. A default gate that goes
// red because a binary is missing is a gate nobody trusts.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	v5 "github.com/eclipse/paho.golang/packets"
	log "github.com/sirupsen/logrus"
	"github.com/whereiskurt/meshtk/pkg/config"
)

const (
	// The client-side identity meshtk's own Authenticator verifies. It is
	// deliberately NOT the broker identity: the entire point of the CONNECT
	// swap is that mosquitto never learns this pair.
	e2eClientUser = "runner"
	e2eClientPass = "meshpass"

	// The broker identity the proxy swaps in. mosquitto's password file holds
	// ONLY this pair, so `u'public'` in the broker log proves the swap
	// happened -- and a leaked client credential would be refused by mosquitto
	// rather than silently accepted.
	e2eBrokerUser = "public"
	e2eBrokerPass = "31337"

	// mosquitto 2.0's default CONNACK for this config, before and after the
	// proxy strips TopicAliasMaximum:
	//   20 09 | 00 00 | 06 | 22 000a | 21 0014   <- broker: alias budget 10
	//   20 06 | 00 00 | 03 |         | 21 0014   <- client: property gone
	e2eBrokerConnackHex   = "200900000622000a210014"
	e2eStrippedConnackHex = "2006000003210014"
)

// ---------------------------------------------------------------------------
// Concurrency-safe log capture
// ---------------------------------------------------------------------------

// syncBuffer is a bytes.Buffer with a mutex. The mosquitto process writer and
// logrus both write from other goroutines while the test goroutine greps, so an
// unguarded bytes.Buffer is a data race -- and an intermittent one, which is
// worse than a deterministic failure.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// Len is the cursor a subtest uses to scope an assertion to its OWN traffic:
// snapshot the length, do the thing, then look only at what was appended. A
// negative assertion like "no New client connected line" is trivially false by
// the time any subtest but the first one runs without this.
func (s *syncBuffer) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Len()
}

func (s *syncBuffer) since(mark int) string {
	all := s.String()
	if mark > len(all) {
		return ""
	}
	return all[mark:]
}

// waitForLog polls until substr appears after mark. Broker logging is
// asynchronous relative to the client's socket write, so a bare assertion
// straight after a publish is a race that passes on a fast laptop and fails
// everywhere else.
func waitForLog(t *testing.T, name string, buf *syncBuffer, mark int, substr string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if tail := buf.since(mark); strings.Contains(tail, substr) {
			return tail
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s log never contained %q within %s.\n--- %s log since mark ---\n%s",
				name, substr, timeout, name, buf.since(mark))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// assertNoLog is the negative twin: settle, then assert the substring is still
// absent. Used to prove a rejected CONNECT never reached the broker at all.
func assertNoLog(t *testing.T, name string, buf *syncBuffer, mark int, substr string, settle time.Duration) {
	t.Helper()
	time.Sleep(settle)
	if tail := buf.since(mark); strings.Contains(tail, substr) {
		t.Fatalf("%s log unexpectedly contained %q.\n--- %s log since mark ---\n%s", name, substr, name, tail)
	}
}

// ---------------------------------------------------------------------------
// Authenticator
// ---------------------------------------------------------------------------

// pairAuthenticator accepts exactly one credential pair. The package's existing
// mockAuthenticator answers the same way regardless of input, which cannot
// express "this run has a good CONNECT and a bad one".
type pairAuthenticator struct {
	user string
	pass string

	mu    sync.Mutex
	calls int
}

func (a *pairAuthenticator) Verify(_ context.Context, username string, password []byte) (bool, error) {
	a.mu.Lock()
	a.calls++
	a.mu.Unlock()
	return username == a.user && string(password) == a.pass, nil
}

// ---------------------------------------------------------------------------
// Broker
// ---------------------------------------------------------------------------

// e2eBroker is a live mosquitto with its stdout captured.
type e2eBroker struct {
	addr string
	logs *syncBuffer
}

// resolveBroker finds a mosquitto to run: the Homebrew install (2.0.22 on the
// dev machine), then whatever is on PATH, then a docker container on
// alpine:3.21 -- the exact base of the production run.mqtt mosquitto image, so
// `apk add mosquitto` yields 2.0.20, the version actually serving
// mqtt.defcon.run.
//
// MESHTK_E2E_DOCKER=1 forces the container path so the fallback can be
// exercised on a machine that also has a local broker.
//
// Returns "" when nothing is reachable, which SKIPs the whole test.
func resolveBroker() (path string, useDocker bool) {
	if os.Getenv("MESHTK_E2E_DOCKER") == "1" {
		if p, err := exec.LookPath("docker"); err == nil {
			return p, true
		}
		return "", false
	}
	if fi, err := os.Stat("/opt/homebrew/sbin/mosquitto"); err == nil && !fi.IsDir() {
		return "/opt/homebrew/sbin/mosquitto", false
	}
	if p, err := exec.LookPath("mosquitto"); err == nil {
		return p, false
	}
	if p, err := exec.LookPath("docker"); err == nil {
		return p, true
	}
	return "", false
}

// brokerConfig renders the committed testdata fixture into dir. The fixture is
// version controlled (so the broker's shape is reviewable) while the port and
// the password-file path are per-run.
func brokerConfig(t *testing.T, dir string, port int, bind, passwdPath string) string {
	t.Helper()
	tmpl, err := os.ReadFile(filepath.Join("testdata", "mosquitto.e2e.conf"))
	if err != nil {
		t.Fatalf("read broker fixture: %v", err)
	}
	rendered := strings.NewReplacer(
		"__PORT__", fmt.Sprintf("%d", port),
		"__BIND__", bind,
		"__PASSWORD_FILE__", passwdPath,
	).Replace(string(tmpl))

	confPath := filepath.Join(dir, "mosquitto.conf")
	if err := os.WriteFile(confPath, []byte(rendered), 0o644); err != nil {
		t.Fatalf("write broker config: %v", err)
	}
	return confPath
}

// startBroker brings up mosquitto on a free localhost port and tears it down in
// t.Cleanup. The credential material is generated into t.TempDir() at run time
// and never touches the repository.
func startBroker(t *testing.T) *e2eBroker {
	t.Helper()

	bin, useDocker := resolveBroker()
	if bin == "" {
		t.Skip("no mosquitto binary and no docker: install mosquitto or start docker to run the e2e")
	}

	dir := t.TempDir()
	port := freePort(t)
	logs := &syncBuffer{}

	if useDocker {
		startDockerBroker(t, dir, port, logs)
	} else {
		startLocalBroker(t, bin, dir, port, logs)
	}

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	waitForListener(t, addr, 90*time.Second, "mosquitto", logs)
	return &e2eBroker{addr: addr, logs: logs}
}

func startLocalBroker(t *testing.T, bin, dir string, port int, logs *syncBuffer) {
	t.Helper()

	passwdPath := filepath.Join(dir, "passwd")
	passwdBin := mosquittoPasswdFor(t, bin)
	if out, err := exec.Command(passwdBin, "-c", "-b", passwdPath, e2eBrokerUser, e2eBrokerPass).CombinedOutput(); err != nil {
		t.Fatalf("mosquitto_passwd failed: %v\n%s", err, out)
	}

	conf := brokerConfig(t, dir, port, "127.0.0.1", passwdPath)
	cmd := exec.Command(bin, "-c", conf)
	cmd.Stdout = logs
	cmd.Stderr = logs
	if err := cmd.Start(); err != nil {
		t.Fatalf("start mosquitto: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
}

// mosquittoPasswdFor locates mosquitto_passwd. Homebrew puts the broker in
// sbin/ and the tools in bin/, so "next to the broker" alone is not enough.
func mosquittoPasswdFor(t *testing.T, brokerBin string) string {
	t.Helper()
	if p, err := exec.LookPath("mosquitto_passwd"); err == nil {
		return p
	}
	sibling := filepath.Join(filepath.Dir(brokerBin), "mosquitto_passwd")
	if fi, err := os.Stat(sibling); err == nil && !fi.IsDir() {
		return sibling
	}
	t.Skipf("mosquitto found at %s but no mosquitto_passwd alongside it or on PATH", brokerBin)
	return ""
}

// startDockerBroker is the prod-identical fallback. The container binds
// 0.0.0.0 inside its own network namespace and the port is published to
// localhost only.
func startDockerBroker(t *testing.T, dir string, port int, logs *syncBuffer) {
	t.Helper()

	// The password file needs a mosquitto_passwd that exists; on a docker-only
	// machine that is the container's own.
	gen := exec.Command("docker", "run", "--rm",
		"-v", dir+":/fixture",
		"alpine:3.21", "sh", "-c",
		"apk add --no-cache mosquitto >/dev/null 2>&1 && "+
			"mosquitto_passwd -c -b /fixture/passwd "+e2eBrokerUser+" "+e2eBrokerPass)
	if out, err := gen.CombinedOutput(); err != nil {
		t.Skipf("docker fallback unusable (mosquitto_passwd step): %v\n%s", err, out)
	}

	// password_file is resolved INSIDE the container, so it is the mount path.
	brokerConfig(t, dir, port, "0.0.0.0", "/fixture/passwd")

	cmd := exec.Command("docker", "run", "--rm",
		"-p", fmt.Sprintf("127.0.0.1:%d:%d", port, port),
		"-v", dir+":/fixture",
		"alpine:3.21", "sh", "-c",
		"apk add --no-cache mosquitto >/dev/null 2>&1 && exec mosquitto -c /fixture/mosquitto.conf")
	cmd.Stdout = logs
	cmd.Stderr = logs
	if err := cmd.Start(); err != nil {
		t.Skipf("docker fallback unusable (broker start): %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
}

// ---------------------------------------------------------------------------
// Proxy under test
// ---------------------------------------------------------------------------

// e2eHarness is a live proxy in front of a live broker, plus both log streams
// and the long-lived v5 client the traffic subtests share.
type e2eHarness struct {
	n         *ServerCmd
	proxyAddr string
	broker    *e2eBroker
	proxyLog  *syncBuffer
	auth      *pairAuthenticator

	v5c *v5Client
}

// startHarness runs the REAL StartProxyServer -- accept loop, proxyproto
// listener wrapper and handleProxy dispatch all included. A hand-rolled
// listener here would skip precisely the code path that has to work.
func startHarness(t *testing.T) *e2eHarness {
	t.Helper()

	broker := startBroker(t)

	proxyLog := &syncBuffer{}
	logger := log.New()
	logger.SetOutput(proxyLog)
	logger.SetLevel(log.DebugLevel)
	logger.SetFormatter(&log.TextFormatter{DisableColors: true, DisableTimestamp: true})

	auth := &pairAuthenticator{user: e2eClientUser, pass: e2eClientPass}

	n := &ServerCmd{Config: &config.Config{}, InspectorLogger: logger}
	n.Config.Log = logger
	n.Authenticator = auth
	n.Config.Server.ProxyListenAddress = fmt.Sprintf("127.0.0.1:%d", freePort(t))
	n.Config.Server.ProxyForwardAddress = broker.addr
	n.Config.Server.ProxyUsername = e2eBrokerUser
	n.Config.Server.ProxyPassword = e2eBrokerPass
	n.Config.Server.CredCache.TimeoutSecs = 5
	n.Config.Server.ShouldLogAllows = true
	n.Config.Server.ShouldLogBlocks = true
	// Empty on purpose: the admin HTTP server is unrelated to the proxy path
	// and starting it would bind another port for nothing.
	n.Config.Server.AdminListenAddress = ""

	// Without this PacketDecider is nil and the first PUBLISH nil-derefs.
	n.LoadInspectorRules()

	// StartProxyServer blocks on SIGINT by design; this goroutine simply never
	// returns for the lifetime of the test binary. It also initializes
	// ConnTrack and ConnMutex, which is why nothing may dial before the
	// listener is up.
	go func() { _ = n.StartProxyServer() }()

	waitForListener(t, n.Config.Server.ProxyListenAddress, 10*time.Second, "proxy", proxyLog)

	return &e2eHarness{
		n:         n,
		proxyAddr: n.Config.Server.ProxyListenAddress,
		broker:    broker,
		proxyLog:  proxyLog,
		auth:      auth,
	}
}

// dump prints both log streams. Called from a failing subtest so a red run is
// diagnosable from the test output alone, without re-running anything by hand.
func (h *e2eHarness) dump(t *testing.T, brokerMark, proxyMark int) {
	t.Helper()
	t.Logf("--- mosquitto log ---\n%s", h.broker.logs.since(brokerMark))
	t.Logf("--- proxy inspector log ---\n%s", h.proxyLog.since(proxyMark))
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("allocate free port: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port
}

func waitForListener(t *testing.T, addr string, timeout time.Duration, name string, logs *syncBuffer) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		c, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
		if err == nil {
			_ = c.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never accepted on %s within %s.\n--- %s log ---\n%s", name, addr, timeout, name, logs.String())
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// ---------------------------------------------------------------------------
// Raw v5 client
// ---------------------------------------------------------------------------

// v5Client is a hand-rolled MQTT 5.0 client: a plain TCP socket, the wire codec
// and the proxy's own readFrame. No client library, deliberately -- a library
// would normalize the bytes, and the bytes are the thing under test.
type v5Client struct {
	conn net.Conn
	r    *bufio.Reader
}

func dialV5(t *testing.T, addr string) *v5Client {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return &v5Client{conn: conn, r: bufio.NewReader(conn)}
}

func (c *v5Client) send(t *testing.T, cp *v5.ControlPacket) {
	t.Helper()
	_ = c.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if _, err := cp.WriteTo(c.conn); err != nil {
		t.Fatalf("write %s: %v", cp.PacketType(), err)
	}
}

func (c *v5Client) sendRaw(t *testing.T, b []byte) {
	t.Helper()
	_ = c.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.conn.Write(b); err != nil {
		t.Fatalf("write raw %x: %v", b, err)
	}
}

// readFrame reads one packet as RAW BYTES through the proxy's own frame reader,
// so every assertion is on what a client actually sees rather than on a
// re-encoding of it.
func (c *v5Client) readFrame(timeout time.Duration) ([]byte, byte, error) {
	_ = c.conn.SetReadDeadline(time.Now().Add(timeout))
	return readFrame(c.r)
}

func (c *v5Client) mustReadFrame(t *testing.T, timeout time.Duration) ([]byte, byte) {
	t.Helper()
	frame, typ, err := c.readFrame(timeout)
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	return frame, typ
}

// connectPacket builds the shape mqttastic actually sends: a client id, a
// keepalive, clean start, credentials, and a properties block that includes
// TopicAliasMaximum -- which the proxy must strip on the way to the broker.
func connectPacket(clientID, user, pass string) *v5.ControlPacket {
	cp := v5.NewControlPacket(v5.CONNECT)
	c := cp.Content.(*v5.Connect)
	c.ClientID = clientID
	c.KeepAlive = 60
	c.CleanStart = true
	c.UsernameFlag = true
	c.Username = user
	c.PasswordFlag = true
	c.Password = []byte(pass)
	aliasMax := uint16(10)
	sessionExpiry := uint32(0)
	c.Properties = &v5.Properties{
		TopicAliasMaximum:     &aliasMax,
		SessionExpiryInterval: &sessionExpiry,
		User:                  []v5.User{{Key: "src", Value: "meshtk-e2e"}},
	}
	return cp
}

// ---------------------------------------------------------------------------
// The test
// ---------------------------------------------------------------------------

func TestE2EDualCodec(t *testing.T) {
	if os.Getenv("MESHTK_E2E") != "1" {
		t.Skip("e2e gated: set MESHTK_E2E=1 to run against a live mosquitto")
	}

	h := startHarness(t)

	// --- CONNECT rejection paths -------------------------------------------
	//
	// Both must be answered by the PROXY before it dials the broker. Asserting
	// the broker log gained no connection is what proves that: a rejecting
	// client that still costs a broker connection is an amplifier, and
	// mqttastic retries every 5-25s forever.

	t.Run("v5_bad_credentials_rejected_before_broker", func(t *testing.T) {
		brokerMark, proxyMark := h.broker.logs.Len(), h.proxyLog.Len()

		c := dialV5(t, h.proxyAddr)
		c.send(t, connectPacket("mqttastic-e2e-bad", e2eClientUser, "wrong-password"))

		frame, typ := c.mustReadFrame(t, 10*time.Second)
		if typ != v5.CONNACK {
			h.dump(t, brokerMark, proxyMark)
			t.Fatalf("expected CONNACK, got packet type %d (%x)", typ, frame)
		}
		if got := hex.EncodeToString(frame); got != "2003008700" {
			h.dump(t, brokerMark, proxyMark)
			t.Fatalf("bad-credential CONNACK = %s, want 2003008700 (0x87 Not Authorized)", got)
		}

		assertNoLog(t, "mosquitto", h.broker.logs, brokerMark, "New client connected", 500*time.Millisecond)

		if tail := h.proxyLog.since(proxyMark); !strings.Contains(tail, "action=AUTH_REJECT") {
			t.Fatalf("proxy log missing action=AUTH_REJECT.\n--- proxy log ---\n%s", tail)
		}
	})

	t.Run("v5_enhanced_auth_rejected_before_broker", func(t *testing.T) {
		brokerMark, proxyMark := h.broker.logs.Len(), h.proxyLog.Len()

		c := dialV5(t, h.proxyAddr)
		cp := connectPacket("mqttastic-e2e-auth", e2eClientUser, e2eClientPass)
		cp.Content.(*v5.Connect).Properties.AuthMethod = "SCRAM-SHA-1"
		c.send(t, cp)

		frame, typ := c.mustReadFrame(t, 10*time.Second)
		if typ != v5.CONNACK {
			h.dump(t, brokerMark, proxyMark)
			t.Fatalf("expected CONNACK, got packet type %d (%x)", typ, frame)
		}
		if got := hex.EncodeToString(frame); got != "2003008c00" {
			h.dump(t, brokerMark, proxyMark)
			t.Fatalf("enhanced-auth CONNACK = %s, want 2003008c00 (0x8C Bad Authentication Method)", got)
		}

		assertNoLog(t, "mosquitto", h.broker.logs, brokerMark, "New client connected", 500*time.Millisecond)

		if tail := h.proxyLog.since(proxyMark); !strings.Contains(tail, "action=MQTT5_AUTH_METHOD") {
			t.Fatalf("proxy log missing action=MQTT5_AUTH_METHOD.\n--- proxy log ---\n%s", tail)
		}
	})

	// --- CONNECT success: the swap and the alias strip, from both ends -------

	t.Run("v5_connect_swaps_identity_and_strips_alias_max", func(t *testing.T) {
		brokerMark, proxyMark := h.broker.logs.Len(), h.proxyLog.Len()

		// This connection stays open for the rest of the run: the traffic
		// subtests below share it, which is what makes the 3.1.1 client
		// genuinely concurrent rather than merely sequential.
		h.v5c = dialV5(t, h.proxyAddr)
		h.v5c.send(t, connectPacket("mqttastic-e2e-v5", e2eClientUser, e2eClientPass))

		frame, typ := h.v5c.mustReadFrame(t, 10*time.Second)
		if typ != v5.CONNACK {
			h.dump(t, brokerMark, proxyMark)
			t.Fatalf("expected CONNACK, got packet type %d (%x)", typ, frame)
		}

		if got := hex.EncodeToString(frame); got != e2eStrippedConnackHex {
			h.dump(t, brokerMark, proxyMark)
			t.Fatalf("CONNACK = %s, want %s (mosquitto's own %s with TopicAliasMaximum stripped)",
				got, e2eStrippedConnackHex, e2eBrokerConnackHex)
		}

		// Parsed as well as byte-compared, so the INTENT (no alias budget) is
		// asserted and not just a hex string somebody could update blindly.
		cp, err := v5.ReadPacket(bytes.NewReader(frame))
		if err != nil {
			t.Fatalf("parse CONNACK: %v", err)
		}
		ca := cp.Content.(*v5.Connack)
		if ca.ReasonCode != 0 {
			t.Fatalf("CONNACK reason = 0x%02x, want 0x00", ca.ReasonCode)
		}
		if ca.Properties != nil && ca.Properties.TopicAliasMaximum != nil {
			t.Fatalf("CONNACK still advertises TopicAliasMaximum=%d; the client is free to blind every topic rule",
				*ca.Properties.TopicAliasMaximum)
		}

		// The broker's own view. `p5` proves the protocol version survived the
		// proxy; `u'public'` proves the client's credential did not.
		tail := waitForLog(t, "mosquitto", h.broker.logs, brokerMark, "New client connected", 10*time.Second)
		if !strings.Contains(tail, "p5") {
			t.Fatalf("mosquitto did not log the proxied connection as protocol 5.\n--- mosquitto log ---\n%s", tail)
		}
		if !strings.Contains(tail, "u'"+e2eBrokerUser+"'") {
			t.Fatalf("mosquitto did not log the SWAPPED identity u'%s'.\n--- mosquitto log ---\n%s", e2eBrokerUser, tail)
		}
		if strings.Contains(tail, e2eClientUser) || strings.Contains(tail, e2eClientPass) {
			t.Fatalf("client credential material reached the broker log.\n--- mosquitto log ---\n%s", tail)
		}

		if !strings.Contains(h.proxyLog.since(proxyMark), "action=MQTT5_CONNECT") {
			t.Fatalf("proxy log missing action=MQTT5_CONNECT.\n--- proxy log ---\n%s", h.proxyLog.since(proxyMark))
		}
	})
}
