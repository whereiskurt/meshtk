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
	mqtt "github.com/eclipse/paho.mqtt.golang"
	log "github.com/sirupsen/logrus"
	"github.com/whereiskurt/meshtk/pkg/config"
	meshtastic "github.com/whereiskurt/meshtk/protos/meshtastic/generated"
	"google.golang.org/protobuf/proto"
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

	// The two ends of the traffic matrix. They MUST have different gateway ids:
	// the proxy suppresses a downlink whose gateway matches the connection's own
	// uplink gateway, so a shared id would make the downlink assertions
	// unfalsifiable (nothing would ever arrive, and the test would be asserting
	// self-echo suppression while claiming to assert downlink delivery).
	e2eTopicFilter = "msh/US/2/e/dc.run/#"

	e2eV5Gateway = "!435990e4"
	e2eV5Topic   = "msh/US/2/e/dc.run/!435990e4"
	e2eV5From    = uint32(0x435990e4)

	e2eV4Gateway = "!15550041"
	e2eV4Topic   = "msh/US/2/e/dc.run/!15550041"
	e2eV4From    = uint32(0x15550041)

	// Packet id for the QoS1 publish. mosquitto logs it in decimal as m4660,
	// which is what the log assertion greps for.
	e2eQoS1PacketID  = uint16(0x1234)
	e2eQoS1MessageID = "m4660"
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

	// Readiness is the BROKER's own "running" line, not a successful TCP
	// connect. Under docker the published port is served by docker-proxy, which
	// completes the handshake as soon as the container starts -- while
	// `apk add mosquitto` is still running inside it. A dial-only probe
	// therefore returns "ready" against a broker that does not exist yet, and
	// the first CONNECT vanishes into a broken pipe.
	waitForLog(t, "mosquitto", logs, 0, "running", 120*time.Second)

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	waitForListener(t, addr, 30*time.Second, "mosquitto", logs)
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

	// The container is NAMED so teardown can address it directly. Killing the
	// `docker run` process only kills the local CLI -- the container keeps
	// running, keeps the published port bound, and survives the test binary.
	// Three orphans accumulated on this machine before that was caught.
	name := fmt.Sprintf("meshtk-e2e-%d", port)
	cmd := exec.Command("docker", "run", "--rm", "--name", name,
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
		_ = exec.Command("docker", "rm", "-f", name).Run()
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
	// root is the top-level *testing.T. Connections that must survive the
	// subtest that created them register their teardown here; registering it on
	// the subtest's t closes them one subtest later.
	root *testing.T

	n         *ServerCmd
	proxyAddr string
	broker    *e2eBroker
	proxyLog  *syncBuffer
	auth      *pairAuthenticator

	v5c *v5Client
	v4c *mqtt3Client
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

	// Whole-run capture on a red run. Each subtest already dumps its own tail,
	// but a failure late in the matrix is usually explained by something the
	// broker logged earlier -- and re-running an e2e by hand to find out is
	// exactly the loop this is meant to remove.
	t.Cleanup(func() {
		if !t.Failed() && !testing.Verbose() {
			return
		}
		t.Logf("=== FULL mosquitto log ===\n%s", broker.logs.String())
		t.Logf("=== FULL proxy inspector log ===\n%s", proxyLog.String())
	})

	return &e2eHarness{
		root:      t,
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

// dialV5 deliberately registers NO cleanup of its own. The caller owns the
// socket's lifetime, because the two lifetimes in this test are different: the
// throwaway clients in the rejection subtests die with their subtest, while the
// v5 client used by the traffic matrix has to outlive the subtest that created
// it. Registering t.Cleanup here silently closed the shared connection the
// instant the CONNECT subtest returned, and every later subtest failed with
// "use of closed network connection".
func dialV5(t *testing.T, addr string) *v5Client {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	return &v5Client{conn: conn, r: bufio.NewReader(conn)}
}

func (c *v5Client) close() { _ = c.conn.Close() }

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

// expectFrame reads one frame and requires it to be of the wanted type. It is
// deliberately STRICT rather than "read until you see what you want": an
// unexpected packet here is itself a finding. The v5 client is subscribed to
// the topic it publishes on, so if downlink self-echo suppression regressed,
// the client's own PUBLISH would come back and land in this read -- and the
// test would report it instead of quietly skipping past it.
func (c *v5Client) expectFrame(t *testing.T, want byte, timeout time.Duration) []byte {
	t.Helper()
	frame, typ := c.mustReadFrame(t, timeout)
	if typ != want {
		t.Fatalf("expected packet type %d, got %d (%s)", want, typ, hex.EncodeToString(frame))
	}
	return frame
}

// ---------------------------------------------------------------------------
// Meshtastic fixtures
// ---------------------------------------------------------------------------

// e2eEnvelope builds a DECODED NODEINFO ServiceEnvelope. Decoded, not
// encrypted, because an encrypted payload with no configured cipher trips
// BlockInvalidEncryption before the forward -- so this fixture keeps the e2e
// free of channel-key setup while still exercising the hop clamp, the decider
// and the ALLOW path.
//
// This comment used to add that a TEXT_MESSAGE payload reaches
// RewritePayloadString, which dereferences a nil cipher and panics on a
// non-encrypted packet. That was true and is no longer: 68-REVIEW CR-01 is
// closed by 69-01 (the matcher declines, the helper returns an error) and the
// non-crash is asserted on both codecs in rules_rewrite_test.go.
func e2eEnvelope(t *testing.T, gateway string, from, hopLimit, hopStart uint32) []byte {
	t.Helper()
	user, err := proto.Marshal(&meshtastic.User{Id: gateway, LongName: "DC34 e2e", ShortName: "E2E"})
	if err != nil {
		t.Fatalf("marshal user: %v", err)
	}
	payload, err := proto.Marshal(&meshtastic.ServiceEnvelope{
		Packet: &meshtastic.MeshPacket{
			From:     from,
			To:       0xffffffff,
			Id:       0x1234abcd,
			HopLimit: hopLimit,
			HopStart: hopStart,
			PayloadVariant: &meshtastic.MeshPacket_Decoded{
				Decoded: &meshtastic.Data{
					Portnum:  meshtastic.PortNum_NODEINFO_APP,
					Payload:  user,
					Bitfield: proto.Uint32(1),
				},
			},
		},
		GatewayId: gateway,
		ChannelId: "dc.run",
	})
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	return payload
}

func e2eDecodeEnvelope(t *testing.T, payload []byte) *meshtastic.ServiceEnvelope {
	t.Helper()
	env := new(meshtastic.ServiceEnvelope)
	if err := proto.Unmarshal(payload, env); err != nil {
		t.Fatalf("decode ServiceEnvelope off the wire: %v", err)
	}
	return env
}

func e2ePublishPacket(topic string, qos byte, packetID uint16, payload []byte) *v5.ControlPacket {
	cp := v5.NewControlPacket(v5.PUBLISH)
	p := cp.Content.(*v5.Publish)
	p.Topic = topic
	p.QoS = qos
	p.PacketID = packetID
	p.Payload = payload
	p.Properties = &v5.Properties{User: []v5.User{{Key: "src", Value: "meshtk-e2e"}}}
	return cp
}

// ---------------------------------------------------------------------------
// 3.1.1 client
// ---------------------------------------------------------------------------

// mqtt3Client is the real paho.mqtt.golang client -- the SAME library the
// production 3.1.1 fleet effectively speaks -- pointed at the SAME proxy
// instance as the v5 client. It is the local stand-in for the production
// requirement that the iOS/firmware fleet keeps flowing while Android v5
// clients connect.
type mqtt3Client struct {
	c        mqtt.Client
	messages chan mqtt.Message
}

func dialMQTT3(t *testing.T, addr, clientID string) *mqtt3Client {
	t.Helper()

	m := &mqtt3Client{messages: make(chan mqtt.Message, 32)}

	opts := mqtt.NewClientOptions()
	opts.AddBroker("tcp://" + addr)
	opts.SetClientID(clientID)
	opts.SetUsername(e2eClientUser)
	opts.SetPassword(e2eClientPass)
	opts.SetProtocolVersion(4) // 4 == MQTT 3.1.1; mosquitto logs it as p4
	opts.SetCleanSession(true)
	opts.SetAutoReconnect(false)
	opts.SetConnectTimeout(15 * time.Second)
	opts.SetKeepAlive(60 * time.Second)
	opts.SetDefaultPublishHandler(func(_ mqtt.Client, msg mqtt.Message) {
		select {
		case m.messages <- msg:
		default:
		}
	})

	m.c = mqtt.NewClient(opts)
	tok := m.c.Connect()
	if !tok.WaitTimeout(20 * time.Second) {
		t.Fatalf("3.1.1 CONNECT timed out")
	}
	if err := tok.Error(); err != nil {
		t.Fatalf("3.1.1 CONNECT failed: %v", err)
	}
	// Teardown is the caller's, for the same lifetime reason as dialV5.
	return m
}

func (m *mqtt3Client) subscribe(t *testing.T, filter string) {
	t.Helper()
	tok := m.c.Subscribe(filter, 0, nil)
	if !tok.WaitTimeout(10 * time.Second) {
		t.Fatalf("3.1.1 SUBSCRIBE to %s timed out", filter)
	}
	if err := tok.Error(); err != nil {
		t.Fatalf("3.1.1 SUBSCRIBE to %s failed: %v", filter, err)
	}
}

func (m *mqtt3Client) publish(t *testing.T, topic string, payload []byte) {
	t.Helper()
	tok := m.c.Publish(topic, 0, false, payload)
	if !tok.WaitTimeout(10 * time.Second) {
		t.Fatalf("3.1.1 PUBLISH to %s timed out", topic)
	}
	if err := tok.Error(); err != nil {
		t.Fatalf("3.1.1 PUBLISH to %s failed: %v", topic, err)
	}
}

func (m *mqtt3Client) expectMessage(t *testing.T, timeout time.Duration) mqtt.Message {
	t.Helper()
	select {
	case msg := <-m.messages:
		return msg
	case <-time.After(timeout):
		t.Fatalf("3.1.1 client received no downlink within %s", timeout)
		return nil
	}
}

// brokerConnections returns the mosquitto "New client connected" lines, which
// carry the protocol marker (p4 / p5) and the authenticated identity.
func brokerConnections(logs string) []string {
	var out []string
	for _, line := range strings.Split(logs, "\n") {
		if strings.Contains(line, "New client connected") {
			out = append(out, line)
		}
	}
	return out
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
		t.Cleanup(c.close)
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
		t.Cleanup(c.close)
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
		h.root.Cleanup(h.v5c.close)
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

	// --- The traffic matrix -------------------------------------------------
	//
	// From here on the subtests share one long-lived v5 connection and one
	// long-lived 3.1.1 connection, INTERLEAVED on purpose: the 3.1.1 client
	// comes up second, receives the v5 client's traffic, publishes traffic the
	// v5 client receives, and only disconnects at the end. Running the two
	// protocols back to back instead would prove nothing about coexistence.

	t.Run("mqtt3_client_connects_alongside_the_v5_client", func(t *testing.T) {
		if h.v5c == nil {
			t.Skip("v5 connection was not established (run the whole test, not a single subtest)")
		}
		brokerMark, proxyMark := h.broker.logs.Len(), h.proxyLog.Len()

		h.v4c = dialMQTT3(t, h.proxyAddr, "paho-e2e-v4")
		h.root.Cleanup(func() { h.v4c.c.Disconnect(250) })
		h.v4c.subscribe(t, e2eTopicFilter)

		waitForLog(t, "mosquitto", h.broker.logs, brokerMark, "New client connected", 10*time.Second)

		// Scan the WHOLE log, not the tail: the v5 connection was made in the
		// previous subtest and is still open, so "both protocols alive in the
		// same run" is a statement about the run, not about this tail.
		//
		// The markers are mosquitto's INTERNAL protocol enum, not the wire
		// protocol level: mosq_p_mqtt311 == 2 and mosq_p_mqtt5 == 5, so a 3.1.1
		// client logs as p2 even though its CONNECT carries protocol level 4.
		// Verified on 2.0.22 (Homebrew) and 2.0.20 (alpine:3.21, the prod base).
		var sawV5, sawV311 bool
		lines := brokerConnections(h.broker.logs.String())
		for _, line := range lines {
			if strings.Contains(line, "p5") {
				sawV5 = true
			}
			if strings.Contains(line, "p2") {
				sawV311 = true
			}
		}
		if !sawV5 || !sawV311 {
			h.dump(t, 0, proxyMark)
			t.Fatalf("expected both a p5 (MQTT 5.0) and a p2 (MQTT 3.1.1) connection in the same run (p5=%v p2=%v).\nconnection lines:\n%s",
				sawV5, sawV311, strings.Join(lines, "\n"))
		}
	})

	t.Run("v5_subscribe_relayed_raw_and_suback_returned", func(t *testing.T) {
		if h.v5c == nil {
			t.Skip("v5 connection was not established")
		}
		brokerMark, proxyMark := h.broker.logs.Len(), h.proxyLog.Len()

		sub := v5.NewControlPacket(v5.SUBSCRIBE)
		s := sub.Content.(*v5.Subscribe)
		s.PacketID = 1
		s.Subscriptions = []v5.SubOptions{{Topic: e2eTopicFilter, QoS: 0}}
		h.v5c.send(t, sub)

		frame := h.v5c.expectFrame(t, v5.SUBACK, 10*time.Second)
		if got := hex.EncodeToString(frame); got != "900400010000" {
			h.dump(t, brokerMark, proxyMark)
			t.Fatalf("SUBACK = %s, want 900400010000 (packet id 1, one granted QoS0 subscription)", got)
		}

		waitForLog(t, "mosquitto", h.broker.logs, brokerMark, "Received SUBSCRIBE", 10*time.Second)
	})

	// WR-04's observability half. The subtest above only ever checked the
	// BROKER's side, and a SUBACK comes back whether or not the proxy ever
	// looked at the SUBSCRIBE -- which is exactly how "topic rules" quietly
	// meant "topic rules for 3.1.1 clients" for a whole release.
	t.Run("v5_subscribe_is_logged_by_the_proxy", func(t *testing.T) {
		if h.v5c == nil {
			t.Skip("v5 connection was not established")
		}
		brokerMark, proxyMark := h.broker.logs.Len(), h.proxyLog.Len()

		// A filter nothing else in the run uses, so the assertion is falsifiable
		// rather than satisfied by an earlier subtest's line.
		const observabilityFilter = "msh/US/2/e/dc.run/observability/#"

		sub := v5.NewControlPacket(v5.SUBSCRIBE)
		s := sub.Content.(*v5.Subscribe)
		s.PacketID = 2
		s.Subscriptions = []v5.SubOptions{{Topic: observabilityFilter, QoS: 0}}
		h.v5c.send(t, sub)

		// The decision log is written before the frame is relayed, so the SUBACK
		// coming back is a happens-after edge for it. No sleep required.
		h.v5c.expectFrame(t, v5.SUBACK, 10*time.Second)

		tail := h.proxyLog.since(proxyMark)
		if !strings.Contains(tail, "mqtt_type=SUBSCRIBE") {
			h.dump(t, brokerMark, proxyMark)
			t.Fatalf("the proxy never recorded the SUBSCRIBE.\n--- proxy log ---\n%s", tail)
		}
		if !strings.Contains(tail, observabilityFilter) {
			h.dump(t, brokerMark, proxyMark)
			t.Fatalf("the proxy recorded a SUBSCRIBE without its topic filter %q.\n--- proxy log ---\n%s",
				observabilityFilter, tail)
		}
	})

	// CR-03 against a live broker. The unit test proves zero bytes reach a
	// recording writer; this proves mosquitto never opens a session for -- and
	// never even sees -- the second CONNECT's identity, which is the claim SC3
	// actually makes.
	t.Run("v5_second_connect_refused_and_broker_never_sees_it", func(t *testing.T) {
		const (
			secondConnectID  = "mqttastic-e2e-second-connect"
			attackerClientID = "mqttastic-e2e-attacker"
			attackerUser     = "attacker-username"
			attackerPass     = "attacker-plaintext-password"
		)
		brokerMark, proxyMark := h.broker.logs.Len(), h.proxyLog.Len()

		// Its own connection: sending a second CONNECT correctly ends the
		// session, and the shared v5 client is still needed below.
		c := dialV5(t, h.proxyAddr)
		defer c.close()

		c.send(t, connectPacket(secondConnectID, e2eClientUser, e2eClientPass))
		if frame := c.expectFrame(t, v5.CONNACK, 10*time.Second); hex.EncodeToString(frame) != e2eStrippedConnackHex {
			h.dump(t, brokerMark, proxyMark)
			t.Fatalf("establishing CONNACK = %s, want %s", hex.EncodeToString(frame), e2eStrippedConnackHex)
		}
		// POLL for the positive broker-side signal that this session landed --
		// never a fixed sleep.
		waitForLog(t, "mosquitto", h.broker.logs, brokerMark, secondConnectID, 10*time.Second)

		violationMark := h.broker.logs.Len()
		c.send(t, connectPacket(attackerClientID, attackerUser, attackerPass))

		frame := c.expectFrame(t, v5.DISCONNECT, 10*time.Second)
		if got := hex.EncodeToString(frame); got != "e0028200" {
			t.Fatalf("answer to a second CONNECT = %s, want e0028200 (DISCONNECT, protocol error 0x82)", got)
		}
		// A CONNACK would be illegal here (MQTT 5.0 3.2), and the socket must
		// then close rather than carry on serving a client that just violated
		// the protocol.
		if _, _, err := c.readFrame(5 * time.Second); err == nil {
			t.Fatal("the proxy kept the session open after answering a protocol violation")
		}

		// Reading that DISCONNECT is a happens-after edge for everything the
		// proxy did with the frame, and the refusal happens before ANY write to
		// the broker -- so this needs no settle sleep either.
		tail := h.broker.logs.since(violationMark)
		for _, secret := range []string{attackerClientID, attackerUser, attackerPass} {
			if strings.Contains(tail, secret) {
				h.dump(t, violationMark, proxyMark)
				t.Fatalf("the second CONNECT's %q reached mosquitto.\n--- mosquitto log ---\n%s", secret, tail)
			}
		}
		if proxyTail := h.proxyLog.since(proxyMark); !strings.Contains(proxyTail, "action=MQTT5_PROTOCOL_VIOLATION") {
			t.Fatalf("proxy log missing action=MQTT5_PROTOCOL_VIOLATION.\n--- proxy log ---\n%s", proxyTail)
		}
	})

	t.Run("v5_qos0_publish_forwarded_and_reaches_the_mqtt3_client", func(t *testing.T) {
		if h.v5c == nil || h.v4c == nil {
			t.Skip("both clients are required for the cross-codec assertion")
		}
		brokerMark, proxyMark := h.broker.logs.Len(), h.proxyLog.Len()

		// Hops already within the clamp, so no rule mutates this packet and the
		// captured frame is what should be forwarded.
		h.v5c.send(t, e2ePublishPacket(e2eV5Topic, 0, 0, e2eEnvelope(t, e2eV5Gateway, e2eV5From, 3, 3)))

		tail := waitForLog(t, "mosquitto", h.broker.logs, brokerMark, "Received PUBLISH", 10*time.Second)
		if !strings.Contains(tail, "q0") || !strings.Contains(tail, e2eV5Topic) {
			h.dump(t, brokerMark, proxyMark)
			t.Fatalf("mosquitto did not log a q0 PUBLISH on %s.\n--- mosquitto log ---\n%s", e2eV5Topic, tail)
		}

		// Cross-codec delivery: a v5 uplink fanned out to a 3.1.1 subscriber
		// through the same proxy.
		msg := h.v4c.expectMessage(t, 10*time.Second)
		if msg.Topic() != e2eV5Topic {
			t.Fatalf("3.1.1 client received topic %q, want %q", msg.Topic(), e2eV5Topic)
		}
		if gw := e2eDecodeEnvelope(t, msg.Payload()).GetGatewayId(); gw != e2eV5Gateway {
			t.Fatalf("3.1.1 client received gateway %q, want %q", gw, e2eV5Gateway)
		}
	})

	t.Run("v5_qos1_publish_hop_clamped_pubacked_and_allowed", func(t *testing.T) {
		if h.v5c == nil || h.v4c == nil {
			t.Skip("both clients are required for the cross-codec assertion")
		}
		brokerMark, proxyMark := h.broker.logs.Len(), h.proxyLog.Len()

		// HopLimit 7 / HopStart 9 is over budget, so RewriteHopLimit fires,
		// RemarshalEnvelope re-encodes and the forwarded frame must differ from
		// the captured one -- while the packet id survives the round trip.
		h.v5c.send(t, e2ePublishPacket(e2eV5Topic, 1, e2eQoS1PacketID, e2eEnvelope(t, e2eV5Gateway, e2eV5From, 7, 9)))

		// The broker's view: same QoS, same message id.
		tail := waitForLog(t, "mosquitto", h.broker.logs, brokerMark, "Received PUBLISH", 10*time.Second)
		if !strings.Contains(tail, "q1") || !strings.Contains(tail, e2eQoS1MessageID) {
			h.dump(t, brokerMark, proxyMark)
			t.Fatalf("mosquitto did not log a q1 PUBLISH with %s.\n--- mosquitto log ---\n%s", e2eQoS1MessageID, tail)
		}

		// The QoS1 acknowledgement has to come back to the RIGHT client, and
		// with the packet id the client chose -- the frame relay must not
		// rewrite it (paho.golang would inflate 40021234 to 400412340000 if the
		// PUBACK were parsed and re-encoded instead of relayed).
		puback := h.v5c.expectFrame(t, v5.PUBACK, 10*time.Second)
		if len(puback) < 4 || puback[2] != 0x12 || puback[3] != 0x34 {
			t.Fatalf("PUBACK = %s, want packet id 0x1234 at bytes 2..3", hex.EncodeToString(puback))
		}

		if proxyTail := h.proxyLog.since(proxyMark); !strings.Contains(proxyTail, "action=ALLOW") {
			t.Fatalf("proxy log missing action=ALLOW for the QoS1 publish.\n--- proxy log ---\n%s", proxyTail)
		}

		// The clamp reached the WIRE, not just the struct: the 3.1.1 subscriber
		// decodes the envelope the broker actually fanned out. This is the
		// meshtk#22 assertion, end to end through a real broker.
		msg := h.v4c.expectMessage(t, 10*time.Second)
		pkt := e2eDecodeEnvelope(t, msg.Payload()).GetPacket()
		if pkt.GetHopLimit() != 3 || pkt.GetHopStart() != 7 {
			h.dump(t, brokerMark, proxyMark)
			t.Fatalf("hop budget delivered as limit=%d start=%d, want 3/7 (clamp did not reach the wire)",
				pkt.GetHopLimit(), pkt.GetHopStart())
		}
	})

	t.Run("v5_downlink_carries_the_full_topic_and_no_alias", func(t *testing.T) {
		if h.v5c == nil || h.v4c == nil {
			t.Skip("both clients are required for the downlink assertion")
		}
		brokerMark, proxyMark := h.broker.logs.Len(), h.proxyLog.Len()

		// Published by the 3.1.1 client under a DIFFERENT gateway id, so the
		// proxy's self-echo suppression does not (correctly) eat it.
		h.v4c.publish(t, e2eV4Topic, e2eEnvelope(t, e2eV4Gateway, e2eV4From, 3, 3))

		frame := h.v5c.expectFrame(t, v5.PUBLISH, 10*time.Second)
		cp, err := v5.ReadPacket(bytes.NewReader(frame))
		if err != nil {
			h.dump(t, brokerMark, proxyMark)
			t.Fatalf("parse downlink PUBLISH %s: %v", hex.EncodeToString(frame), err)
		}
		p := cp.Content.(*v5.Publish)
		if p.Topic != e2eV4Topic {
			t.Fatalf("downlink topic = %q, want the FULL topic %q", p.Topic, e2eV4Topic)
		}
		if p.Properties != nil && p.Properties.TopicAlias != nil {
			t.Fatalf("downlink carried TopicAlias=%d; the alias suppression regressed and topic rules are blind",
				*p.Properties.TopicAlias)
		}
		if gw := e2eDecodeEnvelope(t, p.Payload).GetGatewayId(); gw != e2eV4Gateway {
			t.Fatalf("downlink gateway = %q, want %q", gw, e2eV4Gateway)
		}
	})

	// PROBE-A end to end. The verifier reproduced CR-04 by wrapping an unclamped
	// hop_limit=7 envelope in a property id paho.golang does not model and
	// watching it sail past the topic guard, the inspector, the decider, the hop
	// clamp and every Block rule.
	//
	// MEASURED, and it corrects this plan's assumption: mosquitto 2.0.22 answers
	// a client-chosen UNKNOWN property id with a malformed-packet disconnect,
	// because MQTT 5.0 2.2.2 makes an unrecognised property exactly that. No
	// spec-conformant broker will route this frame, so its hop values cannot be
	// read off a 3.1.1 subscriber -- that is the BROKER enforcing the spec on the
	// client, not the proxy failing, and it is why this subtest runs on its own
	// connection instead of killing the shared one.
	//
	// What CR-04 made impossible and what is proven live here is everything up to
	// that point: the proxy decoded the ServiceEnvelope inside a frame its codec
	// refused, ran it through the rules engine, logged the decision with the
	// right topic and the right mesh type, and relayed it only afterwards. The
	// clamped BYTES on the same frame are pinned by
	// TestV5UnmodelledPropertyPublishIsClamped, which reads the forwarded frame
	// directly -- the one assertion a spec-conformant broker cannot host.
	t.Run("v5_unmodelled_property_publish_is_clamped_end_to_end", func(t *testing.T) {
		const rawPublishClientID = "mqttastic-e2e-unmodelled-property"
		brokerMark, proxyMark := h.broker.logs.Len(), h.proxyLog.Len()

		c := dialV5(t, h.proxyAddr)
		defer c.close()
		c.send(t, connectPacket(rawPublishClientID, e2eClientUser, e2eClientPass))
		c.expectFrame(t, v5.CONNACK, 10*time.Second)
		waitForLog(t, "mosquitto", h.broker.logs, brokerMark, rawPublishClientID, 10*time.Second)

		publishMark := h.proxyLog.Len()
		frame := unmodelledPropertyFrame(t, e2eV5Topic, e2eEnvelope(t, e2eV5Gateway, e2eV5From, 7, 9))
		c.sendRaw(t, frame)

		// 1. The CR-04 trigger really fired: paho.golang refused the frame.
		waitForLog(t, "proxy", h.proxyLog, publishMark, "action=MQTT5_PARSE_FAIL", 10*time.Second)

		// 2. ...and the hand parser read it anyway, so nothing was refused for
		//    being unreadable.
		if tail := h.proxyLog.since(publishMark); strings.Contains(tail, "MQTT5_PUBLISH_HEADER_FAIL") {
			h.dump(t, brokerMark, proxyMark)
			t.Fatalf("the hand parser could not read a frame the wire format permits.\n--- proxy log ---\n%s", tail)
		}

		// 3. The entire inspection chain CR-04 skipped, running live: topic
		//    recorded, envelope decoded, packet judged.
		tail := waitForLog(t, "proxy", h.proxyLog, publishMark, "action=ALLOW", 10*time.Second)
		for _, want := range []string{"mqtt_type=PUBLISH", e2eV5Topic, "mesh_type=NODEINFO_APP", "mesh_from=435990e4"} {
			if !strings.Contains(tail, want) {
				h.dump(t, brokerMark, proxyMark)
				t.Fatalf("the decision log for a codec-refused PUBLISH is missing %q.\n--- proxy log ---\n%s", want, tail)
			}
		}

		// 4. The forwarded bytes reached mosquitto -- the frame was relayed after
		//    inspection, not dropped -- and mosquitto answered the client's own
		//    unknown property as MQTT 5.0 requires.
		waitForLog(t, "mosquitto", h.broker.logs, brokerMark,
			"Client "+rawPublishClientID+" disconnected due to malformed packet", 15*time.Second)
	})

	// CR-02 end to end. The reaper deletes any ConnTrack entry idle for 180s
	// while the Meshtastic publish cadence is far longer than that, so before
	// touchConnTrack a keepalive-only session lost its entry and its next
	// PUBLISH was Blocked with "Username required for MQTT" and the socket torn
	// down. Backdating the entry is the honest way to test it: the reaper's
	// predicate is the thing that matters, and a real 180s sleep does not belong
	// in a test.
	t.Run("v5_idle_session_survives_and_publishes", func(t *testing.T) {
		if h.v5c == nil || h.v4c == nil {
			t.Skip("both clients are required for the cross-codec assertion")
		}
		brokerMark, proxyMark := h.broker.logs.Len(), h.proxyLog.Len()

		// The proxy keys ConnTrack on conn.RemoteAddr(), which from the client's
		// side is its own local address.
		addr := h.v5c.conn.LocalAddr().String()

		h.n.ConnMutex.Lock()
		entry, ok := h.n.ConnTrack[addr]
		if ok {
			entry.ConnectTime = time.Now().Unix() - 200
		}
		h.n.ConnMutex.Unlock()
		if !ok {
			t.Fatalf("no ConnTrack entry for %s; the harness cannot age a session it cannot find", addr)
		}

		// Keepalives only -- exactly what a phone sends between position reports.
		h.v5c.sendRaw(t, []byte{0xc0, 0x00})
		h.v5c.expectFrame(t, v5.PINGRESP, 10*time.Second)

		// The reaper's OWN predicate, verbatim from SetupTracker.
		h.n.ConnMutex.Lock()
		refreshed, stillThere := h.n.ConnTrack[addr]
		var age int64
		if stillThere {
			age = time.Now().Unix() - refreshed.ConnectTime
		}
		h.n.ConnMutex.Unlock()
		if !stillThere {
			t.Fatalf("the ConnTrack entry for %s vanished across a keepalive", addr)
		}
		if age > 180 {
			t.Fatalf("the reaper would purge this entry (age %ds > 180) despite a keepalive", age)
		}

		h.v5c.send(t, e2ePublishPacket(e2eV5Topic, 0, 0, e2eEnvelope(t, e2eV5Gateway, e2eV5From, 3, 3)))

		msg := h.v4c.expectMessage(t, 10*time.Second)
		if gw := e2eDecodeEnvelope(t, msg.Payload()).GetGatewayId(); gw != e2eV5Gateway {
			h.dump(t, brokerMark, proxyMark)
			t.Fatalf("3.1.1 client received gateway %q, want %q", gw, e2eV5Gateway)
		}
		if tail := h.proxyLog.since(proxyMark); strings.Contains(tail, "Username required for MQTT") {
			h.dump(t, brokerMark, proxyMark)
			t.Fatalf("an aged-but-live session was Blocked for a missing username (CR-02).\n--- proxy log ---\n%s", tail)
		}
	})

	t.Run("v5_pingreq_gets_pingresp", func(t *testing.T) {
		if h.v5c == nil {
			t.Skip("v5 connection was not established")
		}
		brokerMark := h.broker.logs.Len()

		h.v5c.sendRaw(t, []byte{0xc0, 0x00})
		frame := h.v5c.expectFrame(t, v5.PINGRESP, 10*time.Second)
		if got := hex.EncodeToString(frame); got != "d000" {
			t.Fatalf("PINGRESP = %s, want d000", got)
		}
		waitForLog(t, "mosquitto", h.broker.logs, brokerMark, "Received PINGREQ", 10*time.Second)
	})

	t.Run("mqtt3_client_disconnects_cleanly", func(t *testing.T) {
		if h.v4c == nil {
			t.Skip("3.1.1 connection was not established")
		}
		brokerMark := h.broker.logs.Len()
		h.v4c.c.Disconnect(250)
		waitForLog(t, "mosquitto", h.broker.logs, brokerMark, "Received DISCONNECT", 10*time.Second)
	})

	t.Run("v5_zero_length_disconnect_is_graceful", func(t *testing.T) {
		if h.v5c == nil {
			t.Skip("v5 connection was not established")
		}
		brokerMark := h.broker.logs.Len()

		// e000 is a legal v5 DISCONNECT (MQTT 5.0 3.14.2.1: reason code and
		// properties may both be omitted) and paho.golang returns EOF trying to
		// parse it. This single assertion is why the whole v5 path captures
		// frames before it parses: a parse-everything relay would tear the
		// socket down here and mosquitto would log an abnormal disconnection
		// instead of receiving the packet.
		h.v5c.sendRaw(t, []byte{0xe0, 0x00})
		waitForLog(t, "mosquitto", h.broker.logs, brokerMark, "Received DISCONNECT", 10*time.Second)
	})
}
