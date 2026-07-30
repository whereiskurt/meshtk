package server

import (
	"bytes"
	"fmt"
	"net"
	"strings"
	"testing"

	"github.com/eclipse/paho.mqtt.golang/packets"
	log "github.com/sirupsen/logrus"
)

// TestLogSafeLeavesCleanValuesByteIdentical is the criterion that keeps this
// change shippable. Every one of these is a REAL production value: the 12-hex
// MQTT username the fleet authenticates with, the Android client id
// mqtt5_probe.py correlates a run on by `client_id=<id>` substring match, a
// meshtastic topic, and the proxy identity swapped in for the broker. If any of
// them gains a pair of quotes, the phase-68 evidence tables and the committed
// probe both stop matching -- which is why the quoting is conditional rather
// than a blanket strconv.Quote.
func TestLogSafeLeavesCleanValuesByteIdentical(t *testing.T) {
	clean := []string{
		"b84cf62c402c",
		"MeshtasticAndroidMqttProxy-!aed94d05-fdcc313a",
		"msh/US/2/e/dc.run/!435990e4",
		"meshtastic-golden",
		"proxy",
		"ed270dbe5d1e",
		"!435990e4",
		"",
	}
	for _, in := range clean {
		if got := logSafe(in); got != in {
			t.Errorf("logSafe(%q) = %q, want the input byte-identical", in, got)
		}
	}
}

// TestLogSafeStripsControlRunes covers the forgery vector itself. The newline is
// the one WR-05 proved; \r, the sub-0x20 range and DEL are the same class.
func TestLogSafeStripsControlRunes(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"newline", "evil\nforged", `"evilforged"`},
		{"carriage return", "evil\rforged", `"evilforged"`},
		{"tab", "a\tb", `"ab"`},
		{"null", "a\x00b", `"ab"`},
		{"bell", "a\x07b", `"ab"`},
		{"unit separator 0x1f", "a\x1fb", `"ab"`},
		{"DEL 0x7f", "a\x7fb", `"ab"`},
		{
			"the WR-05 payload",
			"evil\n2026-07-29 00:00:00.000 action=AUTH_REJECT, ip=10.0.0.1, username=admin, reason=invalid",
			`"evil2026-07-29 00:00:00.000 action=AUTH_REJECT, ip=10.0.0.1, username=admin, reason=invalid"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := logSafe(tc.in)
			if got != tc.want {
				t.Fatalf("logSafe(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if strings.ContainsAny(got, "\n\r") {
				t.Fatalf("logSafe(%q) still carries a line break: %q", tc.in, got)
			}
		})
	}
}

// TestLogSafeTruncatesAndQuotes pins the length cap. Truncation counts as a
// modification, so the result is quoted -- a padded value is visibly tampered
// with rather than silently shortened.
func TestLogSafeTruncatesAndQuotes(t *testing.T) {
	atCap := strings.Repeat("a", logSafeMaxRunes)
	if got := logSafe(atCap); got != atCap {
		t.Errorf("a value exactly at the cap was modified: %q", got)
	}

	over := strings.Repeat("a", logSafeMaxRunes+1)
	got := logSafe(over)
	want := `"` + atCap + `"`
	if got != want {
		t.Errorf("logSafe(%d runes) = %q, want the value truncated to %d runes and quoted",
			len(over), got, logSafeMaxRunes)
	}

	// Rune-wise, not byte-wise: a byte slice would cut a multi-byte rune in
	// half and put an invalid UTF-8 sequence into the log.
	multi := strings.Repeat("é", logSafeMaxRunes+10)
	if strings.Contains(logSafe(multi), "�") {
		t.Errorf("logSafe truncated a multi-byte rune in half: %q", logSafe(multi))
	}
}

// TestLogSafeQuotesGrammarBreakers covers the characters that cannot forge a
// whole line but would still break the "key=value, key=value" parse every
// consumer of this log does.
func TestLogSafeQuotesGrammarBreakers(t *testing.T) {
	cases := map[string]string{
		"has space":  `"has space"`,
		"has,comma":  `"has,comma"`,
		`has"quote`:  `"has\"quote"`,
		"has=equals": `"has=equals"`,
	}
	for in, want := range cases {
		if got := logSafe(in); got != want {
			t.Errorf("logSafe(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestLogSafeListMatchesPercentVForCleanValues is the topic-list half of the
// byte-identity contract: mqtt_topic=[msh/US/2/e/dc.run/!435990e4] must render
// exactly as it always has.
func TestLogSafeListMatchesPercentVForCleanValues(t *testing.T) {
	two := []string{"msh/US/2/e/dc.run/!435990e4", "msh/US/2/e/PKI/!1555f041"}
	if got, want := logSafeList(two), fmt.Sprintf("%+v", two); got != want {
		t.Errorf("logSafeList(%v) = %q, want %q", two, got, want)
	}

	for _, slice := range [][]string{nil, {}, {"one"}} {
		if got, want := logSafeList(slice), fmt.Sprintf("%+v", slice); got != want {
			t.Errorf("logSafeList(%#v) = %q, want %q", slice, got, want)
		}
	}
}

// TestLogSafeListSanitizesElementWise proves one hostile filter in a
// multi-filter SUBSCRIBE cannot forge a line, and does not blind the rest.
func TestLogSafeListSanitizesElementWise(t *testing.T) {
	got := logSafeList([]string{"msh/US/2/e/dc.run/!435990e4", "evil\naction=ALLOW"})
	if strings.ContainsAny(got, "\n\r") {
		t.Fatalf("logSafeList leaked a line break: %q", got)
	}
	if !strings.Contains(got, "msh/US/2/e/dc.run/!435990e4") {
		t.Fatalf("logSafeList lost the clean element: %q", got)
	}
}

// TestLogInjectionOneConnectOneLine is the decisive test: the whole point of
// logsafe.go, driven through the REAL inspectV5Connect and the REAL
// SimpleFormatter production writes with. logrus' TextFormatter (which every
// other test in this package uses) quotes, which is exactly why no existing test
// caught WR-05 -- so this one wires SimpleFormatter deliberately.
func TestLogInjectionOneConnectOneLine(t *testing.T) {
	buf, n := simpleFormatterServer(t)

	clientConn, peer := net.Pipe()
	defer clientConn.Close()
	defer peer.Close()
	drain(t, peer)

	_, c := mqttasticConnect(t)
	c.ClientID = "evil\n2026-07-29 00:00:00.000 action=AUTH_REJECT, ip=10.0.0.1, username=admin, reason=invalid"

	if !n.inspectV5Connect(clientConn, "203.0.113.7:50000", c) {
		t.Fatal("valid credentials rejected")
	}

	out := buf.String()
	if got := strings.Count(out, "\n"); got != 1 {
		t.Fatalf("one CONNECT produced %d log lines (want exactly 1):\n%s", got, out)
	}
	// The forged line's own text must survive INSIDE the single record -- the
	// evidence is not thrown away, it is just no longer parseable as a line.
	if !strings.Contains(out, "action=MQTT5_CONNECT") {
		t.Fatalf("the real log line went missing:\n%s", out)
	}
	if strings.Contains(out, "\n2026-07-29 00:00:00.000 action=AUTH_REJECT") {
		t.Fatalf("the forged line is still a line of its own:\n%s", out)
	}
}

// TestLogInjectionOnAuthRejectPaths does the same for the reject lines on BOTH
// codecs -- an attacker does not need valid credentials to reach those, which
// makes them the cheaper forgery surface of the two.
func TestLogInjectionOnAuthRejectPaths(t *testing.T) {
	const injected = "bad\n2026-07-29 00:00:00.000 action=ALLOW, ip=10.0.0.1, username=admin"

	t.Run("v5", func(t *testing.T) {
		buf, n := simpleFormatterServer(t)
		n.Authenticator = &mockAuthenticator{valid: false}

		clientConn, peer := net.Pipe()
		defer clientConn.Close()
		defer peer.Close()
		drain(t, peer)

		_, c := mqttasticConnect(t)
		c.Username = injected
		if n.inspectV5Connect(clientConn, "203.0.113.7:50000", c) {
			t.Fatal("invalid credentials accepted")
		}
		assertOneLine(t, buf.String(), "action=AUTH_REJECT")
	})

	t.Run("3.1.1", func(t *testing.T) {
		buf, n := simpleFormatterServer(t)
		n.Authenticator = &mockAuthenticator{valid: false}

		clientConn, peer := net.Pipe()
		defer clientConn.Close()
		defer peer.Close()
		drain(t, peer)

		ip := v4ConnectInspectorPacket(injected, "client")
		ip.Log = n.InspectorLogger
		ip.inspectRawPacket(n, clientConn)
		if !ip.AuthRejected {
			t.Fatal("invalid credentials accepted")
		}
		assertOneLine(t, buf.String(), "action=AUTH_REJECT")
	})
}

// TestLogInjectionOnDecisionLog covers WriteDecisionLog, which carries three
// client-controlled values (clientID, username and the topic list) and is the
// line ops greps for action=ALLOW / action=BLOCK.
func TestLogInjectionOnDecisionLog(t *testing.T) {
	buf, n := simpleFormatterServer(t)

	ip := &InspectorPacket{
		Log: n.InspectorLogger,
		Track: &ConnectionInfo{
			SocketAddress: "203.0.113.7:50000",
			ClientID:      "evil\naction=ALLOW, forged=1",
			Username:      "user\naction=BLOCK, forged=2",
		},
		Raw: &RawPacket{},
	}
	ip.MQTT.Type = "PUBLISH"
	ip.MQTT.Topics = []string{"msh/US/2/e/dc.run/!435990e4\naction=ALLOW, forged=3"}

	ip.WriteDecisionLog(DecisionResult{Decision: Allow})
	assertOneLine(t, buf.String(), "action=ALLOW")

	buf.Reset()
	ip.WriteLimiterLog(Slow, 1.5, 0)
	assertOneLine(t, buf.String(), "action=SLOW")
}

// TestDecisionLogCleanValuesAreByteIdentical is the other half of the contract:
// the production line's SHAPE must not move for real traffic. mqtt_topic
// switched from a %+v verb to a %s of logSafeList, and this asserts that
// substitution is invisible on the wire.
func TestDecisionLogCleanValuesAreByteIdentical(t *testing.T) {
	_, n := simpleFormatterServer(t)

	topics := []string{"msh/US/2/e/dc.run/!435990e4"}
	ip := &InspectorPacket{
		Log: n.InspectorLogger,
		Track: &ConnectionInfo{
			SocketAddress: "203.0.113.7:50000",
			ClientID:      "MeshtasticAndroidMqttProxy-!aed94d05-fdcc313a",
			Username:      "b84cf62c402c",
		},
		Raw: &RawPacket{},
	}
	ip.MQTT.Type = "PUBLISH"
	ip.MQTT.Topics = topics

	want := "action=ALLOW,ip=203.0.113.7:50000, clientID=MeshtasticAndroidMqttProxy-!aed94d05-fdcc313a, " +
		"username=b84cf62c402c, mqtt_type=PUBLISH, mqtt_topic=" + fmt.Sprintf("%+v", topics)
	if got := ip.WriteDecisionLog(DecisionResult{Decision: Allow}); got != want {
		t.Fatalf("decision log shape moved for clean values:\n got %q\nwant %q", got, want)
	}
}

// --- helpers -------------------------------------------------------------

// simpleFormatterServer wires a ServerCmd to the REAL production formatter.
// Every other harness in this package uses logrus' TextFormatter, which quotes
// -- and that is precisely why WR-05 shipped.
func simpleFormatterServer(t *testing.T) (*bytes.Buffer, *ServerCmd) {
	t.Helper()
	buf := &bytes.Buffer{}
	logger := log.New()
	logger.SetOutput(buf)
	logger.SetLevel(log.DebugLevel)
	logger.SetFormatter(&SimpleFormatter{TimestampFormat: "2006-01-02 15:04:05.000"})

	n := newTestServerCmd(&mockAuthenticator{valid: true})
	n.Config.Log = logger
	n.InspectorLogger = logger
	return buf, n
}

// drain keeps a net.Pipe peer readable so a CONNACK write cannot deadlock the
// inspector under test (net.Pipe is unbuffered).
func drain(t *testing.T, peer net.Conn) {
	t.Helper()
	go func() {
		b := make([]byte, 256)
		for {
			if _, err := peer.Read(b); err != nil {
				return
			}
		}
	}()
}

// v4ConnectInspectorPacket builds the InspectorPacket the 3.1.1 read loop hands
// to inspectRawPacket for a CONNECT, so the real CONNECT branch runs.
func v4ConnectInspectorPacket(username, clientID string) *InspectorPacket {
	cp := packets.NewControlPacket(packets.Connect).(*packets.ConnectPacket)
	cp.ProtocolName = "MQTT"
	cp.ProtocolVersion = 4
	cp.CleanSession = true
	cp.Keepalive = 60
	cp.ClientIdentifier = clientID
	cp.UsernameFlag = true
	cp.Username = username
	cp.PasswordFlag = true
	cp.Password = []byte("hunter2")

	var packet packets.ControlPacket = cp
	return &InspectorPacket{
		Track: &ConnectionInfo{SocketAddress: "203.0.113.7:50000"},
		Raw:   &RawPacket{MQTT: &packet},
	}
}

func assertOneLine(t *testing.T, out, wantAction string) {
	t.Helper()
	if got := strings.Count(out, "\n"); got != 1 {
		t.Fatalf("emitted %d log lines (want exactly 1):\n%s", got, out)
	}
	if !strings.Contains(out, wantAction) {
		t.Fatalf("the real %s line went missing:\n%s", wantAction, out)
	}
}
