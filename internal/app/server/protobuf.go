package server

import (
	"io"
	"net"

	meshtastic "github.com/whereiskurt/meshtk/protos/meshtastic/generated"

	"github.com/whereiskurt/meshtk/protos/security/generated"
	"google.golang.org/protobuf/proto"
)

func (n *ServerCmd) handleProtobuf(conn net.Conn) {
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
