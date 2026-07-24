package fleet

import (
	"go/ast"
	"testing"
	"time"
)

// The live failure this fixes: a radio retransmits an unacked DM ~3x, ~8s apart,
// and each copy used to start its own pkiReplyRetryCount chain -- 3 x 3 = 9
// copies of every reply line. Only the first copy may produce a reply.
func TestRetransmitBurstRepliesOnce(t *testing.T) {
	seen := map[string]time.Time{}
	base := time.Unix(1784483865, 0)
	key := "1129943268:Hihi"

	replies := 0
	for _, offset := range []time.Duration{0, 8 * time.Second, 16 * time.Second} {
		if !dedupRequest(seen, key, base.Add(offset), requestDedupWindow) {
			replies++
		}
	}
	if replies != 1 {
		t.Errorf("retransmit burst produced %d reply chains, want 1", replies)
	}
}

func TestDedupRequestAllowsAfterWindow(t *testing.T) {
	seen := map[string]time.Time{}
	base := time.Unix(1784483865, 0)
	key := "42:hello"

	if dedupRequest(seen, key, base, requestDedupWindow) {
		t.Fatal("first request treated as duplicate")
	}
	if !dedupRequest(seen, key, base.Add(requestDedupWindow), requestDedupWindow) {
		t.Error("request at exactly the window edge should still be a duplicate")
	}
	if dedupRequest(seen, key, base.Add(requestDedupWindow+time.Second), requestDedupWindow) {
		t.Error("request past the window should be allowed through")
	}
}

// Dedup is per (requester, text): a different attendee, or a different message,
// must never be suppressed by someone else's traffic.
func TestDedupRequestIsPerRequesterAndText(t *testing.T) {
	seen := map[string]time.Time{}
	now := time.Unix(1784483865, 0)

	if dedupRequest(seen, "1:Hi", now, requestDedupWindow) {
		t.Fatal("first request deduped")
	}
	if dedupRequest(seen, "2:Hi", now, requestDedupWindow) {
		t.Error("a different requester with the same text was suppressed")
	}
	if dedupRequest(seen, "1:Hello", now, requestDedupWindow) {
		t.Error("the same requester with different text was suppressed")
	}
}

// The map must not grow forever over a multi-day fleet lifetime.
func TestDedupRequestPrunesExpiredEntries(t *testing.T) {
	seen := map[string]time.Time{}
	base := time.Unix(1784483865, 0)

	for i := 0; i < 500; i++ {
		dedupRequest(seen, string(rune(i))+":msg", base, requestDedupWindow)
	}
	if len(seen) != 500 {
		t.Fatalf("setup: %d entries, want 500", len(seen))
	}
	dedupRequest(seen, "fresh:msg", base.Add(10*time.Minute), requestDedupWindow)
	if len(seen) != 1 {
		t.Errorf("after pruning, %d entries remain, want 1 (only the fresh one)", len(seen))
	}
}

// The guard has to run before ANY chatbot path, otherwise lyrics or the OTP
// unlock side effects still fire once per retransmitted copy.
func TestRetransmitGuardRunsBeforeChatbotPaths(t *testing.T) {
	fd, _ := funcBody(t, "FleetNodeHandler")

	var guardPos, firstReplyPos int
	ast.Inspect(fd, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		switch sel.Sel.Name {
		case "isRetransmit":
			if guardPos == 0 {
				guardPos = int(call.Pos())
			}
		case "sendPKIReplyReliable", "sendFlagReveal", "handleLyricsChat", "handleLLMChat":
			if firstReplyPos == 0 {
				firstReplyPos = int(call.Pos())
			}
		}
		return true
	})

	if guardPos == 0 {
		t.Fatal("FleetNodeHandler does not call isRetransmit; retransmits will multiply every reply")
	}
	if firstReplyPos == 0 {
		t.Fatal("no chatbot reply call found in FleetNodeHandler")
	}
	if guardPos > firstReplyPos {
		t.Error("isRetransmit guard runs after a chatbot reply path; it must gate all of them")
	}
}
