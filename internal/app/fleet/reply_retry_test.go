package fleet

import (
	"go/ast"
	"go/parser"
	"go/token"
	"sync"
	"testing"
	"time"
)

// One-shot chatbot replies must be re-sent 3x, ~30s apart, to survive the ~60s
// iOS proxy reconnect gap (mosquitto: persistence false + QoS0 = no queue).
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

	sendSpread(pkiReplyRetryCount, pkiReplyRetrySpacing, sleep, send)

	for i := 0; i < pkiReplyRetryCount; i++ {
		select {
		case <-sends:
		case <-time.After(2 * time.Second):
			t.Fatalf("only got %d of %d sends", i, pkiReplyRetryCount)
		}
	}
	select {
	case <-sends:
		t.Fatal("got more sends than pkiReplyRetryCount")
	case <-time.After(50 * time.Millisecond):
	}

	mu.Lock()
	defer mu.Unlock()
	if len(sleeps) != pkiReplyRetryCount-1 {
		t.Fatalf("sleeps = %d, want %d", len(sleeps), pkiReplyRetryCount-1)
	}
	for i, d := range sleeps {
		if d != pkiReplyRetrySpacing {
			t.Errorf("sleep[%d] = %v, want %v", i, d, pkiReplyRetrySpacing)
		}
	}
}

// The retry constants are the tuned contract: 3 sends over ~90s covers ~1.5
// flapping cycles at the measured 60s median reconnect interval.
func TestPKIReplyRetryConstants(t *testing.T) {
	if pkiReplyRetryCount != 3 {
		t.Errorf("pkiReplyRetryCount = %d, want 3", pkiReplyRetryCount)
	}
	if pkiReplyRetrySpacing != 30*time.Second {
		t.Errorf("pkiReplyRetrySpacing = %v, want 30s", pkiReplyRetrySpacing)
	}
	total := pkiReplyRetrySpacing * time.Duration(pkiReplyRetryCount-1)
	if total != 60*time.Second {
		t.Errorf("retry spread = %v, want 60s", total)
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
	f, err := parser.ParseFile(fset, "cmd.go", nil, 0)
	if err != nil {
		t.Fatalf("parse cmd.go: %v", err)
	}
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == name {
			return fd, fset
		}
	}
	t.Fatalf("func %s not found in cmd.go", name)
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
// otp_failure (non-unlocking branch) and the "OpenAI key not configured"
// fallback. Every one of them must go through the reliable wrapper.
func TestOneShotReplyPathsUseReliableRetry(t *testing.T) {
	fd, _ := funcBody(t, "FleetNodeHandler")
	calls := calleeNames(fd)
	if got := calls["sendPKIReplyReliable"]; got != 3 {
		t.Errorf("FleetNodeHandler has %d sendPKIReplyReliable calls, want 3 (otp_success, otp_failure, openai-fallback)", got)
	}
	if got := calls["sendPKIReply"]; got != 0 {
		t.Errorf("FleetNodeHandler still has %d bare sendPKIReply calls; one-shot replies must retry", got)
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
