package mqtt

import (
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/sha256"
	"fmt"
)

// DeriveNodeKey deterministically derives a valid X25519 keypair from a
// server-only secret and the node's stable ID. The public key is computed from
// the private scalar, so the pair always matches. Same (secret, nodeID) yields
// the same keypair on every call — keys survive restarts unchanged.
func DeriveNodeKey(secret string, nodeID uint32) (pub []byte, priv []byte, err error) {
	info := fmt.Sprintf("meshtk-node-key:%08x", nodeID)
	seed, err := hkdf.Key(sha256.New, []byte(secret), nil, info, 32)
	if err != nil {
		return nil, nil, fmt.Errorf("hkdf: %w", err)
	}

	privKey, err := ecdh.X25519().NewPrivateKey(seed)
	if err != nil {
		return nil, nil, fmt.Errorf("x25519 private key: %w", err)
	}
	return privKey.PublicKey().Bytes(), privKey.Bytes(), nil
}

// HexKey formats raw key bytes as meshtk's on-wire "0x"-prefixed lowercase hex.
func HexKey(b []byte) string {
	return fmt.Sprintf("0x%x", b)
}
