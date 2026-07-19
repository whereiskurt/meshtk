package fleet

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// Default (0 or 1) must not change behaviour: every node, every tic.
func TestMovementDueDefaultIsEveryTic(t *testing.T) {
	for _, every := range []int{0, 1} {
		for tic := 0; tic < 10; tic++ {
			if !movementDue(every, tic, 0x435990e4) {
				t.Fatalf("every=%d tic=%d: want due", every, tic)
			}
		}
	}
}

// A node publishes position exactly once per `every` tics -- the thinning ratio
// is what frees channel bandwidth for NodeInfo.
func TestMovementDueRatio(t *testing.T) {
	const tics = 300
	for _, every := range []int{2, 3, 4, 6} {
		for _, node := range []uint32{0x435990e4, 0x1555f041, 1, 2, 3} {
			due := 0
			for tic := 0; tic < tics; tic++ {
				if movementDue(every, tic, node) {
					due++
				}
			}
			if want := tics / every; due != want {
				t.Errorf("every=%d node=%08x: due %d times in %d tics, want %d",
					every, node, due, tics, want)
			}
		}
	}
}

// Nodes must stagger across tics rather than all firing on the same one --
// otherwise thinning just converts a steady trickle into a periodic burst,
// which is the drowning failure mode we are trying to avoid.
func TestMovementDueStaggersNodes(t *testing.T) {
	const every = 4
	nodes := []uint32{100, 101, 102, 103}
	hit := map[int]int{}
	for _, n := range nodes {
		for tic := 0; tic < every; tic++ {
			if movementDue(every, tic, n) {
				hit[tic]++
			}
		}
	}
	if len(hit) != every {
		t.Errorf("nodes fired on %d distinct tics, want %d (all bunched on one tic)", len(hit), every)
	}
	for tic, c := range hit {
		if c != 1 {
			t.Errorf("tic %d had %d nodes fire, want 1", tic, c)
		}
	}
}

// NodeInfo cadence must be untouched: movementDue gates only the movement /
// position branch, never the nodeinfo publish. Guarded structurally in
// behaviours.go -- see TestNodeInfoNotGatedByMovementThrottle.
func TestMovementDueNeverBlocksEveryNode(t *testing.T) {
	const every = 3
	for _, n := range []uint32{7, 8, 9} {
		anyDue := false
		for tic := 0; tic < 3*every; tic++ {
			if movementDue(every, tic, n) {
				anyDue = true
			}
		}
		if !anyDue {
			t.Errorf("node %d never publishes position at every=%d", n, every)
		}
	}
}

// The throttle must gate ONLY movement/position. If publishNodeInfo ever ends
// up inside the movementDue branch, a reconnecting radio would learn the fleet
// more slowly -- the exact opposite of what this change is for.
func TestNodeInfoNotGatedByMovementThrottle(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "behaviours.go", nil, 0)
	if err != nil {
		t.Fatalf("parse behaviours.go: %v", err)
	}
	var fn *ast.FuncDecl
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == "behaviours" {
			fn = fd
		}
	}
	if fn == nil {
		t.Fatal("behaviours() not found")
	}

	var gated []string
	ast.Inspect(fn, func(n ast.Node) bool {
		ifs, ok := n.(*ast.IfStmt)
		if !ok {
			return true
		}
		call, ok := ifs.Cond.(*ast.CallExpr)
		if !ok {
			return true
		}
		id, ok := call.Fun.(*ast.Ident)
		if !ok || id.Name != "movementDue" {
			return true
		}
		ast.Inspect(ifs.Body, func(m ast.Node) bool {
			if c, ok := m.(*ast.CallExpr); ok {
				if sel, ok := c.Fun.(*ast.SelectorExpr); ok {
					gated = append(gated, sel.Sel.Name)
				}
			}
			return true
		})
		return true
	})

	if len(gated) == 0 {
		t.Fatal("no movementDue-gated block found in behaviours()")
	}
	for _, name := range gated {
		if name == "publishNodeInfo" {
			t.Error("publishNodeInfo is inside the movementDue gate; NodeInfo cadence must not be throttled")
		}
	}
}
