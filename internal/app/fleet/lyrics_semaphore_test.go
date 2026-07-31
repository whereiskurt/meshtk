package fleet

import (
	"go/ast"
	"testing"
)

// The lyric cooldown is per-REQUESTER, so N distinct radios start N concurrent
// songs with no global bound. Per-radio load is fine (~1 message every 3.6s);
// the exposure is aggregate RF airtime, which is what the semaphore defends.
// None of these properties need a live FleetCmd, MQTT or Bedrock — just the
// slot bookkeeping itself, in the style of burst_lock_test.go.

func TestLyricSlotsCapacityIsTwelveByDefault(t *testing.T) {
	t.Setenv("MESHTK_LYRICS_MAX_CONCURRENT", "")
	if got := lyricsMaxConcurrent(); got != 12 {
		t.Fatalf("lyricsMaxConcurrent() = %d, want 12", got)
	}
}

// A missing, non-numeric or non-positive override must fall back to the default
// and NEVER to zero: a zero-capacity channel refuses every acquisition, which
// would silence ricky entirely rather than merely capping the crowd.
func TestLyricsMaxConcurrentFallsBackToDefaultNeverZero(t *testing.T) {
	for _, v := range []string{"", "   ", "banana", "0", "-1", "12.5"} {
		t.Setenv("MESHTK_LYRICS_MAX_CONCURRENT", v)
		if got := lyricsMaxConcurrent(); got != 12 {
			t.Errorf("override %q gave %d, want the 12 fallback", v, got)
		}
	}
}

func TestLyricsMaxConcurrentHonorsPositiveOverride(t *testing.T) {
	t.Setenv("MESHTK_LYRICS_MAX_CONCURRENT", "3")
	if got := lyricsMaxConcurrent(); got != 3 {
		t.Fatalf("lyricsMaxConcurrent() = %d, want 3", got)
	}
}

// Acquiring the cap-th slot succeeds; the next fails; one release lets the next
// through. Acquisition is non-blocking, so an over-cap request is refused
// immediately — a queued backlog would outlive the requester's interest and
// then dump a burst on a channel nobody is listening to any more.
func TestLyricSlotAcquireCapsAndRecoversAfterRelease(t *testing.T) {
	f := &FleetCmd{LyricSlots: make(chan struct{}, 3)}

	for i := 1; i <= 3; i++ {
		if !f.acquireLyricSlot() {
			t.Fatalf("acquisition %d of 3 was refused below capacity", i)
		}
	}
	if f.acquireLyricSlot() {
		t.Fatal("acquisition above capacity succeeded; the cap does not bind")
	}

	f.releaseLyricSlot()
	if !f.acquireLyricSlot() {
		t.Fatal("acquisition after a release was refused; slots do not recycle")
	}
}

// The existing tests construct bare &FleetCmd{} values, so a nil slot channel
// must degrade to unlimited rather than panic or (worse) refuse everything.
func TestLyricSlotNilChannelDegradesToUnlimited(t *testing.T) {
	f := &FleetCmd{}
	for i := 0; i < 100; i++ {
		if !f.acquireLyricSlot() {
			t.Fatalf("nil slot channel refused acquisition %d", i)
		}
	}
	f.releaseLyricSlot() // must not panic or block
}

// Releasing more often than acquiring must not block or corrupt the count —
// the release helper is called on four different exit paths and a double
// release on any of them would otherwise wedge the whole cap.
func TestLyricSlotReleaseWithoutAcquireIsHarmless(t *testing.T) {
	f := &FleetCmd{LyricSlots: make(chan struct{}, 1)}
	f.releaseLyricSlot()
	f.releaseLyricSlot()
	if !f.acquireLyricSlot() {
		t.Fatal("over-release consumed capacity")
	}
	if f.acquireLyricSlot() {
		t.Fatal("over-release inflated capacity")
	}
}

// A slot leak is permanent and silent: it costs one twelfth of the stage for
// the life of the process. Every exit path out of handleLyricsChat must release
// — the decode failure, the empty-entry parse, and a defer at the top of the
// playback goroutine (which covers both normal completion and the termination
// timer). Three is the floor, not the target.
func TestLyricSlotReleasedOnEveryExitPath(t *testing.T) {
	fd, _ := funcBody(t, "handleLyricsChat")
	calls := calleeNames(fd)
	if got := calls["releaseLyricSlot"]; got < 3 {
		t.Errorf("handleLyricsChat releases a lyric slot %d times, want at least 3 (decode failure, empty entries, goroutine defer)", got)
	}
	if got := calls["acquireLyricSlot"]; got != 1 {
		t.Errorf("handleLyricsChat acquires a lyric slot %d times, want exactly 1", got)
	}
}

// An over-cap requester must be able to retry the moment a slot frees, so the
// acquisition has to happen BEFORE the cooldown is marked and the refusal has
// to return without falling through to the mark. Marking first and refusing
// second would lock the requester out for ten minutes and hand them no song at
// all — the worst of both mechanisms.
func TestOverCapRefusalDoesNotBurnTheCooldown(t *testing.T) {
	fd, _ := funcBody(t, "handleLyricsChat")

	var acquireIf *ast.IfStmt
	ast.Inspect(fd, func(n ast.Node) bool {
		is, ok := n.(*ast.IfStmt)
		if !ok || acquireIf != nil {
			return true
		}
		ast.Inspect(is.Cond, func(c ast.Node) bool {
			call, ok := c.(*ast.CallExpr)
			if !ok {
				return true
			}
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "acquireLyricSlot" {
				acquireIf = is
			}
			return true
		})
		return true
	})
	if acquireIf == nil {
		t.Fatal("no `if ...acquireLyricSlot()` guard found in handleLyricsChat")
	}

	// The refusal branch must terminate the call, not fall through.
	body := acquireIf.Body.List
	if len(body) == 0 {
		t.Fatal("the over-cap branch is empty")
	}
	if _, ok := body[len(body)-1].(*ast.ReturnStmt); !ok {
		t.Error("the over-cap branch does not end in a return; it would fall through and mark the cooldown")
	}

	// ...and it must sit ahead of the cooldown write.
	var writePos = -1
	ast.Inspect(fd, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Lhs) == 0 || writePos >= 0 {
			return true
		}
		ast.Inspect(as.Lhs[0], func(l ast.Node) bool {
			if sel, ok := l.(*ast.SelectorExpr); ok && sel.Sel.Name == "LyricsResponded" {
				writePos = int(as.Pos())
			}
			return true
		})
		return true
	})
	if writePos < 0 {
		t.Fatal("no assignment to LyricsResponded found in handleLyricsChat")
	}
	if int(acquireIf.Pos()) > writePos {
		t.Error("the cooldown is marked before the slot is acquired; a refused requester would burn their 10-minute cooldown for nothing")
	}
}

// The cap is GLOBAL, not per fleet: the bound being defended is aggregate RF
// airtime, which knows nothing about fleet indices. A per-fleet channel would
// multiply the cap by the fleet count.
func TestLyricSlotsIsASingleProcessWideChannel(t *testing.T) {
	f := &FleetCmd{LyricSlots: make(chan struct{}, 1)}
	if !f.acquireLyricSlot() {
		t.Fatal("first acquisition refused")
	}
	// Nothing in the acquire path takes a fleet index, so a second fleet's
	// request contends on the same channel by construction.
	if f.acquireLyricSlot() {
		t.Fatal("the cap did not bind across a second caller")
	}
}
