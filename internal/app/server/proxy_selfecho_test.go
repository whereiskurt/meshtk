package server

import (
	"net"
	"testing"

	"github.com/eclipse/paho.mqtt.golang/packets"
	log "github.com/sirupsen/logrus"
	"github.com/whereiskurt/meshtk/pkg/config"
	meshtastic "github.com/whereiskurt/meshtk/protos/meshtastic/generated"
	"google.golang.org/protobuf/proto"
)

func newSelfEchoTestServer(t *testing.T) *ServerCmd {
	t.Helper()
	logger := log.New()
	logger.SetLevel(log.PanicLevel)
	return &ServerCmd{
		Config:    &config.Config{Log: logger},
		ConnTrack: map[string]*ConnectionInfo{},
	}
}

func downlinkPublish(t *testing.T, gatewayID string, from, to uint32) *packets.PublishPacket {
	t.Helper()
	envelope := &meshtastic.ServiceEnvelope{
		Packet: &meshtastic.MeshPacket{
			From: from,
			To:   to,
			Id:   0x1234abcd,
			PayloadVariant: &meshtastic.MeshPacket_Encrypted{
				Encrypted: []byte{0xde, 0xad},
			},
		},
		GatewayId: gatewayID,
		ChannelId: "PKI",
	}
	payload, err := proto.Marshal(envelope)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	pub := packets.NewControlPacket(packets.Publish).(*packets.PublishPacket)
	pub.TopicName = "msh/US/2/e/PKI/" + gatewayID
	pub.Payload = payload
	return pub
}

// A downlink packet gatewayed by the SAME connection is a broker self-echo:
// suppress it. Everything else -- other gateways, unknown connections,
// envelopes without a gateway id -- must flow.
func TestSelfEchoSuppression(t *testing.T) {
	n := newSelfEchoTestServer(t)
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	const addr = "203.0.113.7:50000"
	n.ConnTrack[addr] = &ConnectionInfo{SocketAddress: addr, GatewayID: "!435990e4"}

	// The connection's own uplink echoed back: suppress.
	if !n.logDownlink(server, addr, downlinkPublish(t, "!435990e4", 0x435990e4, 0x1555f041)) {
		t.Error("own-gateway echo was forwarded; the radio gets its own DM back down BLE")
	}

	// A different gateway's packet (e.g. a ghost's reply): forward.
	if n.logDownlink(server, addr, downlinkPublish(t, "!1555f041", 0x1555f041, 0x435990e4)) {
		t.Error("another gateway's packet was suppressed; replies would never reach the radio")
	}

	// Connection with no recorded gateway (map viewers, meshobserv): forward.
	if n.logDownlink(server, "198.51.100.9:1234", downlinkPublish(t, "!435990e4", 0x435990e4, 0x1555f041)) {
		t.Error("packet suppressed for a connection that never uplinked; subscribers-only clients lose traffic")
	}

	// Envelope without a gateway id: forward.
	if n.logDownlink(server, addr, downlinkPublish(t, "", 0x99, 0x435990e4)) {
		t.Error("gateway-less envelope suppressed; empty must never match")
	}
}

// The uplink inspector must record the gateway id a connection publishes as --
// without that, suppression can never match and silently degrades to off.
func TestRememberGatewayRoundTrip(t *testing.T) {
	n := newSelfEchoTestServer(t)
	const addr = "203.0.113.7:50000"

	// Unknown connection: no-op, no panic.
	n.rememberGateway(addr, "!435990e4")
	if got := n.gatewayFor(addr); got != "" {
		t.Errorf("gatewayFor untracked conn = %q, want empty", got)
	}

	n.ConnTrack[addr] = &ConnectionInfo{SocketAddress: addr}
	n.rememberGateway(addr, "!435990e4")
	if got := n.gatewayFor(addr); got != "!435990e4" {
		t.Errorf("gatewayFor = %q, want !435990e4", got)
	}
}
