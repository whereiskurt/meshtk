package fleet

import (
	"go/ast"
	"go/parser"
	"go/token"
	"sync"
	"testing"
	"time"
)

// sendSpread's mechanics: N sends, evenly spaced, first one synchronous. Uses a
// literal 3 rather than pkiReplyRetryCount so retuning the constant does not
// silently weaken this test.
func TestSendSpreadEmitsThreeSendsThirtySecondsApart(t *testing.T) {
	var mu sync.Mutex
	var sleeps []time.Duration
	sends := make(chan struct{}, 16)

	sleep := func(d time.Duration) {
		mu.Lock()
		sleeps = append(sleeps, d)
		mu.Unlock()
	}
	send := func() { sends <- struct{}{} }

	sendSpread(3, pkiReplyRetrySpacing, sleep, send) // 3 explicitly: this tests sendSpread, not the tuning

	for i := 0; i < 3; i++ {
		select {
		case <-sends:
		case <-time.After(2 * time.Second):
			t.Fatalf("only got %d of 3 sends", i)
		}
	}
	select {
	case <-sends:
		t.Fatal("got more sends than requested")
	case <-time.After(50 * time.Millisecond):
	}

	mu.Lock()
	defer mu.Unlock()
	if len(sleeps) != 2 {
		t.Fatalf("sleeps = %d, want 2", len(sleeps))
	}
	for i, d := range sleeps {
		if d != pkiReplyRetrySpacing {
			t.Errorf("sleep[%d] = %v, want %v", i, d, pkiReplyRetrySpacing)
		}
	}
}

// The tuned contract, post proxy fix: a recipient should never see more than 2
// copies of a reply line (1 immediate + at most 1 store-and-forward flush).
func TestPKIReplyRetryConstants(t *testing.T) {
	// Two timed sends, 10s apart: a fast second chance for the packet-of-a-burst
	// the BLE link regularly eats, well before the ~60s beacon flush. Safe only
	// because copies are byte-identical (one packet id per line).
	if pkiReplyRetryCount != 2 {
		t.Errorf("pkiReplyRetryCount = %d, want 2", pkiReplyRetryCount)
	}
	if pkiReplyRetrySpacing != 10*time.Second {
		t.Errorf("pkiReplyRetrySpacing = %v, want 10s", pkiReplyRetrySpacing)
	}
	// Wire copies may exceed what a user should SEE because every re-send is
	// byte-identical (one packet id per line; the device dedups repeats). The
	// cooldown must land the first flush on the recipient's FIRST beacon after
	// a loss (~60s cadence), and keep total wire traffic bounded.
	if pendingMaxFlush != 2 {
		t.Errorf("pendingMaxFlush = %d, want 2", pendingMaxFlush)
	}
	if pendingFlushCooldown != 20*time.Second {
		t.Errorf("pendingFlushCooldown = %v, want 20s (first beacon after a loss, not the second)", pendingFlushCooldown)
	}
}

// Every copy of a one-shot reply must be the SAME bytes: the reliable path
// builds once (buildPKIReply) and republishes via PublishEnvelopeBytes. A
// fresh build per copy (PublishPKIMessage / sendPKIReply) mints a new packet
// id each time, and duplicate lines display on the device -- observed live
// 2026-07-19 as ten copies of a two-line reply.
func TestReliableRepliesAreByteIdentical(t *testing.T) {
	for _, fn := range []string{"sendPKIReplyReliable", "onNodeSeen"} {
		fd, _ := funcBody(t, fn)
		calls := calleeNames(fd)
		if calls["PublishPKIMessage"] != 0 || calls["sendPKIReply"] != 0 {
			t.Errorf("%s builds a fresh packet per copy; it must republish stored envelope bytes", fn)
		}
		if calls["PublishEnvelopeBytes"] == 0 {
			t.Errorf("%s does not publish the stored envelope bytes", fn)
		}
	}
	fd, _ := funcBody(t, "sendPKIReplyReliable")
	if calleeNames(fd)["buildPKIReply"] == 0 {
		t.Error("sendPKIReplyReliable no longer builds the envelope once up front")
	}
}

// Consecutive lines to one recipient are spaced apart; a same-millisecond
// 2-line burst down the BLE pipe regularly delivered exactly one line.
func TestStaggerDelaySpacesConsecutiveSends(t *testing.T) {
	base := time.Unix(1000, 0)

	// First send: immediate, and the gate advances by one spacing.
	delay, next := staggerDelay(time.Time{}, base, 2*time.Second)
	if delay != 0 {
		t.Errorf("first send delayed %v, want 0 (first line must not lag)", delay)
	}
	if want := base.Add(2 * time.Second); !next.Equal(want) {
		t.Errorf("next = %v, want %v", next, want)
	}

	// Second send in the same instant: waits out the spacing.
	delay, next = staggerDelay(next, base, 2*time.Second)
	if delay != 2*time.Second {
		t.Errorf("second send delayed %v, want 2s", delay)
	}
	if want := base.Add(4 * time.Second); !next.Equal(want) {
		t.Errorf("next = %v, want %v", next, want)
	}

	// A send after the gate has passed: immediate again.
	late := base.Add(10 * time.Second)
	delay, _ = staggerDelay(next, late, 2*time.Second)
	if delay != 0 {
		t.Errorf("post-gate send delayed %v, want 0", delay)
	}
}

// The first send is synchronous — the reply must not be delayed by the retry
// machinery — and the handler must not be stalled for the full spread.
func TestSendSpreadFirstSendIsSynchronous(t *testing.T) {
	n := 0
	blockingSleep := func(time.Duration) { time.Sleep(10 * time.Millisecond) }
	done := make(chan struct{})
	go func() {
		sendSpread(2, time.Second, blockingSleep, func() { n++ })
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("sendSpread blocked on the retry goroutine")
	}
	if n < 1 {
		t.Fatal("first send did not happen synchronously")
	}
}

func TestSendSpreadCountOneDoesNotRetry(t *testing.T) {
	sends := 0
	sendSpread(1, time.Second, func(time.Duration) { t.Fatal("must not sleep") }, func() { sends++ })
	if sends != 1 {
		t.Fatalf("sends = %d, want 1", sends)
	}
}

// funcBody returns the AST of the named method in cmd.go.
func funcBody(t *testing.T, name string) (*ast.FuncDecl, *token.FileSet) {
	t.Helper()
	fset := token.NewFileSet()
	for _, file := range []string{"cmd.go", "claimlink.go"} {
		f, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == name {
				return fd, fset
			}
		}
	}
	t.Fatalf("func %s not found in cmd.go/claimlink.go", name)
	return nil, nil
}

func calleeNames(fd *ast.FuncDecl) map[string]int {
	out := map[string]int{}
	ast.Inspect(fd, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
			out[sel.Sel.Name]++
		}
		return true
	})
	return out
}

// ricky's lyrics already emit ~60 messages per request; retrying them 3x would
// produce ~180 and re-create the channel drowning that this work just fixed.
func TestLyricsChatDoesNotUseReliableRetry(t *testing.T) {
	fd, _ := funcBody(t, "handleLyricsChat")
	calls := calleeNames(fd)
	if got := calls["sendPKIReplyReliable"]; got != 0 {
		t.Errorf("handleLyricsChat calls sendPKIReplyReliable %d times; lyrics must stay single-send", got)
	}
	if calls["sendPKIReply"] == 0 {
		t.Error("handleLyricsChat no longer calls sendPKIReply at all")
	}
}

// The one-shot reply paths all live in FleetNodeHandler: otp_success,
// otp_failure (non-unlocking branch), and — in the unlocked branch — the
// guardrail-block refusal. Every one of them must go through the reliable
// wrapper. (The deterministic flag reveal moved into sendFlagReveal — covered
// by TestFlagRevealUsesReliableRetry below.)
func TestOneShotReplyPathsUseReliableRetry(t *testing.T) {
	fd, _ := funcBody(t, "FleetNodeHandler")
	calls := calleeNames(fd)
	if got := calls["sendPKIReplyReliable"]; got != 3 {
		t.Errorf("FleetNodeHandler has %d sendPKIReplyReliable calls, want 3 (otp_success, otp_failure, guard-refusal)", got)
	}
	if calls["sendFlagReveal"] == 0 {
		t.Error("FleetNodeHandler no longer routes the flag reveal through sendFlagReveal")
	}
	if got := calls["sendPKIReply"]; got != 0 {
		t.Errorf("FleetNodeHandler still has %d bare sendPKIReply calls; one-shot replies must retry", got)
	}
}

// Every send inside the claim-link reveal (found-a-flag + link, and the
// mint-failure static fallback) is a one-shot reply and must retry reliably.
func TestFlagRevealUsesReliableRetry(t *testing.T) {
	fd, _ := funcBody(t, "sendFlagReveal")
	calls := calleeNames(fd)
	if got := calls["sendPKIReplyReliable"]; got != 3 {
		t.Errorf("sendFlagReveal has %d sendPKIReplyReliable calls, want 3 (fallback, found-a-flag, link)", got)
	}
	if got := calls["sendPKIReply"]; got != 0 {
		t.Errorf("sendFlagReveal has %d bare sendPKIReply calls; one-shot replies must retry", got)
	}
}

// Keying the dedup map by the ghost ('to') meant only the FIRST requester per
// fleet lifetime ever got lyrics. It must be keyed by the requester ('from').
func TestLyricsDedupKeyedByRequester(t *testing.T) {
	fd, _ := funcBody(t, "handleLyricsChat")

	var keys []string
	ast.Inspect(fd, func(n ast.Node) bool {
		idx, ok := n.(*ast.IndexExpr)
		if !ok {
			return true
		}
		// match n.LyricsResponded[toFleetIdx][<key>]
		inner, ok := idx.X.(*ast.IndexExpr)
		if !ok {
			return true
		}
		sel, ok := inner.X.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "LyricsResponded" {
			return true
		}
		if id, ok := idx.Index.(*ast.Ident); ok {
			keys = append(keys, id.Name)
		}
		return true
	})

	if len(keys) == 0 {
		t.Fatal("no LyricsResponded[fleet][key] accesses found in handleLyricsChat")
	}
	for _, k := range keys {
		if k != "from" {
			t.Errorf("LyricsResponded keyed by %q, want \"from\" (the requester, not the ghost)", k)
		}
	}
}
