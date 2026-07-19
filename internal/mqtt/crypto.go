package mqtt

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/whereiskurt/meshtk/internal/keycache"
	meshtastic "github.com/whereiskurt/meshtk/protos/meshtastic/generated"
	"golang.org/x/crypto/curve25519"
)

// pubKeyCache remembers the last public key successfully learned for each node
// (keyed by nodeID). meshobserv prunes a node's whole entry — pubkey included —
// once its SeenBy entries age out, so the sender's key flaps in and out of
// nodes.json. Caching lets PKI decrypt + reply-encrypt keep working across those
// gaps once the key has been seen. Shared across all fleet clients in-process.
var pubKeyCache sync.Map // map[uint32]string

func (c *MqttClient) decryptPKI(packet *meshtastic.MeshPacket, encryptedData []byte) ([]byte, error) {

	recipientNode, exists := (*c.nodes)[packet.GetTo()]
	if !exists {
		return nil, fmt.Errorf("recipient node %d not found in nodeDB", packet.GetTo())
	}

	// Check if recipient private key is available
	if recipientNode.PrivKey == "" {
		return nil, fmt.Errorf("recipient node %d has no private key in nodeDB (node not fully initialized)", packet.GetTo())
	}

	// Debug: Check key length and format
	c.log.Debugf("Raw recipient private key string: '%s' (length: %d)", recipientNode.PrivKey, len(recipientNode.PrivKey))

	recipientPrivateKeyBytes, err := c.ParseHexKey(recipientNode.PrivKey)
	if err != nil {
		return nil, fmt.Errorf("failed to parse recipient private key: %v", err)
	}

	// Validate key length
	if len(recipientPrivateKeyBytes) != 32 {
		return nil, fmt.Errorf("recipient private key has invalid length: %d bytes (expected 32)", len(recipientPrivateKeyBytes))
	}

	// Resolve the sender public key from the authoritative keycache (DDB
	// MeshRadio), NOT the unauthenticated nodes.json feed. On a keycache miss
	// the configured Fallback decides: nodes.json (bring-up) or NACK (none,
	// poisoning-resistant). A returned error flows into mqtt.go's nackHandler.
	senderPubKeyHex, err := c.resolveSenderPubKey(packet.GetFrom())
	if err != nil {
		return nil, fmt.Errorf("failed to resolve sender public key: %v", err)
	}

	senderPublicKeyBytes, err := c.ParseHexKey(senderPubKeyHex)
	if err != nil {
		return nil, fmt.Errorf("failed to parse sender public key: %v", err)
	}

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

func (c *MqttClient) ParseHexKey(hexKey string) ([]byte, error) {
	// Handle both "0x..." and plain hex formats
	cleanHex := strings.TrimPrefix(hexKey, "0x")

	// Debug: Log what we're parsing
	c.log.Debugf("parseHexKey: input='%s' (len=%d), cleaned='%s' (len=%d)", hexKey, len(hexKey), cleanHex, len(cleanHex))

	keyBytes, err := hex.DecodeString(cleanHex)
	if err != nil {
		return nil, fmt.Errorf("invalid hex: %v", err)
	}
	if len(keyBytes) != 32 {
		return nil, fmt.Errorf("invalid key length: %d, expected 32 (hex string length was %d)", len(keyBytes), len(cleanHex))
	}
	return keyBytes, nil
}

// resolveSenderPubKey returns a node's 0x-hex decrypt pubkey from the
// authoritative keycache (DDB MeshRadio), applying the configured Fallback on a
// miss. This is the single decision point shared by BOTH the decrypt site
// (crypto.go decryptPKI) and the reply-encrypt site (fleet/cmd.go sendPKIReply)
// — migrating only one would leave stale-key replies (landmine L4).
//
//   - keycache hit               → (0x hex, nil)
//   - miss/degraded, "nodes.json" → FetchPublicKeyFromDefcon (bring-up feed)
//   - miss/degraded, "none"       → error → existing nackHandler (poisoning closed)
//
// Every fallback/miss is logged (never key material) for enrollment-coverage
// measurement before the deploy-time flip to "none". A nil resolver (e.g. the
// nodeinfo utility) preserves the legacy feed path.
//
// ResolveSenderPubKey is the exported wrapper used by the reply-encrypt site in
// package fleet; the decrypt site in this package uses the unexported form.
func (c *MqttClient) ResolveSenderPubKey(nodeNum uint32) (string, error) {
	return c.resolveSenderPubKey(nodeNum)
}

func (c *MqttClient) resolveSenderPubKey(nodeNum uint32) (string, error) {
	if c.keyResolver == nil {
		// No authoritative resolver wired: legacy feed behavior.
		return c.fallbackFeedFetch(nodeNum)
	}

	hexKey, ok, err := c.keyResolver.Resolve(context.Background(), nodeNum)
	if err == nil && ok {
		return hexKey, nil
	}

	// Distinguish a plain miss (ok=false, err=nil) from a store/circuit-breaker
	// error; both apply the same Fallback branch (degraded ≈ miss, V-degraded).
	if err != nil {
		c.log.Warnf("keycache degraded for node !%08x (%v); applying fallback=%q", nodeNum, err, c.keyFallback)
	}

	switch c.keyFallback {
	case "none":
		// Poisoning-resistant: a NODEINFO-injected feed key can NOT change decrypt
		// behavior. The error flows into the existing nackHandler.
		c.log.Infof("keycache miss for node !%08x, fallback=none, NACKing (enrollment-coverage)", nodeNum)
		return "", fmt.Errorf("keycache miss for node !%08x with fallback=none: %w", nodeNum, keycache.ErrNotFound)
	default: // "nodes.json" (bring-up)
		c.log.Infof("keycache miss for node !%08x, nodes.json fallback used (enrollment-coverage)", nodeNum)
		return c.fallbackFeedFetch(nodeNum)
	}
}

// fallbackFeedFetch performs the nodes.json feed lookup, routed through an
// optional test seam (nodesFeedFn) so the fallback branch can be exercised
// without network I/O.
func (c *MqttClient) fallbackFeedFetch(nodeNum uint32) (string, error) {
	if c.nodesFeedFn != nil {
		return c.nodesFeedFn(nodeNum)
	}
	return c.FetchPublicKeyFromDefcon(nodeNum)
}

// FetchPublicKeyFromDefcon returns the public key for a nodeID, preferring the
// live nodes.json feed but falling back to the last key cached for that node
// when the feed has pruned it (see pubKeyCache). A successful feed lookup
// refreshes the cache.
func (c *MqttClient) FetchPublicKeyFromDefcon(nodeID uint32) (string, error) {
	pubKeyStr, err := c.fetchPublicKeyFromFeed(nodeID)
	if err == nil && pubKeyStr != "" {
		pubKeyCache.Store(nodeID, pubKeyStr)
		return pubKeyStr, nil
	}
	if cached, ok := pubKeyCache.Load(nodeID); ok {
		c.log.Debugf("pubkey feed miss for node %d (%v); using cached key", nodeID, err)
		return cached.(string), nil
	}
	return "", err
}

// fetchPublicKeyFromFeed fetches the public key for a given nodeID from the DEFCON nodes API
func (c *MqttClient) fetchPublicKeyFromFeed(nodeID uint32) (string, error) {
	// The JSON node DB is served at /nodes.json. /map/nodes.json returns the
	// map SPA's index.html (nginx SPA fallback), which fails to decode as JSON
	// and silently breaks every PKI chatbot reply.
	url := "https://mqtt.defcon.run/nodes.json"

	client := &http.Client{
		Timeout: 10 * time.Second,
	}

	resp, err := client.Get(url)
	if err != nil {
		return "", fmt.Errorf("failed to fetch nodes from DEFCON API: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("DEFCON API returned status %d", resp.StatusCode)
	}

	var nodes map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&nodes); err != nil {
		return "", fmt.Errorf("failed to decode JSON response: %v", err)
	}

	// Convert nodeID to string for map lookup
	nodeIDStr := strconv.FormatUint(uint64(nodeID), 10)

	nodeData, exists := nodes[nodeIDStr]
	if !exists {
		return "", fmt.Errorf("node %d not found in DEFCON nodes API", nodeID)
	}

	nodeMap, ok := nodeData.(map[string]interface{})
	if !ok {
		return "", fmt.Errorf("invalid node data format for node %d", nodeID)
	}

	// meshobserv writes the public key under "pubkey" in nodes.json; older/alternate
	// schemas used "publicKey". Try both so PKI decrypt + reply-encrypt can find it.
	pubKey, exists := nodeMap["pubkey"]
	if !exists {
		pubKey, exists = nodeMap["publicKey"]
	}
	if !exists {
		return "", fmt.Errorf("PubKey not found for node %d", nodeID)
	}

	pubKeyStr, ok := pubKey.(string)
	if !ok {
		return "", fmt.Errorf("PubKey is not a string for node %d", nodeID)
	}

	if pubKeyStr == "" {
		return "", fmt.Errorf("PubKey is empty for node %d", nodeID)
	}

	c.log.Debugf("Fetched PubKey from DEFCON API for node %d: %s", nodeID, pubKeyStr)
	return pubKeyStr, nil
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
	auth := make([]byte, 8)
	copy(auth, encryptedBytes[authStart:authStart+8]) // Copy to avoid shared memory

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
	ciphertext := make([]byte, numBytes-12)
	copy(ciphertext, encryptedBytes[:numBytes-12]) // Copy to avoid shared memory
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
	// const L = 2
	// const M = 8

	//c.log.Debugf("=== Firmware aes_ccm_ad ===")
	//c.log.Debugf("Key: %x", key)
	//c.log.Debugf("Nonce: %x", nonce)
	//c.log.Debugf("Ciphertext: %x", ciphertext)
	//c.log.Debugf("Auth tag: %x", authTag)

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

	//c.log.Debugf("=== PKI Decryption SUCCESS ===")
	//c.log.Debugf("Plaintext: %x", plaintext)

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

	//c.log.Debugf("S_0 keystream: %x", s0)
	//c.log.Debugf("Decrypted auth tag: %x", decryptedAuthTag)

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

	//c.log.Debugf("Decrypted plaintext: %x", plaintext)
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

	//c.log.Debugf("B_0 block: %x", b0)

	// X_1 = E(K, B_0)
	x := make([]byte, 16)
	block.Encrypt(x, b0)

	//c.log.Debugf("X after B_0: %x", x)

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
	computedAuthTag := make([]byte, 8)
	copy(computedAuthTag, x[:8]) // Copy to avoid shared memory

	//c.log.Debugf("Final MAC block: %x", x)
	//c.log.Debugf("Computed auth tag: %x", computedAuthTag)
	//c.log.Debugf("Expected auth tag: %x", expectedAuthTag)

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

// encryptCurve25519 implements PKI encryption using X25519 and AES-CCM
func (c *MqttClient) encryptCurve25519(senderPrivateKey, recipientPublicKey, plaintext []byte, packetId, fromNode uint32) ([]byte, error) {
	// Calculate shared secret using X25519
	sharedSecret, err := curve25519.X25519(senderPrivateKey, recipientPublicKey)
	if err != nil {
		return nil, fmt.Errorf("X25519 failed: %v", err)
	}

	// Hash the shared secret with SHA256
	sharedKey := sha256.Sum256(sharedSecret)

	////c.log.Debugf("Raw shared secret: %x", sharedSecret)
	////c.log.Debugf("Shared key: %x", sharedKey[:])

	// Generate random extraNonce
	extraNonceBuf := make([]byte, 4)
	if _, err := rand.Read(extraNonceBuf); err != nil {
		return nil, fmt.Errorf("failed to generate extraNonce: %v", err)
	}
	extraNonce := binary.LittleEndian.Uint32(extraNonceBuf)

	// Initialize nonce
	nonce := c.initNonce(fromNode, uint64(packetId), extraNonce)
	//c.log.Debugf("Generated nonce: %x", nonce)

	// Encrypt using AES-CCM
	ciphertext, authTag, err := c.aesCCMEncrypt(sharedKey[:], nonce, plaintext)
	if err != nil {
		return nil, err
	}

	// Build encrypted packet: ciphertext + authTag + extraNonce
	encryptedData := make([]byte, len(ciphertext)+8+4) // ciphertext + 8-byte auth + 4-byte extraNonce
	copy(encryptedData, ciphertext)
	copy(encryptedData[len(ciphertext):], authTag)
	binary.LittleEndian.PutUint32(encryptedData[len(ciphertext)+8:], extraNonce)

	//c.log.Debugf("Encrypted data (%d bytes): %x", len(encryptedData), encryptedData)

	return encryptedData, nil
}

// aesCCMEncrypt implements AES-CCM encryption
func (c *MqttClient) aesCCMEncrypt(key, nonce, plaintext []byte) ([]byte, []byte, error) {
	// Create AES cipher
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, nil, fmt.Errorf("AES cipher creation failed: %v", err)
	}

	// Constants matching firmware: L=2, M=8
	// const L = 2
	// const M = 8

	//c.log.Debugf("=== AES-CCM Encrypt ===")
	//c.log.Debugf("Key: %x", key)
	//c.log.Debugf("Nonce: %x", nonce)
	//c.log.Debugf("Plaintext: %x", plaintext)

	// Step 1: Generate authentication tag
	authTag, err := c.generateAuthTag(block, nonce, plaintext)
	if err != nil {
		return nil, nil, err
	}

	// Step 2: Encrypt plaintext using CTR mode
	ciphertext, err := c.encryptCTR(block, nonce, plaintext)
	if err != nil {
		return nil, nil, err
	}

	// Step 3: Encrypt auth tag with S_0
	encryptedAuthTag, err := c.encryptAuthTag(block, nonce, authTag)
	if err != nil {
		return nil, nil, err
	}

	//c.log.Debugf("Ciphertext: %x", ciphertext)
	//c.log.Debugf("Encrypted auth tag: %x", encryptedAuthTag)

	return ciphertext, encryptedAuthTag, nil
}

// generateAuthTag generates the authentication tag for AES-CCM
func (c *MqttClient) generateAuthTag(block cipher.Block, nonce, plaintext []byte) ([]byte, error) {
	// Build B_0 block
	b0 := make([]byte, 16)
	// Flags = (aad_len ? 0x40 : 0) | (((M-2)/2) << 3) | (L-1)
	// aad_len=0, M=8, L=2 → Flags = 0 | (3 << 3) | 1 = 25 = 0x19
	b0[0] = 0x19
	copy(b0[1:], nonce) // 13-byte nonce
	binary.BigEndian.PutUint16(b0[14:16], uint16(len(plaintext)))

	// X_1 = E(K, B_0)
	x := make([]byte, 16)
	block.Encrypt(x, b0)

	//c.log.Debugf("B_0 block: %x", b0)
	//c.log.Debugf("X after B_0: %x", x)

	// Process plaintext blocks
	for i := 0; i < len(plaintext); i += 16 {
		// Create block with padding
		tempBlock := make([]byte, 16)
		end := i + 16
		if end > len(plaintext) {
			end = len(plaintext)
		}
		copy(tempBlock, plaintext[i:end])
		// Remaining bytes are already zero (proper padding)

		// XOR with X
		for j := 0; j < 16; j++ {
			x[j] ^= tempBlock[j]
		}

		// Encrypt X
		block.Encrypt(x, x)
	}

	// Return first 8 bytes as auth tag
	return x[:8], nil
}

// encryptCTR encrypts plaintext using CTR mode
func (c *MqttClient) encryptCTR(block cipher.Block, nonce, plaintext []byte) ([]byte, error) {
	ciphertext := make([]byte, len(plaintext))

	// CTR mode with counter starting at 1
	a := make([]byte, 16)
	a[0] = 1           // L-1 = 2-1 = 1
	copy(a[1:], nonce) // 13-byte nonce

	counter := 1
	for i := 0; i < len(plaintext); i += 16 {
		// Set counter
		binary.BigEndian.PutUint16(a[14:16], uint16(counter))

		// Generate keystream S_i
		s := make([]byte, 16)
		block.Encrypt(s, a)

		// XOR with plaintext to get ciphertext
		end := i + 16
		if end > len(plaintext) {
			end = len(plaintext)
		}

		for j := i; j < end; j++ {
			ciphertext[j] = plaintext[j] ^ s[j-i]
		}

		counter++
	}

	return ciphertext, nil
}

// encryptAuthTag encrypts the auth tag with S_0
func (c *MqttClient) encryptAuthTag(block cipher.Block, nonce, authTag []byte) ([]byte, error) {
	// Build A_0 for S_0 generation
	a0 := make([]byte, 16)
	a0[0] = 1                                // L-1 = 2-1 = 1
	copy(a0[1:], nonce)                      // 13-byte nonce
	binary.BigEndian.PutUint16(a0[14:16], 0) // counter = 0

	// S_0 = E(K, A_0)
	s0 := make([]byte, 16)
	block.Encrypt(s0, a0)

	// Encrypt: encrypted_auth = auth XOR S_0
	encryptedAuthTag := make([]byte, 8)
	for i := 0; i < 8; i++ {
		encryptedAuthTag[i] = authTag[i] ^ s0[i]
	}

	//c.log.Debugf("S_0 keystream: %x", s0)

	return encryptedAuthTag, nil
}
