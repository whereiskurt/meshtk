package mqtt

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"os"
	"os/signal"
	"strings"

	"syscall"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	log "github.com/sirupsen/logrus"
	"github.com/whereiskurt/meshtk/pkg/config"
	meshtastic "github.com/whereiskurt/meshtk/protos/meshtastic/generated"
	"google.golang.org/protobuf/proto"
)

type MqttClient struct {
	log            *log.Logger
	blockCipher    cipher.Block
	messageHandler func(to, from uint32, topic string, portNum meshtastic.PortNum, payload []byte)
	client         mqtt.Client
	topics         []string
	channel        string //Needed for meshtastic publish packet construction
	key            string //Needed for meshtastic publish packet construction
	pkiPrivateKey  []byte
	pkiPublicKey   []byte
	nodes          *NodeDB
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
		c.Log.Tracef("MQTT connected")
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

	var envelope meshtastic.ServiceEnvelope
	if err := proto.Unmarshal(msg.Payload(), &envelope); err != nil {
		c.log.Warnf("could not parse ServiceEnvelope on %v: %v: %+v", topic, err, msg.Payload())
		return
	}

	packet := envelope.GetPacket()
	if packet == nil {
		c.log.Warnf("skipping ServiceEnvelope with no MeshPacket on %v", topic)
		return
	}

	from := packet.GetFrom()
	to := packet.GetTo()

	isEncrypted := false
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
				c.log.Errorf("failed to unmarshal decrypted data: %v", err)
				return
			}
			isEncrypted = true
		} else {
			c.log.Tracef("MeshPacket from %v with PKI encryption on %v", from, topic)

			// payload := data.GetPayload()
			copy(nonce[8:11], encrypted[len(encrypted)-4:])

			pkiDecrypted, pkiErr := c.decryptWithPKI(from, nonce, encrypted)
			if pkiErr != nil {
				c.log.Warnf("PKI(1) decrypt error failed for packet from %v on %v: %v", from, topic, pkiErr)
				return
			}
			data = new(meshtastic.Data)
			if err := proto.Unmarshal(pkiDecrypted, data); err != nil {
				c.log.Errorf("PKI(2) unmarshal error decrypted data: %v", err)
				return
			}

			// c.log.Tracef("success PKI decrypted payload from %v on %v: %x", from, topic, pkiDecrypted)
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

	c.log.Tracef(`{'from': %v, 'topic': '%v', 'portNum': %v, 'isEncrypted': %v, 'payload': '0x%x'}`, from, topic, portNum, isEncrypted, payload)
	c.messageHandler(to, from, topic, portNum, payload)
}

// This is not working yet and totally wrong actually
// Review the source: src/mesh/CryptoEngine.cpp in the meshtastic_firmware repo
func (c *MqttClient) decryptWithPKI(from uint32, nonce []byte, encrypted []byte) ([]byte, error) {
	//Do we have the senders public key?
	node, exists := (*c.nodes)[from]
	if !exists {
		return nil, fmt.Errorf("haven't see node with ID %v does not exist", from)
	}

	publicKeyBytes, err := hex.DecodeString(strings.TrimPrefix(node.PubKey, "0x"))
	if err != nil {
		return nil, fmt.Errorf("failed to read public key: %v", err)
	}

	sharedSecret, err := GenerateSharedSecret(c.pkiPrivateKey, publicKeyBytes)
	if err != nil {
		return nil, fmt.Errorf("failed to generate shared secret: %v", err)
	}

	// Ensure the shared secret is the correct length for AES
	if len(sharedSecret) != 32 {
		return nil, fmt.Errorf("unexpected shared secret length: %d", len(sharedSecret))
	}

	// Create AES cipher block
	block, err := aes.NewCipher(sharedSecret)
	if err != nil {
		return nil, fmt.Errorf("failed to create AES cipher: %v", err)
	}

	// Decrypt the message using AES-CTR
	decrypted := make([]byte, len(encrypted))
	cipher.NewCTR(block, nonce).XORKeyStream(decrypted, encrypted)

	return decrypted, nil
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

func (c *MqttClient) Connect() error {
	token := c.client.Connect()
	<-token.Done()
	if err := token.Error(); err != nil {
		return err
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

func GenerateSharedSecret(privateKeyBytes, publicKeyBytes []byte) ([]byte, error) {
	curve := ecdh.X25519()

	// Create private key object
	privateKey, err := curve.NewPrivateKey(privateKeyBytes)
	if err != nil {
		return nil, fmt.Errorf("invalid private key: %v", err)
	}

	// Create public key object
	publicKey, err := curve.NewPublicKey(publicKeyBytes)
	if err != nil {
		return nil, fmt.Errorf("invalid public key: %v", err)
	}

	// Perform ECDH key exchange to generate the shared secret
	sharedSecret, err := privateKey.ECDH(publicKey)
	if err != nil {
		return nil, fmt.Errorf("ECDH key exchange failed: %v", err)
	}

	hash := sha256.Sum256(sharedSecret)
	return hash[:], nil
}
