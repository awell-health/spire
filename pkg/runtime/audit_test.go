package runtime_test

// This test file enforces the "no ambient CWD for identity" invariant
// required by docs/design/spi-xplwy-runtime-contract.md §1.1. It scans
// the source of every runtime-critical package and fails on any
// os.Getwd() call that is not covered by an explicit allowlist entry.
//
// Runtime-critical means: reachable during a worker run. That is
// pkg/executor, pkg/wizard, pkg/apprentice, and pkg/agent.
//
// Allowlisted uses fall into two categories:
//
//  1. Reading per-repo config (spire.yaml) from the CWD. This is
//     legitimate CLI UX and does not cross the runtime-identity
//     boundary — a worker's spire.yaml tells it how to behave, not
//     which tower/prefix it belongs to.
//
//  2. Resolving mount sources and scratch paths in the Docker backend
//     where the spawn caller's CWD is genuinely the source directory.
//     These are not identity derivations; they are path plumbing.
//
// Any new os.Getwd() call in these packages must either:
//   - be rejected (derive identity from RepoIdentity instead), or
//   - be added to the allowlist below with a clear justification.
//
// See docs/CLI-MIGRATION.md for the human-facing rationale.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// runtimeCriticalPackages are the package directories scanned by this
// audit. Paths are relative to the repo root.
var runtimeCriticalPackages = []string{
	"pkg/executor",
	"pkg/wizard",
	"pkg/apprentice",
	"pkg/agent",
}

// allowedGetwdSites lists file-line prefixes we accept. Entries are
// matched as "<relpath>:<line>". Any os.Getwd hit NOT covered here
// fails the test. Keep this list short and well-justified.
//
// Format: "<repo-relative-path>:<line number>": "<justification>".
var allowedGetwdSites = map[string]string{
	// pkg/agent: backend selection reads spire.yaml from CWD to pick
	// the backend kind (process/docker/k8s). Not an identity derivation
	// — it's per-repo config that happens to live where the user runs
	// the command.
	"pkg/agent/backend.go": "repoconfig.Load(cwd) for backend selection only — not identity",
	// pkg/agent: docker backend uses cwd as the mount source / scratch
	// base. These are path plumbing for the host <-> container
	// mapping, not tower/prefix identity.
	"pkg/agent/spawn_docker.go": "docker mount path plumbing, not identity derivation",
	// pkg/apprentice: test file that chdir's to set up a worktree and
	// needs to restore the original cwd.
	"pkg/apprentice/submit_test.go": "test-only: save/restore cwd around chdir",
	// pkg/agent: backend resolution tests chdir to simulate the
	// operator-pod scenario (WorkingDir above the clone, spi-vrzhf) and
	// need to restore the original cwd.
	"pkg/agent/backend_test.go": "test-only: save/restore cwd around chdir in spi-vrzhf regression test",
}

// violation describes one flagged call site.
type violation struct {
	File string // repo-relative, forward-slash
	Line int
	Call string // e.g. "os.Getwd" or "dolt.ReadBeadsDBName"
}

func (v violation) String() string {
	return v.File + ":" + itoa(v.Line) + "  " + v.Call + "(...)"
}

// TestNoAmbientCWDForIdentity fails when any runtime-critical package
// calls os.Getwd outside the allowlist. This is the chunk-3 regression
// guard for docs/design/spi-xplwy-runtime-contract.md §1.1.
func TestNoAmbientCWDForIdentity(t *testing.T) {
	repoRoot, err := findRepoRoot()
	if err != nil {
		t.Fatalf("find repo root: %v", err)
	}

	violations := scanRuntimeCalls(t, repoRoot, "os", "Getwd", allowedGetwdSites)

	if len(violations) > 0 {
		lines := make([]string, 0, len(violations))
		for _, v := range violations {
			lines = append(lines, v.String())
		}
		t.Fatalf(
			"os.Getwd() in runtime-critical package(s) — runtime code must derive identity from RepoIdentity, not CWD.\n"+
				"See docs/design/spi-xplwy-runtime-contract.md §1.1.\n"+
				"If the call is legitimate (not identity-related), add the file path to allowedGetwdSites in pkg/runtime/audit_test.go with a one-line justification.\n"+
				"Violations:\n  %s",
			strings.Join(lines, "\n  "),
		)
	}
}

// TestNoReadBeadsDBNameInRuntime fails when pkg/executor / pkg/wizard /
// pkg/apprentice / pkg/agent calls dolt.ReadBeadsDBName, which was the
// original ambient-CWD ingress point. Other packages (CLI helpers,
// local tooling) may legitimately use it.
func TestNoReadBeadsDBNameInRuntime(t *testing.T) {
	repoRoot, err := findRepoRoot()
	if err != nil {
		t.Fatalf("find repo root: %v", err)
	}
	violations := scanRuntimeCalls(t, repoRoot, "dolt", "ReadBeadsDBName", nil)
	if len(violations) > 0 {
		lines := make([]string, 0, len(violations))
		for _, v := range violations {
			lines = append(lines, v.String())
		}
		t.Fatalf(
			"dolt.ReadBeadsDBName in runtime-critical package(s) — this was the chunk-3 ambient-CWD ingress and is removed.\n"+
				"Identity must come from RepoIdentity.TowerName instead.\n"+
				"Violations:\n  %s",
			strings.Join(lines, "\n  "),
		)
	}
}

// scanRuntimeCalls parses every .go file under the runtime-critical
// package roots and reports any call to pkgName.funcName that is not
// covered by the allowlist (matched by repo-relative file path). It
// uses go/parser so comments and string literals are never flagged.
//
// allowlist may be nil to mean "no sites allowed".
func scanRuntimeCalls(t *testing.T, repoRoot, pkgName, funcName string, allowlist map[string]string) []violation {
	t.Helper()
	var hits []violation
	for _, pkg := range runtimeCriticalPackages {
		pkgDir := filepath.Join(repoRoot, pkg)
		err := filepath.Walk(pkgDir, func(path string, info os.FileInfo, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if info.IsDir() {
				return nil
			}
			if !strings.HasSuffix(path, ".go") {
				return nil
			}
			rel, relErr := filepath.Rel(repoRoot, path)
			if relErr != nil {
				rel = path
			}
			rel = filepath.ToSlash(rel)
			if _, ok := allowlist[rel]; ok {
				return nil
			}
			fset := token.NewFileSet()
			// parser.SkipObjectResolution keeps this fast and avoids
			// resolving imported-package identifiers — we only care
			// about the syntactic SelectorExpr name shape.
			file, parseErr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			if parseErr != nil {
				// Parse errors would already have broken the build;
				// just skip this file.
				return nil
			}
			ast.Inspect(file, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				ident, ok := sel.X.(*ast.Ident)
				if !ok {
					return true
				}
				if ident.Name == pkgName && sel.Sel.Name == funcName {
					pos := fset.Position(call.Pos())
					hits = append(hits, violation{
						File: rel,
						Line: pos.Line,
						Call: pkgName + "." + funcName,
					})
				}
				return true
			})
			// Also catch the "passed-by-name" case: os.Getwd without
			// calling it (e.g. dolt.ReadBeadsDBName(os.Getwd)).
			ast.Inspect(file, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				// Skip if this selector is itself the target of a call
				// expression we already counted above. Simpler: recount
				// bare selectors and dedupe by line.
				ident, ok := sel.X.(*ast.Ident)
				if !ok {
					return true
				}
				if ident.Name != pkgName || sel.Sel.Name != funcName {
					return true
				}
				pos := fset.Position(sel.Pos())
				// Dedupe against the CallExpr pass.
				for _, h := range hits {
					if h.File == rel && h.Line == pos.Line {
						return true
					}
				}
				hits = append(hits, violation{
					File: rel,
					Line: pos.Line,
					Call: pkgName + "." + funcName,
				})
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", pkgDir, err)
		}
	}
	return hits
}

// findRepoRoot walks upward from the test's cwd (which is the package
// directory under test) looking for a go.mod file that declares the
// spire module.
func findRepoRoot() (string, error) {
	start, err := os.Getwd()
	if err != nil {
		return "", err
	}
	dir := start
	for {
		gomod := filepath.Join(dir, "go.mod")
		if data, err := os.ReadFile(gomod); err == nil {
			if strings.Contains(string(data), "module github.com/awell-health/spire") {
				return dir, nil
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", os.ErrNotExist
		}
		dir = parent
	}
}

// itoa is a tiny int-to-string helper to avoid pulling in strconv just
// for one format call.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := false
	if n < 0 {
		neg = true
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
