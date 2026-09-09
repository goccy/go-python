package internal

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"testing"
)

// TestMethodIDsMatchGeneratedBinding pins the hand-maintained method-id
// table (the mid* constants instance.go dispatches with) to the generated
// binding in python.go. The proto numbers the service's methods
// alphabetically over the py.h exports, so ONE added export renumbers every
// later id; this test reads the generated per-export wrappers — each calls
// exactly one wasm2go.Inv_0_<id> — and fails when the two disagree, instead
// of letting a stale table dispatch to the wrong export at run time.
func TestMethodIDsMatchGeneratedBinding(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "python.go", nil, 0)
	if err != nil {
		t.Fatalf("parse generated python.go: %v", err)
	}

	// invokerOf returns the N of the single wasm2go.Inv_0_N selector a
	// generated wrapper references, or -1 when it references none.
	invokerOf := func(fn *ast.FuncDecl) int {
		id := -1
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok || pkg.Name != "wasm2go" || !strings.HasPrefix(sel.Sel.Name, "Inv_0_") {
				return true
			}
			n64, perr := strconv.Atoi(strings.TrimPrefix(sel.Sel.Name, "Inv_0_"))
			if perr != nil {
				t.Fatalf("%s: unparsable invoker %s", fn.Name.Name, sel.Sel.Name)
			}
			if id != -1 && id != n64 {
				t.Fatalf("%s: references two invokers (%d and %d)", fn.Name.Name, id, n64)
			}
			id = n64
			return true
		})
		return id
	}

	seen := map[string]bool{}
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv != nil || !strings.HasPrefix(fn.Name.Name, "Py") {
			continue
		}
		id := invokerOf(fn)
		if id == -1 {
			continue // not a per-export wrapper
		}
		want, known := generatedMethodIDs[fn.Name.Name]
		if !known {
			t.Errorf("generated wrapper %s (Inv_0_%d) has no entry in generatedMethodIDs: add the export to the table", fn.Name.Name, id)
			continue
		}
		if int32(id) != want {
			t.Errorf("%s: generated binding dispatches to Inv_0_%d, table says %d", fn.Name.Name, id, want)
		}
		seen[fn.Name.Name] = true
	}
	for name := range generatedMethodIDs {
		if !seen[name] {
			t.Errorf("table entry %s has no generated wrapper in python.go", name)
		}
	}
}
