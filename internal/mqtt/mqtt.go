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

	"github.com/pschlump/aesccm"
	"google.golang.org/protobuf/proto"

	mqtt "github.com/eclipse/paho.mqtt.golang"

	log "github.com/sirupsen/logrus"
	"github.com/whereiskurt/meshtk/pkg/config"
	meshtastic "github.com/whereiskurt/meshtk/protos/meshtastic/generated"
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
		c.log.Debugf("skippng parse ServiceEnvelope on %v: %+v", topic, msg.Payload())
		return
	}

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

	// isEncrypted := false
	data := packet.GetDecoded()
	if data == nil {
		encrypted := packet.GetEncrypted()
		if encrypted == nil {
			c.log.Warnf("skipping MeshPacket from %v with no data on %v", from, topic)
			return
		}

		if !packet.GetPkiEncrypted() {
			// Construct base nonce: [8-byte packetId][4-byte fromNode][4-byte extraNonce/counter]
			nonce := make([]byte, 16)
			binary.LittleEndian.PutUint64(nonce[0:8], uint64(packet.GetId()))
			binary.LittleEndian.PutUint32(nonce[8:12], from)
			binary.LittleEndian.PutUint32(nonce[12:16], 0) // extraNonce/counter

			// Standard AES-CTR decryption for channel messages
			decrypted := make([]byte, len(encrypted))
			cipher.NewCTR(c.blockCipher, nonce).XORKeyStream(decrypted, encrypted)
			data = new(meshtastic.Data)
			if err := proto.Unmarshal(decrypted, data); err != nil {
				c.log.Errorf("failed to unmarshal decrypted data: %v", err)
				return
			}
			// isEncrypted = true
		} else {
			c.log.Tracef("MeshPacket from %v with PKI encryption on %v", from, topic)

			// PKI Decryption using exact C# Meshtastic implementation
			pkiDecrypted, pkiErr := c.decryptPKI(packet, encrypted)
			if pkiErr != nil {
				c.log.Warnf("PKI decrypt failed for packet from %v on %v: %v", from, topic, pkiErr)
				return
			}
			data = new(meshtastic.Data)
			c.log.Tracef("PKI attempting to unmarshal decrypted data: %x", pkiDecrypted)
			c.log.Tracef("PKI decrypted data as string: %q", string(pkiDecrypted))
			if err := proto.Unmarshal(pkiDecrypted, data); err != nil {
				c.log.Errorf("PKI unmarshal error for decrypted data: %v", err)
				c.log.Tracef("PKI trying to interpret as raw text: %s", string(pkiDecrypted))
				return
			}

			c.log.Tracef("Successfully PKI decrypted payload from %v on %v", from, topic)
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

// decryptPKI implements PKI decryption exactly like the C# Meshtastic implementation
func (c *MqttClient) decryptPKI(packet *meshtastic.MeshPacket, encryptedData []byte) ([]byte, error) {
	c.log.Debugf("PKI decrypt: packet ID=%d, from=%d, to=%d", packet.GetId(), packet.GetFrom(), packet.GetTo())
	c.log.Debugf("PKI encrypted data (%d bytes): %x", len(encryptedData), encryptedData)

	// Get recipient's private key from nodeDB
	recipientNode, exists := (*c.nodes)[packet.GetTo()]
	if !exists {
		return nil, fmt.Errorf("recipient node %d not found in nodeDB", packet.GetTo())
	}

	// Parse and validate recipient's private key

	c.log.Debugf("KPHKPH: Recipient node: %+v", recipientNode.PrivKey)
	recipientPrivateKeyBytes, err := c.parsePrivateKey(recipientNode.PrivKey)
	if err != nil {
		return nil, fmt.Errorf("failed to parse recipient private key: %v", err)
	}

	// Get sender's public key from packet
	senderPublicKeyBytes := packet.GetPublicKey()
	if len(senderPublicKeyBytes) != 32 {
		return nil, fmt.Errorf("invalid sender public key length: %d", len(senderPublicKeyBytes))
	}

	c.log.Debugf("Recipient private key: %x", recipientPrivateKeyBytes)
	c.log.Debugf("Sender public key: %x", senderPublicKeyBytes)

	// Extract extraNonce from last 4 bytes (C# implementation)
	if len(encryptedData) < 4 {
		return nil, fmt.Errorf("encrypted data too short: %d bytes", len(encryptedData))
	}

	extraNonce := binary.LittleEndian.Uint32(encryptedData[len(encryptedData)-4:])
	c.log.Debugf("Extra nonce: %d", extraNonce)

	// Call the exact C# Decrypt method equivalent
	return c.decrypt(recipientPrivateKeyBytes, senderPublicKeyBytes, encryptedData, uint32(packet.GetId()), uint32(packet.GetFrom()))
}

// parsePrivateKey parses and validates a private key, handling known corruption patterns
func (c *MqttClient) parsePrivateKey(privKeyHex string) ([]byte, error) {
	// Remove 0x prefix if present
	privKeyHex = strings.TrimPrefix(privKeyHex, "0x")

	c.log.Debugf("Raw private key from DB: %s", privKeyHex)

	privKeyBytes, err := hex.DecodeString(privKeyHex)
	if err != nil {
		return nil, fmt.Errorf("invalid hex: %v", err)
	}

	if len(privKeyBytes) != 32 {
		return nil, fmt.Errorf("invalid key length: %d, expected 32", len(privKeyBytes))
	}

	return privKeyBytes, nil
}

// decrypt implements the exact C# Decrypt method
func (c *MqttClient) decrypt(recipientPrivateKey, senderPublicKey, encryptedData []byte, packetId, senderNodeId uint32) ([]byte, error) {
	// 1. Generate shared key (C# GenerateSharedKey)
	sharedKey, err := c.generateSharedKey(recipientPrivateKey, senderPublicKey)
	if err != nil {
		return nil, fmt.Errorf("failed to generate shared key: %v", err)
	}

	c.log.Debugf("Shared key: %x", sharedKey)

	// 2. Extract extra nonce from last 4 bytes
	extraNonce := binary.LittleEndian.Uint32(encryptedData[len(encryptedData)-4:])

	// 3. Generate nonce (C# GenerateNonce)
	nonce := c.generateNonce(packetId, senderNodeId, extraNonce)
	c.log.Debugf("Generated nonce: %x", nonce)

	// 4. Initialize AES-CCM cipher (C# CcmBlockCipher configuration)
	block, err := aes.NewCipher(sharedKey)
	if err != nil {
		return nil, fmt.Errorf("failed to create AES cipher: %v", err)
	}

	// Create CCM cipher with 8-byte tag (64-bit) like C# AeadParameters
	ccm, err := aesccm.NewCCM(block, 8, len(nonce))
	if err != nil {
		return nil, fmt.Errorf("failed to create CCM cipher: %v", err)
	}

	// 5. Prepare data for CCM decryption
	// Firmware structure: [ciphertext][8-byte auth tag][4-byte extraNonce]
	// CCM expects: [ciphertext + auth tag]
	ciphertextLen := len(encryptedData) - 12  // exclude 8-byte auth + 4-byte extraNonce
	authTagStart := ciphertextLen
	authTagEnd := authTagStart + 8
	
	// Combine ciphertext + auth tag for CCM library
	dataToDecrypt := make([]byte, ciphertextLen + 8)
	copy(dataToDecrypt[:ciphertextLen], encryptedData[:ciphertextLen])           // ciphertext
	copy(dataToDecrypt[ciphertextLen:], encryptedData[authTagStart:authTagEnd]) // auth tag
	
	c.log.Debugf("Ciphertext (%d bytes): %x", ciphertextLen, encryptedData[:ciphertextLen])
	c.log.Debugf("Auth tag (8 bytes): %x", encryptedData[authTagStart:authTagEnd])
	c.log.Debugf("Data to decrypt (%d bytes): %x", len(dataToDecrypt), dataToDecrypt)

	// Self-test: verify CCM cipher works with known data
	testPlain := []byte("test")
	testSealed := ccm.Seal(nil, nonce, testPlain, nil)
	c.log.Debugf("CCM self-test - sealed: %x", testSealed)
	testOpened, testErr := ccm.Open(nil, nonce, testSealed, nil)
	if testErr != nil {
		c.log.Warnf("CCM self-test failed: %v", testErr)
	} else {
		c.log.Debugf("CCM self-test passed: %q", string(testOpened))
	}

	plaintext, err := ccm.Open(nil, nonce, dataToDecrypt, nil)
	if err != nil {
		c.log.Warnf("C# style CCM decryption failed: %v", err)
		c.log.Debugf("Trying firmware2-style nonce construction as fallback")
		
		// Try firmware2-style nonce construction (matches initNonce in CryptoEngine.cpp)
		firmwareNonce := c.generateFirmware2Nonce(packetId, senderNodeId, extraNonce)
		c.log.Debugf("Firmware2-style nonce: %x", firmwareNonce)
		
		plaintext, err = ccm.Open(nil, firmwareNonce, dataToDecrypt, nil)
		if err != nil {
			c.log.Errorf("Both C# and firmware2 style CCM decryption failed: %v", err)
			c.log.Errorf("Input details - C# nonce: %x, firmware2 nonce: %x, data: %x", nonce, firmwareNonce, dataToDecrypt)
			return nil, fmt.Errorf("CCM decryption failed with both nonce styles: %v", err)
		}
		c.log.Infof("Firmware2-style nonce construction succeeded!")
	}

	c.log.Debugf("Decrypted plaintext (%d bytes): %x", len(plaintext), plaintext)
	c.log.Debugf("Decrypted as string: %q", string(plaintext))

	return plaintext, nil
}

// generateSharedKey implements the exact C# GenerateSharedKey method
func (c *MqttClient) generateSharedKey(privateKeyBytes, publicKeyBytes []byte) ([]byte, error) {
	// Use X25519 for key exchange
	curve := ecdh.X25519()

	privateKey, err := curve.NewPrivateKey(privateKeyBytes)
	if err != nil {
		return nil, fmt.Errorf("invalid private key: %v", err)
	}

	publicKey, err := curve.NewPublicKey(publicKeyBytes)
	if err != nil {
		return nil, fmt.Errorf("invalid public key: %v", err)
	}

	// Generate shared secret
	sharedSecret, err := privateKey.ECDH(publicKey)
	if err != nil {
		return nil, fmt.Errorf("ECDH failed: %v", err)
	}

	c.log.Debugf("Raw shared secret: %x", sharedSecret)

	// Hash with SHA256 (firmware2 does this in-place: crypto->hash(shared_key, 32))
	hashedSharedKey := sha256.Sum256(sharedSecret)
	c.log.Debugf("Shared key after SHA256: %x", hashedSharedKey[:])
	return hashedSharedKey[:], nil
}

// generateNonce implements the exact C# GenerateNonce method
func (c *MqttClient) generateNonce(packetId, senderNodeId, extraNonce uint32) []byte {
	// C# implementation:
	// [..BitConverter.GetBytes(packetId), ..BitConverter.GetBytes(extraNonce), ..BitConverter.GetBytes(senderNodeId), 0]
	nonce := make([]byte, 13)

	// packetId (4 bytes, little-endian)
	binary.LittleEndian.PutUint32(nonce[0:4], packetId)

	// extraNonce (4 bytes, little-endian)
	binary.LittleEndian.PutUint32(nonce[4:8], extraNonce)

	// senderNodeId (4 bytes, little-endian)
	binary.LittleEndian.PutUint32(nonce[8:12], senderNodeId)

	// trailing zero (1 byte)
	nonce[12] = 0

	c.log.Debugf("Nonce components: packetId=%d, extraNonce=%d, senderNodeId=%d", packetId, extraNonce, senderNodeId)

	return nonce
}

// generateFirmware2Nonce implements the exact firmware2 initNonce method from CryptoEngine.cpp:259-268
func (c *MqttClient) generateFirmware2Nonce(packetId, senderNodeId, extraNonce uint32) []byte {
	// Firmware2 implementation from CryptoEngine.cpp initNonce():
	// memset(nonce, 0, sizeof(nonce));  // 16-byte nonce, cleared
	// memcpy(nonce, &packetId, sizeof(uint64_t));  // 8 bytes: packetId as uint64
	// memcpy(nonce + sizeof(uint64_t), &fromNode, sizeof(uint32_t));  // 4 bytes: fromNode at position 8
	// if (extraNonce)
	//     memcpy(nonce + sizeof(uint32_t), &extraNonce, sizeof(uint32_t));  // overwrites bytes 4-7!
	
	nonce := make([]byte, 16) // firmware uses 16-byte nonce
	
	// packetId as 8-byte uint64 (little-endian) - note: upper 4 bytes will be zero initially
	binary.LittleEndian.PutUint64(nonce[0:8], uint64(packetId))
	
	// senderNodeId (fromNode) at position 8 (4 bytes, little-endian)
	binary.LittleEndian.PutUint32(nonce[8:12], senderNodeId)
	
	// extraNonce overwrites bytes 4-7 (the upper 4 bytes of packetId) if present
	if extraNonce != 0 {
		binary.LittleEndian.PutUint32(nonce[4:8], extraNonce)
	}
	
	c.log.Debugf("Firmware2 nonce components: packetId=%d (as uint64), senderNodeId=%d, extraNonce=%d", packetId, senderNodeId, extraNonce)
	
	// Return first 13 bytes for CCM (L=2 requires 15-L=13 byte nonce)
	return nonce[:13]
}

