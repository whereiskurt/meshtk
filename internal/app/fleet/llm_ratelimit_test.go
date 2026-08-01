package fleet

import (
	"go/ast"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/whereiskurt/meshtk/pkg/config"
)

// The bound this file defends: llm.go has no limiter of any kind, so every
// unlocked DM to a ghost is a metered Bedrock Converse call. requestDedupWindow
// only collapses BYTE-IDENTICAL repeats, so varying one character defeats it
// entirely. None of these properties need a live FleetCmd, MQTT or Bedrock —
// time is a parameter to the pure half, in the style of request_dedup_test.go,
// so there is no sleep anywhere and no flake.

// newTestFleetWithBuckets builds the bucket state NewFleets would build, without
// needing a live config. Fleet count and the hourly ceiling are explicit so a
// test can pin a small cap and still assert real burst behaviour.
func newTestFleetWithBuckets(fleets, callsPerHour int) *FleetCmd {
	f := &FleetCmd{LLMCallsPerHour: callsPerHour}
	for i := 0; i < fleets; i++ {
		f.LLMBuckets = append(f.LLMBuckets, make(map[uint32]*llmBucket))
		f.LLMBucketMux = append(f.LLMBucketMux, sync.Mutex{})
	}
	return f
}

func TestLLMCallsPerHourDefaultsWhenUnset(t *testing.T) {
	// t.Setenv registers the restore of whatever the process really had; the
	// Unsetenv right after is what actually puts the knob in the "absent" state.
	t.Setenv("MESHTK_LLM_CALLS_PER_HOUR", "sentinel")
	os.Unsetenv("MESHTK_LLM_CALLS_PER_HOUR")
	if got := llmCallsPerHour(); got != defaultLLMCallsPerHour {
		t.Fatalf("llmCallsPerHour() with the knob absent = %d, want %d", got, defaultLLMCallsPerHour)
	}
}

// A typo must never silently become a fleet-wide kill switch. Blank,
// whitespace-only, non-numeric, negative and fractional all fall back to the
// default — only a clean "0" reaches the brake (see the next test).
func TestLLMCallsPerHourTypoNeverBecomesAKillSwitch(t *testing.T) {
	for _, v := range []string{"", "   ", "banana", "-1", "12.5", "0x0", "60s", "sixty"} {
		t.Setenv("MESHTK_LLM_CALLS_PER_HOUR", v)
		if got := llmCallsPerHour(); got != defaultLLMCallsPerHour {
			t.Errorf("override %q gave %d, want the %d fallback — a typo must never become a kill switch", v, got, defaultLLMCallsPerHour)
		}
	}
}

// Zero is the DELIBERATE operator brake, and this is the point where this knob
// departs from lyricsMaxConcurrent (which coerces 0 to its default): the ghosts
// keep answering in words, they just stop costing money.
func TestLLMCallsPerHourZeroIsTheOperatorKillSwitch(t *testing.T) {
	for _, v := range []string{"0", " 0 "} {
		t.Setenv("MESHTK_LLM_CALLS_PER_HOUR", v)
		if got := llmCallsPerHour(); got != 0 {
			t.Errorf("override %q gave %d, want 0 (the deliberate kill switch)", v, got)
		}
	}
}

func TestLLMCallsPerHourHonorsPositiveOverride(t *testing.T) {
	t.Setenv("MESHTK_LLM_CALLS_PER_HOUR", "120")
	if got := llmCallsPerHour(); got != 120 {
		t.Fatalf("llmCallsPerHour() = %d, want 120", got)
	}
}

// A fresh bucket starts FULL, so a radio's first N questions in a quiet hour all
// go through and the N+1th costs nothing.
func TestTakeLLMTokenAllowsCapacityThenRefuses(t *testing.T) {
	base := time.Unix(1784483865, 0)
	b := &llmBucket{}

	for i := 1; i <= 5; i++ {
		if !takeLLMToken(b, 5, llmRateWindow, base) {
			t.Fatalf("call %d of 5 refused below capacity", i)
		}
	}
	if takeLLMToken(b, 5, llmRateWindow, base) {
		t.Fatal("the call past capacity was allowed; the cap does not bind")
	}
}

// Tokens return continuously rather than all at once on a window boundary, so a
// throttled radio is back in the conversation within a minute or two.
func TestTakeLLMTokenRefillsContinuously(t *testing.T) {
	base := time.Unix(1784483865, 0)
	b := &llmBucket{}

	for i := 1; i <= defaultLLMCallsPerHour; i++ {
		if !takeLLMToken(b, defaultLLMCallsPerHour, llmRateWindow, base) {
			t.Fatalf("call %d refused on a fresh bucket", i)
		}
	}
	if takeLLMToken(b, defaultLLMCallsPerHour, llmRateWindow, base) {
		t.Fatal("the 61st call inside the window was allowed")
	}

	half := base.Add(llmRateWindow / 2)
	allowed := 0
	for takeLLMToken(b, defaultLLMCallsPerHour, llmRateWindow, half) {
		allowed++
		if allowed > 200 {
			t.Fatal("refill never converged; the bucket is not bounded by capacity")
		}
	}
	if allowed < 29 || allowed > 31 {
		t.Errorf("half a window refilled %d calls, want ~30 (half of %d)", allowed, defaultLLMCallsPerHour)
	}
}

// Idle time must not bank credit. Without the clamp, a radio silent overnight
// would arrive with thousands of tokens and the ceiling would mean nothing.
func TestTakeLLMTokenNeverAccumulatesBeyondCapacity(t *testing.T) {
	base := time.Unix(1784483865, 0)
	b := &llmBucket{tokens: 0, last: base}

	late := base.Add(10 * llmRateWindow)
	allowed := 0
	for takeLLMToken(b, 10, llmRateWindow, late) {
		allowed++
		if allowed > 100 {
			t.Fatal("bucket accumulated past capacity; a long idle period becomes a burst")
		}
	}
	if allowed != 10 {
		t.Errorf("after 10 idle windows the bucket allowed %d calls, want the 10 cap", allowed)
	}
}

func TestTakeLLMTokenZeroCapacityRefusesEveryCall(t *testing.T) {
	base := time.Unix(1784483865, 0)
	b := &llmBucket{}
	for i := 0; i < 10; i++ {
		if takeLLMToken(b, 0, llmRateWindow, base.Add(time.Duration(i)*time.Hour)) {
			t.Fatalf("call %d allowed at capacity 0; the kill switch does not hold", i)
		}
	}
}

// Clock skew must not hand out free tokens: a backwards `now` refills nothing.
func TestTakeLLMTokenIgnoresBackwardsClock(t *testing.T) {
	base := time.Unix(1784483865, 0)
	b := &llmBucket{}
	for i := 1; i <= 3; i++ {
		if !takeLLMToken(b, 3, llmRateWindow, base) {
			t.Fatalf("call %d refused below capacity", i)
		}
	}
	if takeLLMToken(b, 3, llmRateWindow, base.Add(-time.Hour)) {
		t.Error("a backwards clock refilled the bucket")
	}
}

// The map must not grow forever over a multi-day fleet lifetime. A radio idle
// for a full window is provably back at full capacity, so its entry carries no
// information at all — keeping it is pure leak. Mirrors
// TestDedupRequestPrunesExpiredEntries.
func TestPruneLLMBucketsDropsFullyRecoveredRadios(t *testing.T) {
	base := time.Unix(1784483865, 0)
	m := map[uint32]*llmBucket{}
	for i := 0; i < 500; i++ {
		m[uint32(i)] = &llmBucket{tokens: 0, last: base}
	}
	if len(m) != 500 {
		t.Fatalf("setup: %d entries, want 500", len(m))
	}

	now := base.Add(2 * llmRateWindow)
	m[999999] = &llmBucket{tokens: 3, last: now}

	pruneLLMBuckets(m, now, llmRateWindow)
	if len(m) != 1 {
		t.Errorf("after pruning, %d entries remain, want 1 (only the radio being touched)", len(m))
	}
	if _, ok := m[999999]; !ok {
		t.Error("the radio being touched was pruned")
	}
}

// Pruning happens on the real access path, under the per-fleet mutex, not only
// in the pure helper.
func TestLLMRateAllowPrunesOnAccess(t *testing.T) {
	f := newTestFleetWithBuckets(1, defaultLLMCallsPerHour)
	stale := time.Now().Add(-2 * llmRateWindow)
	for i := 0; i < 500; i++ {
		f.LLMBuckets[0][uint32(i)] = &llmBucket{tokens: 0, last: stale}
	}

	if !f.allowLLMCall(0, 999999) {
		t.Fatal("a fresh radio was refused")
	}
	if got := len(f.LLMBuckets[0]); got != 1 {
		t.Errorf("bucket map holds %d entries after prune-on-access, want 1", got)
	}
}

// Every existing test in this package constructs a bare &FleetCmd{}, whose
// slices are nil; indexing a nil slice panics. Degrading to the old unbounded
// behaviour is far better than panicking or silently refusing everything —
// the same call that acquireLyricSlot makes for its nil channel.
func TestLLMRateAllowNilStateDegradesToUnlimited(t *testing.T) {
	f := &FleetCmd{}
	for i := 0; i < 100; i++ {
		if !f.allowLLMCall(0, 42) {
			t.Fatalf("bare &FleetCmd{} refused call %d; nil state must degrade to unlimited", i)
		}
	}

	var nilCmd *FleetCmd
	if !nilCmd.allowLLMCall(0, 42) {
		t.Error("a nil *FleetCmd refused a call")
	}
}

// The guard is deliberately stricter than isRetransmit, which indexes
// RecentReq[toFleetIdx] bare: allowLLMCall indexes BOTH slices, so clearing one
// while the other is short would still panic.
func TestLLMRateAllowOutOfRangeIndexDegradesToUnlimited(t *testing.T) {
	f := newTestFleetWithBuckets(1, 1)
	for _, idx := range []int{-1, 1, 99} {
		if !f.allowLLMCall(idx, 7) {
			t.Errorf("fleet index %d was refused; an out-of-range index must degrade to unlimited", idx)
		}
	}

	short := &FleetCmd{LLMCallsPerHour: 1}
	short.LLMBuckets = append(short.LLMBuckets, make(map[uint32]*llmBucket))
	if !short.allowLLMCall(0, 7) {
		t.Error("a bucket slice longer than the mutex slice refused a call instead of degrading")
	}

	nilMap := &FleetCmd{
		LLMCallsPerHour: 1,
		LLMBuckets:      []map[uint32]*llmBucket{nil},
		LLMBucketMux:    make([]sync.Mutex, 1),
	}
	if !nilMap.allowLLMCall(0, 7) {
		t.Error("a nil per-fleet bucket map refused a call instead of degrading")
	}
}

func TestLLMRateAllowZeroCapacityRefusesEveryCall(t *testing.T) {
	f := newTestFleetWithBuckets(1, 0)
	for i := 0; i < 10; i++ {
		if f.allowLLMCall(0, 7) {
			t.Fatalf("call %d allowed with the operator kill switch engaged", i)
		}
	}
}

// The fleet is NEVER globally silenced by one abusive radio: buckets key on
// (fleet, sender), so an exhausted sender takes nothing away from anybody else.
func TestLLMRateAllowBucketsAreIndependentPerRadioAndFleet(t *testing.T) {
	f := newTestFleetWithBuckets(2, 2)

	if !f.allowLLMCall(0, 1) || !f.allowLLMCall(0, 1) {
		t.Fatal("radio 1 refused below its cap")
	}
	if f.allowLLMCall(0, 1) {
		t.Fatal("radio 1 exceeded its cap")
	}

	if !f.allowLLMCall(0, 2) {
		t.Error("a different radio was refused by someone else's exhausted bucket")
	}
	if !f.allowLLMCall(1, 1) {
		t.Error("radio 1's fleet-0 bucket leaked into fleet 1")
	}
}

// The refusal must not tell an attacker there is a control, what it costs, or
// what model is behind it — and it must not be silence, because a blackholed
// request is indistinguishable from a dead bot.
func TestLLMRateLimitReplyRevealsNoControl(t *testing.T) {
	if strings.TrimSpace(llmRateLimitReply) == "" {
		t.Fatal("refusal copy is empty; an over-cap requester must be answered in words")
	}
	low := strings.ToLower(llmRateLimitReply)
	for _, leak := range []string{"rate", "limit", "quota", "cap", "throttl", "token", "bedrock", "claude", "anthropic", "model", "cost", "spend", "budget", "$"} {
		if strings.Contains(low, leak) {
			t.Errorf("refusal copy leaks %q: %q", leak, llmRateLimitReply)
		}
	}
}

// callPositions returns the source position of the FIRST call to each named
// function inside fd, handling both method calls (n.foo()) and package-level
// calls (foo()). Position, not line number, so the assertion survives any
// reformatting of the file.
func callPositions(fd *ast.FuncDecl, names ...string) map[string]int {
	want := map[string]bool{}
	for _, n := range names {
		want[n] = true
	}
	out := map[string]int{}
	ast.Inspect(fd, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		name := ""
		switch fn := call.Fun.(type) {
		case *ast.SelectorExpr:
			name = fn.Sel.Name
		case *ast.Ident:
			name = fn.Name
		}
		if want[name] {
			if _, seen := out[name]; !seen {
				out[name] = int(call.Pos())
			}
		}
		return true
	})
	return out
}

// The whole point of the limiter is that a refused request costs NOTHING, so
// the guard has to sit ahead of generateReply — behind it, the Converse call
// has already been paid for by the time anybody is refused. Shape borrowed from
// TestRetransmitGuardRunsBeforeChatbotPaths.
func TestLLMRateGuardRunsBeforeGenerateReply(t *testing.T) {
	fd, _ := funcBody(t, "handleLLMChat")
	pos := callPositions(fd, "allowLLMCall", "generateReply")

	if pos["allowLLMCall"] == 0 {
		t.Fatal("handleLLMChat never consults the limiter; per-radio Bedrock spend is unbounded")
	}
	if pos["generateReply"] == 0 {
		t.Fatal("no generateReply call found in handleLLMChat")
	}
	if pos["allowLLMCall"] > pos["generateReply"] {
		t.Error("the limiter runs AFTER generateReply; a refused request would still pay for the model call")
	}
}

// An over-cap requester is refused in WORDS, never blackholed — a blackholed
// request is indistinguishable from a dead bot (stageFullReply's reasoning).
// The refusal rides the PLAIN send path, the same one the adjacent
// generate-failure branch uses.
//
// The sendPKIReply floor is 1 rather than an exact count on purpose: this
// function already had plain sends before the limiter existed (the generate
// failure, the guardrail refusal and the paced burst), so pinning an exact
// number would break on any unrelated edit. The load-bearing half of this test
// is the llmRateLimitReply check below, which no pre-existing send satisfies.
func TestHandleLLMChatRefusesInWords(t *testing.T) {
	fd, _ := funcBody(t, "handleLLMChat")

	if got := calleeNames(fd)["sendPKIReply"]; got < 1 {
		t.Errorf("handleLLMChat has %d plain sendPKIReply sites, want at least 1 so an over-cap radio gets words rather than silence", got)
	}

	sendsRefusal := false
	ast.Inspect(fd, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && id.Name == "llmRateLimitReply" {
			sendsRefusal = true
		}
		return true
	})
	if !sendsRefusal {
		t.Error("handleLLMChat never sends llmRateLimitReply; an over-cap radio would be blackholed")
	}
}

// A fleet whose bucket slices are short degrades to unlimited — safe, but it
// silently removes the bound, so the allocation is worth pinning. The ceiling
// is resolved exactly once here so a mid-flight env change cannot move it.
func TestNewFleetsAllocatesOneLLMBucketPerFleet(t *testing.T) {
	t.Setenv("MESHTK_LLM_CALLS_PER_HOUR", "7")

	c := &config.Config{Fleet: []config.Fleet{{}, {}}}
	// Valid keycache config so buildKeyResolver takes its fast in-process path
	// rather than probing for AWS credentials; mirrors keyresolver_test.go.
	c.Server.KeyCache = config.KeyCacheConfig{
		TTLSecs:         90,
		MaxSizeMB:       16,
		TableName:       "run-human-electro",
		TableRegion:     "us-east-1",
		NegativeTTLSecs: 60,
		Fallback:        "nodes.json",
	}

	f := NewFleets(c)

	if got := len(f.LLMBuckets); got != 2 {
		t.Fatalf("len(LLMBuckets) = %d, want one per fleet (2)", got)
	}
	if got := len(f.LLMBucketMux); got != 2 {
		t.Fatalf("len(LLMBucketMux) = %d, want one per fleet (2)", got)
	}
	for i, m := range f.LLMBuckets {
		if m == nil {
			t.Errorf("fleet %d has a nil bucket map, so its radios are unbounded", i)
		}
	}
	if f.LLMCallsPerHour != 7 {
		t.Errorf("LLMCallsPerHour = %d, want 7 resolved once at construction", f.LLMCallsPerHour)
	}

	// End to end on the constructed fleet: the 8th call in the window is
	// refused, and only for the radio that spent its allowance.
	for i := 1; i <= 7; i++ {
		if !f.allowLLMCall(0, 11) {
			t.Fatalf("call %d of 7 refused below the constructed ceiling", i)
		}
	}
	if f.allowLLMCall(0, 11) {
		t.Error("the 8th call was allowed; the constructed ceiling does not bind")
	}
	if !f.allowLLMCall(0, 12) {
		t.Error("a different radio was silenced by an exhausted neighbour")
	}
}
