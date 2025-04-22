package protoserver

import (
	"io"
	"net"
	"os"
	"os/signal"

	meshtastic "github.com/whereiskurt/meshtk/protos/meshtastic/generated"

	"github.com/whereiskurt/meshtk/protos/security/generated"
	"google.golang.org/protobuf/proto"
)

func (n *ProtoBufServerCmd) StartInspectorServer() error {
	address := n.Config.ProtoBufServer.InspectorAddress

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
				go n.handleConn(conn)
			}
		}
	}()

	n.Config.Log.Infof("Press CTRL+C to interrupt")

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt)

	<-stop
	n.Config.Log.Infof("Shutting down the server gracefully...")
	return nil

}
func (n *ProtoBufServerCmd) handleConn(conn net.Conn) {
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

	isMeshtastic := false
	shouldBlock := false
	blockReason := ""
	mutated := req.Payload
	var envelope meshtastic.ServiceEnvelope
	if err := proto.Unmarshal(req.Payload, &envelope); err != nil {
		n.Config.Log.Warnf("Not a Meshtastic ServiceEnvelope on %v: %v: %+v", topic, req.Payload, err)
		blockReason = "ServiceEnvelope"
	}

	if !isMeshtastic {
		n.Config.Log.Warnf("Not a Meshtastic packet on %v: from: %v: %v", topic, req.Username, req.Payload)
		blockReason = "NotMeshtastic"
		shouldBlock = true
	} else {
		isMeshtastic = true

		n.Config.Log.Infof("Mutating packet on %v from %v: original hop limit: %v", topic, req.Username, envelope.Packet.HopLimit)
		envelope.Packet.HopLimit = 3

		mutated, err = proto.Marshal(&envelope)
		if err != nil {
			n.Config.Log.Errorf("no mutate: failed to marshal data: %v", err)
			mutated = req.Payload
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
