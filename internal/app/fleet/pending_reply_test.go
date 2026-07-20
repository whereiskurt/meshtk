package fleet

import (
	"go/ast"
	"testing"
	"time"
)

func mkPending(created, lastFlush time.Time, flushes int) *pendingReply {
	return &pendingReply{
		fleetIdx: 0, ghost: 357953601, to: 1129943268,
		topic: "msh/US/2/e/PKI/!435990e4", text: "❌ Did you have that OTP?",
		created: created, lastFlush: lastFlush, flushes: flushes,
	}
}

// The core case: a reply published into a disconnect gap is re-sent the moment
// the radio next transmits, once the cooldown has passed.
func TestPendingFlushesAfterCooldown(t *testing.T) {
	now := time.Unix(1784486034, 0)
	e := mkPending(now.Add(-60*time.Second), now.Add(-60*time.Second), 0)

	kept, due := selectDueFlushes([]*pendingReply{e}, now)
	if len(due) != 1 {
		t.Fatalf("due = %d, want 1 (radio seen after cooldown)", len(due))
	}
	if len(kept) != 1 {
		t.Errorf("kept = %d, want 1 (entry has flushes left)", len(kept))
	}
	if e.flushes != 1 {
		t.Errorf("flushes = %d, want 1", e.flushes)
	}
	if !e.lastFlush.Equal(now) {
		t.Error("lastFlush not advanced")
	}
}

// A radio beacons constantly while connected. Without the cooldown every one of
// those packets would re-send the reply, which is the flood we keep fixing.
func TestPendingDoesNotFlushWithinCooldown(t *testing.T) {
	now := time.Unix(1784486034, 0)
	e := mkPending(now.Add(-10*time.Second), now.Add(-10*time.Second), 0)

	_, due := selectDueFlushes([]*pendingReply{e}, now)
	if len(due) != 0 {
		t.Errorf("due = %d, want 0 (still inside the %v cooldown)", len(due), pendingFlushCooldown)
	}
}

// Repeated sightings must not resend forever -- the entry retires after
// pendingMaxFlush and is dropped from the queue.
func TestPendingRetiresAfterMaxFlushes(t *testing.T) {
	base := time.Unix(1784486034, 0)
	e := mkPending(base, base, 0)

	entries := []*pendingReply{e}
	sends := 0
	for i := 1; i <= pendingMaxFlush+3; i++ {
		now := base.Add(time.Duration(i) * pendingFlushCooldown)
		var due []*pendingReply
		entries, due = selectDueFlushes(entries, now)
		sends += len(due)
	}
	if sends != pendingMaxFlush {
		t.Errorf("re-sent %d times, want %d", sends, pendingMaxFlush)
	}
	if len(entries) != 0 {
		t.Errorf("queue still holds %d entries after exhausting flushes", len(entries))
	}
}

// A radio that vanishes for good must not leave entries in the map forever.
func TestPendingExpiresAfterTTL(t *testing.T) {
	base := time.Unix(1784486034, 0)
	e := mkPending(base, base, 0)

	kept, due := selectDueFlushes([]*pendingReply{e}, base.Add(pendingReplyTTL+time.Second))
	if len(due) != 0 {
		t.Errorf("due = %d, want 0 (expired)", len(due))
	}
	if len(kept) != 0 {
		t.Errorf("kept = %d, want 0 (expired entries must be dropped)", len(kept))
	}
}

// Queue-then-flush end to end through the real map, with no MQTT involved.
func TestQueuePendingReplyStoresPerRecipient(t *testing.T) {
	n := &FleetCmd{Pending: make(map[uint32][]*pendingReply)}
	n.queuePendingReply(0, 357953601, 1129943268, "t", "one", []byte{1})
	n.queuePendingReply(0, 794953058, 1129943268, "t", "two", []byte{2})
	n.queuePendingReply(0, 357953601, 42, "t", "other radio", []byte{3})

	if got := len(n.Pending[1129943268]); got != 2 {
		t.Errorf("recipient has %d pending, want 2", got)
	}
	if got := len(n.Pending[42]); got != 1 {
		t.Errorf("other radio has %d pending, want 1", got)
	}
	for _, e := range n.Pending[1129943268] {
		if e.lastFlush.IsZero() {
			t.Error("lastFlush must start at enqueue time, else the first sighting flushes immediately")
		}
	}
}

// The flush trigger has to run before FleetNodeHandler's broadcast early-return:
// the reconnect beacon we key on IS a broadcast, so a hook placed after that
// return would never fire for the case it exists to handle.
func TestOnNodeSeenRunsBeforeBroadcastSkip(t *testing.T) {
	fd, _ := funcBody(t, "FleetNodeHandler")

	var hookPos int
	ast.Inspect(fd, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "onNodeSeen" && hookPos == 0 {
				hookPos = int(call.Pos())
			}
		}
		return true
	})
	if hookPos == 0 {
		t.Fatal("FleetNodeHandler never calls onNodeSeen; pending replies would never flush")
	}

	// Locate the broadcast guard `if to == 4294967295 { return }`.
	var guardPos int
	ast.Inspect(fd, func(n ast.Node) bool {
		ifs, ok := n.(*ast.IfStmt)
		if !ok {
			return true
		}
		bin, ok := ifs.Cond.(*ast.BinaryExpr)
		if !ok {
			return true
		}
		if lit, ok := bin.Y.(*ast.BasicLit); ok && lit.Value == "4294967295" && guardPos == 0 {
			guardPos = int(ifs.Pos())
		}
		return true
	})
	if guardPos == 0 {
		t.Fatal("broadcast guard not found in FleetNodeHandler")
	}
	if hookPos > guardPos {
		t.Error("onNodeSeen runs after the broadcast early-return; reconnect beacons are broadcasts and would never reach it")
	}
}
