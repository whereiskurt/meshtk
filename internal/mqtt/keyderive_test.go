package mqtt

import (
	"bytes"
	"encoding/hex"
	"testing"

	"golang.org/x/crypto/curve25519"
)

func TestDeriveNodeKeyDeterministic(t *testing.T) {
	pub1, priv1, err := DeriveNodeKey("top-secret", 2076591764)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	pub2, priv2, err := DeriveNodeKey("top-secret", 2076591764)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !bytes.Equal(priv1, priv2) || !bytes.Equal(pub1, pub2) {
		t.Fatal("derivation not deterministic for identical inputs")
	}
	if len(priv1) != 32 || len(pub1) != 32 {
		t.Fatalf("expected 32-byte keys, got priv=%d pub=%d", len(priv1), len(pub1))
	}
}

func TestDeriveNodeKeyVariesByInput(t *testing.T) {
	_, privA, _ := DeriveNodeKey("top-secret", 1)
	_, privB, _ := DeriveNodeKey("top-secret", 2)
	_, privC, _ := DeriveNodeKey("other-secret", 1)
	if bytes.Equal(privA, privB) {
		t.Fatal("different nodeIDs produced identical private keys")
	}
	if bytes.Equal(privA, privC) {
		t.Fatal("different secrets produced identical private keys")
	}
}

func TestDeriveNodeKeyRoundTrip(t *testing.T) {
	// Two derived nodes must be able to compute a shared X25519 secret,
	// proving pub is the true match for priv (production uses curve25519.X25519).
	pubA, privA, _ := DeriveNodeKey("top-secret", 100)
	pubB, privB, _ := DeriveNodeKey("top-secret", 200)

	sharedAB, err := curve25519.X25519(privA, pubB)
	if err != nil {
		t.Fatalf("X25519 A->B failed: %v", err)
	}
	sharedBA, err := curve25519.X25519(privB, pubA)
	if err != nil {
		t.Fatalf("X25519 B->A failed: %v", err)
	}
	if !bytes.Equal(sharedAB, sharedBA) {
		t.Fatal("shared secrets differ — derived pub does not match priv")
	}
}

func TestApplyDerivedKeyOverwrites(t *testing.T) {
	node := NewNode("msh/US/2/e/dc.run")
	node.From = 2076591764
	node.PubKey = "0xaaaa"
	node.PrivKey = "0xbbbb"

	if err := node.ApplyDerivedKey("top-secret"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	pub, priv, _ := DeriveNodeKey("top-secret", 2076591764)
	wantPub := "0x" + hex.EncodeToString(pub)
	wantPriv := "0x" + hex.EncodeToString(priv)
	if node.PubKey != wantPub {
		t.Fatalf("pubkey = %s, want %s", node.PubKey, wantPub)
	}
	if node.PrivKey != wantPriv {
		t.Fatalf("privkey = %s, want %s", node.PrivKey, wantPriv)
	}
}
