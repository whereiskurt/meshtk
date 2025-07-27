package mqtt

import (
	"crypto/aes"
	"crypto/ecdh"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"strings"

	aesccm "github.com/pschlump/AesCCM"
	meshtastic "github.com/whereiskurt/meshtk/protos/meshtastic/generated"
)

func (c *MqttClient) decryptPKI(packet *meshtastic.MeshPacket, encryptedData []byte) ([]byte, error) {

	recipientNode, exists := (*c.nodes)[packet.GetTo()]
	if !exists {
		return nil, fmt.Errorf("recipient node %d not found in nodeDB", packet.GetTo())
	}

	recipientPrivateKeyBytes, err := c.parsePrivateKey(recipientNode.PrivKey)
	if err != nil {
		return nil, fmt.Errorf("failed to parse recipient private key: %v", err)
	}

	senderPublicKeyBytes := packet.GetPublicKey()
	if len(senderPublicKeyBytes) != 32 {
		return nil, fmt.Errorf("invalid sender public key length: %d", len(senderPublicKeyBytes))
	}

	c.log.Debugf("PKI decrypt: packet ID=%d, from=%d, to=%d", packet.GetId(), packet.GetFrom(), packet.GetTo())
	c.log.Debugf("PKI encrypted data (%d bytes): %x", len(encryptedData), encryptedData)
	c.log.Debugf("Recipient private key: %x", recipientPrivateKeyBytes)
	c.log.Debugf("Sender public key: %x", senderPublicKeyBytes)

	// Call the exact firmware2 decrypt method equivalent
	return c.decryptCurve25519(recipientPrivateKeyBytes, senderPublicKeyBytes, encryptedData, uint32(packet.GetId()), uint32(packet.GetFrom()))
}

func (c *MqttClient) parsePrivateKey(privKeyHex string) ([]byte, error) {
	privKeyBytes, err := hex.DecodeString(strings.TrimPrefix(privKeyHex, "0x"))
	if err != nil {
		return nil, fmt.Errorf("invalid hex: %v", err)
	}
	if len(privKeyBytes) != 32 {
		return nil, fmt.Errorf("invalid key length: %d, expected 32", len(privKeyBytes))
	}
	return privKeyBytes, nil
}

// decryptCurve25519 implements the exact firmware2 CryptoEngine::decryptCurve25519 method
func (c *MqttClient) decryptCurve25519(recipientPrivateKey, senderPublicKey, encryptedData []byte, packetId, senderNodeId uint32) ([]byte, error) {
	// Firmware2 structure: const uint8_t *auth = bytes + numBytes - 12;
	// Layout: [ciphertext][8-byte auth tag][4-byte extraNonce]
	if len(encryptedData) < 12 {
		return nil, fmt.Errorf("encrypted data too short: %d bytes", len(encryptedData))
	}

	// Extract extraNonce from last 4 bytes (auth + 8)
	extraNonce := binary.LittleEndian.Uint32(encryptedData[len(encryptedData)-4:])
	c.log.Debugf("Extra nonce: %d", extraNonce)

	// 1. Generate shared key using firmware2 method
	sharedKey, err := c.generateSharedKey(recipientPrivateKey, senderPublicKey)
	if err != nil {
		return nil, fmt.Errorf("failed to generate shared key: %v", err)
	}

	c.log.Debugf("Shared key: %x", sharedKey)

	// 2. Generate nonce using firmware2 initNonce method
	nonce := c.initNonce(senderNodeId, uint64(packetId), extraNonce)

	// 3. Prepare data for AES-CCM decryption
	ciphertextLen := len(encryptedData) - 12 // exclude 8-byte auth + 4-byte extraNonce
	authTagStart := ciphertextLen
	authTagEnd := authTagStart + 8

	ciphertext := encryptedData[:ciphertextLen]
	authTag := encryptedData[authTagStart:authTagEnd]

	c.log.Debugf("Generated nonce: %x", nonce)
	c.log.Debugf("Ciphertext (%d bytes): %x", len(ciphertext), ciphertext)
	c.log.Debugf("Auth tag (%d bytes): %x", len(authTag), authTag)

	// 4. Perform AES-CCM decryption using firmware2 parameters
	plaintext, err := c.aesCCMDecrypt(sharedKey, nonce, ciphertext, authTag)
	if err != nil {
		c.log.Errorf("AES-CCM decryption failed: %v", err)
		return nil, fmt.Errorf("AES-CCM decryption failed: %v", err)
	}

	c.log.Debugf("Decrypted plaintext (%d bytes): %x", len(plaintext), plaintext)

	return plaintext, nil
}

// generateSharedKey implements firmware2 X25519 ECDH + SHA256 key generation
func (c *MqttClient) generateSharedKey(privateKeyBytes, publicKeyBytes []byte) ([]byte, error) {
	// Use X25519 for key exchange (firmware2: Curve25519::dh2)
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

	// Hash with SHA256 (firmware2: crypto->hash(shared_key, 32))
	hashedSharedKey := sha256.Sum256(sharedSecret)
	return hashedSharedKey[:], nil
}

// initNonce implements the exact firmware2 CryptoEngine::initNonce method
func (c *MqttClient) initNonce(fromNode uint32, packetId uint64, extraNonce uint32) []byte {
	// Firmware2 implementation from CryptoEngine.cpp:259-268:
	// memset(nonce, 0, sizeof(nonce));  // 16-byte nonce, cleared
	// memcpy(nonce, &packetId, sizeof(uint64_t));  // 8 bytes: packetId as uint64
	// memcpy(nonce + sizeof(uint64_t), &fromNode, sizeof(uint32_t));  // 4 bytes: fromNode at position 8
	// if (extraNonce)
	//     memcpy(nonce + sizeof(uint32_t), &extraNonce, sizeof(uint32_t));  // overwrites bytes 4-7!

	nonce := make([]byte, 16) // firmware uses 16-byte nonce

	// packetId as 8-byte uint64 (little-endian)
	binary.LittleEndian.PutUint64(nonce[0:8], packetId)

	// fromNode at position 8 (4 bytes, little-endian)
	binary.LittleEndian.PutUint32(nonce[8:12], fromNode)

	// extraNonce overwrites bytes 4-7 (the upper 4 bytes of packetId) if present
	if extraNonce != 0 {
		binary.LittleEndian.PutUint32(nonce[4:8], extraNonce)
	}

	c.log.Debugf("Nonce components: fromNode=%d, packetId=%d, extraNonce=%d", fromNode, packetId, extraNonce)

	// Return first 13 bytes for CCM (L=2 requires 15-L=13 byte nonce)
	return nonce[:13]
}

// aesCCMDecrypt implements AES-CCM decryption using firmware2 parameters with pschlump AesCCM
func (c *MqttClient) aesCCMDecrypt(key, nonce, ciphertext, authTag []byte) ([]byte, error) {
	// Firmware2 parameters: L=2, M=8, no AAD
	// aes_ccm_ad(shared_key, 32, nonce, 8, bytes, numBytes - 12, nullptr, 0, auth, bytesOut)

	c.log.Debugf("AesCCM decrypt - key: %x", key)
	c.log.Debugf("AesCCM decrypt - nonce: %x", nonce)
	c.log.Debugf("AesCCM decrypt - ciphertext: %x", ciphertext)
	c.log.Debugf("AesCCM decrypt - authTag: %x", authTag)

	// Create AES cipher block
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("failed to create AES cipher: %v", err)
	}

	// Create pschlump AesCCM cipher with 8-byte tag size and 13-byte nonce (firmware2 parameters)
	ccmCipher, err := aesccm.NewCCM(block, 8, len(nonce))
	if err != nil {
		return nil, fmt.Errorf("failed to create CCM cipher: %v", err)
	}

	// Combine ciphertext + auth tag for AesCCM library
	encryptedData := make([]byte, len(ciphertext)+len(authTag))
	copy(encryptedData[:len(ciphertext)], ciphertext)
	copy(encryptedData[len(ciphertext):], authTag)

	c.log.Debugf("AesCCM combined data: %x", encryptedData)

	// Decrypt with no AAD (Additional Authenticated Data)
	plaintext, err := ccmCipher.Open(nil, nonce, encryptedData, nil)
	if err != nil {
		return nil, fmt.Errorf("AesCCM decryption failed: %v", err)
	}

	return plaintext, nil
}
