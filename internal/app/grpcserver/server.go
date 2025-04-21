package grpcserver

import (
	"io"
	"net"
	"os"
	"os/signal"

	meshtastic "github.com/whereiskurt/meshtk/protos/meshtastic/generated"

	"github.com/whereiskurt/meshtk/protos/security/generated"
	"google.golang.org/protobuf/proto"
)

func (n *GrpcServerCmd) StartInspectorServer() error {
	address := n.Config.GRpcServer.SecurityAddress

	ln, err := net.Listen("tcp", address)
	if err != nil {
		n.Config.Log.Errorf("Failed to listen: %v", err)
		return err
	}
	defer ln.Close()

	go func() {
		n.Config.Log.Infof("Meshtastic inspector protobuff server listening on %s", address)
		for {
			conn, err := ln.Accept()
			if err != nil {
				n.Config.Log.Printf("Accept error: %v", err)
				continue
			}
			go n.handleConn(conn)
		}
	}()

	n.Config.Log.Infof("Press CTRL+C to interrupt")

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt)

	<-stop
	n.Config.Log.Infof("Shutting down the server gracefully...")
	return nil

}
func (n *GrpcServerCmd) handleConn(conn net.Conn) {
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
	isMeshtastic := true
	var envelope meshtastic.ServiceEnvelope
	if err := proto.Unmarshal(req.Payload, &envelope); err != nil {
		n.Config.Log.Warnf("Not a Meshtastic ServiceEnvelope on %v: %v: %+v", topic, req.Payload, err)
		isMeshtastic = false
	}

	resp := &generated.PacketResponse{
		Payload:     req.Payload,   // no payload change
		ShouldBlock: !isMeshtastic, // if not meshtastic, block the packet
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
