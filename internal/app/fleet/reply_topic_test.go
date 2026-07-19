package fleet

import "testing"

// TestReplyTopicFor locks the Phase-66 chatbot-reply fix. A ghost's PKI reply
// must go to: (1) its OWN gateway id, not the sender's (else the sending device
// self-drops it), and (2) the PKI base, not the channel base (else the device
// tries channel-decryption on a PKI packet and drops it). Both were confirmed
// on-wire: replies on ".../PKI/!<sender>" and ".../dc.run/!<ghost>" never
// displayed; ".../PKI/!<ghost>" did.
func TestReplyTopicFor(t *testing.T) {
	const incoming = "msh/US/2/e/PKI/!435990e4" // DM arrived from the user's device
	const ghost = uint32(0x1555f041)            // goldstein

	got := replyTopicFor(incoming, ghost)

	if want := "msh/US/2/e/PKI/!1555f041"; got != want {
		t.Fatalf("reply topic = %q, want %q", got, want)
	}

	// Must keep the PKI base from the incoming DM, never the channel base.
	if got == "msh/US/2/e/dc.run/!1555f041" {
		t.Fatalf("reply must stay on the PKI base, not the channel base: %q", got)
	}

	// Must NOT reuse the sender's gateway topic (self-echo drop).
	if got == incoming {
		t.Fatalf("reply reused the sender's gateway topic %q — device self-drops it", incoming)
	}

	// Region-less base is preserved too (fleet subscribes msh/2/e/PKI/# as well).
	if r := replyTopicFor("msh/2/e/PKI/!435990e4", 0xabc); r != "msh/2/e/PKI/!00000abc" {
		t.Fatalf("region-less base or pad-8 hex wrong: %q", r)
	}
}
