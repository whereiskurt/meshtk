package mqtt

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"os"
	"os/signal"
	"strings"

	"syscall"
	"time"

	"google.golang.org/protobuf/proto"

	mqtt "github.com/eclipse/paho.mqtt.golang"

	log "github.com/sirupsen/logrus"
	"github.com/whereiskurt/meshtk/internal/keycache"
	"github.com/whereiskurt/meshtk/pkg/config"
	meshtastic "github.com/whereiskurt/meshtk/protos/meshtastic/generated"
)

type MqttClient struct {
	log            *log.Logger
	blockCipher    cipher.Block
	messageHandler func(to, from uint32, topic string, portNum meshtastic.PortNum, payload []byte)
	ackHandler     func(to, from uint32, requestId uint32) // Handler for sending ACKs
	nackHandler    func(to, from uint32, requestId uint32) // Handler for routing NACK to tell sender to initiate nodeinfo
	client         mqtt.Client
	topics         []string
	channel        string //Needed for meshtastic publish packet construction
	key            string //Needed for meshtastic publish packet construction
	pkiPrivateKey  []byte
	pkiPublicKey   []byte
	nodes          *NodeDB

	// keyResolver is the shared, process-wide authoritative pubkey source
	// (internal/keycache, DDB MeshRadio). ONE resolver is threaded into every
	// fleet client so all ~34 in-process clients share one cache — never
	// per-client, never per-packet. When nil (e.g. the nodeinfo utility, or a
	// resolver that failed to build), resolveSenderPubKey preserves the legacy
	// nodes.json feed behavior.
	keyResolver *keycache.KeyResolver
	// keyFallback selects miss behavior: "nodes.json" (bring-up) resolves misses
	// via the broadcast feed; "none" (poisoning-resistant) returns an error so the
	// existing nackHandler fires. Plan 66-07; default stays "nodes.json".
	keyFallback string
	// nodesFeedFn is a test seam for the nodes.json fallback branch. nil → the
	// real FetchPublicKeyFromDefcon (feed + pubKeyCache).
	nodesFeedFn func(nodeNum uint32) (string, error)

	// ackStyle shapes the MeshPacket metadata of published ACKs: "faithful"
	// (default, firmware-shaped) or "legacy" (historical fabricated rx fields).
	// See config.Config.AckMode; "off" never reaches here (no handler is wired).
	ackStyle string
}

// SetAckStyle selects the ACK packet shape ("faithful" or "legacy").
func (c *MqttClient) SetAckStyle(style string) {
	c.ackStyle = style
}

// SetKeyResolver wires the shared authoritative pubkey resolver and the miss
// fallback mode into this client. Called once per fleet client with the ONE
// process-wide resolver built at fleet startup.
func (c *MqttClient) SetKeyResolver(r *keycache.KeyResolver, fallback string) {
	c.keyResolver = r
	c.keyFallback = fallback
}

func NewMqttClient(c *config.Config, nodes *NodeDB, handler func(to, from uint32, topic string, portNum meshtastic.PortNum, payload []byte)) *MqttClient {
	mqc := MqttClient{
		log:     c.Log,
		nodes:   nodes,
		channel: c.Meshtastic.Channels[0].Name,
		key:     c.Meshtastic.Channels[0].EncryptKey,
	}

	var err error

	mqc.pkiPublicKey, err = hex.DecodeString(strings.TrimPrefix(c.NodeInfo.PKI.PublicKey, "0x"))
	if err != nil {
		c.Log.Errorf("failed to decode public key: %v", err)
	}
	mqc.pkiPrivateKey, err = hex.DecodeString(strings.TrimPrefix(c.NodeInfo.PKI.PrivateKey, "0x"))
	if err != nil {
		c.Log.Errorf("failed to decode private key: %v", err)
	}

	base64Key := c.Meshtastic.Channels[0].EncryptKey
	keyBytes, err := base64.StdEncoding.DecodeString(base64Key)
	if err != nil {
		c.Log.Fatalf("The PRIMARY channel key '%s' is invalid hex: %v", c.Meshtastic.Channels[0].EncryptKey, err)
	}

	// Expand the single byte key to 16 bytes for AES-256
	if len(keyBytes) == 1 && base64Key == "AQ==" {
		keyBytes = append(keyBytes, make([]byte, 15)...)
		//c.Meshtastic.Channels[0].EncryptKey = base64.StdEncoding.EncodeToString(keyBytes)
	}

	mqc.blockCipher = NewAESCipher(keyBytes)

	mqc.SetMessageHandler(handler)

	opts := mqtt.NewClientOptions()

	opts.SetConnectRetry(true)
	opts.SetConnectRetryInterval(5 * time.Second)
	opts.SetDefaultPublishHandler(mqc.dispatcher)
	opts.SetKeepAlive(5 * time.Second)

	opts.OnConnectionLost = func(_ mqtt.Client, err error) {
		c.Log.Warnf("MQTT broker connection was lost unexpectedly: %v", err)
	}

	opts.OnConnect = func(_ mqtt.Client) {
		// c.Log.Tracef("MQTT connected")
		if err := mqc.subscribeMultiple(c.NodeInfo.SubscribedTopics); err != nil {
			c.Log.Errorf("MQTT subscribe failed: %v", err)
		}
	}

	randomHex := make([]byte, 16) // 8 bytes = 16 hex digits
	rand.Read(randomHex)
	rndHex := hex.EncodeToString(randomHex)

	opts.AddBroker(c.Mqtt.BrokerUri)
	opts.SetUsername(c.Mqtt.Username)
	opts.SetPassword(c.Mqtt.Password)
	opts.SetClientID(fmt.Sprintf("%s-%s", c.Mqtt.ClientId, rndHex))
	opts.WillEnabled = false

	mqc.client = mqtt.NewClient(opts)

	// Populate the loggers to trickle up mqtt logs
	// mqtt.ERROR = c.Log
	// mqtt.CRITICAL = c.Log
	// mqtt.WARN = c.Log
	// mqtt.DEBUG = c.Log

	return &mqc
}

func (c *MqttClient) dispatcher(_ mqtt.Client, msg mqtt.Message) {
	topic := msg.Topic()

	if topic == "/will" {
		// c.log.Debugf("skippng parse ServiceEnvelope on %v: %+v", topic, msg.Payload())
		return
	}

	var envelope meshtastic.ServiceEnvelope
	if err := proto.Unmarshal(msg.Payload(), &envelope); err != nil {
		//c.log.Debugf("could not parse ServiceEnvelope on %v: %v: %+v", topic, err, msg.Payload())
		return
	}

	packet := envelope.GetPacket()
	if packet == nil {
		// c.log.Debugf("skipping ServiceEnvelope with no MeshPacket on %v", topic)
		return
	}

	from := packet.GetFrom()
	to := packet.GetTo()

	// isEncrypted := false
	data := packet.GetDecoded()
	if data == nil {
		encrypted := packet.GetEncrypted()
		if encrypted == nil {
			c.log.Warnf("skipping MeshPacket from %v with no data on %v", from, topic)
			return
		}
		nonce := make([]byte, 16)
		binary.LittleEndian.PutUint32(nonce[0:], packet.GetId())
		binary.LittleEndian.PutUint32(nonce[8:], from)

		if !packet.GetPkiEncrypted() {
			decrypted := make([]byte, len(encrypted))
			cipher.NewCTR(c.blockCipher, nonce).XORKeyStream(decrypted, encrypted)
			data = new(meshtastic.Data)
			if err := proto.Unmarshal(decrypted, data); err != nil {
				//c.log.Errorf("failed to unmarshal decrypted data: %v", err)
				return
			}
			// isEncrypted = true
		} else {
			c.log.Tracef("MeshPacket from %v with PKI encryption on %v", from, topic)

			// PKI Decryption using firmware2-exact implementation
			pkiDecrypted, pkiErr := c.decryptPKI(packet, encrypted)
			if pkiErr != nil {
				c.log.Warnf("PKI decrypt failed for packet from %v on %v: %v", from, topic, pkiErr)
				// Send NACK to tell sender to initiate nodeinfo message
				if c.nackHandler != nil {
					c.log.Debugf("Sending NACK for PKI message from %v to %v (request_id: %v)", from, to, packet.GetId())
					c.nackHandler(to, from, packet.GetId())
				}
				return
			}
			data = new(meshtastic.Data)
			if err := proto.Unmarshal(pkiDecrypted, data); err != nil {
				c.log.Errorf("PKI unmarshal error for decrypted data: %v", err)
				return
			}

			c.log.Tracef("Successfully PKI decrypted payload from %v on %v", from, topic)

			// Only ack when the sender asked for one -- firmware never acks
			// unsolicited (want_ack=false) packets, and a real radio's DM
			// always sets want_ack (that flag is what drives its retransmits).
			if packet.GetWantAck() && c.ackHandler != nil {
				c.log.Debugf("Sending ACK for PKI message from %v to %v (request_id: %v)", from, to, packet.GetId())
				c.ackHandler(to, from, packet.GetId())
			}
		}
	}

	portNum := data.GetPortnum()
	if portNum == 0 {
		c.log.Warnf("skipping Data from %v with no portnum on %v", from, topic)
		return
	}

	payload := data.GetPayload()
	if payload == nil {
		c.log.Warnf("skipping Data from %v with no payload on %v", from, topic)
		return
	}

	// Every NODEINFO flowing past feeds the observed-pubkey map (OTP delivery's
	// last-resort recipient key).
	if portNum == meshtastic.PortNum_NODEINFO_APP {
		noteObservedNodeInfo(from, payload)
	}

	//c.log.Tracef(`{'from': %v, 'topic': '%v', 'portNum': %v, 'isEncrypted': %v, 'payload': '0x%x'}`, from, topic, portNum, isEncrypted, payload)
	c.messageHandler(to, from, topic, portNum, payload)
}

func (c *MqttClient) subscribeMultiple(topics []string) error {
	if c.messageHandler == nil {
		return fmt.Errorf("message handler is not set")
	}

	if !c.client.IsConnected() {
		return fmt.Errorf("mqtt client is not connected")
	}

	topicQos := make(map[string]byte)
	for _, topic := range topics {
		topicQos[topic] = 0 //QoS 0
	}
	token := c.client.SubscribeMultiple(topicQos, nil)
	<-token.Done()
	if err := token.Error(); err != nil {
		return fmt.Errorf("mqtt subscribe failed: %v", err)
	}
	return nil
}

func (c *MqttClient) SetAckHandler(handler func(to, from uint32, requestId uint32)) {
	c.ackHandler = handler
}

func (c *MqttClient) SetNackHandler(handler func(to, from uint32, requestId uint32)) {
	c.nackHandler = handler
}

func (c *MqttClient) Connect() error {

	token := c.client.Connect()

	if !token.WaitTimeout(5 * time.Second) {
		return fmt.Errorf("MQTT connect timeout after 5 seconds")
	}

	return nil
}

func (c *MqttClient) Disconnect() {
	if c.client.IsConnected() {
		c.client.Disconnect(1000)
	}
}

func (c *MqttClient) WaitUntilKill() {
	terminate := make(chan os.Signal, 1)
	signal.Notify(terminate, syscall.SIGINT, syscall.SIGTERM)
	<-terminate
	c.Disconnect()
}

func (c *MqttClient) ConnectAndListen(topics []string) error {
	c.log.Tracef("MQTT ConnectAndListen ...")

	c.topics = topics

	if c.client.IsConnected() {
		c.log.Tracef("mqtt already connected, disconnecting first...")
		c.Disconnect()
	}
	err := c.Connect()
	if err != nil {
		c.log.Error(err)
		return err
	}

	err = c.subscribeMultiple(topics)
	if err != nil {
		c.log.Error(err)
		return err
	}

	c.log.Tracef("background listening on topics: %v", c.topics)

	return nil
}

func (c *MqttClient) ReconnectAndListen() error {

	if c.topics == nil {
		return fmt.Errorf("no topics were previously listening")
	}
	return c.ConnectAndListen(c.topics)
}

func (c *MqttClient) SetMessageHandler(f func(to, from uint32, topic string, portNum meshtastic.PortNum, payload []byte)) {
	c.messageHandler = f
}

func NewAESCipher(key []byte) cipher.Block {
	c, err := aes.NewCipher(key)
	if err != nil {
		panic(err)
	}
	return c
}

func (c *MqttClient) GenerateKeyPair() {
	curve := ecdh.X25519()

	privateKey, err := curve.GenerateKey(rand.Reader)
	if err != nil {
		return
	}

	publicKeyBytes := privateKey.PublicKey().Bytes()
	privateKeyBytes := privateKey.Bytes()

	fmt.Printf("Public Key: %x\n", publicKeyBytes)
	fmt.Printf("Private Key: %x\n", privateKeyBytes)

	// c.pkiPrivateKey = privateKey

}
