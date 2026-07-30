package server

import (
	"fmt"
	"strconv"
	"strings"
)

// logSafeMaxRunes caps how much of a client-controlled string reaches a log
// line. 128 runes comfortably clears every real value the fleet produces -- a
// 12-hex MQTT username is 12, and the longest observed Android client id
// ("MeshtasticAndroidMqttProxy-!aed94d05-fdcc313a") is 45 -- while stopping a
// client from padding the security log with kilobytes per CONNECT.
const logSafeMaxRunes = 128

// logSafe makes a client-controlled string safe to interpolate into an
// InspectorLogger format string.
//
// WHY THIS EXISTS. The inspector log is written through SimpleFormatter
// (cmd.go), which is a bare fmt.Sprintf("%s %s\n", timestamp, message): no
// quoting, no escaping. That file is rotated to S3 and grepped for
// action=AUTH_REJECT, action=MQTT5_CONNECT and action=ALLOW -- the telemetry
// phase 68's verification and the committed mqtt5_probe.py both correlate on.
// So a newline embedded in a client id, a username or an auth method lets any
// credential holder append a fully-formed, fully-plausible line of their
// choosing to the security log. It was PROVEN: one CONNECT whose ClientID
// carried "\n<timestamp> action=AUTH_REJECT, ip=10.0.0.1, username=admin,
// reason=invalid" produced two log lines (68-REVIEW WR-05).
//
// WHY THE QUOTING IS CONDITIONAL. A blanket strconv.Quote would wrap every
// client id, username and topic in the production log in double quotes,
// changing the shape of every ALLOW / AUTH_REJECT / MQTT5_CONNECT line and
// breaking both the ops greps and mqtt5_probe.py's `client_id=<id>` substring
// correlation. The forgery vector is specifically the control character, so a
// clean value passes through BYTE-IDENTICALLY and only a value that was
// modified, was truncated, or would otherwise break the "key=value, key=value"
// grammar is quoted. A quoted value in production is therefore itself a signal:
// something tried to tamper with the log.
//
// SCOPE. This applies to InspectorLogger sites only. Config.Log uses logrus'
// TextFormatter, which already quotes any value containing a space or a control
// character -- that is why the "[proxy] ALLOW" / "[proxy] BLOCK" lines and
// 69-02's action=PANIC_RECOVERED (which carries a whole goroutine stack) are
// deliberately left alone.
//
// THE PASSWORD IS NEVER LOGGED, ON EITHER CODEC, AND MUST STAY THAT WAY. It is
// not routed through this function and must not be: passing it here would imply
// it is loggable-with-care. It is not loggable at all.
func logSafe(s string) string {
	modified := false

	cleaned := strings.Map(func(r rune) rune {
		// Everything below 0x20 (which is \n and \r) plus DEL. These are the
		// runes that can forge a line or scramble a terminal; nothing else in
		// a client id, username, auth method or topic needs removing.
		if r < 0x20 || r == 0x7f {
			modified = true
			return -1
		}
		return r
	}, s)

	// Rune-wise, not byte-wise: slicing a UTF-8 string at a byte offset can cut
	// a multi-byte rune in half and emit an invalid sequence into the log.
	if runes := []rune(cleaned); len(runes) > logSafeMaxRunes {
		cleaned = string(runes[:logSafeMaxRunes])
		modified = true
	}

	// A space, a comma, a double quote or an equals sign inside a VALUE breaks
	// the "key=value, key=value" grammar every consumer of this log parses by,
	// so those get quoted too even though they cannot forge a whole line.
	if modified || strings.ContainsAny(cleaned, " ,\"=") {
		return strconv.Quote(cleaned)
	}
	return cleaned
}

// logSafeList is logSafe for the topic slices the decision and limiter logs
// carry.
//
// It reproduces what "%+v" prints for a []string exactly, so a clean topic list
// still renders as mqtt_topic=[msh/US/2/e/dc.run/!435990e4] -- byte-identical to
// what production has always emitted, which is what keeps the existing evidence
// tables and probe greps working. A topic carrying a control character is
// sanitized element-wise, so one hostile filter in a multi-filter SUBSCRIBE
// cannot forge a line while the rest of the list stays readable.
func logSafeList(vals []string) string {
	safe := make([]string, len(vals))
	for i, v := range vals {
		safe[i] = logSafe(v)
	}
	return fmt.Sprintf("%+v", safe)
}
