package otpqueue

import "testing"

// LOCKED cross-language contract with run.human's
// mesh-otp-pending-key-parity.test.ts — same fixture nodeId !433d1cec
// (nodeNum 1128078572), byte-identical strings. Drift here silently strands
// every OTP delivery (Query matches nothing) — never change one side alone.
func TestQueueKeyParity(t *testing.T) {
	if queuePK != "$run#queue_otp" {
		t.Fatalf("queue pk drifted: %q", queuePK)
	}
	if got := queueSK("!433d1cec"); got != "$meshotppending_1#nodeid_!433d1cec" {
		t.Fatalf("queue sk drifted: %q", got)
	}
	if queueSKPrefix != "$meshotppending_1#nodeid_" {
		t.Fatalf("sk prefix drifted: %q", queueSKPrefix)
	}
}

// Mirrors keycache's parity lock on the MeshRadio primary key — the poller
// stamps codeSentAt onto this row after a successful send.
func TestMeshRadioKeyParity(t *testing.T) {
	k := meshRadioKey(1128078572) // 0x433d1cec
	if k.PK != "$run#nodeid_!433d1cec" || k.SK != "$meshradio_1" {
		t.Fatalf("MeshRadio key drifted: %+v", k)
	}
}
