package mqtt

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"strings"

	meshtastic "github.com/whereiskurt/meshtk/protos/meshtastic/generated"
	"golang.org/x/crypto/curve25519"
)

func (c *MqttClient) decryptPKI(packet *meshtastic.MeshPacket, encryptedData []byte) ([]byte, error) {

	recipientNode, exists := (*c.nodes)[packet.GetTo()]
	if !exists {
		return nil, fmt.Errorf("recipient node %d not found in nodeDB", packet.GetTo())
	}

	recipientPrivateKeyBytes, err := c.parseHexKey(recipientNode.PrivKey)
	if err != nil {
		return nil, fmt.Errorf("failed to parse recipient private key: %v", err)
	}

	senderNode, exists := (*c.nodes)[packet.GetFrom()]
	if !exists {
		return nil, fmt.Errorf("sender node %d not found in nodeDB", packet.GetFrom())
	}

	senderPublicKeyBytes, err := c.parseHexKey(senderNode.PubKey)
	if err != nil {
		return nil, fmt.Errorf("failed to parse sender public key: %v", err)
	}
	// senderPublicKeyBytes := packet.GetPublicKey()

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

func (c *MqttClient) parseHexKey(hexKey string) ([]byte, error) {
	keyBytes, err := hex.DecodeString(strings.TrimPrefix(hexKey, "0x"))
	if err != nil {
		return nil, fmt.Errorf("invalid hex: %v", err)
	}
	if len(keyBytes) != 32 {
		return nil, fmt.Errorf("invalid key length: %d, expected 32", len(keyBytes))
	}
	return keyBytes, nil
}

// decryptCurve25519 exactly implements CryptoEngine::decryptCurve25519 from the firmware
func (c *MqttClient) decryptCurve25519(recipientPrivateKey, senderPublicKey, encryptedBytes []byte, packetId, fromNode uint32) ([]byte, error) {
	// Firmware: CryptoEngine.cpp:122-146

	numBytes := len(encryptedBytes)
	if numBytes < 12 { // MESHTASTIC_PKC_OVERHEAD = 12
		return nil, fmt.Errorf("encrypted data too short: %d bytes", numBytes)
	}

	// Extract auth and extraNonce from the last 12 bytes
	// Firmware: const uint8_t *auth = bytes + numBytes - 12;
	authStart := numBytes - 12
	auth := encryptedBytes[authStart : authStart+8] // First 8 bytes of last 12

	// Firmware: memcpy(&extraNonce, auth + 8, sizeof(uint32_t));
	extraNonce := binary.LittleEndian.Uint32(encryptedBytes[authStart+8:])

	c.log.Debugf("ExtraNonce: %d (0x%08x)", extraNonce, extraNonce)

	// Firmware: if (!crypto->setDHPublicKey(remotePublic.bytes)) return false;
	// Calculate shared secret using X25519
	sharedSecret, err := curve25519.X25519(recipientPrivateKey, senderPublicKey)
	if err != nil {
		return nil, fmt.Errorf("X25519 failed: %v", err)
	}

	// Firmware: crypto->hash(shared_key, 32);
	// Hash the shared secret with SHA256
	sharedKey := sha256.Sum256(sharedSecret)

	c.log.Debugf("Raw shared secret: %x", sharedSecret)
	c.log.Debugf("Shared key: %x", sharedKey[:])

	// Firmware: initNonce(fromNode, packetNum, extraNonce);
	nonce := c.initNonce(fromNode, uint64(packetId), extraNonce)
	c.log.Debugf("Generated nonce: %x", nonce)

	// Extract ciphertext (everything except the last 12 bytes)
	ciphertext := encryptedBytes[:numBytes-12]
	c.log.Debugf("Ciphertext (%d bytes): %x", len(ciphertext), ciphertext)
	c.log.Debugf("Auth tag (8 bytes): %x", auth)

	// Firmware: return aes_ccm_ad(shared_key, 32, nonce, 8, bytes, numBytes - 12, nullptr, 0, auth, bytesOut);
	return c.aesCCMAD(sharedKey[:], nonce, ciphertext, auth)
}

// initNonce implements the exact C++ firmware CryptoEngine::initNonce method
func (c *MqttClient) initNonce(fromNode uint32, packetId uint64, extraNonce uint32) []byte {
	// Firmware: CryptoEngine.cpp:259-268
	nonce := make([]byte, 16)

	// packetId (8 bytes, little-endian) at offset 0
	binary.LittleEndian.PutUint64(nonce[0:8], packetId)

	// fromNode (4 bytes, little-endian) at offset 8
	binary.LittleEndian.PutUint32(nonce[8:12], fromNode)

	// extraNonce overwrites bytes 4-7 (firmware: memcpy(&nonce[4], &extraNonce, sizeof(uint32_t)))
	binary.LittleEndian.PutUint32(nonce[4:8], extraNonce)

	// Return first 13 bytes (firmware uses 13-byte nonce)
	return nonce[:13]
}

// aesCCMAD implements the exact firmware aes_ccm_ad function
func (c *MqttClient) aesCCMAD(key, nonce, ciphertext, authTag []byte) ([]byte, error) {
	// Firmware: aes-ccm.cpp:162-185

	// Create AES cipher
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("AES cipher creation failed: %v", err)
	}

	// Firmware constants: L=2, M=8
	const L = 2
	const M = 8

	c.log.Debugf("=== Firmware aes_ccm_ad ===")
	c.log.Debugf("Key: %x", key)
	c.log.Debugf("Nonce: %x", nonce)
	c.log.Debugf("Ciphertext: %x", ciphertext)
	c.log.Debugf("Auth tag: %x", authTag)

	// Step 1: Decrypt auth tag with S_0 (firmware: aes_ccm_decr_auth)
	decryptedAuthTag, err := c.decryptAuthTag(block, nonce, authTag)
	if err != nil {
		return nil, err
	}

	// Step 2: Decrypt ciphertext (firmware: aes_ccm_encr)
	plaintext, err := c.decryptCiphertext(block, nonce, ciphertext)
	if err != nil {
		return nil, err
	}

	// Step 3: Verify authentication (firmware: aes_ccm_auth_start + aes_ccm_auth)
	// CRITICAL: Firmware passes crypt_len to aes_ccm_auth, not plaintext length!
	if err := c.verifyAuthentication(block, nonce, plaintext, decryptedAuthTag, len(ciphertext)); err != nil {
		return nil, fmt.Errorf("authentication failed: %v", err)
	}

	c.log.Debugf("=== PKI Decryption SUCCESS ===")
	c.log.Debugf("Plaintext: %x", plaintext)

	return plaintext, nil
}

// decryptAuthTag implements firmware aes_ccm_decr_auth
func (c *MqttClient) decryptAuthTag(block cipher.Block, nonce, encryptedAuthTag []byte) ([]byte, error) {
	// Build A_0 for S_0 generation
	a0 := make([]byte, 16)
	a0[0] = 1                                // L-1 = 2-1 = 1
	copy(a0[1:], nonce)                      // 13-byte nonce
	binary.BigEndian.PutUint16(a0[14:16], 0) // counter = 0

	// S_0 = E(K, A_0)
	s0 := make([]byte, 16)
	block.Encrypt(s0, a0)

	// Decrypt: T = auth XOR S_0
	decryptedAuthTag := make([]byte, 8)
	for i := 0; i < 8; i++ {
		decryptedAuthTag[i] = encryptedAuthTag[i] ^ s0[i]
	}

	c.log.Debugf("S_0 keystream: %x", s0)
	c.log.Debugf("Decrypted auth tag: %x", decryptedAuthTag)

	return decryptedAuthTag, nil
}

// decryptCiphertext implements firmware aes_ccm_encr (for decryption)
func (c *MqttClient) decryptCiphertext(block cipher.Block, nonce, ciphertext []byte) ([]byte, error) {
	plaintext := make([]byte, len(ciphertext))

	// Process blocks with CTR mode
	a := make([]byte, 16)
	a[0] = 1           // L-1 = 2-1 = 1
	copy(a[1:], nonce) // 13-byte nonce

	counter := 1
	for i := 0; i < len(ciphertext); i += 16 {
		// Set counter
		binary.BigEndian.PutUint16(a[14:16], uint16(counter))

		// Generate keystream S_i
		s := make([]byte, 16)
		block.Encrypt(s, a)

		// XOR with ciphertext to get plaintext
		end := i + 16
		if end > len(ciphertext) {
			end = len(ciphertext)
		}

		for j := i; j < end; j++ {
			plaintext[j] = ciphertext[j] ^ s[j-i]
		}

		counter++
	}

	c.log.Debugf("Decrypted plaintext: %x", plaintext)
	return plaintext, nil
}

// verifyAuthentication implements firmware aes_ccm_auth_start + aes_ccm_auth
func (c *MqttClient) verifyAuthentication(block cipher.Block, nonce, plaintext, expectedAuthTag []byte, ciphertextLen int) error {
	// Build B_0 block (firmware: aes_ccm_auth_start)
	b0 := make([]byte, 16)
	// Flags = (aad_len ? 0x40 : 0) | (((M-2)/2) << 3) | (L-1)
	// aad_len=0, M=8, L=2 → Flags = 0 | (3 << 3) | 1 = 25 = 0x19
	b0[0] = 0x19
	copy(b0[1:], nonce) // 13-byte nonce
	// CRITICAL: Use ciphertext length for decryption (firmware line 173)
	binary.BigEndian.PutUint16(b0[14:16], uint16(ciphertextLen))

	c.log.Debugf("B_0 block: %x", b0)

	// X_1 = E(K, B_0)
	x := make([]byte, 16)
	block.Encrypt(x, b0)

	c.log.Debugf("X after B_0: %x", x)

	// Process plaintext blocks (firmware: aes_ccm_auth)
	// CRITICAL: Firmware line 174: aes_ccm_auth(plain, crypt_len, x)
	// Only process ciphertextLen bytes of plaintext, not full plaintext length!
	for i := 0; i < ciphertextLen; i += 16 {
		// XOR plaintext block with X
		end := i + 16
		if end > ciphertextLen {
			end = ciphertextLen
		}

		// Create temporary block for XOR and padding
		tempBlock := make([]byte, 16)
		for j := i; j < end; j++ {
			tempBlock[j-i] = plaintext[j]
		}
		// Remaining bytes in tempBlock are already zero (proper padding)

		// XOR with X
		for j := 0; j < 16; j++ {
			x[j] ^= tempBlock[j]
		}

		// Encrypt X
		block.Encrypt(x, x)
	}

	// Extract computed auth tag (first 8 bytes)
	computedAuthTag := x[:8]

	c.log.Debugf("Final MAC block: %x", x)
	c.log.Debugf("Computed auth tag: %x", computedAuthTag)
	c.log.Debugf("Expected auth tag: %x", expectedAuthTag)

	// Constant-time comparison (firmware: constant_time_compare)
	if !constantTimeCompare(computedAuthTag, expectedAuthTag) {
		return fmt.Errorf("MAC verification failed")
	}

	return nil
}

// constantTimeCompare implements firmware constant_time_compare function
func constantTimeCompare(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}

	var result byte
	for i := 0; i < len(a); i++ {
		result |= a[i] ^ b[i]
	}

	return result == 0
}
