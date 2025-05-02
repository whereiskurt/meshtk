package protoserver

import (
	"bytes"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"time"

	"github.com/eclipse/paho.mqtt.golang/packets"
	proxyproto "github.com/pires/go-proxyproto"
	meshtastic "github.com/whereiskurt/meshtk/protos/meshtastic/generated"

	"github.com/whereiskurt/meshtk/protos/security/generated"
	"google.golang.org/protobuf/proto"
)

func (n *ProtoBufServerCmd) StartInspectorServer() error {
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
				go n.handleInspector(conn)
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

func (n *ProtoBufServerCmd) handleProxy(conn net.Conn) {
	defer conn.Close()

	var clientIP string
	if proxyConn, ok := conn.(*proxyproto.Conn); ok {
		proxyHeader := proxyConn.ProxyHeader()
		if proxyHeader != nil && proxyHeader.SourceAddr != nil {
			clientIP = proxyHeader.SourceAddr.String()
		}
	}
	if clientIP == "" && conn.RemoteAddr() != nil {
		clientIP = conn.RemoteAddr().String()
	}

	// Create a buffer to read the first few bytes to determine packet type
	firstByteBuf := make([]byte, 1)
	_, err := conn.Read(firstByteBuf)
	if err != nil {
		n.Config.Log.Errorf("Read error from %s: %v", clientIP, err)
		return
	}

	// Reset the connection with the first byte
	r := io.MultiReader(bytes.NewReader(firstByteBuf), conn)

	// Use Paho MQTT packet parser
	controlPacketType := firstByteBuf[0] >> 4
	packet, err := packets.ReadPacket(r)
	if err != nil {
		n.Config.Log.Errorf("Failed to parse MQTT packet from %s: %v", clientIP, err)
		// Try to fall back to raw forwarding
		connectBuf := make([]byte, 4096)
		bytesRead, err := r.Read(connectBuf)
		if err != nil {
			n.Config.Log.Errorf("Read error from %s: %v", clientIP, err)
			return
		}
		payload := connectBuf[:bytesRead]
		n.Config.Log.Tracef("Falling back to raw forwarding: %d bytes from %s", bytesRead, clientIP)
		n.forward(conn, firstByteBuf, payload)
		return
	}

	// Log MQTT packet details
	switch p := packet.(type) {
	case *packets.ConnectPacket:
		n.Config.Log.Infof("MQTT CONNECT from %s: client=%s, username=%s, protocol=%d", clientIP, p.ClientIdentifier, p.Username, p.ProtocolVersion)
	case *packets.PublishPacket:
		n.Config.Log.Infof("MQTT PUBLISH from %s: topic=%s, QoS=%d, retained=%v", clientIP, p.TopicName, p.Qos, p.Retain)
	case *packets.SubscribePacket:
		topics := make([]string, 0, len(p.Topics))
		topics = append(topics, p.Topics...)
		n.Config.Log.Infof("MQTT SUBSCRIBE from %s: topics=%v", clientIP, topics)
	default:
		n.Config.Log.Debugf("MQTT %s packet from %s", packets.PacketNames[controlPacketType], clientIP)
	}

	var buf bytes.Buffer
	if err := packet.Write(&buf); err != nil {
		n.Config.Log.Errorf("Failed to serialize MQTT packet: %v", err)
		return
	}

	payload := buf.Bytes()
	n.Config.Log.Tracef("Forwarding %d bytes from %s", len(payload), clientIP)

	// Forward the packet to the backend
	n.forwardMQTT(conn, payload)
}

// Modified to accept only the payload without the first byte
func (n *ProtoBufServerCmd) forward(conn net.Conn, firstByte []byte, payload []byte) {
	address := n.Config.ProtoBufServer.ProxyForwardAddress
	backendConn, err := net.DialTimeout("tcp", address, 5*time.Second)
	if err != nil {
		n.Config.Log.Errorf("Failed to connect to backend MQTT broker: %v", err)
		return
	}
	defer backendConn.Close()

	// Write first byte and payload
	if _, err := backendConn.Write(firstByte); err != nil {
		n.Config.Log.Errorf("Failed to forward initial byte: %v", err)
		return
	}
	if _, err := backendConn.Write(payload); err != nil {
		n.Config.Log.Errorf("Failed to forward initial payload: %v", err)
		return
	}

	done := make(chan struct{}, 2)
	go func() {
		_, err = io.Copy(backendConn, conn) // client → backend
		if err != nil {
			n.Config.Log.Errorf("Failed to forward data from client to backend: %v", err)
		}
		done <- struct{}{}
	}()
	go func() {
		_, err = io.Copy(conn, backendConn) // backend → client
		if err != nil {
			n.Config.Log.Errorf("Failed to forward data from backend to client: %v", err)
		}
		done <- struct{}{}
	}()
	<-done
}

// New function to forward already-parsed MQTT packets
func (n *ProtoBufServerCmd) forwardMQTT(conn net.Conn, payload []byte) {
	address := n.Config.ProtoBufServer.ProxyForwardAddress
	backendConn, err := net.DialTimeout("tcp", address, 5*time.Second)
	if err != nil {
		n.Config.Log.Errorf("Failed to connect to backend MQTT broker: %v", err)
		return
	}
	defer backendConn.Close()

	if _, err := backendConn.Write(payload); err != nil {
		n.Config.Log.Errorf("Failed to forward initial payload: %v", err)
		return
	}

	done := make(chan struct{}, 2)
	go func() {
		_, err = io.Copy(backendConn, conn) // client → backend
		if err != nil {
			n.Config.Log.Errorf("Failed to forward data from client to backend: %v", err)
		}
		done <- struct{}{}
	}()
	go func() {
		_, err = io.Copy(conn, backendConn) // backend → client
		if err != nil {
			n.Config.Log.Errorf("Failed to forward data from backend to client: %v", err)
		}
		done <- struct{}{}
	}()
	<-done
}

func (n *ProtoBufServerCmd) handleInspector(conn net.Conn) {
	defer conn.Close()

	// Read incoming request
	buf := make([]byte, 4096)
	blen, err := conn.Read(buf)
	if err != nil {
		if err != io.EOF {
			n.Config.Log.Errorf("Read error: %v", err)
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
