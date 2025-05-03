package protoserver

import (
	"bufio"
	"bytes"
	"crypto/cipher"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/eclipse/paho.mqtt.golang/packets"
	proxyproto "github.com/pires/go-proxyproto"
	meshtastic "github.com/whereiskurt/meshtk/protos/meshtastic/generated"
	"google.golang.org/protobuf/proto"
)

// ConnectionInfo stores information about a client connection
type ConnectionInfo struct {
	ClientID      string
	Username      string
	Password      string
	SocketAddress string
	ConnectTime   int64
}

func (n *ProtoBufServerCmd) freeConnTrack() {
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()

		for range ticker.C {
			now := time.Now().Unix()
			n.ConnMutex.Lock()
			for clientAddr, connInfo := range n.ConnTrack {
				if now-connInfo.ConnectTime > 1800 {
					delete(n.ConnTrack, clientAddr)
				}
			}
			n.ConnMutex.Unlock()
		}
	}()
}

func (n *ProtoBufServerCmd) handleProxy(conn net.Conn) {
	defer func() {
		if conn.RemoteAddr() != nil {
			clientAddr := conn.RemoteAddr().String()
			n.ConnMutex.Lock()
			delete(n.ConnTrack, clientAddr)
			n.ConnMutex.Unlock()
		}
		conn.Close()
	}()

	clientAddr := n.TrackClient(conn)

	backendAddress := n.Config.ProtoBufServer.ProxyForwardAddress
	backendConn, err := net.DialTimeout("tcp", backendAddress, 5*time.Second)
	if err != nil {
		n.Config.Log.Errorf("Failed to connect to backend MQTT broker: %v", err)
		return
	}
	defer backendConn.Close()

	request := bufio.NewReader(conn)
	backend := bufio.NewReader(backendConn)

	done := make(chan struct{})

	go n.backendHandler(conn, backend, done)

	for {
		select {
		case <-done:
			return
		default:
			if err := n.handleMQTT(request, backendConn, clientAddr); err != nil {
				return
			}
		}
	}
}

func (*ProtoBufServerCmd) TrackClient(conn net.Conn) (clientAddr string) {
	if conn.RemoteAddr() != nil {
		clientAddr = conn.RemoteAddr().String()
	}

	if proxyConn, ok := conn.(*proxyproto.Conn); ok {
		proxyHeader := proxyConn.ProxyHeader()
		if proxyHeader != nil && proxyHeader.SourceAddr != nil {
			clientAddr = proxyHeader.SourceAddr.String()
		}
	}
	return clientAddr
}

func (n *ProtoBufServerCmd) handleMQTT(connReader *bufio.Reader, backendConn net.Conn, socketAddress string) error {
	packet, err := packets.ReadPacket(connReader)
	if err != nil {
		if err != io.EOF {
			n.Config.Log.Errorf("Failed to parse MQTT packet from %s: %v", socketAddress, err)
		}
		return err
	}
	switch p := packet.(type) {
	case *packets.ConnectPacket:
		connectInfo := &ConnectionInfo{
			ClientID:      p.ClientIdentifier,
			Username:      p.Username,
			Password:      fmt.Sprintf("%x", p.Password),
			SocketAddress: socketAddress,
			ConnectTime:   time.Now().Unix(),
		}

		n.ConnMutex.Lock()
		n.ConnTrack[socketAddress] = connectInfo
		n.ConnMutex.Unlock()

		n.Config.Log.Tracef("MQTT CONNECT from %s: client=%s, username=%s, protocol=%d", socketAddress, p.ClientIdentifier, p.Username, p.ProtocolVersion)

	case *packets.PublishPacket:
		n.ConnMutex.RLock()
		connInfo, exists := n.ConnTrack[socketAddress]
		n.ConnMutex.RUnlock()

		if !exists {
			return fmt.Errorf("connection %s not found in clientIDByConnID map", socketAddress)
		}

		username := connInfo.Username
		clientID := connInfo.ClientID

		n.Config.Log.Tracef("MQTT PUBLISH from %s (user=%s, client=%s): topic=%s, QoS=%d, retained=%v", socketAddress, username, clientID, p.TopicName, p.Qos, p.Retain)

		payload := p.Payload
		var envelope meshtastic.ServiceEnvelope
		if err := proto.Unmarshal(payload, &envelope); err != nil {
			n.Config.Log.Warnf("not meshtastic on %v: from: %v: %v", p.TopicName, username, err)
		} else {
			n.handleMeshtastic(&envelope, p.TopicName, connInfo)
		}

	case *packets.SubscribePacket:
		n.ConnMutex.RLock()
		connInfo, exists := n.ConnTrack[socketAddress]
		n.ConnMutex.RUnlock()

		if !exists {
			return fmt.Errorf("connection %s not found in clientIDByConnID map", socketAddress)
		}

		username := connInfo.Username
		clientID := connInfo.ClientID

		topics := make([]string, 0, len(p.Topics))
		topics = append(topics, p.Topics...)
		n.Config.Log.Tracef("MQTT SUBSCRIBE from %s (user=%s, client=%s): topics=%v", socketAddress, username, clientID, topics)

	default:
		// Other packet types
	}

	// Serialize the packet for forwarding
	var buf bytes.Buffer
	if err := packet.Write(&buf); err != nil {
		n.Config.Log.Errorf("Failed to serialize MQTT packet: %v", err)
		return err
	}

	// Forward the packet to the backend
	if _, err := backendConn.Write(buf.Bytes()); err != nil {
		n.Config.Log.Errorf("Failed to forward packet to backend: %v", err)
		return err
	}

	return nil
}

func (n *ProtoBufServerCmd) backendHandler(conn net.Conn, backendReader *bufio.Reader, done chan struct{}) {
	defer func() {
		done <- struct{}{}
	}()

	for {
		backendPacket, err := packets.ReadPacket(backendReader)
		if err != nil {
			if err != io.EOF && err != io.ErrUnexpectedEOF {
				n.Config.Log.Errorf("Error reading from backend: %v", err)
			}
			return
		}

		var buf bytes.Buffer
		if err := backendPacket.Write(&buf); err != nil {
			n.Config.Log.Errorf("Failed to serialize backend response packet: %v", err)
			return
		}

		if _, err := conn.Write(buf.Bytes()); err != nil {
			n.Config.Log.Errorf("Failed to write backend response to client: %v", err)
			return
		}
	}
}

func (n *ProtoBufServerCmd) handleMeshtastic(envelope *meshtastic.ServiceEnvelope, topic string, connInfo *ConnectionInfo) {

	packet := envelope.GetPacket()
	if packet == nil {
		n.Config.Log.Errorf("Empty packet in envelope from topic %s", topic)
		return
	}

	from := packet.GetFrom()
	to := packet.GetTo()

	var portNum meshtastic.PortNum
	var payload []byte

	decoded := packet.GetDecoded()
	if decoded == nil {
		encrypted := packet.GetEncrypted()

		if encrypted != nil {
			var err error

			n.Config.Log.Tracef("Received encrypted packet from %d on topic %s", from, topic)
			decoded, err = n.tryCiphers(packet, from, encrypted)
			if err != nil {
				n.Config.Log.Errorf("failed to decrypt data with any cipher")
				return
			}

		} else {
			n.Config.Log.Errorf("Packet contains neither decoded nor encrypted data from topic %s", topic)
			return
		}
	}

	portNum = decoded.GetPortnum()
	payload = decoded.GetPayload()
	// Process the message based on portNum (only for decoded messages)
	switch portNum {
	case meshtastic.PortNum_NODEINFO_APP:
		var user meshtastic.User
		if err := proto.Unmarshal(payload, &user); err != nil {
			n.Config.Log.Warnf("Failed to unmarshal User from NODEINFO_APP: %v", err)
			return
		}
		n.Config.Log.Infof("NODEINFO from %d: id=%s, longName=%s, shortName=%s, hwModel=%s, role=%s", from, user.GetId(), user.GetLongName(), user.GetShortName(), user.GetHwModel().String(), user.GetRole().String())

	case meshtastic.PortNum_POSITION_APP:
		var position meshtastic.Position
		if err := proto.Unmarshal(payload, &position); err != nil {
			n.Config.Log.Warnf("Failed to unmarshal Position: %v", err)
			return
		}
		n.Config.Log.Infof("POSITION from %d: lat=%d, lng=%d, alt=%d, precision=%d", from, position.GetLatitudeI(), position.GetLongitudeI(), position.GetAltitude(), position.GetPrecisionBits())

	case meshtastic.PortNum_TEXT_MESSAGE_APP:
		n.Config.Log.Infof("TEXT_MESSAGE from %d to %d: %s", from, to, string(payload))

	case meshtastic.PortNum_MAP_REPORT_APP:
		var mapReport meshtastic.MapReport
		if err := proto.Unmarshal(payload, &mapReport); err != nil {
			n.Config.Log.Warnf("Failed to unmarshal MapReport: %v", err)
			return
		}
		n.Config.Log.Infof("MAP_REPORT from %d: longName=%s, shortName=%s, lat=%d, lng=%d", from, mapReport.GetLongName(), mapReport.GetShortName(), mapReport.GetLatitudeI(), mapReport.GetLongitudeI())

	case meshtastic.PortNum_TELEMETRY_APP:
		var telemetry meshtastic.Telemetry
		if err := proto.Unmarshal(payload, &telemetry); err != nil {
			n.Config.Log.Warnf("Failed to unmarshal Telemetry: %v", err)
			return
		}
		if deviceMetrics := telemetry.GetDeviceMetrics(); deviceMetrics != nil {
			n.Config.Log.Infof("TELEMETRY_DEVICE from %d: battery=%d%%, voltage=%.2fV", from, deviceMetrics.GetBatteryLevel(), deviceMetrics.GetVoltage())
		} else if envMetrics := telemetry.GetEnvironmentMetrics(); envMetrics != nil {
			n.Config.Log.Infof("TELEMETRY_ENVIRONMENT from %d: temp=%.1f°C, humidity=%.1f%%", from, envMetrics.GetTemperature(), envMetrics.GetRelativeHumidity())
		}

	case meshtastic.PortNum_NEIGHBORINFO_APP:
		var neighborInfo meshtastic.NeighborInfo
		if err := proto.Unmarshal(payload, &neighborInfo); err != nil {
			n.Config.Log.Warnf("Failed to unmarshal NeighborInfo: %v", err)
			return
		}
		n.Config.Log.Infof("NEIGHBORINFO from %d: nodeId=%d, neighbors=%d", from, neighborInfo.GetNodeId(), len(neighborInfo.GetNeighbors()))

	default:
		n.Config.Log.Infof("Message with PortNum %s from %d on topic %s", portNum.String(), from, topic)
	}

}

func (n *ProtoBufServerCmd) tryCiphers(packet *meshtastic.MeshPacket, from uint32, encrypted []byte) (decoded *meshtastic.Data, err error) {
	nonce := make([]byte, 16)
	binary.LittleEndian.PutUint32(nonce[0:], packet.GetId())
	binary.LittleEndian.PutUint32(nonce[8:], from)
	decrypted := make([]byte, len(encrypted))

	for _, cipherInstance := range n.Ciphers {
		cipher.NewCTR(cipherInstance, nonce).XORKeyStream(decrypted, encrypted)
		decoded = new(meshtastic.Data)
		if err := proto.Unmarshal(decrypted, decoded); err == nil {
			return decoded, nil
		}
	}
	return nil, fmt.Errorf("failed to decrypt data with any cipher")
}
