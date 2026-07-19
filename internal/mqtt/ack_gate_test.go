package mqtt

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// The ack call sat behind a commented-out want_ack check for months, so the
// fleet acked every PKI packet it could decrypt -- including ones that never
// asked. Firmware only acks want_ack packets; a real radio's DM always sets it
// (that flag is what drives its retransmits), so the gate costs nothing and
// keeps us protocol-faithful. Guard against the gate being commented out again.
func TestAckHandlerIsGatedOnWantAck(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "mqtt.go", nil, 0)
	if err != nil {
		t.Fatalf("parse mqtt.go: %v", err)
	}

	// Find every call through c.ackHandler and check the enclosing if
	// condition mentions GetWantAck.
	gated := false
	found := false
	ast.Inspect(f, func(n ast.Node) bool {
		ifStmt, ok := n.(*ast.IfStmt)
		if !ok {
			return true
		}
		callsAck := false
		ast.Inspect(ifStmt.Body, func(m ast.Node) bool {
			if call, ok := m.(*ast.CallExpr); ok {
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "ackHandler" {
					callsAck = true
				}
			}
			return true
		})
		if !callsAck {
			return true
		}
		found = true
		ast.Inspect(ifStmt.Cond, func(m ast.Node) bool {
			if sel, ok := m.(*ast.SelectorExpr); ok && sel.Sel.Name == "GetWantAck" {
				gated = true
			}
			return true
		})
		return true
	})

	if !found {
		t.Fatal("no if-gated ackHandler call found in mqtt.go")
	}
	if !gated {
		t.Error("ackHandler fires without checking packet.GetWantAck(); we would ack packets that never requested an ack")
	}
}
