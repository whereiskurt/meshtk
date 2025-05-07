package protoserver

import (
	"bufio"
	"bytes"
	"crypto/cipher"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	"github.com/eclipse/paho.mqtt.golang/packets"
	meshtastic "github.com/whereiskurt/meshtk/protos/meshtastic/generated"
	"google.golang.org/protobuf/proto"
)

type InspectorPacket struct {
	Track *ConnectionInfo

	Raw struct {
		MQTT       *packets.ControlPacket
		Meshtastic *meshtastic.ServiceEnvelope
	}

	MQTT struct {
		Type   string
		Topics []string //Subscribe topics can have multiple topics
	}

	Meshtastic struct {
		WasUnmarshalled bool
		From            uint32
		To              uint32
		PortNum         meshtastic.PortNum
		Payload         []byte
		PayloadString   string
		HexKey          string
		WasEncrypted    bool
		WasPKIEncrypted bool
	}
}

func (n *ProtoBufServerCmd) handleProxy(conn net.Conn) {
	defer func() {
		if conn.RemoteAddr() != nil {
			socketAddr := conn.RemoteAddr().String()
			n.ConnMutex.Lock()
			delete(n.ConnTrack, socketAddr)
			n.ConnMutex.Unlock()
		}
		conn.Close()
	}()

	socketAddr := n.TrackConnection(conn)

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

	go n.handleBackend(conn, backend, done)

	for {
		select {
		case <-done:
			return

		default:
			ip := new(InspectorPacket)
			ip.Track = new(ConnectionInfo)
			ip.Track.SocketAddress = socketAddr

			if err := n.processMQTT(request, ip); err != nil {
				//This is a non-fatal error that happens whenever the client disconnects
				n.Config.Log.Tracef("failed to process MQTT packet: %v", err)
				return
			}

			n.Config.Log.Tracef("%s,%s,%s,%s,%s", ip.Track.Username, ip.Track.ClientID, ip.Track.SocketAddress, ip.MQTT.Type, ip.Meshtastic.PortNum)

			// Serialize the packet for forwarding
			var buf bytes.Buffer
			if err := (*ip.Raw.MQTT).Write(&buf); err != nil {
				n.Config.Log.Tracef("failed to serialize MQTT packet: %v", err)
				return
			}

			// Forward the packet to the backend
			if _, err := backendConn.Write(buf.Bytes()); err != nil {
				n.Config.Log.Tracef("failed to forward packet to backend: %v", err)
				return
			}
		}
	}
}

func (n *ProtoBufServerCmd) handleBackend(conn net.Conn, backendReader *bufio.Reader, done chan struct{}) {
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

func (n *ProtoBufServerCmd) processMQTT(connReader *bufio.Reader, ip *InspectorPacket) error {
	socketAddress := ip.Track.SocketAddress

	packet, err := packets.ReadPacket(connReader)
	if err != nil {
		if err != io.EOF {
			n.Config.Log.Errorf("Failed to parse MQTT packet from %s: %v", socketAddress, err)
		}
		return err
	}
	ip.Raw.MQTT = &packet

	switch p := packet.(type) {
	case *packets.ConnectPacket:
		connInfo := &ConnectionInfo{
			ClientID:      p.ClientIdentifier,
			Username:      p.Username,
			Password:      fmt.Sprintf("%x", p.Password),
			SocketAddress: socketAddress,
			ConnectTime:   time.Now().Unix(),
		}
		ip.MQTT.Type = "CONNECT"
		ip.Track = connInfo
		n.ConnMutex.Lock()
		n.ConnTrack[socketAddress] = connInfo
		n.ConnMutex.Unlock()

	case *packets.PublishPacket:
		n.SetConnTrack(ip, socketAddress)
		ip.MQTT.Type = "PUBLISH"
		topics := make([]string, 0, 1)
		ip.MQTT.Topics = append(topics, p.TopicName)

		var env meshtastic.ServiceEnvelope
		if err := proto.Unmarshal(p.Payload, &env); err == nil {
			ip.Raw.Meshtastic = &env
			n.processMeshtastic(ip)
		}

	case *packets.SubscribePacket:
		n.SetConnTrack(ip, socketAddress)
		ip.MQTT.Type = "SUBSCRIBE"
		topics := make([]string, 0, len(p.Topics))
		ip.MQTT.Topics = append(topics, p.Topics...)

	case *packets.PingreqPacket:
		n.SetConnTrack(ip, socketAddress)
		ip.MQTT.Type = "PINGREQ"
	case *packets.PingrespPacket:
		n.SetConnTrack(ip, socketAddress)
		ip.MQTT.Type = "PINGRESP"

	default:
		ip.MQTT.Type = fmt.Sprintf("%T", packet)
		ip.MQTT.Type = ip.MQTT.Type[strings.LastIndex(ip.MQTT.Type, ".")+1:]
		n.ConnMutex.RLock()
		connInfo, exists := n.ConnTrack[socketAddress]
		n.ConnMutex.RUnlock()
		if exists {
			ip.Track = connInfo
		}
	}

	return nil
}

func (n *ProtoBufServerCmd) processMeshtastic(ip *InspectorPacket) {
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

	from := packet.GetFrom()
	to := packet.GetTo()

	var portNum meshtastic.PortNum
	var payload []byte

	decoded := packet.GetDecoded()
	if decoded == nil {
		encrypted := packet.GetEncrypted()

		if encrypted != nil {
			d, hexKey, err := n.decipherMeshtastic(packet, from, encrypted)
			if err == nil {
				decoded = d
				ip.Meshtastic.HexKey = hexKey
				ip.Meshtastic.WasEncrypted = true
				ip.Meshtastic.WasUnmarshalled = true
			}

		} else {
			n.Config.Log.Errorf("Packet contains neither decoded nor encrypted data from topic %s", topic)
			return
		}
	}

	portNum = decoded.GetPortnum()
	payload = decoded.GetPayload()

	switch portNum {
	case meshtastic.PortNum_TEXT_MESSAGE_APP:
		ip.Meshtastic.WasUnmarshalled = true

	case meshtastic.PortNum_NODEINFO_APP:
		var user meshtastic.User
		if err := proto.Unmarshal(payload, &user); err == nil {
			ip.Meshtastic.WasUnmarshalled = true
		}

	case meshtastic.PortNum_POSITION_APP:
		var position meshtastic.Position
		if err := proto.Unmarshal(payload, &position); err == nil {
			ip.Meshtastic.WasUnmarshalled = true
		}

	case meshtastic.PortNum_MAP_REPORT_APP:
		var mapReport meshtastic.MapReport
		if err := proto.Unmarshal(payload, &mapReport); err == nil {
			ip.Meshtastic.WasUnmarshalled = true
		}

	case meshtastic.PortNum_TELEMETRY_APP:
		var telemetry meshtastic.Telemetry
		if err := proto.Unmarshal(payload, &telemetry); err == nil {
			ip.Meshtastic.WasUnmarshalled = true
		}

	case meshtastic.PortNum_NEIGHBORINFO_APP:
		var neighborInfo meshtastic.NeighborInfo
		if err := proto.Unmarshal(payload, &neighborInfo); err == nil {
			ip.Meshtastic.WasUnmarshalled = true
		}

	default:
		n.Config.Log.Infof("Message with PortNum %s from %d on topic %s", portNum.String(), from, topic)
	}

	ip.Meshtastic.From = from
	ip.Meshtastic.To = to
	ip.Meshtastic.PortNum = decoded.GetPortnum()
	ip.Meshtastic.Payload = decoded.GetPayload()
	ip.Meshtastic.PayloadString = string(decoded.GetPayload())

}

func (n *ProtoBufServerCmd) decipherMeshtastic(packet *meshtastic.MeshPacket, from uint32, encrypted []byte) (decoded *meshtastic.Data, hexkey string, err error) {
	nonce := make([]byte, 16)
	binary.LittleEndian.PutUint32(nonce[0:], packet.GetId())
	binary.LittleEndian.PutUint32(nonce[8:], from)
	decrypted := make([]byte, len(encrypted))

	for k, cipherInstance := range n.Ciphers {
		hexKey := n.Config.Meshtastic.Channels[k].EncryptKey

		cipher.NewCTR(cipherInstance, nonce).XORKeyStream(decrypted, encrypted)
		decoded = new(meshtastic.Data)
		if err := proto.Unmarshal(decrypted, decoded); err == nil {
			return decoded, hexKey, nil
		}

	}
	return nil, "", fmt.Errorf("failed to decrypt data with any cipher")
}
