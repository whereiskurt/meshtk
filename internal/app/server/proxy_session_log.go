package server

import (
	"errors"
	"io"
	"math"
	"net"
	"os"
	"time"
)

// Why a proxied MQTT session ended. These strings are a LOG CONTRACT -- they
// are grepped in CloudWatch, so renaming one silently breaks whatever query is
// pointed at it. Add new reasons rather than repurposing existing ones.
//
// The distinction that matters operationally is reasonReadTimeout vs
// reasonClientEOF: the first says the proxy's own read deadline fired (look at
// minProxyReadTimeout and the negotiated keepalive), the second says the client
// closed the socket (look at the device). Before these existed, both looked
// identical in production -- a CONNECT, some SUBSCRIBEs, then silence.
const (
	reasonClientDisconnect = "client_disconnect"
	reasonReadTimeout      = "read_timeout"
	reasonClientEOF        = "client_eof"
	reasonReadError        = "read_error"
)

// closeReason classifies the error that ended a proxy read loop. A nil error
// means the loop exited without a read failure, which only happens when the
// client sent a real DISCONNECT.
func closeReason(err error) string {
	if err == nil {
		return reasonClientDisconnect
	}

	// Checked BEFORE the generic net.Error branch: a deadline we set is our
	// own doing, and conflating it with a client-side timeout is exactly the
	// ambiguity these reasons exist to remove.
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return reasonReadTimeout
	}

	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return reasonReadTimeout
	}

	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return reasonClientEOF
	}

	return reasonReadError
}

// sessionLog is the per-connection state behind the SESSION_START /
// SESSION_END pair. One value per proxied connection, owned by the uplink
// goroutine -- the downlink goroutine never touches it, so it needs no lock.
type sessionLog struct {
	socketAddr  string
	clientID    string
	username    string
	keepalive   uint16
	readTimeout time.Duration
	started     time.Time

	// reason is set by the uplink loop as it exits and read by logEnd from a
	// defer. It stays reasonReadError until something more specific is known,
	// so a path that forgets to classify is loud rather than silently "clean".
	reason string

	// started tracking only after a CONNECT is seen and inspected; a
	// connection that never authenticated has no session to close out.
	open bool

	// sawDisconnect latches a client-sent DISCONNECT so the EOF that follows
	// it cannot downgrade a clean exit to client_eof.
	sawDisconnect bool
}

// logStart records the negotiated keepalive and the deadline actually derived
// from it. Both are logged because they diverge whenever minProxyReadTimeout's
// floor binds, and that divergence is precisely what makes a reconnect cadence
// misread as client flapping.
//
// Client-controlled values go through logSafe for the same reason every other
// line in this package does: a clientID is attacker-chosen and must not be able
// to forge log structure.
func (s *sessionLog) logStart(n *ServerCmd) {
	s.open = true
	n.InspectorLogger.Infof(
		"action=SESSION_START, ip=%s, clientID=%s, username=%s, keepalive=%ds, read_timeout=%ds",
		s.socketAddr,
		logSafe(s.clientID),
		logSafe(s.username),
		s.keepalive,
		// Rounded, not truncated: an odd keepalive derives a .5 deadline
		// (15s -> 22.5s), and logging that as "22s" invites an operator to
		// conclude we hung up early when we did not.
		int(math.Round(s.readTimeout.Seconds())),
	)
}

// logEnd closes out a session that logStart opened. Sessions that never got
// past CONNECT are skipped: the rejection paths already log their own reason,
// and a SESSION_END without a matching SESSION_START would break the pairing
// that makes these lines greppable.
func (s *sessionLog) logEnd(n *ServerCmd) {
	if !s.open {
		return
	}
	n.InspectorLogger.Infof(
		"action=SESSION_END, ip=%s, clientID=%s, username=%s, reason=%s, duration=%.1fs",
		s.socketAddr,
		logSafe(s.clientID),
		logSafe(s.username),
		s.reason,
		time.Since(s.started).Seconds(),
	)
}
