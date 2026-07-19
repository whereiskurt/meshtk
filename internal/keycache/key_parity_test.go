package keycache

import (
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// TestKeyParity is the Go twin of run.human's mesh-radio-key-parity.test.ts
// (sibling of qr-key-parity.test.ts). It LOCKS meshtk's composed GetItem key to
// the byte-identical ElectroDB MeshRadio primary key. Any drift in entity name,
// version, service, or nodeId canonicalization silently 404s every decrypt
// (RESEARCH.md landmine L1) — this test fails first.
//
// ElectroDB v3.5 format for MeshRadio keyed by nodeId:
//   pk = "$run#nodeid_<nodeId>"   sk = "$meshradio_1"
// For nodeNum 0x433d1cec, nodeId canonicalizes to "!433d1cec".
func TestKeyParity_MeshRadioComposedKey(t *testing.T) {
	const nodeNum = uint32(0x433d1cec)

	const wantNodeID = "!433d1cec"
	const wantPK = "$run#nodeid_!433d1cec"
	const wantSK = "$meshradio_1"

	if got := NodeIDFromNum(nodeNum); got != wantNodeID {
		t.Errorf("NodeIDFromNum(%#x) = %q, want %q", nodeNum, got, wantNodeID)
	}

	key := meshRadioKey(nodeNum)

	pk, ok := key["pk"].(*types.AttributeValueMemberS)
	if !ok {
		t.Fatalf("key[pk] type = %T, want *AttributeValueMemberS", key["pk"])
	}
	if pk.Value != wantPK {
		t.Errorf("pk = %q, want %q", pk.Value, wantPK)
	}

	sk, ok := key["sk"].(*types.AttributeValueMemberS)
	if !ok {
		t.Fatalf("key[sk] type = %T, want *AttributeValueMemberS", key["sk"])
	}
	if sk.Value != wantSK {
		t.Errorf("sk = %q, want %q", sk.Value, wantSK)
	}
}

// TestKeyParity_PadsLeadingZeroNodeNum guards RESEARCH.md landmine L2: a nodeNum
// with leading-zero bytes must pad to 8 lowercase hex digits, matching the
// run.human write boundary's "!" + toString(16).padStart(8,"0").
func TestKeyParity_PadsLeadingZeroNodeNum(t *testing.T) {
	if got := NodeIDFromNum(0x00000abc); got != "!00000abc" {
		t.Errorf("NodeIDFromNum(0x00000abc) = %q, want %q", got, "!00000abc")
	}
	if got := NodeIDFromNum(0x00000001); got != "!00000001" {
		t.Errorf("NodeIDFromNum(0x1) = %q, want %q", got, "!00000001")
	}
	if got := NodeIDFromNum(0xffffffff); got != "!ffffffff" {
		t.Errorf("NodeIDFromNum(0xffffffff) = %q, want %q", got, "!ffffffff")
	}
}
