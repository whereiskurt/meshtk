package protoserver

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/eclipse/paho.mqtt.golang/packets"
	proxyproto "github.com/pires/go-proxyproto"
)

func (n *ProtoBufServerCmd) handleProxy(conn net.Conn) {
	defer func() {
		if conn.RemoteAddr() != nil {
			clientIP := conn.RemoteAddr().String()
			n.connectionsMutex.Lock()
			delete(n.clientIDByConnID, clientIP)
			n.connectionsMutex.Unlock()
		}
		conn.Close()
	}()

	var clientIP string
	if conn.RemoteAddr() != nil {
		clientIP = conn.RemoteAddr().String()
	}

	if proxyConn, ok := conn.(*proxyproto.Conn); ok {
		proxyHeader := proxyConn.ProxyHeader()
		if proxyHeader != nil && proxyHeader.SourceAddr != nil {
			clientIP = proxyHeader.SourceAddr.String()
		}
	}

	address := n.Config.ProtoBufServer.ProxyForwardAddress
	backendConn, err := net.DialTimeout("tcp", address, 5*time.Second)
	if err != nil {
		n.Config.Log.Errorf("Failed to connect to backend MQTT broker: %v", err)
		return
	}
	defer backendConn.Close()

	connReader := bufio.NewReader(conn)
	backendReader := bufio.NewReader(backendConn)

	done := make(chan struct{})

	go n.processBackendResponses(conn, backendReader, done)

	for {
		select {
		case <-done:
			return
		default:
			if err := n.processClientPacket(connReader, backendConn, clientIP); err != nil {
				return
			}
		}
	}
}

func (n *ProtoBufServerCmd) processBackendResponses(conn net.Conn, backendReader *bufio.Reader, done chan struct{}) {
	defer func() {
		done <- struct{}{}
	}()

	for {
		backendPacket, err := packets.ReadPacket(backendReader)
		if err != nil {
			if err != io.EOF {
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

func (n *ProtoBufServerCmd) processClientPacket(connReader *bufio.Reader, backendConn net.Conn, clientIP string) error {
	packet, err := packets.ReadPacket(connReader)
	if err != nil {
		if err != io.EOF {
			n.Config.Log.Errorf("Failed to parse MQTT packet from %s: %v", clientIP, err)
		}
		return err
	}

	switch p := packet.(type) {
	case *packets.ConnectPacket:
		connectInfo := &ConnectionInfo{
			ClientID:    p.ClientIdentifier,
			Username:    p.Username,
			Password:    fmt.Sprintf("%x", p.Password),
			IPAddress:   clientIP,
			ConnectTime: time.Now().Unix(),
		}

		n.connectionsMutex.Lock()
		n.clientIDByConnID[clientIP] = connectInfo
		n.connectionsMutex.Unlock()

		n.Config.Log.Tracef("MQTT CONNECT from %s: client=%s, username=%s, protocol=%d", clientIP, p.ClientIdentifier, p.Username, p.ProtocolVersion)

	case *packets.PublishPacket:
		n.connectionsMutex.RLock()
		connInfo, exists := n.clientIDByConnID[clientIP]
		n.connectionsMutex.RUnlock()

		if !exists {
			return fmt.Errorf("connection %s not found in clientIDByConnID map", clientIP)
		}

		username := connInfo.Username
		clientID := connInfo.ClientID

		n.Config.Log.Tracef("MQTT PUBLISH from %s (user=%s, client=%s): topic=%s, QoS=%d, retained=%v", clientIP, username, clientID, p.TopicName, p.Qos, p.Retain)

	case *packets.SubscribePacket:
		n.connectionsMutex.RLock()
		connInfo, exists := n.clientIDByConnID[clientIP]
		n.connectionsMutex.RUnlock()

		if !exists {
			return fmt.Errorf("connection %s not found in clientIDByConnID map", clientIP)
		}

		username := connInfo.Username
		clientID := connInfo.ClientID

		topics := make([]string, 0, len(p.Topics))
		topics = append(topics, p.Topics...)
		n.Config.Log.Tracef("MQTT SUBSCRIBE from %s (user=%s, client=%s): topics=%v", clientIP, username, clientID, topics)

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
