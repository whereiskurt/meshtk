package server

import (
	"context"
	"crypto/cipher"
	"encoding/binary"
	"fmt"
	"net"
	"strings"
	"time"

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
	MQTT       *packets.ControlPacket
	Meshtastic *meshtastic.ServiceEnvelope
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
					ip.Track.SocketAddress, p.Username, err)
				writeConnackRejection(clientConn)
				ip.AuthRejected = true
			} else if !valid {
				n.InspectorLogger.Warnf("action=AUTH_REJECT, ip=%s, username=%s, reason=invalid",
					ip.Track.SocketAddress, p.Username)
				writeConnackRejection(clientConn)
				ip.AuthRejected = true
			} else {
				// Valid -- swap to generic Mosquitto credentials
				p.Username = n.Config.Server.ProxyUsername
				p.Password = []byte(n.Config.Server.ProxyPassword)
			}
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

func (ip *InspectorPacket) RewritePayloadString() (error, bool) {
	if ip.Meshtastic.WasPKIEncrypted {
		return fmt.Errorf("cannot rewrite packet is PKI encrypted"), false
	}

	dataBytes, _ := proto.Marshal(&meshtastic.Data{
		Portnum: ip.Meshtastic.PortNum,
		Payload: []byte(ip.Meshtastic.PayloadString),
	})

	// Prepare the encrypted payload
	encrypted := make([]byte, len(dataBytes))
	nonce := make([]byte, 16)
	binary.LittleEndian.PutUint32(nonce[0:], ip.Meshtastic.Id)
	binary.LittleEndian.PutUint32(nonce[8:], ip.Meshtastic.From)
	cipher.NewCTR(*ip.Meshtastic.Cipher, nonce).XORKeyStream(encrypted, dataBytes)

	ip.Raw.Meshtastic.Packet.PayloadVariant = &meshtastic.MeshPacket_Encrypted{
		Encrypted: encrypted,
	}

	switch p := (*ip.Raw.MQTT).(type) {
	case *packets.PublishPacket:
		payloadBytes, err := proto.Marshal(ip.Raw.Meshtastic)
		if err != nil {
			return fmt.Errorf("failed to marshal Meshtastic payload: %v", err), false
		}
		p.Payload = payloadBytes
	}

	return nil, false
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

	action_log += fmt.Sprintf(",clientID=%s, username=%s, mqtt_type=%s, mqtt_topic=%+v",
		ip.Track.ClientID, ip.Track.Username, ip.MQTT.Type, ip.MQTT.Topics)

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

	action_log += fmt.Sprintf(",ip=%s, clientID=%s, username=%s, mqtt_type=%s, mqtt_topic=%+v",
		ip.Track.SocketAddress, ip.Track.ClientID, ip.Track.Username, ip.MQTT.Type, ip.MQTT.Topics)

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
