package web

// The event-wire structural guard (C9b, the B2-7a lesson): every
// model.Event implementation must either have an encodeEventFrame
// case or be EXPLICITLY listed as not-on-the-wire with a reason.
// encodeEventFrame's default branch silently drops unknown event
// types — a missing case is invisible to every behavior test (that
// is exactly how B2 7a shipped the approval events unwired), so the
// guard reads the SOURCE with go/ast instead: it enumerates the
// model package's Type() implementations and the wire's switch
// cases, and fails on any drift in either direction.
//
// Why source parsing over a model-package registry: the wire is the
// web package's concern — a registry in model would make production
// code carry a test concern (and the registry itself could drift).
// The source-path coupling is deliberate and fails loudly if a file
// moves.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"testing"
)

// notOnWire lists events deliberately absent from the SSE stream,
// each with its reason. Entries must name real events (a stale entry
// fails the guard too). Currently empty: after C6c every event ships.
var notOnWire = map[string]string{}

// eventImplementations parses EVERY .go file of the model package
// (a single-file scope would silently miss an event moved into a new
// file) and returns the type names that have a `func (T) Type()
// EventType` method — the model.Event implementations.
func eventImplementations(t *testing.T, dir string) map[string]bool {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil || len(files) == 0 {
		t.Fatalf("glob %s: %v (the package moved?)", dir, err)
	}
	events := map[string]bool{}
	for _, path := range files {
		collectEventTypes(t, path, events)
	}
	return events
}

func collectEventTypes(t *testing.T, path string, events map[string]bool) {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "Type" || fn.Recv == nil || len(fn.Recv.List) != 1 {
			continue
		}
		star, isStar := fn.Recv.List[0].Type.(*ast.StarExpr)
		var recvType ast.Expr = fn.Recv.List[0].Type
		if isStar {
			recvType = star.X
		}
		ident, ok := recvType.(*ast.Ident)
		if !ok {
			continue
		}
		if fn.Type.Results != nil && len(fn.Type.Results.List) == 1 {
			ret := fn.Type.Results.List[0].Type
			// EventType is unqualified inside the model package itself.
			if ident2, ok := ret.(*ast.Ident); ok && ident2.Name == "EventType" {
				events[ident.Name] = true
			}
		}
	}
}

// wiredEventCases parses the wire source and returns the type names
// appearing in encodeEventFrame's `case model.X:` arms.
func wiredEventCases(t *testing.T, path string) map[string]bool {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	cases := map[string]bool{}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "encodeEventFrame" || fn.Body == nil {
			continue
		}
		// The `switch event := event.(type)` inside: every CaseClause
		// of a type switch carries the case types in its List.
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			clause, ok := n.(*ast.CaseClause)
			if !ok {
				return true
			}
			for _, expr := range clause.List {
				if sel, ok := expr.(*ast.SelectorExpr); ok {
					if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "model" {
						cases[sel.Sel.Name] = true
					}
				}
			}
			return true
		})
	}
	return cases
}

func TestEventWireCoverage(t *testing.T) {
	modelDir := filepath.Join("..", "..", "model")
	events := eventImplementations(t, modelDir)
	if len(events) < 20 {
		t.Fatalf("found only %d event implementations in %s — the source moved?", len(events), modelDir)
	}
	wired := wiredEventCases(t, "wire.go")
	if len(wired) < 20 {
		t.Fatalf("found only %d encodeEventFrame cases — the source moved?", len(wired))
	}

	// Every event is wired or explicitly off the wire.
	for name := range events {
		if !wired[name] {
			if _, listed := notOnWire[name]; !listed {
				t.Errorf("%s implements model.Event but has no encodeEventFrame case and is not in the notOnWire list — the default branch would silently drop it", name)
			}
		}
	}
	// The notOnWire list stays honest: entries must name real events.
	for name := range notOnWire {
		if !events[name] {
			t.Errorf("notOnWire lists %s, which is not a model.Event implementation", name)
		}
	}
	// And the wire maps only real events (a case for a renamed type
	// would be a compile error, but the guard keeps the invariant
	// local and readable).
	for name := range wired {
		if !events[name] {
			t.Errorf("encodeEventFrame maps %s, which is not a model.Event implementation", name)
		}
	}
}
