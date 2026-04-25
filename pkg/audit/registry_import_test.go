package audit

// registry_import_test.go pins the spi-p6unf3 architectural boundary:
// no production code under pkg/, cmd/, or operator/ may import the
// removed pkg/registry. The wizardregistry.Registry interface is the
// only sanctioned wizard-tracking surface across local and cluster
// modes. A new import that brings pkg/registry back would re-fork the
// liveness contract.

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// TestPkgRegistryRemoved walks pkg/, cmd/, operator/ and fails if any
// .go file imports the deleted pkg/registry package. _test.go files are
// included in the scan because a test bringing back the legacy import
// would resurrect the same architectural fork.
func TestPkgRegistryRemoved(t *testing.T) {
	const forbidden = `"github.com/awell-health/spire/pkg/registry"`
	root := repoRoot(t)
	roots := []string{
		filepath.Join(root, "pkg"),
		filepath.Join(root, "cmd"),
		filepath.Join(root, "operator"),
	}
	fset := token.NewFileSet()
	violations := 0
	for _, r := range roots {
		_ = filepath.WalkDir(r, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				return nil
			}
			if !strings.HasSuffix(path, ".go") {
				return nil
			}
			f, perr := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
			if perr != nil {
				return nil
			}
			for _, imp := range f.Imports {
				if imp.Path.Value == forbidden {
					t.Errorf("forbidden import in %s: %s — pkg/registry was removed by spi-p6unf3; use pkg/wizardregistry instead", path, imp.Path.Value)
					violations++
				}
			}
			return nil
		})
	}
	if violations == 0 {
		t.Logf("scan clean: no imports of pkg/registry under pkg/, cmd/, or operator/")
	}
}
