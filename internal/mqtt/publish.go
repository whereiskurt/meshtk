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
	"google.golang.org/protobuf/proto"
)

func (c *MqttClient) PublishNodeInfo(from uint32, to uint32, topic string, longName, shortName string, hwModel meshtastic.HardwareModel, role meshtastic.Config_DeviceConfig_Role) error {
	fromStr := fmt.Sprintf("!%08x", from)

	user := &meshtastic.User{
		// The Id field should be properly cast to match what the meshtastic proto expects
		Id:        fromStr,
		LongName:  longName,
		ShortName: shortName,
		HwModel:   hwModel,
		Role:      role,
		PublicKey: c.pkiPublicKey,
	}

	// Serialize the user data
	userBytes, err := proto.Marshal(user)
	if err != nil {
		return fmt.Errorf("failed to serialize user data: %v", err)
	}

	// Send the NodeInfo message
	return c.PublishMessageEncrypted(from, to, topic, meshtastic.PortNum_NODEINFO_APP, userBytes)
}
func (c *MqttClient) PublishMessagePlain(from uint32, to uint32, topic string, portNum meshtastic.PortNum, payload []byte) error {
	// Create Data protobuf
	data := &meshtastic.Data{
		Portnum: portNum,
		Payload: payload,
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
		ViaMqtt: true,
		RxTime:  uint32(time.Now().Unix()),
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
	token := c.client.Publish(topic, 0, false, envelopeBytes)
	<-token.Done()
	if err := token.Error(); err != nil {
		return fmt.Errorf("failed to publish message: %v", err)
	}

	c.log.Tracef("published plain message to %s: %s", topic, data)
	return nil
}

func (c *MqttClient) PublishMessageEncrypted(from uint32, to uint32, topic string, portNum meshtastic.PortNum, payload []byte) error {
	data := &meshtastic.Data{
		Portnum: portNum,
		Payload: payload,
	}

	// Serialize the data
	dataBytes, err := proto.Marshal(data)
	if err != nil {
		return fmt.Errorf("failed to serialize data: %v", err)
	}

	// Create a random message ID
	msgID := make([]byte, 4)
	if _, err := rand.Read(msgID); err != nil {
		return fmt.Errorf("failed to generate message ID: %v", err)
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
		Channel: uint32(GenerateChannelHash(c.channel, c.key)),
		RxTime:  uint32(time.Now().Unix()),
		RxRssi:  -20,
		ViaMqtt: true,
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
	token := c.client.Publish(topic, 0, false, envelopeBytes)
	<-token.Done()
	if err := token.Error(); err != nil {
		return fmt.Errorf("failed to publish message: %v", err)
	}

	return nil
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
