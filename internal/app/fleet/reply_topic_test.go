package fleet

import "testing"

// TestOwnGatewayTopic locks the Phase-66 chatbot-reply fix: a ghost must publish
// its PKI reply on ITS OWN gateway topic, never on the sender's incoming topic.
//
// Field bug: sendPKIReply reused the incoming `topic`
// (msh/US/2/e/PKI/!<sender>). The sender is its own MQTT gateway, and a
// Meshtastic device ignores traffic on its own gateway topic (self-echo), so
// replies were published + ACKed + PKI-encrypted correctly yet never displayed.
// Republishing a captured reply to !<ghost> made it appear instantly on-device.
func TestOwnGatewayTopic(t *testing.T) {
	const base = "msh/US/2/e/dc.run"
	const ghost = uint32(0x1555f041) // goldstein
	const sender = uint32(0x435990e4) // the user's device (was the incoming gateway)

	got := ownGatewayTopic(base, ghost)

	if want := "msh/US/2/e/dc.run/!1555f041"; got != want {
		t.Fatalf("reply topic = %q, want %q", got, want)
	}

	// Regression guard: the reply must NOT go to the sender's gateway topic.
	if senderTopic := ownGatewayTopic(base, sender); got == senderTopic {
		t.Fatalf("reply topic reused the sender's gateway topic %q — device would self-drop it", senderTopic)
	}

	// pad-8 lowercase hex, matching publishACK/publishNodeInfo formatting.
	if small := ownGatewayTopic(base, 0xabc); small != "msh/US/2/e/dc.run/!00000abc" {
		t.Fatalf("node id not pad-8 lowercase hex: %q", small)
	}
}
