// Static-AST invariant for the cluster-native dispatch boundary.
//
// In cluster-native deployments the operator owns child-pod dispatch:
// no scheduling code path may call pkg/agent.Spawner.Spawn or
// backend.Spawn. See docs/VISION-CLUSTER.md → "Operator-owned dispatch
// (cluster-native invariant)" for the contract this test enforces.
//
// The test walks Go source for the dispatch packages (pkg/executor,
// pkg/wizard, pkg/steward) and flags any call expression whose method
// selector resolves to "Spawn". An explicit per-file allowlist names
// the local-native files where Spawn calls are intentional. Adding a
// new Spawn site without amending the allowlist fails the test —
// the allowlist is the review surface.
//
// Today's allowlist intentionally covers every call site that exists
// in this snapshot of main, including those still slated for migration
// onto the operator seam under spi-5bzu9r.2 (executor child dispatch +
// wizard review-fix re-entry) and spi-5bzu9r.3 (steward review /
// cleric dispatch). Each entry on the allowlist names the bead that
// will remove it; the entry should be deleted in the same PR as the
// migration. The allowlist shrinking, not the absence of a check, is
// the visible signal that those migrations have landed.
//
// The test is intentionally pessimistic: any unrecognized Spawn call
// site is flagged. False positives can be silenced by adding the file
// to the allowlist with a one-line justification.

package executor

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// allowedSpawnFile records a file path (relative to the module root)
// that is allowed to contain Spawn call expressions, plus a one-line
// reason. New Spawn call sites in unlisted files fail the invariant
// test — that is the boundary this allowlist guards.
type allowedSpawnFile struct {
	// Path is the file path relative to the module root, e.g.
	// "pkg/executor/graph_actions.go".
	Path string

	// Reason is a short justification readable in PR review. Bead IDs
	// reference the migration that will remove the entry.
	Reason string
}

// scannedDispatchPackages is the set of package directories the
// invariant scans, relative to the module root. Any call expression
// whose Sel.Name == "Spawn" found inside these directories must be
// covered by the allowlist below.
var scannedDispatchPackages = []string{
	"pkg/executor",
	"pkg/wizard",
	"pkg/steward",
}

// allowedSpawnCallSites is the explicit allowlist of files containing
// Spawn call expressions. Each entry is one local-native dispatch
// site. The list shrinks as migration tracks land:
//
//   - pkg/executor entries migrate under spi-5bzu9r.2 (executor child
//     dispatch onto the cluster intent seam).
//   - pkg/wizard/wizard_review.go migrates under spi-5bzu9r.2
//     (review-fix re-entry).
//   - pkg/steward entries are converged: cluster_dispatch.go's
//     dispatchPhase keeps a local-native fallback by design,
//     steward.go owns the local-native ready-work loop.
//
// Test-file Spawn calls (mocks, spies, factory invocations) are
// excluded by the scanner itself — see filepath.Ext check below.
var allowedSpawnCallSites = []allowedSpawnFile{
	// pkg/steward — converged.
	{
		Path:   "pkg/steward/steward.go",
		Reason: "local-native ready-work loop (TowerCycle); cluster path goes through dispatchClusterNative",
	},
	{
		Path:   "pkg/steward/cluster_dispatch.go",
		Reason: "dispatchPhase local-native branch + safety-valve fallback when ClusterDispatch is unwired (cluster-native helpers in this file emit intents only)",
	},

	// pkg/wizard — partially migrated.
	{
		Path:   "pkg/wizard/wizard.go",
		Reason: "local-native sage review spawn; cluster-native review emit happens in steward dispatchPhase (spi-agmsk5)",
	},
	{
		Path:   "pkg/wizard/wizard_review.go",
		Reason: "review-fix re-entry — migrating to operator seam under spi-5bzu9r.2",
	},

	// pkg/executor — pending migration under spi-5bzu9r.2 / .3.
	{
		Path:   "pkg/executor/graph_actions.go",
		Reason: "wizard.run action handler — pending migration under spi-5bzu9r.2",
	},
	{
		Path:   "pkg/executor/graph_interpreter.go",
		Reason: "single-bead executor spawn — pending migration under spi-5bzu9r.2",
	},
	{
		Path:   "pkg/executor/action_dispatch.go",
		Reason: "wave / sequential / direct child dispatch — pending migration under spi-5bzu9r.2",
	},
	// Cleric foundation (spi-h2d7yn) deleted pkg/executor/recovery_phase.go
	// and pkg/executor/recovery_actions_agentic.go along with the rest of
	// the inline-recovery cycle, so their entries are gone from this list.
	// The cluster-native invariant they guarded is preserved by the
	// remaining allowlisted dispatch sites; recovery worker spawn no
	// longer exists in this package.
}

// TestNoDirectSpawnInClusterDispatch is the static AST invariant that
// guards the cluster-native dispatch boundary documented in
// docs/VISION-CLUSTER.md. It walks every non-test .go file in the
// scanned dispatch packages and asserts that any call expression with
// Sel.Name == "Spawn" lives in an allowlisted file.
//
// Intentionally pure-AST so the test runs without loading the full
// type-checker. The Spawn method name is unique to pkg/agent.Backend /
// pkg/agent.Spawner in this codebase (see Spawn() definitions —
// ProcessBackend, DockerBackend, K8sBackend, ProcessSpawner,
// DockerSpawner). A future helper named "Spawn" outside that surface
// would still need to land on the allowlist explicitly, which is
// fine — the invariant's value is in catching new dispatch paths,
// not in disambiguating method overloads.
func TestNoDirectSpawnInClusterDispatch(t *testing.T) {
	moduleRoot := findModuleRoot(t)

	allowed := make(map[string]string, len(allowedSpawnCallSites))
	for _, e := range allowedSpawnCallSites {
		allowed[filepath.ToSlash(e.Path)] = e.Reason
	}

	type violation struct {
		file string
		line int
		expr string
	}
	var violations []violation

	for _, pkgPath := range scannedDispatchPackages {
		dirAbs := filepath.Join(moduleRoot, pkgPath)
		entries, err := os.ReadDir(dirAbs)
		if err != nil {
			t.Fatalf("read dispatch package %q: %v", pkgPath, err)
		}

		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			name := entry.Name()
			if !strings.HasSuffix(name, ".go") {
				continue
			}
			// Test files (mocks, spies, factory wiring) are excluded
			// from the boundary check — the boundary is on production
			// code only.
			if strings.HasSuffix(name, "_test.go") {
				continue
			}

			absPath := filepath.Join(dirAbs, name)
			relPath := filepath.ToSlash(filepath.Join(pkgPath, name))

			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, absPath, nil, parser.SkipObjectResolution)
			if err != nil {
				t.Fatalf("parse %s: %v", relPath, err)
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
				if sel.Sel == nil || sel.Sel.Name != "Spawn" {
					return true
				}

				if _, ok := allowed[relPath]; ok {
					return true
				}

				pos := fset.Position(call.Lparen)
				violations = append(violations, violation{
					file: relPath,
					line: pos.Line,
					expr: exprString(sel),
				})
				return true
			})
		}
	}

	if len(violations) == 0 {
		return
	}

	sort.Slice(violations, func(i, j int) bool {
		if violations[i].file != violations[j].file {
			return violations[i].file < violations[j].file
		}
		return violations[i].line < violations[j].line
	})

	var b strings.Builder
	b.WriteString("cluster-native dispatch boundary violated — direct Spawn call(s) in unauthorized files:\n")
	for _, v := range violations {
		b.WriteString("  ")
		b.WriteString(v.file)
		b.WriteString(":")
		b.WriteString(itoa(v.line))
		b.WriteString(" — ")
		b.WriteString(v.expr)
		b.WriteString("(...)\n")
	}
	b.WriteString("\nIf this call is an intentional local-native dispatch site, add the file path to ")
	b.WriteString("allowedSpawnCallSites in pkg/executor/cluster_dispatch_invariant_test.go ")
	b.WriteString("with a justification.\n")
	b.WriteString("If you intended to dispatch in cluster-native mode, route through the operator seam ")
	b.WriteString("(pkg/steward/intent.IntentPublisher) — see docs/VISION-CLUSTER.md.\n")
	t.Errorf("%s", b.String())
}

// TestAllowedSpawnFilesExist sanity-checks that every entry on the
// allowlist names a file that actually exists in the tree. Stale
// entries weaken the invariant by hiding intent — if a migration
// removes a file, its allowlist entry should be removed too.
func TestAllowedSpawnFilesExist(t *testing.T) {
	moduleRoot := findModuleRoot(t)
	for _, e := range allowedSpawnCallSites {
		abs := filepath.Join(moduleRoot, e.Path)
		if _, err := os.Stat(abs); err != nil {
			t.Errorf("allowlisted file %q does not exist (stale entry): %v", e.Path, err)
		}
	}
}

// findModuleRoot walks up from this test file until it finds go.mod,
// then returns the absolute directory. Robust against test invocation
// from anywhere within the module.
func findModuleRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatalf("runtime.Caller failed — cannot locate module root")
	}
	dir := filepath.Dir(thisFile)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("walked to filesystem root without finding go.mod (started near %s)", thisFile)
		}
		dir = parent
	}
}

// exprString renders a selector expression as text suitable for an
// error message. Avoids pulling in go/format for a single use.
func exprString(sel *ast.SelectorExpr) string {
	var b strings.Builder
	walkSelector(&b, sel.X)
	b.WriteString(".")
	b.WriteString(sel.Sel.Name)
	return b.String()
}

func walkSelector(b *strings.Builder, e ast.Expr) {
	switch n := e.(type) {
	case *ast.Ident:
		b.WriteString(n.Name)
	case *ast.SelectorExpr:
		walkSelector(b, n.X)
		b.WriteString(".")
		b.WriteString(n.Sel.Name)
	case *ast.CallExpr:
		walkSelector(b, n.Fun)
		b.WriteString("()")
	default:
		b.WriteString("?")
	}
}

// itoa is a tiny int→string helper to keep the failure message free of
// strconv import noise.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits [20]byte
	i := len(digits)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		digits[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		digits[i] = '-'
	}
	return string(digits[i:])
}
