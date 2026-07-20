package fleet

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
	"time"
)

// Only the explicit "off" disables acks. Empty (configs predating the knob)
// and every real style must keep acking.
func TestAckEnabled(t *testing.T) {
	cases := []struct {
		mode string
		want bool
	}{
		{"", true},
		{"faithful", true},
		{"legacy", true},
		{"off", false},
	}
	for _, c := range cases {
		if got := ackEnabled(c.mode); got != c.want {
			t.Errorf("ackEnabled(%q) = %v, want %v", c.mode, got, c.want)
		}
	}
}

// The A/B experiment depends on AckMode=off actually wiring NO ack handler.
// Guard that SetAckHandler stays behind the ackEnabled gate in cmd.go.
func TestSetAckHandlerIsGatedOnAckMode(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "cmd.go", nil, 0)
	if err != nil {
		t.Fatalf("parse cmd.go: %v", err)
	}

	gated := false
	found := false
	ast.Inspect(f, func(n ast.Node) bool {
		ifStmt, ok := n.(*ast.IfStmt)
		if !ok {
			return true
		}
		callsSet := false
		ast.Inspect(ifStmt.Body, func(m ast.Node) bool {
			if call, ok := m.(*ast.CallExpr); ok {
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "SetAckHandler" {
					callsSet = true
				}
			}
			return true
		})
		if !callsSet {
			return true
		}
		found = true
		ast.Inspect(ifStmt.Cond, func(m ast.Node) bool {
			if id, ok := m.(*ast.Ident); ok && id.Name == "ackEnabled" {
				gated = true
			}
			return true
		})
		return true
	})

	if !found {
		t.Fatal("no if-gated SetAckHandler call found in cmd.go")
	}
	if !gated {
		t.Error("SetAckHandler is wired without consulting ackEnabled(AckMode); AckMode=off would silently keep acking")
	}
}

// Ricky's lyric lines carry 1-based sequence numbers so the stream doubles as
// a live probe for delivery order and gaps.
func TestNumberLyric(t *testing.T) {
	if got := numberLyric(0, "never gonna give you up"); got != "01: never gonna give you up" {
		t.Errorf("numberLyric(0) = %q", got)
	}
	if got := numberLyric(11, "x"); got != "12: x" {
		t.Errorf("numberLyric(11) = %q", got)
	}
}

// Ghosts must answer directed user-info exchanges (NODEINFO_APP requests) --
// real firmware does, and a freshly-wiped radio depends on it to learn a
// ghost's details on demand.
func TestFleetNodeHandlerAnswersNodeInfoRequests(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "cmd.go", nil, 0)
	if err != nil {
		t.Fatalf("parse cmd.go: %v", err)
	}
	var fn *ast.FuncDecl
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == "FleetNodeHandler" {
			fn = fd
		}
	}
	if fn == nil {
		t.Fatal("FleetNodeHandler not found")
	}
	responds := false
	ast.Inspect(fn, func(m ast.Node) bool {
		if call, ok := m.(*ast.CallExpr); ok {
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "respondNodeInfo" {
				responds = true
			}
		}
		return true
	})
	if !responds {
		t.Error("FleetNodeHandler no longer answers NODEINFO_APP requests via respondNodeInfo")
	}
}

// Repeat lyric requests must get an encore after the cooldown -- and an ANSWER
// (the encore notice), never silence, while on cooldown. Once-per-lifetime
// blackholed every repeat request (acked, then nothing; observed 2026-07-20).
func TestLyricsCooldownNotOncePerLifetime(t *testing.T) {
	if lyricsEncoreCooldown <= 0 || lyricsEncoreCooldown > 30*time.Minute {
		t.Errorf("lyricsEncoreCooldown = %v; want a bounded cooldown, not once-per-lifetime", lyricsEncoreCooldown)
	}
	fd, _ := funcBody(t, "handleLyricsChat")
	calls := calleeNames(fd)
	if calls["sendPKIReply"] < 2 {
		t.Error("handleLyricsChat has fewer than 2 sendPKIReply sites; the cooldown branch must answer with an encore notice, not silence")
	}
}
