package server

import (
	"net"

	v5 "github.com/eclipse/paho.golang/packets"
)

// inspectV5Connect is the v5 mirror of inspectRawPacket's 3.1.1 CONNECT branch:
// it decides whether a connection may reach the broker and answers the client
// itself on every rejection path. A false return means "already answered, stop".
//
// FIRST INCREMENT (this commit): the seam exists and fails CLOSED -- every v5
// CONNECT is refused with 0x87, so the framing/dispatch work below can land
// without any possibility of an unauthenticated connection reaching mosquitto.
// The next commit gives this the full 3.1.1 parity: Passthrough allowlist,
// Authenticator.Verify, credential swap, 0x8C for enhanced auth, ConnTrack
// stamping and topic-alias suppression.
func (n *ServerCmd) inspectV5Connect(clientConn net.Conn, socketAddr string, c *v5.Connect) (allow bool) {
	n.InspectorLogger.Warnf("action=AUTH_REJECT, ip=%s, username=%s, reason=v5_auth_not_wired",
		socketAddr, c.Username)
	writeMqtt5Connack(clientConn, v5.ConnackNotAuthorized)
	return false
}
