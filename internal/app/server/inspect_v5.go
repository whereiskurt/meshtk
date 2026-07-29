package server

import (
	"context"
	"fmt"
	"net"
	"time"

	v5 "github.com/eclipse/paho.golang/packets"
	meshtastic "github.com/whereiskurt/meshtk/protos/meshtastic/generated"
	"google.golang.org/protobuf/proto"
)

// inspectV5Connect is the v5 mirror of inspectRawPacket's 3.1.1 CONNECT branch,
// decision for decision. It answers the client itself on every rejection path,
// so a false return means "already answered, close without dialing the backend".
//
// The v5 reason codes are the point of the exercise: 3.1.1's return code 0x05
// is meaningless in v5 and renders in the Meshtastic app as a bogus "check
// credentials" error, and 0x84 (Unsupported Protocol Version) is reserved for
// protocol levels above 5 -- answering it here is what made mqttastic
// retry-loop rather than surface the real problem.
func (n *ServerCmd) inspectV5Connect(clientConn net.Conn, socketAddr string, c *v5.Connect) (allow bool) {
	// The 3.1.1 CONNECT branch REPLACES the ConnTrack entry, so the protocol
	// version has to be stamped on the struct built here -- a version stamped
	// earlier in the connection's life would be overwritten and lost.
	//
	// The password is stored hex-encoded exactly as the 3.1.1 path does. Never
	// widen that: the plaintext must not reach any log line.
	connInfo := &ConnectionInfo{
		ClientID:        c.ClientID,
		Username:        c.Username,
		Password:        fmt.Sprintf("%x", c.Password),
		SocketAddress:   socketAddr,
		ConnectTime:     time.Now().Unix(),
		ProtocolVersion: 5,
	}
	n.ConnMutex.Lock()
	n.ConnTrack[socketAddr] = connInfo
	n.ConnMutex.Unlock()

	// Enhanced auth (MQTT 5.0 §4.12) is not supported and must be refused
	// BEFORE the backend dial -- an AUTH packet must never be relayed into an
	// authenticated session. mosquitto answers 0x8C to the same CONNECT, so
	// this matches the broker's own behavior rather than inventing one.
	//
	// Distinct action, not AUTH_REJECT: research assumption A3 is that mqttastic
	// does not use enhanced auth, and if that is wrong EVERY Android client gets
	// 0x8C -- which has to be greppable on its own, not buried in the
	// bad-credential stream.
	if c.Properties != nil && c.Properties.AuthMethod != "" {
		n.InspectorLogger.Warnf("action=MQTT5_AUTH_METHOD, ip=%s, username=%s, auth_method=%s, reason=enhanced_auth_unsupported",
			socketAddr, c.Username, c.Properties.AuthMethod)
		writeMqtt5Connack(clientConn, v5.ConnackBadAuthenticationMethod)
		return false
	}

	passthrough := false
	for _, pt := range n.Config.Server.CredCache.Passthrough {
		if c.Username == pt {
			passthrough = true
			break
		}
	}

	if passthrough {
		// Passthrough -- forward as-is with the original credentials.
	} else if c.Username == "" {
		// Empty username -- reject (fail closed). Silent, exactly as the 3.1.1
		// branch is: this path mirrors it decision for decision.
		writeMqtt5Connack(clientConn, v5.ConnackNotAuthorized)
		return false
	} else {
		// Identical Verify contract and cred-cache timeout as the 3.1.1 path;
		// the DynamoDB-backed authenticator neither knows nor cares about the
		// protocol version.
		ctx, cancel := context.WithTimeout(context.Background(),
			time.Duration(n.Config.Server.CredCache.TimeoutSecs)*time.Second)
		defer cancel()

		valid, err := n.Authenticator.Verify(ctx, c.Username, c.Password)
		if err != nil {
			n.InspectorLogger.Warnf("action=AUTH_REJECT, ip=%s, username=%s, reason=error, err=%v",
				socketAddr, c.Username, err)
			writeMqtt5Connack(clientConn, v5.ConnackNotAuthorized)
			return false
		} else if !valid {
			n.InspectorLogger.Warnf("action=AUTH_REJECT, ip=%s, username=%s, reason=invalid",
				socketAddr, c.Username)
			writeMqtt5Connack(clientConn, v5.ConnackNotAuthorized)
			return false
		}

		// Valid -- swap to the generic mosquitto identity. Client credentials
		// must never reach the broker.
		c.Username = n.Config.Server.ProxyUsername
		c.Password = []byte(n.Config.Server.ProxyPassword)
	}

	// Uplink half of topic-alias suppression: telling the BROKER the client
	// grants it no alias budget means downlink PUBLISHes always carry a real
	// topic, so logDownlink and every topic-based rule can see it. (The
	// downlink half -- stripping the broker's own budget out of the CONNACK --
	// is in handleBackendV5.) nil rather than a pointer to 0: both are legal
	// and mean the same thing, nil just produces the smaller packet.
	if c.Properties != nil {
		c.Properties.TopicAliasMaximum = nil
	}

	// Logged with the ORIGINAL username from connInfo, not the swapped broker
	// identity -- the point of the line is measuring Android v5 adoption.
	n.InspectorLogger.Infof("action=MQTT5_CONNECT, ip=%s, username=%s, client_id=%s",
		socketAddr, connInfo.Username, connInfo.ClientID)
	return true
}

// logDownlinkV5 is the v5 adapter onto logDownlinkEnvelope, the codec-independent
// core of logDownlink. Everything the downlink side decides -- the log level, the
// gateway-id self-echo comparison -- comes from the payload and the topic, and
// those are the only two fields either codec has to supply.
func (n *ServerCmd) logDownlinkV5(conn net.Conn, socketAddr string, p *v5.Publish) bool {
	return n.logDownlinkEnvelope(conn, socketAddr, p.Payload, p.Topic)
}

// inspectV5Publish is the v5 mirror of inspectRawPacket's 3.1.1 PUBLISH branch.
// It builds the SAME InspectorPacket the rules engine has always consumed --
// only Raw.MQTT5 is populated instead of Raw.MQTT, and Raw.Meshtastic, MQTT.Type
// and MQTT.Topics are filled identically. The decrypt path, inspectMeshtastic
// and every rule are therefore reached with no v5 branch of their own: they
// operate on ip.Raw.Meshtastic, not on MQTT types.
//
// The deliberately rejected alternative is synthesizing a fake 3.1.1
// PublishPacket so the rules see a familiar type. That reintroduces meshtk#22:
// rules would mutate the shim and the mutation would never reach the v5 wire.
func (n *ServerCmd) inspectV5Publish(socketAddr string, cp *v5.ControlPacket) *InspectorPacket {
	ip := &InspectorPacket{
		Log:   n.InspectorLogger,
		Track: &ConnectionInfo{SocketAddress: socketAddr},
		Raw:   &RawPacket{MQTT5: cp},
	}

	// Load-bearing, not cosmetic: SetConnTrack swaps in the tracked
	// ConnectionInfo carrying the ORIGINAL client username (the CONNECT
	// forwarded to the broker carries the swapped proxy identity). The
	// RequireMQTTUserName rule Blocks on an empty Track.Username, so skipping
	// this would Block every publish on an already-authenticated connection.
	n.SetConnTrack(ip)

	p, ok := cp.Content.(*v5.Publish)
	if !ok {
		return ip
	}

	ip.MQTT.Type = "PUBLISH"
	topics := make([]string, 0, 1)
	ip.MQTT.Topics = append(topics, p.Topic)

	var env meshtastic.ServiceEnvelope
	if err := proto.Unmarshal(p.Payload, &env); err == nil {
		ip.Raw.Meshtastic = &env
		if gw := env.GetGatewayId(); gw != "" {
			n.rememberGateway(socketAddr, gw)
		}
		ip.inspectMeshtastic(n)
	}

	return ip
}
