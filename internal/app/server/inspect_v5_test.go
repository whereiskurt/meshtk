package server

import (
	"net"
	"testing"

	v5 "github.com/eclipse/paho.golang/packets"
	"github.com/eclipse/paho.mqtt.golang/packets"
	log "github.com/sirupsen/logrus"
)

// quietLogger keeps test output clean: every inspector path logs, and the
// assertions here are about return values and wire bytes, not log noise.
func quietLogger() *log.Logger {
	logger := log.New()
	logger.SetLevel(log.PanicLevel)
	return logger
}

// --- setPublishPayload: the codec-dispatched rewrite seam -------------------
//
// RewritePayloadString and RemarshalEnvelope used to end in a bare
// `switch p := (*ip.Raw.MQTT).(type)`. On a v5 connection Raw.MQTT is nil and
// that dereference panics; and if the switch had merely failed to match, the
// hop clamp and the payload censor would have become silent no-ops for every
// Android user -- which is exactly the meshtk#22 bug class.

func TestSetPublishPayloadDispatch3111(t *testing.T) {
	pub := packets.NewControlPacket(packets.Publish).(*packets.PublishPacket)
	pub.TopicName = "msh/US/2/e/dc.run/!435990e4"
	pub.Payload = []byte("before")
	var cp packets.ControlPacket = pub

	ip := &InspectorPacket{
		Log:   quietLogger(),
		Track: &ConnectionInfo{SocketAddress: "203.0.113.7:50000"},
		Raw:   &RawPacket{MQTT: &cp},
	}

	if err := ip.setPublishPayload([]byte("after")); err != nil {
		t.Fatalf("setPublishPayload: %v", err)
	}
	if string(pub.Payload) != "after" {
		t.Errorf("3.1.1 payload = %q, want %q", pub.Payload, "after")
	}
	if !ip.WireRewritten {
		t.Error("WireRewritten not set; the uplink loop would forward the pre-rewrite bytes")
	}
}

func TestSetPublishPayloadDispatchV5(t *testing.T) {
	cp := v5.NewControlPacket(v5.PUBLISH)
	p := cp.Content.(*v5.Publish)
	p.Topic = "msh/US/2/e/dc.run/!435990e4"
	p.Payload = []byte("before")

	ip := &InspectorPacket{
		Log:   quietLogger(),
		Track: &ConnectionInfo{SocketAddress: "203.0.113.7:50000", ProtocolVersion: 5},
		Raw:   &RawPacket{MQTT5: cp},
	}

	if err := ip.setPublishPayload([]byte("after")); err != nil {
		t.Fatalf("setPublishPayload: %v", err)
	}
	if string(p.Payload) != "after" {
		t.Errorf("v5 payload = %q, want %q", p.Payload, "after")
	}
	if !ip.WireRewritten {
		t.Error("WireRewritten not set; the v5 uplink loop would forward the captured frame instead of the rewrite")
	}
}

// Neither codec present must be a loud error, never a panic: a panic in the
// proxy read loop takes down the process, not the connection.
func TestSetPublishPayloadNeitherIsError(t *testing.T) {
	ip := &InspectorPacket{
		Log:   quietLogger(),
		Track: &ConnectionInfo{SocketAddress: "203.0.113.7:50000"},
		Raw:   &RawPacket{},
	}

	var err error
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("setPublishPayload panicked with no raw packet: %v", r)
			}
		}()
		err = ip.setPublishPayload([]byte("after"))
	}()

	if err == nil {
		t.Fatal("setPublishPayload returned nil with neither Raw.MQTT nor Raw.MQTT5 set")
	}
	if ip.WireRewritten {
		t.Error("WireRewritten set even though nothing was written")
	}
}

// A non-PUBLISH packet cannot carry a rewritten envelope either. Erroring keeps
// a misrouted rewrite loud instead of silently dropping it on the floor.
func TestSetPublishPayloadNonPublishIsError(t *testing.T) {
	var cp packets.ControlPacket = packets.NewControlPacket(packets.Connect).(*packets.ConnectPacket)
	ip := &InspectorPacket{
		Log:   quietLogger(),
		Track: &ConnectionInfo{},
		Raw:   &RawPacket{MQTT: &cp},
	}
	if err := ip.setPublishPayload([]byte("after")); err == nil {
		t.Fatal("setPublishPayload accepted a CONNECT packet")
	}
	if ip.WireRewritten {
		t.Error("WireRewritten set for a packet nothing was written to")
	}
}

// --- logDownlinkEnvelope: the payload/topic core the v5 loop calls ----------
//
// The extraction is only safe if the wrapper and the core agree on every
// decision. logDownlink keeps its 3.1.1 signature and TestSelfEchoSuppression
// keeps passing untouched; this pins the two implementations to each other.
func TestLogDownlinkEnvelopeParity(t *testing.T) {
	n := newSelfEchoTestServer(t)
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	const addr = "203.0.113.7:50000"
	n.ConnTrack[addr] = &ConnectionInfo{SocketAddress: addr, GatewayID: "!435990e4"}

	cases := []struct {
		desc     string
		addr     string
		gateway  string
		to       uint32
		suppress bool
	}{
		{"own gateway echoed back", addr, "!435990e4", 0x1555f041, true},
		{"another gateway's reply", addr, "!1555f041", 0x435990e4, false},
		{"broadcast fan-out", addr, "!1555f041", meshBroadcast, false},
		{"connection that never uplinked", "198.51.100.9:1234", "!435990e4", 0x1555f041, false},
		{"envelope with no gateway id", addr, "", 0x435990e4, false},
	}

	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			pub := downlinkPublish(t, tc.gateway, 0x435990e4, tc.to)

			wrapper := n.logDownlink(server, tc.addr, pub)
			core := n.logDownlinkEnvelope(server, tc.addr, pub.Payload, pub.TopicName)

			if wrapper != core {
				t.Fatalf("logDownlink = %v but logDownlinkEnvelope = %v; the extraction changed behavior", wrapper, core)
			}
			if wrapper != tc.suppress {
				t.Fatalf("suppress = %v, want %v", wrapper, tc.suppress)
			}
		})
	}

	// A payload that is not a ServiceEnvelope at all must not suppress.
	if n.logDownlinkEnvelope(server, addr, []byte{0xff, 0xff, 0xff, 0xff}, "msh/US/2/e/dc.run/x") {
		t.Error("unparseable payload suppressed; downlink would silently disappear")
	}
}
