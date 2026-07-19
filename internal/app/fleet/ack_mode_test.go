package fleet

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
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
