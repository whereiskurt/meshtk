package keycache

import (
	"context"
	"errors"
)

// ErrNotFound is returned when a pubkey lookup finds no matching MeshRadio item.
var ErrNotFound = errors.New("pubkey not found")

// Key holds a sender node's authoritative decrypt pubkey from the MeshRadio
// DynamoDB entity. PubKeyHex is the 0x-prefixed X25519 public key as written by
// run.human's register-radio boundary (base64 → 0x hex), ready for ParseHexKey.
type Key struct {
	NodeID    string `dynamodbav:"nodeId"`
	NodeNum   uint32 `dynamodbav:"nodeNum"`
	PubKeyHex string `dynamodbav:"publicKey"`
	Negative  bool   // Not stored in DynamoDB — marks negative cache entries.
}

// KeyStore defines the interface for fetching a node's pubkey from a backend.
// Fetch is keyed by the uint32 nodeNum; implementations compose the canonical
// nodeId ("!%08x") to build the authoritative DynamoDB key.
type KeyStore interface {
	Fetch(ctx context.Context, nodeNum uint32) (*Key, error)
}
