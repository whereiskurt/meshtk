package server

import (
	"bufio"
	"bytes"
	"net"
	"testing"
	"time"

	"github.com/eclipse/paho.mqtt.golang/packets"
)

// writerConn is a minimal net.Conn over a bytes.Buffer — only Write is real,
// which is all writeMqtt5UnsupportedConnack touches.
type writerConn struct{ buf *bytes.Buffer }

func (w writerConn) Write(b []byte) (int, error)  { return w.buf.Write(b) }
func (w writerConn) Read([]byte) (int, error)     { return 0, nil }
func (w writerConn) Close() error                 { return nil }
func (w writerConn) LocalAddr() net.Addr          { return nil }
func (w writerConn) RemoteAddr() net.Addr         { return nil }
func (w writerConn) SetDeadline(time.Time) error  { return nil }
func (w writerConn) SetReadDeadline(time.Time) error  { return nil }
func (w writerConn) SetWriteDeadline(time.Time) error { return nil }

// buildConnect encodes a CONNECT packet with the given protocol name/version
// using the paho 3.1.1 codec. The bytes through the keepalive field are
// layout-identical across 3.1.1 and v5, which is exactly the region
// peekConnectProtocolVersion inspects — so this is a faithful stand-in for a
// real v5 CONNECT's leading bytes.
func buildConnect(t *testing.T, name string, version byte) []byte {
	t.Helper()
	cp := packets.NewControlPacket(packets.Connect).(*packets.ConnectPacket)
	cp.ProtocolName = name
	cp.ProtocolVersion = version
	cp.ClientIdentifier = "mqttastic-android-test"
	cp.UsernameFlag = true
	cp.Username = "ed270dbe5d1e"
	cp.PasswordFlag = true
	cp.Password = []byte("hunter2")
	cp.Keepalive = 60

	var buf bytes.Buffer
	if err := cp.Write(&buf); err != nil {
		t.Fatalf("encode CONNECT: %v", err)
	}
	return buf.Bytes()
}

func TestPeekConnectProtocolVersion(t *testing.T) {
	cases := []struct {
		desc    string
		raw     []byte
		wantVer byte
		wantOK  bool
	}{
		{"v3.1.1 CONNECT", buildConnect(t, "MQTT", 4), 4, true},
		{"v5 CONNECT", buildConnect(t, "MQTT", 5), 5, true},
		{"legacy MQIsdp 3.1 CONNECT", buildConnect(t, "MQIsdp", 3), 0, false},
		{"non-CONNECT first packet", func() []byte {
			pp := packets.NewControlPacket(packets.Publish).(*packets.PublishPacket)
			pp.TopicName = "msh/US/2/e/dc.run/!deadbeef"
			pp.Payload = []byte{0x01}
			var buf bytes.Buffer
			pp.Write(&buf)
			return buf.Bytes()
		}(), 0, false},
		{"short garbage", []byte{0x10, 0x02, 0x00}, 0, false},
	}

	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			r := bufio.NewReader(bytes.NewReader(tc.raw))
			ver, ok := peekConnectProtocolVersion(r)
			if ok != tc.wantOK || ver != tc.wantVer {
				t.Fatalf("got (ver=%d ok=%v), want (ver=%d ok=%v)", ver, ok, tc.wantVer, tc.wantOK)
			}

			// Peek must not consume: the packet has to remain fully readable
			// by the 3.1.1 codec afterwards (the pass-through path).
			if tc.wantOK {
				if _, err := packets.ReadPacket(r); err != nil {
					t.Fatalf("packet not re-readable after peek: %v", err)
				}
			}
		})
	}
}

func TestWriteMqtt5UnsupportedConnackWire(t *testing.T) {
	// v5 CONNACK: type 0x20, remaining length 3, ack-flags 0x00,
	// reason 0x84 (Unsupported Protocol Version), empty properties 0x00.
	want := []byte{0x20, 0x03, 0x00, 0x84, 0x00}

	var buf bytes.Buffer
	if err := writeMqtt5UnsupportedConnack(writerConn{&buf}); err != nil {
		t.Fatalf("write: %v", err)
	}
	if !bytes.Equal(buf.Bytes(), want) {
		t.Fatalf("wire bytes = %x, want %x", buf.Bytes(), want)
	}
}
