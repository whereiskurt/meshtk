// Package otpqueue drains the MeshOtpPending delivery queue: run.human
// enqueues a radio-verification code on manual add/resend, and the fleet's
// poller (internal/app/fleet/otpsend.go) PKI-DMs it to the device, deletes the
// item, and stamps codeSentAt on the MeshRadio row.
//
// Key strings are a LOCKED cross-language contract with run.human's ElectroDB
// entities (mesh-otp-pending-key-parity.test.ts / mesh-radio-key-parity.test.ts
// ↔ key_parity_test.go here). The live table has DDB TTL disabled, so expiry
// is enforced by the poller via MaxAgeMs.
package otpqueue

import (
	"context"
	"fmt"
)

const (
	queuePK       = "$run#queue_otp"
	queueSKPrefix = "$meshotppending_1#nodeid_"

	// MaxAgeMs: items older than this are reaped unsent (no DDB TTL on the
	// live table). Matches the spec's 24 h.
	MaxAgeMs = 24 * 60 * 60 * 1000

	// MaxAttempts: give up on an item after this many failed publishes.
	MaxAttempts = 10
)

func queueSK(nodeID string) string { return queueSKPrefix + nodeID }

type radioKey struct{ PK, SK string }

// meshRadioKey composes the byte-identical ElectroDB MeshRadio primary key —
// the same contract keycache locks for pubkey reads; here it targets the
// guarded codeSentAt stamp.
func meshRadioKey(nodeNum uint32) radioKey {
	return radioKey{PK: fmt.Sprintf("$run#nodeid_!%08x", nodeNum), SK: "$meshradio_1"}
}

// Item is one pending OTP delivery, as written by run.human's MeshOtpPending
// entity. Unknown attributes (__edb_e__ etc.) are ignored on unmarshal.
type Item struct {
	NodeID    string `dynamodbav:"nodeId"`
	NodeNum   uint32 `dynamodbav:"nodeNum"`
	Code      string `dynamodbav:"code"`
	PublicKey string `dynamodbav:"publicKey"` // "0x…" or "" when the user supplied none
	UserID    string `dynamodbav:"userId"`
	Attempts  int    `dynamodbav:"attempts"`
	CreatedAt int64  `dynamodbav:"createdAt"` // epoch ms (run.human Date.now())
}

// Store is the queue surface the fleet poller consumes; DynamoDBStore is the
// production implementation, tests use fakes.
type Store interface {
	List(ctx context.Context) ([]Item, error)
	Delete(ctx context.Context, nodeID string) error
	BumpAttempts(ctx context.Context, nodeID string, attempts int) error
	MarkRadioCodeSent(ctx context.Context, nodeNum uint32, sentAtMs int64) error
}
