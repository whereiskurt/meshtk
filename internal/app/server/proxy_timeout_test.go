package server

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
	"time"
)

// The live failure: a hardcoded 10s client read deadline hung up on any radio
// that was merely idle. An MQTT client is allowed to say nothing until its
// keepalive expires, so the deadline must follow the keepalive, never a
// constant shorter than one.
func TestProxyReadTimeoutFollowsKeepalive(t *testing.T) {
	cases := []struct {
		keepalive uint16
		want      time.Duration
	}{
		{60, 90 * time.Second},   // typical mobile client: 1.5x
		{120, 180 * time.Second}, // 1.5x
		{300, 450 * time.Second}, // 1.5x
	}
	for _, c := range cases {
		if got := proxyReadTimeout(c.keepalive); got != c.want {
			t.Errorf("keepalive %ds -> %v, want %v", c.keepalive, got, c.want)
		}
	}
}

// A client negotiating a very small keepalive must not be able to recreate the
// teardown loop: the floor wins.
func TestProxyReadTimeoutHasFloor(t *testing.T) {
	for _, ka := range []uint16{1, 5, 10, 30} {
		got := proxyReadTimeout(ka)
		if got < minProxyReadTimeout {
			t.Errorf("keepalive %ds -> %v, below floor %v", ka, got, minProxyReadTimeout)
		}
	}
}

// Keepalive 0 means "no keepalive" in MQTT; fall back to the generous default
// rather than to something that disconnects a healthy client.
func TestProxyReadTimeoutZeroKeepaliveUsesDefault(t *testing.T) {
	if got := proxyReadTimeout(0); got != defaultProxyReadTimeout {
		t.Errorf("keepalive 0 -> %v, want %v", got, defaultProxyReadTimeout)
	}
}

// Every timeout this function can return must exceed the 10s that caused the
// outage, and must comfortably clear a 60s mobile keepalive.
func TestProxyReadTimeoutNeverTooShortForAnIdleClient(t *testing.T) {
	for ka := 0; ka <= 600; ka += 5 {
		got := proxyReadTimeout(uint16(ka))
		if got <= 10*time.Second {
			t.Fatalf("keepalive %ds -> %v: back to hanging up on idle clients", ka, got)
		}
		if ka > 0 && got < time.Duration(ka)*time.Second {
			t.Errorf("keepalive %ds -> %v: shorter than the keepalive itself", ka, got)
		}
	}
}

// handleBackend reads from the BACKEND, so that is the socket whose deadline it
// may set. Deadlining the client socket there both left backend reads unbounded
// and raced the uplink loop on the same socket.
func TestHandleBackendDeadlinesTheBackendSocket(t *testing.T) {
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

	var receivers []string
	ast.Inspect(fn, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "SetReadDeadline" {
			return true
		}
		if id, ok := sel.X.(*ast.Ident); ok {
			receivers = append(receivers, id.Name)
		}
		return true
	})

	for _, r := range receivers {
		if r == "conn" {
			t.Error("handleBackend sets a read deadline on `conn` (the client socket); it must deadline `backendConn`, the socket it reads")
		}
	}
}

// The uplink loop must derive its deadline from the CONNECT keepalive rather
// than a literal. Guards against someone reinstating a constant.
func TestHandleProxyUsesKeepaliveDerivedTimeout(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "proxy.go", nil, 0)
	if err != nil {
		t.Fatalf("parse proxy.go: %v", err)
	}
	var fn *ast.FuncDecl
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == "handleProxy" {
			fn = fd
		}
	}
	if fn == nil {
		t.Fatal("handleProxy not found")
	}

	callsProxyReadTimeout := false
	ast.Inspect(fn, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "proxyReadTimeout" {
				callsProxyReadTimeout = true
			}
		}
		return true
	})
	if !callsProxyReadTimeout {
		t.Error("handleProxy no longer derives its read deadline from the client's keepalive")
	}
}
