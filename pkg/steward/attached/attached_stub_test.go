package attached

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/awell-health/spire/pkg/steward/intent"
)

// TestAttachedDispatch_ReturnsNotImplemented covers a spread of inputs —
// zero-value, fully-populated, and a populated-with-different-phase
// variant — to pin the invariant: no matter what the caller passes, the
// stub returns ErrAttachedNotImplemented. If a future change silently
// allows some inputs to succeed, this test fails.
func TestAttachedDispatch_ReturnsNotImplemented(t *testing.T) {
	cases := []struct {
		name string
		in   intent.WorkloadIntent
	}{
		{
			name: "zero value",
			in:   intent.WorkloadIntent{},
		},
		{
			name: "populated implement phase",
			in: intent.WorkloadIntent{
				TaskID:      "spi-4tfdo",
				DispatchSeq: 1,
				RepoIdentity: intent.RepoIdentity{
					URL:        "https://example.com/repo.git",
					BaseBranch: "main",
					Prefix:     "spi",
				},
				FormulaPhase: "implement",
				Resources: intent.Resources{
					CPURequest:    "500m",
					CPULimit:      "1000m",
					MemoryRequest: "256Mi",
					MemoryLimit:   "1Gi",
				},
				HandoffMode: "bundle",
			},
		},
		{
			name: "populated review phase",
			in: intent.WorkloadIntent{
				TaskID:       "spi-other",
				DispatchSeq:  2,
				RepoIdentity: intent.RepoIdentity{
					URL:        "https://example.com/other.git",
					BaseBranch: "trunk",
					Prefix:     "oth",
				},
				FormulaPhase: "review",
				HandoffMode:  "transitional",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := AttachedDispatch(context.Background(), tc.in)
			if err == nil {
				t.Fatalf("AttachedDispatch(%q) returned nil, want ErrAttachedNotImplemented", tc.name)
			}
			if !errors.Is(err, ErrAttachedNotImplemented) {
				t.Fatalf("AttachedDispatch(%q) = %v, want ErrAttachedNotImplemented", tc.name, err)
			}
		})
	}
}

// TestErrAttachedNotImplemented_MessagePointsAtDocs guards the operator
// experience: the error message must reference docs/attached-mode.md so
// an operator who observes the error at runtime can find the reservation
// document without grepping the source tree.
func TestErrAttachedNotImplemented_MessagePointsAtDocs(t *testing.T) {
	msg := ErrAttachedNotImplemented.Error()
	if !strings.Contains(msg, "docs/attached-mode.md") {
		t.Errorf("ErrAttachedNotImplemented message %q should reference docs/attached-mode.md", msg)
	}
	if !strings.Contains(msg, "not implemented") {
		t.Errorf("ErrAttachedNotImplemented message %q should say 'not implemented'", msg)
	}
}

// TestPackage_ExportsOnlyStubSymbols parses the non-test .go files in
// this package and asserts that the only exported top-level identifiers
// are AttachedDispatch and ErrAttachedNotImplemented. This guards the
// reservation: no new exports may be added to pkg/steward/attached
// before attached mode has been designed end-to-end. If attached mode
// graduates from reserved to implemented, the stub should be replaced
// outright — not extended — and this test should be rewritten to pin
// the real shape.
func TestPackage_ExportsOnlyStubSymbols(t *testing.T) {
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("abs of .: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir %q: %v", dir, err)
	}

	fset := token.NewFileSet()
	var exported []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join(dir, name)
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %q: %v", path, err)
		}
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if d.Recv != nil {
					// Methods on unexported types can't be exported;
					// we have no exported types here, so surface any
					// method as a drift signal.
					if d.Name.IsExported() {
						exported = append(exported, "method:"+d.Name.Name)
					}
					continue
				}
				if d.Name.IsExported() {
					exported = append(exported, d.Name.Name)
				}
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					switch s := spec.(type) {
					case *ast.ValueSpec:
						for _, ident := range s.Names {
							if ident.IsExported() {
								exported = append(exported, ident.Name)
							}
						}
					case *ast.TypeSpec:
						if s.Name.IsExported() {
							exported = append(exported, s.Name.Name)
						}
					}
				}
			}
		}
	}
	sort.Strings(exported)

	want := []string{"AttachedDispatch", "ErrAttachedNotImplemented"}
	sort.Strings(want)

	if !reflect.DeepEqual(exported, want) {
		t.Fatalf("pkg/steward/attached exports drifted: got %v, want %v — if you are adding real attached-mode code, replace the stub rather than extending it", exported, want)
	}
}
