package server

import (
	"context"
	"crypto/cipher"
	"encoding/binary"
	"fmt"
	"net"
	"strings"
	"time"

	v5 "github.com/eclipse/paho.golang/packets"
	"github.com/eclipse/paho.mqtt.golang/packets"
	proxyproto "github.com/pires/go-proxyproto"
	log "github.com/sirupsen/logrus"
	meshtastic "github.com/whereiskurt/meshtk/protos/meshtastic/generated"
	"google.golang.org/protobuf/proto"
)

type InspectorPacket struct {
	Track        *ConnectionInfo
	Log          *log.Logger
	Raw          *RawPacket
	AuthRejected bool
	// WireRewritten records that a rule mutated the raw packet, so the forwarder
	// must re-encode it instead of relaying the bytes it captured off the wire.
	// The v5 uplink loop has both options available and forwarding BOTH -- or
	// mutating the struct while forwarding the original frame -- is the exact
	// silent-no-op failure mode meshtk#22 shipped.
	WireRewritten bool

	MQTT struct {
		Type   string
		Topics []string //Subscribe topics can have multiple topics
	}

	Meshtastic struct {
		Decoded         *meshtastic.Data
		Cipher          *cipher.Block
		WasUnmarshalled bool
		Id              uint32
		From            uint32
		To              uint32
		PortNum         meshtastic.PortNum
		Payload         []byte
		PayloadString   string
		HexKey          string
		WasEncrypted       bool
		WasPKIEncrypted    bool
		HadEncryptedPayload bool
	}
}
type RawPacket struct {
	MQTT *packets.ControlPacket
	// MQTT5 carries the packet for MQTT 5.0 connections; MQTT5Raw carries a
	// hand-parsed view of a v5 PUBLISH the codec refused to read (see
	// proxy_v5_rawpublish.go); MQTT5RawSub carries a hand-parsed view of a v5
	// SUBSCRIBE the codec refused to read (see proxy_v5_rawsubscribe.go).
	// AT MOST ONE of MQTT, MQTT5, MQTT5Raw and MQTT5RawSub is ever non-nil --
	// FOUR fields now, not three -- and all four may be nil: the codecs are
	// separate modules with separate wire formats, and synthesizing a 3.1.1
	// shim (or a v5.Subscribe) for a packet the codec never produced would let
	// rules mutate something that never reaches the wire (meshtk#22).
	//
	// EVERY reader must nil-guard the field it wants -- rules.go's
	// AllowMQTTControl and inspect.go's setPublishPayload are the two that
	// dispatch across all of them, and a bare dereference in either takes down
	// the whole proxy process rather than one connection.
	MQTT5       *v5.ControlPacket
	MQTT5Raw    *v5RawPublish
	MQTT5RawSub *v5RawSubscribe
	Meshtastic  *meshtastic.ServiceEnvelope
}

type ConnectionInfo struct {
	ClientID      string
	Username      string
	Password      string
	SocketAddress string
	ConnectTime   int64
	// GatewayID is the Meshtastic gateway id ("!435990e4") this connection
	// uplinks ServiceEnvelopes as -- learned from its first meshtastic PUBLISH.
	// The downlink side uses it to suppress self-echoes: mosquitto bounces every
	// publish back to a subscriber of its own topic (MQTT 3.1.1 has no no-local),
	// and pushing a radio's own packets back down its BLE pipe is pure waste.
	GatewayID string
	// ProtocolVersion is the MQTT protocol level this connection negotiated
	// (4 = 3.1.1, 5 = v5). Stamped by the CONNECT inspector, which REPLACES the
	// ConnTrack entry -- a version stamped before that point is lost.
	ProtocolVersion byte
}

func (i *InspectorPacket) String() string {
	return fmt.Sprintf("ClientID: %s, Username: %s, SocketAddress: %s, MQTTType: %s, MeshtasticPortNum: %d",
		i.Track.ClientID, i.Track.Username, i.Track.SocketAddress, i.MQTT.Type, i.Meshtastic.PortNum)
}

// TODO: Refactor the inspcet* functions to work on InspectorPacket instead of ServerCmd
func (ip *InspectorPacket) inspectRawPacket(n *ServerCmd, clientConn net.Conn) {

	switch p := (*ip.Raw.MQTT).(type) {
	case *packets.ConnectPacket:
		connInfo := &ConnectionInfo{
			ClientID:      p.ClientIdentifier,
			Username:      p.Username,
			Password:      fmt.Sprintf("%x", p.Password),
			SocketAddress: ip.Track.SocketAddress,
			ConnectTime:   time.Now().Unix(),
		}
		ip.MQTT.Type = "CONNECT"
		ip.Track = connInfo
		n.ConnMutex.Lock()
		n.ConnTrack[ip.Track.SocketAddress] = connInfo
		n.ConnMutex.Unlock()

		// Passthrough check
		passthrough := false
		for _, pt := range n.Config.Server.CredCache.Passthrough {
			if p.Username == pt {
				passthrough = true
				break
			}
		}

		if passthrough {
			// Passthrough -- forward as-is with original credentials
		} else if p.Username == "" {
			// Empty username -- reject (fail closed)
			writeConnackRejection(clientConn)
			ip.AuthRejected = true
		} else {
			// Credential validation
			ctx, cancel := context.WithTimeout(context.Background(),
				time.Duration(n.Config.Server.CredCache.TimeoutSecs)*time.Second)
			defer cancel()

			valid, err := n.Authenticator.Verify(ctx, p.Username, p.Password)
			if err != nil {
				n.InspectorLogger.Warnf("action=AUTH_REJECT, ip=%s, username=%s, reason=error, err=%v",
					ip.Track.SocketAddress, logSafe(p.Username), err)
				writeConnackRejection(clientConn)
				ip.AuthRejected = true
			} else if !valid {
				n.InspectorLogger.Warnf("action=AUTH_REJECT, ip=%s, username=%s, reason=invalid",
					ip.Track.SocketAddress, logSafe(p.Username))
				writeConnackRejection(clientConn)
				ip.AuthRejected = true
			} else {
				// Valid -- swap to generic Mosquitto credentials
				p.Username = n.Config.Server.ProxyUsername
				p.Password = []byte(n.Config.Server.ProxyPassword)
			}
		}

		// Last Will strip -- the 3.1.1 mirror of inspectV5Connect's. Same
		// action name, same field names, same field order, so ONE production
		// grep for the WILL_STRIPPED action returns both codecs and the codec
		// is distinguishable by protocol_version alone. See inspect_v5.go for
		// the full reasoning: the BROKER publishes a Will on disconnect, so its
		// payload can never traverse the uplink inspection chain, which makes
		// it a client-chosen uninspected uplink on any topic that defeats
		// RewriteHopLimit's fleet-wide RF flood-radius control (68-REVIEW
		// CR-02).
		//
		// Placed after the whole passthrough/credential chain and gated on
		// NOTHING: passthrough forwards the client's own credentials by design,
		// but it must not also forward an uninspected Will, and unlike the v5
		// inspector -- which returns early on every rejection -- this branch
		// falls through to a caller that decides whether to forward. An
		// unconditional strip cannot be bypassed by any future edit to that
		// decision.
		//
		// Note WillQos, lowercase -- the 3.1.1 codec spells it differently from
		// v5's WillQOS. The log carries the payload LENGTH, never its content,
		// and the ORIGINAL client username from connInfo, not the swapped
		// proxy identity.
		if p.WillFlag {
			n.InspectorLogger.Warnf("action=WILL_STRIPPED, ip=%s, protocol_version=4, username=%s, will_topic=%s, will_bytes=%d",
				ip.Track.SocketAddress,
				logSafe(connInfo.Username),
				logSafe(p.WillTopic),
				len(p.WillMessage))

			p.WillFlag, p.WillTopic, p.WillMessage, p.WillQos, p.WillRetain =
				false, "", nil, 0, false
		}

	case *packets.PublishPacket:
		n.SetConnTrack(ip)
		ip.MQTT.Type = "PUBLISH"
		topics := make([]string, 0, 1)
		ip.MQTT.Topics = append(topics, p.TopicName)

		var env meshtastic.ServiceEnvelope
		if err := proto.Unmarshal(p.Payload, &env); err == nil {
			ip.Raw.Meshtastic = &env
			if gw := env.GetGatewayId(); gw != "" {
				n.rememberGateway(ip.Track.SocketAddress, gw)
			}
			ip.inspectMeshtastic(n)
		}

	case *packets.SubscribePacket:
		n.SetConnTrack(ip)
		ip.MQTT.Type = "SUBSCRIBE"
		topics := make([]string, 0, len(p.Topics))
		ip.MQTT.Topics = append(topics, p.Topics...)

	case *packets.PingreqPacket:
		n.SetConnTrack(ip)
		ip.MQTT.Type = "PINGREQ"
	case *packets.PingrespPacket:
		n.SetConnTrack(ip)
		ip.MQTT.Type = "PINGRESP"

	default:
		n.SetConnTrack(ip)
		ip.MQTT.Type = fmt.Sprintf("%T", *ip.Raw.MQTT)
		ip.MQTT.Type = ip.MQTT.Type[strings.LastIndex(ip.MQTT.Type, ".")+1:]

	}
}

func (ip *InspectorPacket) inspectMeshtastic(n *ServerCmd) {
	if ip.Raw.Meshtastic == nil {
		return
	}

	envelope := ip.Raw.Meshtastic
	topic := ip.MQTT.Topics[0] //Meshtastic are publish packets, and are only for one topic

	packet := envelope.GetPacket()
	if packet == nil {
		ip.Meshtastic.WasUnmarshalled = false
		return
	}

	ip.Meshtastic.Id = packet.GetId()
	ip.Meshtastic.From = packet.GetFrom()
	ip.Meshtastic.To = packet.GetTo()

	decoded := packet.GetDecoded()
	if decoded == nil {
		encrypted := packet.GetEncrypted()
		if encrypted != nil {
			ip.Meshtastic.HadEncryptedPayload = true
			if packet.GetPkiEncrypted() {
				ip.Meshtastic.WasPKIEncrypted = true
			} else {
				d, hexKey, c, err := n.DecryptMeshtastic(packet.Id, ip.Meshtastic.From, encrypted)
				if err == nil {
					decoded = d
					ip.Meshtastic.HexKey = hexKey
					ip.Meshtastic.Cipher = c
					ip.Meshtastic.WasEncrypted = true
				}
			}
		} else {
			ip.Log.Errorf("packet contains neither decoded nor encrypted data from topic %s", topic)
			return
		}
		if decoded == nil {
			return
		}
	}

	ip.Meshtastic.Decoded = decoded
	ip.Meshtastic.PortNum = decoded.GetPortnum()
	ip.Meshtastic.Payload = decoded.GetPayload()
	ip.Meshtastic.PayloadString = string(decoded.GetPayload())

	switch ip.Meshtastic.PortNum {
	case meshtastic.PortNum_TEXT_MESSAGE_APP:
		ip.Meshtastic.WasUnmarshalled = true

	case meshtastic.PortNum_NODEINFO_APP:
		var user meshtastic.User
		if err := proto.Unmarshal(ip.Meshtastic.Payload, &user); err == nil {
			ip.Meshtastic.WasUnmarshalled = true
		}

	case meshtastic.PortNum_POSITION_APP:
		var position meshtastic.Position
		if err := proto.Unmarshal(ip.Meshtastic.Payload, &position); err == nil {
			ip.Meshtastic.WasUnmarshalled = true
		}

	case meshtastic.PortNum_MAP_REPORT_APP:
		var mapReport meshtastic.MapReport
		if err := proto.Unmarshal(ip.Meshtastic.Payload, &mapReport); err == nil {
			ip.Meshtastic.WasUnmarshalled = true
		}

	case meshtastic.PortNum_TELEMETRY_APP:
		var telemetry meshtastic.Telemetry
		if err := proto.Unmarshal(ip.Meshtastic.Payload, &telemetry); err == nil {
			ip.Meshtastic.WasUnmarshalled = true
		}

	case meshtastic.PortNum_NEIGHBORINFO_APP:
		var neighborInfo meshtastic.NeighborInfo
		if err := proto.Unmarshal(ip.Meshtastic.Payload, &neighborInfo); err == nil {
			ip.Meshtastic.WasUnmarshalled = true
		}

	case meshtastic.PortNum_ROUTING_APP:
		var router meshtastic.RouteDiscovery
		if err := proto.Unmarshal(ip.Meshtastic.Payload, &router); err == nil {
			ip.Meshtastic.WasUnmarshalled = true
		}

	default:
		ip.Log.Infof("Message with PortNum %s from %d on topic %s", ip.Meshtastic.PortNum.String(), ip.Meshtastic.From, topic)
	}

}

func (n *ServerCmd) DecryptMeshtastic(id, from uint32, payload []byte) (decoded *meshtastic.Data, hexkey string, c *cipher.Block, err error) {
	nonce := make([]byte, 16)
	binary.LittleEndian.PutUint32(nonce[0:], id)
	binary.LittleEndian.PutUint32(nonce[8:], from)
	decrypted := make([]byte, len(payload))

	for k, cipherInstance := range n.Ciphers {
		hexKey := n.Config.Meshtastic.Channels[k].EncryptKey

		cipher.NewCTR(cipherInstance, nonce).XORKeyStream(decrypted, payload)
		decoded = new(meshtastic.Data)
		if err := proto.Unmarshal(decrypted, decoded); err == nil {
			return decoded, hexKey, &cipherInstance, nil
		}

	}
	return nil, "", nil, fmt.Errorf("failed to decrypt data with any cipher")
}

// setPublishPayload writes b into the PUBLISH packet this InspectorPacket
// actually carries, dispatching on which codec produced it: a 3.1.1 connection
// fills Raw.MQTT, a v5 connection fills Raw.MQTT5, and a v5 PUBLISH the codec
// refused to read fills Raw.MQTT5Raw with a hand-parsed view instead. At most
// one of the three is ever non-nil.
//
// Both rewrite paths funnel through here for two reasons. First, the bare
// `(*ip.Raw.MQTT)` dereference this replaces PANICS on a v5 packet, and a panic
// in the proxy read loop takes down the whole process. Second -- the subtler
// half -- if the old type switch had merely failed to match, the hop clamp and
// the payload censor would have become silent no-ops for every v5 client:
// rules would report Rewrote while the original bytes went to the broker. That
// is meshtk#22 exactly, so a rewrite that cannot reach the wire is an error
// here, never a quiet nothing.
func (ip *InspectorPacket) setPublishPayload(b []byte) error {
	switch {
	case ip.Raw == nil:
		return fmt.Errorf("cannot set publish payload: no raw packet")

	case ip.Raw.MQTT != nil:
		p, ok := (*ip.Raw.MQTT).(*packets.PublishPacket)
		if !ok {
			return fmt.Errorf("cannot set publish payload: 3.1.1 packet is %T, not a PUBLISH", *ip.Raw.MQTT)
		}
		p.Payload = b

	case ip.Raw.MQTT5 != nil:
		p, ok := ip.Raw.MQTT5.Content.(*v5.Publish)
		if !ok {
			return fmt.Errorf("cannot set publish payload: v5 packet is %T, not a PUBLISH", ip.Raw.MQTT5.Content)
		}
		p.Payload = b

	case ip.Raw.MQTT5Raw != nil:
		// No type assertion to make: parseV5PublishFrame only ever produces a
		// view of a PUBLISH. The forwarder splices this payload back into the
		// captured frame, which preserves the very property bytes the codec
		// refused to parse -- see spliceV5PublishPayload.
		ip.Raw.MQTT5Raw.Payload = b

	case ip.Raw.MQTT5RawSub != nil:
		// A hand-parsed v5 SUBSCRIBE has no payload to set -- a SUBSCRIBE has
		// none at all. Saying so explicitly rather than falling into the tail
		// below keeps this switch a real dispatch across every RawPacket member,
		// which is what the struct's doc comment promises its readers.
		return fmt.Errorf("cannot set publish payload: the packet is a hand-parsed v5 SUBSCRIBE, not a PUBLISH")

	default:
		return fmt.Errorf("cannot set publish payload: no RawPacket member is set")
	}

	ip.WireRewritten = true
	return nil
}

// RewritePayloadString re-encrypts the (possibly censored)
// Meshtastic.PayloadString back into the packet and pushes the result onto the
// wire through setPublishPayload.
//
// It returns a single error. The old (error, bool) form returned false on EVERY
// path, so the bool carried no information, and its only caller discarded both
// values -- which is how a failed censor could be reported as Rewrote while the
// original bytes went to the broker (meshtk#22 class, review WR-08).
//
// The two nil guards are not defensive padding. Meshtastic.Cipher is assigned in
// exactly one place -- inspectMeshtastic's DECRYPT branch -- so a packet that
// arrived with a DECODED (unencrypted) payload reaches here with Cipher nil, and
// the old code dereferenced it. That was review CR-01: a SIGSEGV in the proxy
// read loop, and there is no recover() on that path, so ONE authenticated
// plaintext text message killed the whole process and dropped every connected
// radio -- not one connection.
func (ip *InspectorPacket) RewritePayloadString() error {
	if ip.Meshtastic.WasPKIEncrypted {
		return fmt.Errorf("cannot rewrite packet is PKI encrypted")
	}
	if ip.Meshtastic.Decoded == nil {
		return fmt.Errorf("cannot rewrite: packet has no decoded Data")
	}
	if ip.Meshtastic.Cipher == nil {
		return fmt.Errorf("cannot rewrite: no channel cipher for this packet")
	}

	// Mutate the ALREADY-PARSED Data in place instead of rebuilding it from a
	// handful of fields. proto.Marshal of a fresh struct emits only what was set,
	// so the old three-field rebuild silently dropped want_response, dest,
	// source, request_id, reply_id and emoji -- 2.8 tapbacks, threaded replies,
	// delivery ACKs and DM routing -- off every rewritten text message on the
	// fleet (review CR-03). Enumerating fields was the wrong fix shape: it has to
	// be re-audited every time the Data message grows, and it cannot preserve
	// protobuf unknown fields at all. Portnum and Bitfield come along for free
	// because they were already on the parsed message, so the explicit Bitfield
	// line meshtk#21 added (2.8 pre-hop drop) is subsumed, not removed.
	//
	// ORDERING IS LOAD-BEARING: marshal BEFORE reassigning PayloadVariant below.
	// On a decoded packet the parsed Data is reachable through that same variant,
	// so swapping it first would marshal a message the assignment just detached.
	data := ip.Meshtastic.Decoded
	data.Payload = []byte(ip.Meshtastic.PayloadString)
	dataBytes, err := proto.Marshal(data)
	if err != nil {
		return fmt.Errorf("failed to marshal rewritten Data: %w", err)
	}

	// Prepare the encrypted payload
	encrypted := make([]byte, len(dataBytes))
	nonce := make([]byte, 16)
	binary.LittleEndian.PutUint32(nonce[0:], ip.Meshtastic.Id)
	binary.LittleEndian.PutUint32(nonce[8:], ip.Meshtastic.From)
	cipher.NewCTR(*ip.Meshtastic.Cipher, nonce).XORKeyStream(encrypted, dataBytes)

	ip.Raw.Meshtastic.Packet.PayloadVariant = &meshtastic.MeshPacket_Encrypted{
		Encrypted: encrypted,
	}

	payloadBytes, err := proto.Marshal(ip.Raw.Meshtastic)
	if err != nil {
		return fmt.Errorf("failed to marshal Meshtastic payload: %v", err)
	}
	return ip.setPublishPayload(payloadBytes)
}

// RemarshalEnvelope serializes the (possibly mutated) parsed ServiceEnvelope
// back into the raw MQTT PUBLISH payload so struct edits actually reach the
// wire — the proxy forwards ip.Raw.MQTT bytes, not the parsed structs. Only
// outer MeshPacket fields can be edited this way; payload edits must go
// through RewritePayloadString (re-encrypt). protobuf-go retains unknown
// fields across the round-trip, so unmodeled fields survive.
func (ip *InspectorPacket) RemarshalEnvelope() error {
	payloadBytes, err := proto.Marshal(ip.Raw.Meshtastic)
	if err != nil {
		return fmt.Errorf("failed to marshal Meshtastic envelope: %v", err)
	}
	return ip.setPublishPayload(payloadBytes)
}

func (ip *InspectorPacket) WriteLimiterLog(decision Decision, tokenCount float64, penalty time.Duration) string {
	var action_log string
	switch decision {
	case Slow:
		action_log = "action=SLOW"
	case Kill:
		action_log = "action=KILL"
	default:
		panic(fmt.Sprintf("unknown decision: %v", decision))
	}

	action_log += fmt.Sprintf(",tokenCount=%.02f, penalty=%v", tokenCount, penalty)

	// Field names, field order and separators are UNCHANGED -- only the
	// client-controlled values are wrapped. logSafe/logSafeList leave a clean
	// value byte-identical, so this line's production shape does not move.
	action_log += fmt.Sprintf(",clientID=%s, username=%s, mqtt_type=%s, mqtt_topic=%s",
		logSafe(ip.Track.ClientID),
		logSafe(ip.Track.Username),
		ip.MQTT.Type,
		logSafeList(ip.MQTT.Topics))

	if ip.Meshtastic.WasUnmarshalled {
		action_log += fmt.Sprintf(",mesh_type=%s, mesh_from=%08x, mesh_to=%08x, payload=%02x",
			ip.Meshtastic.PortNum.String(), ip.Meshtastic.From, ip.Meshtastic.To, ip.Meshtastic.Payload)
	}
	ip.Log.Infof("%s", action_log)

	return action_log
}

func (ip *InspectorPacket) WriteDecisionLog(result DecisionResult) string {
	action_log := "action=ALLOW"
	switch result.Decision {
	case Block:
		action_log = "action=BLOCK"
	}

	// Same contract as WriteLimiterLog: names and order frozen, values wrapped.
	action_log += fmt.Sprintf(",ip=%s, clientID=%s, username=%s, mqtt_type=%s, mqtt_topic=%s",
		ip.Track.SocketAddress,
		logSafe(ip.Track.ClientID),
		logSafe(ip.Track.Username),
		ip.MQTT.Type,
		logSafeList(ip.MQTT.Topics))

	if ip.Meshtastic.WasUnmarshalled {
		action_log += fmt.Sprintf(",mesh_type=%s, mesh_from=%08x, mesh_to=%08x, payload=%02x",
			ip.Meshtastic.PortNum.String(), ip.Meshtastic.From, ip.Meshtastic.To, ip.Meshtastic.Payload)
	}

	ip.Log.Infof("%s", action_log)

	return action_log
}

func (*ServerCmd) TrackConnection(conn net.Conn) (socketAddr string) {
	if conn.RemoteAddr() != nil {
		socketAddr = conn.RemoteAddr().String()
	}

	if proxyConn, ok := conn.(*proxyproto.Conn); ok {
		proxyHeader := proxyConn.ProxyHeader()
		if proxyHeader != nil && proxyHeader.SourceAddr != nil {
			socketAddr = proxyHeader.SourceAddr.String()
		}
	}
	return socketAddr
}

// rememberGateway records the gateway id a connection uplinks envelopes as,
// so the downlink side can recognize (and suppress) that connection's own
// packets echoed back by the broker.
func (n *ServerCmd) rememberGateway(socketAddr, gatewayID string) {
	n.ConnMutex.Lock()
	if connInfo, exists := n.ConnTrack[socketAddr]; exists {
		connInfo.GatewayID = gatewayID
	}
	n.ConnMutex.Unlock()
}

// gatewayFor returns the recorded uplink gateway id for a connection, or "".
func (n *ServerCmd) gatewayFor(socketAddr string) string {
	n.ConnMutex.Lock()
	defer n.ConnMutex.Unlock()
	if connInfo, exists := n.ConnTrack[socketAddr]; exists {
		return connInfo.GatewayID
	}
	return ""
}

func (n *ServerCmd) SetConnTrack(ip *InspectorPacket) {
	n.ConnMutex.Lock()
	connInfo, exists := n.ConnTrack[ip.Track.SocketAddress]
	if exists {
		// Update the connection time
		connInfo.ConnectTime = time.Now().Unix()
		n.ConnTrack[ip.Track.SocketAddress] = connInfo
		ip.Track = connInfo
	}
	n.ConnMutex.Unlock()
}

// touchConnTrack refreshes a connection's idle timer and nothing else. It is the
// codec-independent half of what SetConnTrack does for the 3.1.1 loop, minus the
// InspectorPacket -- the v5 relay branch never builds one, but the reaper in
// SetupTracker deletes any entry with now-ConnectTime > 180 regardless.
//
// Update-if-exists ONLY, exactly like SetConnTrack. Creating an entry here would
// produce a ConnectionInfo with an empty Username, which is precisely what
// RequireMQTTUserName exists to Block -- a tracker entry must only ever be born
// from a CONNECT.
func (n *ServerCmd) touchConnTrack(socketAddr string) {
	n.ConnMutex.Lock()
	if connInfo, exists := n.ConnTrack[socketAddr]; exists {
		connInfo.ConnectTime = time.Now().Unix()
		n.ConnTrack[socketAddr] = connInfo
	}
	n.ConnMutex.Unlock()
}

func (n *ServerCmd) SetupTracker() {
	n.ConnTrack = make(map[string]*ConnectionInfo)

	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			i := 0
			now := time.Now().Unix()
			n.ConnMutex.Lock()
			for socketAddr, connInfo := range n.ConnTrack {
				// n.Config.Log.Tracef("ConnTrack: %s: %d: %d: diff: %d", socketAddr, connInfo.ConnectTime, now, now-connInfo.ConnectTime)
				if now-connInfo.ConnectTime > 180 {
					// n.Config.Log.Tracef("ConnTrack: purging connection %d: %s", i, socketAddr)
					delete(n.ConnTrack, socketAddr)
					i++
				}
			}
			n.ConnMutex.Unlock()
		}
	}()
}
