package ctrl

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// docs/CONTROLLER.md: at most 48 tunables, all in constants.go. Every
// constant counts, typed or not, except the named enumerations and Stub.
//
// The C2 checkpoint (GPT, 2026-11-08: "Constants: APPROVE an explicit budget
// of 48") approved the eight A1 additions to the r2 budget of 40:
// KeySizeSamples, KeySizeAge, KeySizeQuantile, KeyRefusedMin, KeyRefusedMax
// (orchestrator decision on the C0 r3 key-admission deadlock), ProbeHeadroom
// (A18/A1: an offer at exactly the target measures target − ε), MeterInterval
// (ingress buckets moved from the simulator into ctrl), KeyFast (A1/A8
// switch rung). C2 item 6b replaced MeterInterval with CostCycles (ingress
// costs over complete temporal cycles), pending checkpoint review.
func TestConstantBudget(t *testing.T) {
	enums := map[string]bool{"Rendition": true, "Kind": true, "Class": true, "Cause": true, "KeyStatus": true, "KeyPhase": true, "ProbeClass": true, "SwitchOutcome": true, "AllocationIntent": true}
	files, e := filepath.Glob("*.go")
	if e != nil {
		t.Fatal(e)
	}
	n := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, e := parser.ParseFile(token.NewFileSet(), name, nil, 0)
		if e != nil {
			t.Fatal(e)
		}
		for _, d := range f.Decls {
			g, ok := d.(*ast.GenDecl)
			if !ok || g.Tok != token.CONST {
				continue
			}
			enum := false
			for _, s := range g.Specs {
				v := s.(*ast.ValueSpec)
				if id, ok := v.Type.(*ast.Ident); ok {
					enum = enums[id.Name] // an iota block keeps the type of its first spec
				} else if v.Type != nil {
					enum = false
				}
				for _, id := range v.Names {
					if enum || id.Name == "Stub" {
						continue
					}
					if name != "constants.go" {
						t.Errorf("tunable %s declared in %s, not constants.go", id.Name, name)
					}
					n++
				}
			}
		}
	}
	const budget = 48
	t.Logf("%d tunables (budget %d)", n, budget)
	if n > budget {
		t.Fatalf("%d tunables, budget %d", n, budget)
	}
}

// docs/CONTROLLER.md: non-test internal/sfu (including ctrl) ≤3,000 lines is
// a C2 review target, not a hard gate; this reports it.
func TestSizeBudget(t *testing.T) {
	lines := 0
	filepath.WalkDir("..", func(path string, d os.DirEntry, e error) error {
		if e != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return e
		}
		b, e := os.ReadFile(path)
		lines += strings.Count(string(b), "\n")
		return e
	})
	t.Logf("non-test internal/sfu: %d lines (C2 review target 3000)", lines)
}
