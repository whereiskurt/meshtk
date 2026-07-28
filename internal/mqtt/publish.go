package mqtt

import (
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"strings"
	"time"

	meshtastic "github.com/whereiskurt/meshtk/protos/meshtastic/generated"
	"golang.org/x/crypto/curve25519"
	"google.golang.org/protobuf/proto"
)

// BitfieldOkToMqtt is Data.bitfield bit 0 (BITFIELD_OK_TO_MQTT): the origin
// approves MQTT upload. Real firmware has stamped the bitfield on every packet
// since 2.5.0, and 2.8 uses its PRESENCE to tell modern zero-hop packets apart
// from pre-2.3 "pre-hop" firmware — a decoded packet with hop_start=0 and NO
// bitfield is dropped on ingest (Router::handleReceived pre-hop drop, observed
// live 2026-07-28: a 2.8.0.dafa583 radio discarded the entire fleet). Set it on
// every Data we build; it rides inside the encrypted payload so no transport
// layer can strip it.
const BitfieldOkToMqtt uint32 = 1

func (c *MqttClient) PublishNodeInfo(from uint32, to uint32, topic string, longName, shortName string, pubKey []byte, hwModel meshtastic.HardwareModel, role meshtastic.Config_DeviceConfig_Role) error {
	return c.publishNodeInfoRetain(from, to, topic, longName, shortName, pubKey, hwModel, role, true)
}

// PublishNodeInfoTo answers a directed user-info exchange: same payload as the
// broadcast beacon but retain=false -- MQTT retain is per topic, so a retained
// directed reply would DISPLACE the retained broadcast NodeInfo on the ghost's
// own topic and every future subscriber would learn a stale, misaddressed copy.
func (c *MqttClient) PublishNodeInfoTo(from uint32, to uint32, topic string, longName, shortName string, pubKey []byte, hwModel meshtastic.HardwareModel, role meshtastic.Config_DeviceConfig_Role) error {
	return c.publishNodeInfoRetain(from, to, topic, longName, shortName, pubKey, hwModel, role, false)
}

func (c *MqttClient) publishNodeInfoRetain(from uint32, to uint32, topic string, longName, shortName string, pubKey []byte, hwModel meshtastic.HardwareModel, role meshtastic.Config_DeviceConfig_Role, retain bool) error {
	fromStr := fmt.Sprintf("!%08x", from)
	user := &meshtastic.User{
		// The Id field should be properly cast to match what the meshtastic proto expects
		Id:        fromStr,
		LongName:  longName,
		ShortName: shortName,
		HwModel:   hwModel,
		Role:      role,
		PublicKey: pubKey,
	}

	// Serialize the user data
	userBytes, err := proto.Marshal(user)
	if err != nil {
		return fmt.Errorf("failed to serialize user data: %v", err)
	}

	// Send the NodeInfo message with retain=true so a radio that reconnects (the
	// iOS BLE↔MQTT proxy re-connects roughly every 60s) re-learns the whole fleet
	// the instant it SUBSCRIBEs, instead of waiting minutes for each node's next
	// beacon. MQTT keeps the retained value per topic and a later retain=false
	// publish does NOT overwrite it, so live Position/text on the same
	// msh/US/2/e/<ch>/!<node> topic keep flowing normally.
	//
	// SECURITY: only NodeInfo is ever retained. PKI/direct messages must never be
	// (a retained DM would be replayed to every future subscriber), and Position /
	// MapReport stay unretained too — stale positions are worse than none.
	return c.publishMessageEncrypted(from, to, topic, meshtastic.PortNum_NODEINFO_APP, userBytes, retain)
}
func (c *MqttClient) PublishMessagePlain(from uint32, to uint32, topic string, portNum meshtastic.PortNum, payload []byte) error {
	// Create Data protobuf
	data := &meshtastic.Data{
		Portnum:  portNum,
		Payload:  payload,
		Bitfield: proto.Uint32(BitfieldOkToMqtt),
	}

	// Create a random message ID
	msgID := make([]byte, 4)
	if _, err := rand.Read(msgID); err != nil {
		return fmt.Errorf("failed to generate message ID: %v", err)
	}
	messageID := binary.LittleEndian.Uint32(msgID)

	// Create MeshPacket with the plain data in the PayloadVariant
	packet := &meshtastic.MeshPacket{
		From: from,
		To:   to,
		Id:   messageID,
		PayloadVariant: &meshtastic.MeshPacket_Decoded{
			Decoded: data,
		},
		ViaMqtt:  true,
		RxTime:   uint32(time.Now().Unix()),
		HopLimit: 5,
		HopStart: 5,
	}

	// Create ServiceEnvelope
	envelope := &meshtastic.ServiceEnvelope{
		Packet:    packet,
		GatewayId: fmt.Sprintf("!%08x", from),
		ChannelId: c.channel,
	}

	// Serialize the envelope
	envelopeBytes, err := proto.Marshal(envelope)
	if err != nil {
		return fmt.Errorf("failed to serialize envelope: %v", err)
	}

	// Publish the message
	var lastErr error
	for range 3 {
		token := c.client.Publish(topic, 0, false, envelopeBytes)

		// Add timeout to avoid hanging indefinitely
		select {
		case <-token.Done():
			// Token completed normally
		case <-time.After(5 * time.Second):
			c.log.Warnf("Publish operation timed out after 5 seconds")
			lastErr = fmt.Errorf("publish operation timed out")
			c.Connect()
			continue
		}

		if err := token.Error(); err != nil {
			lastErr = err
			c.Connect()
			continue
		}
		return nil
	}

	// If all attempts fail, return the last error
	return fmt.Errorf("failed to publish message after 3 attempts: %v", lastErr)

}

// PublishMessageEncrypted publishes channel (non-PKI) traffic. It never retains —
// see publishMessageEncrypted / PublishNodeInfo for the one retained case.
func (c *MqttClient) PublishMessageEncrypted(from uint32, to uint32, topic string, portNum meshtastic.PortNum, payload []byte) error {
	return c.publishMessageEncrypted(from, to, topic, portNum, payload, false)
}

func (c *MqttClient) publishMessageEncrypted(from uint32, to uint32, topic string, portNum meshtastic.PortNum, payload []byte, retain bool) error {
	data := &meshtastic.Data{
		Portnum:  portNum,
		Payload:  payload,
		Bitfield: proto.Uint32(BitfieldOkToMqtt),
	}

	// Serialize the data
	dataBytes, err := proto.Marshal(data)
	if err != nil {
		c.log.Errorf("failed to serialize data: %v", err)
		return err
	}

	// Create a random message ID
	msgID := make([]byte, 4)
	if _, err := rand.Read(msgID); err != nil {
		c.log.Errorf("failed to generate message ID: %v", err)
		return err
	}
	messageID := binary.LittleEndian.Uint32(msgID)

	// Encrypt the data with AES-256
	nonce := make([]byte, 16)
	binary.LittleEndian.PutUint32(nonce[0:], messageID)
	binary.LittleEndian.PutUint32(nonce[8:], from)
	encrypted := make([]byte, len(dataBytes))
	cipher.NewCTR(c.blockCipher, nonce).XORKeyStream(encrypted, dataBytes)

	// Create MeshPacket with the encrypted data in the PayloadVariant
	packet := &meshtastic.MeshPacket{
		From: from,
		To:   to,
		Id:   messageID,
		PayloadVariant: &meshtastic.MeshPacket_Encrypted{
			Encrypted: encrypted,
		},
		Channel:  uint32(GenerateChannelHash(c.channel, c.key)),
		RxTime:   uint32(time.Now().Unix()),
		RxRssi:   -2,
		ViaMqtt:  true,
		RxSnr:    2,
		HopLimit: 3,
		// Mirror hop_limit like a fresh firmware send: 2.8 ingest drops any
		// packet where hop_start < hop_limit as provably corrupt.
		HopStart: 3,
	}

	// Create ServiceEnvelope
	envelope := &meshtastic.ServiceEnvelope{
		Packet:    packet,
		GatewayId: fmt.Sprintf("!%08x", from),
		ChannelId: c.channel,
	}

	// Serialize the envelope
	envelopeBytes, err := proto.Marshal(envelope)
	if err != nil {
		c.log.Errorf("failed to serialize envelope: %v", err)
		return err
	}

	// Attempt to publish the message up to 3 times
	var lastErr error
	for range 3 {
		token := c.client.Publish(topic, 0, retain, envelopeBytes)

		// Add timeout to avoid hanging indefinitely
		select {
		case <-token.Done():
		case <-time.After(5 * time.Second):
			c.log.Warnf("Publish operation timed out after 5 seconds...")
			lastErr = fmt.Errorf("publish operation timed out")
			c.Connect()
			continue
		}

		if err := token.Error(); err != nil {
			lastErr = err
			c.Connect()
			continue
		}
		return nil
	}

	// If all attempts fail, return the last error
	c.log.Errorf("failed to publish encrypted message after 3 attempts: %v", lastErr)
	return lastErr
}
func (c *MqttClient) PublishACK(from uint32, to uint32, topic string, requestId uint32, routingBytes []byte) error {
	// Create Data protobuf for ROUTING_APP
	data := &meshtastic.Data{
		Portnum:   meshtastic.PortNum_ROUTING_APP,
		Payload:   routingBytes,
		RequestId: requestId,
		Bitfield:  proto.Uint32(BitfieldOkToMqtt),
	}

	// Serialize the data
	dataBytes, err := proto.Marshal(data)
	if err != nil {
		c.log.Errorf("failed to serialize ACK data: %v", err)
		return err
	}

	// Create a random message ID
	msgID := make([]byte, 4)
	if _, err := rand.Read(msgID); err != nil {
		c.log.Errorf("failed to generate message ID: %v", err)
		return err
	}
	messageID := binary.LittleEndian.Uint32(msgID)

	// Encrypt the data with AES-256
	nonce := make([]byte, 16)
	binary.LittleEndian.PutUint32(nonce[0:], messageID)
	binary.LittleEndian.PutUint32(nonce[8:], from)
	encrypted := make([]byte, len(dataBytes))
	cipher.NewCTR(c.blockCipher, nonce).XORKeyStream(encrypted, dataBytes)

	packet := &meshtastic.MeshPacket{
		From: from,
		To:   to,
		Id:   messageID,
		PayloadVariant: &meshtastic.MeshPacket_Encrypted{
			Encrypted: encrypted,
		},
		Channel:  uint32(GenerateChannelHash(c.channel, c.key)),
		HopLimit: 3,
		Priority: meshtastic.MeshPacket_ACK,
	}

	if c.ackStyle == "legacy" {
		// Historical shape: fabricated receive-side metadata. Kept only for A/B
		// comparison against "faithful".
		packet.RxTime = uint32(time.Now().Unix())
		packet.RxRssi = -2
		packet.RxSnr = 2
		packet.ViaMqtt = true
	} else {
		// Faithful: what real firmware publishes for its own outgoing ack.
		// rx_rssi/rx_snr are receiver-side fields a transmitting node leaves
		// zero, via_mqtt is stamped by the RECEIVING gateway on ingest (never by
		// the sender), and hop_start mirrors hop_limit on a fresh send -- apps
		// derive "hops away" from hop_start - hop_limit, which the legacy shape
		// made negative.
		packet.HopStart = packet.HopLimit
	}

	// Create ServiceEnvelope
	envelope := &meshtastic.ServiceEnvelope{
		Packet:    packet,
		GatewayId: fmt.Sprintf("!%08x", from),
		ChannelId: c.channel,
	}

	// Serialize the envelope
	envelopeBytes, err := proto.Marshal(envelope)
	if err != nil {
		c.log.Errorf("failed to serialize ACK envelope: %v", err)
		return err
	}

	// Attempt to publish the ACK message up to 3 times
	var lastErr error
	for range 3 {
		token := c.client.Publish(topic, 0, false, envelopeBytes)

		// Add timeout to avoid hanging indefinitely
		select {
		case <-token.Done():
		case <-time.After(5 * time.Second):
			c.log.Warnf("ACK publish operation timed out after 5 seconds...")
			lastErr = fmt.Errorf("ACK publish operation timed out")
			c.Connect()
			continue
		}

		if err := token.Error(); err != nil {
			lastErr = err
			c.Connect()
			continue
		}
		return nil
	}

	// If all attempts fail, return the last error
	c.log.Errorf("failed to publish ACK message after 3 attempts: %v", lastErr)
	return lastErr
}

func (c *MqttClient) PublishPosition(from uint32, to uint32, topic string, latitudeI, longitudeI, altitude int32, precision uint32) error {
	// Create Position protobuf
	position := &meshtastic.Position{
		LatitudeI:     &latitudeI,
		LongitudeI:    &longitudeI,
		Altitude:      &altitude,
		PrecisionBits: precision,
		Time:          uint32(time.Now().Unix()),
	}

	// Serialize the position data
	positionBytes, err := proto.Marshal(position)
	if err != nil {
		return fmt.Errorf("failed to serialize position data: %v", err)
	}

	// Send the Position message
	return c.PublishMessageEncrypted(from, to, topic, meshtastic.PortNum_POSITION_APP, positionBytes)
}
func (c *MqttClient) PublishMapReport(from uint32, to uint32, topic string, longName, shortName string, hwModel meshtastic.HardwareModel, role meshtastic.Config_DeviceConfig_Role, firmwareVersion, region, modemPreset string, hasDefaultCh bool, onlineNodes uint32, latitudeI, longitudeI, altitude int32, precision uint32) error {
	// Create MapReport protobuf
	mapReport := &meshtastic.MapReport{
		LongName:            longName,
		ShortName:           shortName,
		HwModel:             hwModel,
		Role:                role,
		FirmwareVersion:     firmwareVersion,
		Region:              meshtastic.Config_LoRaConfig_RegionCode(meshtastic.Config_LoRaConfig_RegionCode_value[region]),
		ModemPreset:         meshtastic.Config_LoRaConfig_ModemPreset(meshtastic.Config_LoRaConfig_ModemPreset_value[modemPreset]),
		HasDefaultChannel:   hasDefaultCh,
		NumOnlineLocalNodes: onlineNodes,
		LatitudeI:           latitudeI,
		LongitudeI:          longitudeI,
		Altitude:            altitude,
		PositionPrecision:   precision,
	}

	// Serialize the map report data
	mapReportBytes, err := proto.Marshal(mapReport)
	if err != nil {
		return fmt.Errorf("failed to serialize map report data: %v", err)
	}

	// Send the MapReport message
	return c.PublishMessagePlain(from, to, topic, meshtastic.PortNum_MAP_REPORT_APP, mapReportBytes)
}

func xorHash(data []byte) int {
	hash := 0
	for _, b := range data {
		hash ^= int(b)
	}
	return hash
}

func GenerateChannelHash(name string, key string) int {
	replacedKey := strings.ReplaceAll(strings.ReplaceAll(key, "-", "+"), "_", "/")
	keyBytes, err := base64.StdEncoding.DecodeString(replacedKey)
	if err != nil {
		panic("failed to decode base64 key: " + err.Error())
	}
	hName := xorHash([]byte(name))
	hKey := xorHash(keyBytes)
	result := hName ^ hKey
	return result
}

// PublishPKIMessage publishes an encrypted message using public key infrastructure
// Takes the sender's private key and recipient's public key to establish secure communication
// PublishPKIMessage builds a fresh PKI envelope (new random packet id) and
// publishes it. For re-sends that must be invisible when the first copy landed,
// build once with BuildPKIMessage and republish the SAME bytes with
// PublishEnvelopeBytes -- the device dedups by packet id, so byte-identical
// re-sends never display twice.
func (c *MqttClient) PublishPKIMessage(from uint32, to uint32, topic string, portNum meshtastic.PortNum, payload []byte, senderPrivateKey []byte, recipientPublicKey []byte) error {
	envelopeBytes, err := c.BuildPKIMessage(from, to, portNum, payload, senderPrivateKey, recipientPublicKey)
	if err != nil {
		return err
	}
	return c.PublishEnvelopeBytes(topic, envelopeBytes)
}

// BuildPKIMessage constructs and marshals a PKI-encrypted ServiceEnvelope with
// a fresh random packet id, without publishing it.
func (c *MqttClient) BuildPKIMessage(from uint32, to uint32, portNum meshtastic.PortNum, payload []byte, senderPrivateKey []byte, recipientPublicKey []byte) ([]byte, error) {
	// Create Data protobuf
	data := &meshtastic.Data{
		Portnum:  portNum,
		Payload:  payload,
		Bitfield: proto.Uint32(BitfieldOkToMqtt),
	}

	// Serialize the data
	dataBytes, err := proto.Marshal(data)
	if err != nil {
		c.log.Errorf("failed to serialize data: %v", err)
		return nil, err
	}

	// Create a random message ID
	msgID := make([]byte, 4)
	if _, err := rand.Read(msgID); err != nil {
		c.log.Errorf("failed to generate message ID: %v", err)
		return nil, err
	}
	messageID := binary.LittleEndian.Uint32(msgID)

	// Encrypt the data using PKI
	encrypted, err := c.encryptCurve25519(senderPrivateKey, recipientPublicKey, dataBytes, messageID, from)
	if err != nil {
		c.log.Errorf("failed to encrypt with PKI: %v", err)
		return nil, err
	}

	// Get sender's public key from the private key
	senderPublicKey := make([]byte, 32)
	curve25519.ScalarBaseMult((*[32]byte)(senderPublicKey), (*[32]byte)(senderPrivateKey))

	// Create MeshPacket with PKI encrypted data. Faithful firmware shape, same
	// rationale as the ack fix: rx_time/rx_rssi/rx_snr are RECEIVE-side fields a
	// transmitting node leaves zero, and via_mqtt is stamped by the receiving
	// gateway on ingest. Fabricating rx_time was worse than cosmetic -- the
	// receiver trusted our build-time stamp over actual arrival, so a pending
	// flush delivered a minute late slotted a minute back in the conversation
	// history (observed live 2026-07-20: replies sorting above newer messages).
	// With rx_time unset the receiver stamps ingest time and order follows
	// arrival. hop_start mirrors hop_limit so hops-away computes as 0.
	packet := &meshtastic.MeshPacket{
		From: from,
		To:   to,
		Id:   messageID,
		PayloadVariant: &meshtastic.MeshPacket_Encrypted{
			Encrypted: encrypted,
		},
		PublicKey:    senderPublicKey,
		PkiEncrypted: true,
		HopLimit:     3,
		HopStart:     3,
	}

	// Create ServiceEnvelope
	envelope := &meshtastic.ServiceEnvelope{
		Packet:    packet,
		GatewayId: fmt.Sprintf("!%08x", from),
		ChannelId: c.channel,
	}

	// Serialize the envelope
	envelopeBytes, err := proto.Marshal(envelope)
	if err != nil {
		c.log.Errorf("failed to serialize envelope: %v", err)
		return nil, err
	}

	return envelopeBytes, nil
}

// PublishEnvelopeBytes publishes an already-marshaled ServiceEnvelope with the
// standard 3-attempt/5s-timeout retry. Publishing the SAME bytes again is safe:
// the device dedups by packet id, so a repeat is invisible when the first copy
// landed -- which is exactly what makes byte-identical re-sends free.
func (c *MqttClient) PublishEnvelopeBytes(topic string, envelopeBytes []byte) error {
	var lastErr error
	for range 3 {
		token := c.client.Publish(topic, 0, false, envelopeBytes)

		// Add timeout to avoid hanging indefinitely
		select {
		case <-token.Done():
		case <-time.After(5 * time.Second):
			c.log.Warnf("Publish operation timed out after 5 seconds...")
			lastErr = fmt.Errorf("publish operation timed out")
			c.Connect()
			continue
		}

		if err := token.Error(); err != nil {
			lastErr = err
			c.Connect()
			continue
		}
		return nil
	}

	// If all attempts fail, return the last error
	c.log.Errorf("failed to publish envelope after 3 attempts: %v", lastErr)
	return lastErr
}
