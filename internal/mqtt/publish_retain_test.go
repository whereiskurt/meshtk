package mqtt

import (
	"crypto/aes"
	"testing"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	log "github.com/sirupsen/logrus"
	meshtastic "github.com/whereiskurt/meshtk/protos/meshtastic/generated"
)

// publishCall records one Publish invocation on the fake broker client.
type publishCall struct {
	topic   string
	qos     byte
	retain  bool
	payload []byte
}

// doneToken is an already-completed, error-free paho Token.
type doneToken struct{ ch chan struct{} }

func newDoneToken() *doneToken {
	c := make(chan struct{})
	close(c)
	return &doneToken{ch: c}
}

func (t *doneToken) Wait() bool                     { return true }
func (t *doneToken) WaitTimeout(time.Duration) bool { return true }
func (t *doneToken) Done() <-chan struct{}          { return t.ch }
func (t *doneToken) Error() error                   { return nil }

// fakeBroker implements paho's mqtt.Client and records publishes. Nothing leaves
// the process.
type fakeBroker struct{ calls []publishCall }

func (f *fakeBroker) IsConnected() bool      { return true }
func (f *fakeBroker) IsConnectionOpen() bool { return true }
func (f *fakeBroker) Connect() mqtt.Token    { return newDoneToken() }
func (f *fakeBroker) Disconnect(uint)        {}
func (f *fakeBroker) Publish(topic string, qos byte, retained bool, payload interface{}) mqtt.Token {
	b, _ := payload.([]byte)
	f.calls = append(f.calls, publishCall{topic: topic, qos: qos, retain: retained, payload: b})
	return newDoneToken()
}
func (f *fakeBroker) Subscribe(string, byte, mqtt.MessageHandler) mqtt.Token { return newDoneToken() }
func (f *fakeBroker) SubscribeMultiple(map[string]byte, mqtt.MessageHandler) mqtt.Token {
	return newDoneToken()
}
func (f *fakeBroker) Unsubscribe(...string) mqtt.Token        { return newDoneToken() }
func (f *fakeBroker) AddRoute(string, mqtt.MessageHandler)    {}
func (f *fakeBroker) OptionsReader() mqtt.ClientOptionsReader { return mqtt.ClientOptionsReader{} }

func newRetainTestClient(t *testing.T) (*MqttClient, *fakeBroker) {
	t.Helper()
	block, err := aes.NewCipher(make([]byte, 32))
	if err != nil {
		t.Fatalf("aes: %v", err)
	}
	fb := &fakeBroker{}
	logger := log.New()
	logger.SetLevel(log.PanicLevel)
	return &MqttClient{
		log:         logger,
		blockCipher: block,
		client:      fb,
		channel:     "dc.run",
		key:         "AQ==",
	}, fb
}

// Part 1: NodeInfo is the ONE retained publish. A reconnecting radio (the iOS
// BLE<->MQTT proxy flaps ~every 60s) then re-learns the whole fleet at SUBSCRIBE
// time instead of waiting minutes for each node's next beacon.
func TestPublishNodeInfoRetains(t *testing.T) {
	c, fb := newRetainTestClient(t)

	err := c.PublishNodeInfo(0x435990e4, 0xffffffff, "msh/US/2/e/dc.run/!435990e4",
		"Test Node", "TN", make([]byte, 32),
		meshtastic.HardwareModel_HELTEC_V3, meshtastic.Config_DeviceConfig_CLIENT)
	if err != nil {
		t.Fatalf("PublishNodeInfo: %v", err)
	}
	if len(fb.calls) != 1 {
		t.Fatalf("publishes = %d, want 1", len(fb.calls))
	}
	if !fb.calls[0].retain {
		t.Error("NodeInfo published with retain=false; reconnecting radios will not re-learn the fleet")
	}
}

// SECURITY, non-negotiable: retaining a PKI direct message would replay a
// private one-to-one DM to every future subscriber of the topic. Channel text,
// ACKs, Position and MapReport also stay unretained (stale positions are worse
// than none, and they share the NodeInfo topic — a retained Position would
// displace the retained NodeInfo).
func TestOnlyNodeInfoRetains(t *testing.T) {
	priv := make([]byte, 32)
	pub := make([]byte, 32)
	pub[0] = 9

	cases := []struct {
		name string
		call func(*MqttClient) error
	}{
		{"channel text", func(c *MqttClient) error {
			return c.PublishMessageEncrypted(1, 2, "msh/US/2/e/dc.run/!00000001",
				meshtastic.PortNum_TEXT_MESSAGE_APP, []byte("hi"))
		}},
		{"plain", func(c *MqttClient) error {
			return c.PublishMessagePlain(1, 2, "msh/US/2/e/dc.run/!00000001",
				meshtastic.PortNum_TEXT_MESSAGE_APP, []byte("hi"))
		}},
		{"ack", func(c *MqttClient) error {
			return c.PublishACK(1, 2, "msh/US/2/e/dc.run/!00000001", 42, []byte{})
		}},
		{"position", func(c *MqttClient) error {
			return c.PublishPosition(1, 2, "msh/US/2/e/dc.run/!00000001", 1, 2, 3, 32)
		}},
		{"map report", func(c *MqttClient) error {
			return c.PublishMapReport(1, 2, "msh/US/2/e/dc.run/!00000001", "Long", "Sh",
				meshtastic.HardwareModel_HELTEC_V3, meshtastic.Config_DeviceConfig_CLIENT,
				"2.5.0", "US", "LONG_FAST", true, 1, 1, 2, 3, 32)
		}},
		{"pki dm", func(c *MqttClient) error {
			return c.PublishPKIMessage(1, 2, "msh/US/2/e/PKI/!00000001",
				meshtastic.PortNum_TEXT_MESSAGE_APP, []byte("secret"), priv, pub)
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, fb := newRetainTestClient(t)
			if err := tc.call(c); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if len(fb.calls) == 0 {
				t.Fatalf("%s: nothing published", tc.name)
			}
			for i, pc := range fb.calls {
				if pc.retain {
					t.Errorf("%s: publish[%d] to %s used retain=true; only NodeInfo may be retained",
						tc.name, i, pc.topic)
				}
			}
		})
	}
}
