package protoserver

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"sync"
	"time"

	"github.com/eclipse/paho.mqtt.golang/packets"
	proxyproto "github.com/pires/go-proxyproto"
	meshtastic "github.com/whereiskurt/meshtk/protos/meshtastic/generated"

	"github.com/whereiskurt/meshtk/protos/security/generated"
	"google.golang.org/protobuf/proto"
)

func (n *ProtoBufServerCmd) StartProtobufServer() error {
	address := n.Config.ProtoBufServer.InspectorListenAddress

	ln, err := net.Listen("tcp", address)
	if err != nil {
		n.Config.Log.Errorf("Failed to listen: %v", err)
		return err
	}
	defer ln.Close()

	go func() {
		n.Config.Log.Infof("Meshtastic protobuff inspector server listening on %s", address)
		for {
			conn, err := ln.Accept()
			if err == nil {
				go n.handleProtobuf(conn)
			}
		}
	}()

	n.Config.Log.Infof("Press CTRL+C to interrupt")

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt)

	<-stop
	n.Config.Log.Infof("Shutting down the inspector server gracefully...")
	return nil

}

func (n *ProtoBufServerCmd) StartProxyServer() error {
	address := n.Config.ProtoBufServer.ProxyListenAddress

	listener, err := net.Listen("tcp", address)
	if err != nil {
		log.Fatal("Listen error:", err)
	}
	proxyListener := &proxyproto.Listener{Listener: listener}
	defer proxyListener.Close()

	n.Config.Log.Tracef("Listening on %v with Proxy Protocol", address)

	n.connectionsMutex = sync.RWMutex{}

	go func() {
		for {
			conn, err := proxyListener.Accept()
			if err != nil {
				log.Println("Accept error:", err)
				continue
			}
			go n.handleProxy(conn)
		}
	}()
	n.Config.Log.Infof("Press CTRL+C to interrupt")

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt)

	<-stop
	n.Config.Log.Infof("Shutting down the proxy server gracefully...")
	return nil
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

func (n *ProtoBufServerCmd) handleProtobuf(conn net.Conn) {
	defer conn.Close()

	// Read incoming request
	buf := make([]byte, 4096)
	blen, err := conn.Read(buf)
	if err != nil {
		if err != io.EOF {
			n.Config.Log.Errorf("1. Read error: %v", err)
		}
		return
	}

	var req generated.PacketRequest
	if err := proto.Unmarshal(buf[:blen], &req); err != nil {
		n.Config.Log.Errorf("Failed to decode PacketRequest: %v", err)
		return
	}

	n.Config.Log.Infof("PacketRequest: IP=%s User=%s ClientID=%s Topic=%s", req.IpAddress, req.Username, req.ClientId, req.Topic)

	topic := req.Topic

	shouldBlock := false
	blockReason := ""
	mutated := req.Payload
	var envelope meshtastic.ServiceEnvelope
	if err := proto.Unmarshal(req.Payload, &envelope); err != nil {
		n.Config.Log.Warnf("Not a Meshtastic packet on %v: from: %v: %v : %v", topic, req.Username, req.Payload, err)
		blockReason = "ServiceEnvelope"
		shouldBlock = true
	} else {
		if envelope.Packet.HopLimit > 3 {
			n.Config.Log.Infof("silent rewrite packet on %v from %v: original hop limit: %v", topic, req.Username, envelope.Packet.HopLimit)
			envelope.Packet.HopLimit = 3
			shouldBlock = false
		}

		mutated, err = proto.Marshal(&envelope)
		if err != nil {
			n.Config.Log.Errorf("no mutate: failed to marshal data: %v", err)
			mutated = req.Payload
			shouldBlock = true
			blockReason = "FailedMarshal"
		}

	}

	resp := &generated.PacketResponse{
		Payload:     mutated,
		ShouldBlock: shouldBlock,
		BlockReason: blockReason,
	}

	outBytes, err := proto.Marshal(resp)
	if err != nil {
		n.Config.Log.Errorf("Failed to encode PacketResponse: %v", err)
		return
	}

	_, err = conn.Write(outBytes)
	if err != nil {
		n.Config.Log.Errorf("Send error: %v", err)
		return
	}

	n.Config.Log.Debugf("Sent %d bytes back to client", len(outBytes))
}
