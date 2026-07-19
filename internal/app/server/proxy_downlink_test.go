package server

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// handleBackend was the proxy's one blind spot: every uplink packet is logged,
// but downlink logged nothing, so a published reply/ACK that never reached the
// device was indistinguishable from one that did. The downlink loop must keep
// feeding PUBLISH packets through logDownlink.
func TestHandleBackendLogsDownlinkPublishes(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "proxy.go", nil, 0)
	if err != nil {
		t.Fatalf("parse proxy.go: %v", err)
	}
	var fn *ast.FuncDecl
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == "handleBackend" {
			fn = fd
		}
	}
	if fn == nil {
		t.Fatal("handleBackend not found")
	}

	callsLogDownlink := false
	ast.Inspect(fn, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "logDownlink" {
				callsLogDownlink = true
			}
		}
		return true
	})
	if !callsLogDownlink {
		t.Error("handleBackend no longer calls logDownlink; the downlink path is a blind spot again")
	}
}
