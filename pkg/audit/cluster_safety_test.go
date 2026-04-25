package audit

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// repoRoot resolves the Spire worktree root from this test file's location.
// The test walks up from the package dir (pkg/audit) two levels.
func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	return filepath.Clean(filepath.Join(wd, "..", ".."))
}

// clusterReachableDirs lists every directory whose sources may be reached
// from the cluster-bootstrap path or the in-pod steward runtime. New
// packages that run in a pod should be added here so the checks below
// enforce the same invariants.
var clusterReachableDirs = []string{
	"cmd/spire-steward-sidecar",
	"pkg/tower",
	"pkg/steward",
	"pkg/metrics",
	// cmd/spire holds both laptop and cluster code paths; cluster-reachable
	// files are listed explicitly rather than scanning the whole dir so
	// laptop-only commands (tower create, doctor, list-differences) do not
	// drag false positives into the check.
}

// clusterReachableFiles lists individual files in otherwise-mixed packages
// that are cluster-reachable. Anything not in this list but in the same
// package is treated as laptop-only for the purposes of these tests.
var clusterReachableFiles = []string{
	"cmd/spire/tower_cluster.go",
	"cmd/spire/cache_bootstrap.go",
	"cmd/spire/cluster.go",
	"cmd/spire/steward.go",
	"cmd/spire/executor_bridge.go",
}

// bdShellAllowlist records exec.Command("bd", ...) call sites that are
// intentionally permitted. Every entry must justify why the call is safe
// in cluster mode (typically: lives only in laptop paths, or explicitly
// propagates BEADS_DIR). An entry that references a bug ID is a TODO —
// remove the entry when the bug is fixed so the regression marker
// protects against reintroduction.
var bdShellAllowlist = map[string]string{
	// cmd/spire/bd.go is the laptop-user-facing `bd` passthrough wrapper
	// and is not reachable from cluster bootstrap. It is NOT in the
	// clusterReachableFiles list so this allowlist entry is defensive.
	"cmd/spire/bd.go:bd":                            "laptop-only bd passthrough",
	"cmd/spire-steward-sidecar/tools.go:runSpire":   "spire CLI shell-out, not bd",
	"cmd/spire-steward-sidecar/tools.go:runKubectl": "kubectl shell-out, not bd",

	// KNOWN-BROKEN: spi-8qhwfo tracks the fix to route these through
	// bdpkg.Client with an explicit BeadsDir. Remove when landed.
	"pkg/metrics/recorder.go:bdSQL":       "tracked by spi-8qhwfo — missing BEADS_DIR in cluster mode",
	"pkg/metrics/recorder.go:bdSQLOutput": "tracked by spi-8qhwfo — missing BEADS_DIR in cluster mode",
}

// extractSQLValueAllowedCallers records callers that have been verified
// to work against the positional parser (post-spi-19v3oa). The allowlist
// exists so new call sites land deliberately.
var extractSQLValueAllowedCallers = map[string]struct{}{
	"pkg/tower/bootstrap.go:IsBlankDB":          {},
	"pkg/tower/metadata.go:ReadMetadata":        {},
	"pkg/dolt/merge_ownership.go:GetCurrentCommitHash": {},
	"cmd/spire/tower.go:extractSQLValue":        {},
}

// parseFile returns the AST of path (absolute) with position info.
func parseFile(t *testing.T, path string) (*token.FileSet, *ast.File) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return fset, f
}

// listGoFiles returns every *.go file under dir (non-recursive for package
// dirs; we scan each listed dir separately). _test.go files are excluded
// because test code is not shipped to cluster pods.
func listGoFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("readdir %s: %v", dir, err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		if strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		out = append(out, filepath.Join(dir, e.Name()))
	}
	return out
}

// clusterReachableSources returns every cluster-reachable .go file as
// absolute paths, combining directory-level and file-level entries.
func clusterReachableSources(t *testing.T) []string {
	t.Helper()
	root := repoRoot(t)
	var out []string
	for _, d := range clusterReachableDirs {
		out = append(out, listGoFiles(t, filepath.Join(root, d))...)
	}
	for _, f := range clusterReachableFiles {
		p := filepath.Join(root, f)
		if _, err := os.Stat(p); err == nil {
			out = append(out, p)
		}
	}
	return out
}

// relFromRoot formats a file path as repo-rooted for diagnostic output.
func relFromRoot(t *testing.T, abs string) string {
	t.Helper()
	root := repoRoot(t)
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		return abs
	}
	return rel
}

// TestNoDirectBdShellOutInClusterPaths asserts that no cluster-reachable
// source file shells out to the bd binary via exec.Command("bd", ...)
// without going through pkg/bd.Client (which sets BEADS_DIR per-call).
//
// Why: in cluster mode BEADS_DIR is typically not set in the pod's env
// and CWD walks up from /etc/spire or /data in unpredictable ways. Using
// bdpkg.Client with an explicit BeadsDir (or adding this call to the
// allowlist above) keeps the failure mode loud instead of silent.
func TestNoDirectBdShellOutInClusterPaths(t *testing.T) {
	sources := clusterReachableSources(t)
	var violations []string

	for _, path := range sources {
		fset, f := parseFile(t, path)
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			if pkg.Name != "exec" {
				return true
			}
			if sel.Sel.Name != "Command" && sel.Sel.Name != "CommandContext" {
				return true
			}
			// First arg for Command is the binary name; for CommandContext
			// it's ctx, second is the binary name.
			binArgIdx := 0
			if sel.Sel.Name == "CommandContext" {
				binArgIdx = 1
			}
			if len(call.Args) <= binArgIdx {
				return true
			}
			lit, ok := call.Args[binArgIdx].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			bin := strings.Trim(lit.Value, `"`)
			if bin != "bd" {
				return true
			}
			// Determine enclosing function for allowlist lookup.
			enclosing := enclosingFuncName(f, call.Pos())
			key := relFromRoot(t, path) + ":" + enclosing
			if _, ok := bdShellAllowlist[key]; ok {
				return true
			}
			pos := fset.Position(call.Pos())
			violations = append(violations, fmt.Sprintf(
				"%s:%d: exec.Command(\"bd\", ...) in cluster-reachable code; use bdpkg.Client or add %q to bdShellAllowlist with justification",
				relFromRoot(t, path), pos.Line, key))
			return true
		})
	}

	if len(violations) > 0 {
		t.Errorf("found %d unsafe bd shell-out(s):\n  %s",
			len(violations), strings.Join(violations, "\n  "))
	}
}

// TestBdpkgNewClientInClusterPathsConfiguresModeExplicitly asserts that
// every bdpkg.NewClient() construction in cluster-reachable code is
// followed, within the same function, by either:
//
//   - a BeadsDir assignment (client.BeadsDir = ...), which propagates
//     BEADS_DIR to the bd subprocess; or
//   - an Init(...) call with Server: true in its InitOpts, pinning the
//     external dolt sql-server path.
//
// Why: bdpkg.NewClient() alone defaults to embedded Dolt, which needs CGO
// and therefore cannot run in the steward image. The pattern this test
// enforces mirrors the clusterRunBdInit wiring that fixed spi-lfkfgh.
func TestBdpkgNewClientInClusterPathsConfiguresModeExplicitly(t *testing.T) {
	sources := clusterReachableSources(t)
	var violations []string

	for _, path := range sources {
		fset, f := parseFile(t, path)
		ast.Inspect(f, func(n ast.Node) bool {
			fn, ok := n.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				return true
			}
			for _, stmt := range fn.Body.List {
				if v, pos := findNewClientAssignment(stmt); v != "" {
					if !functionConfiguresClient(fn, v) {
						p := fset.Position(pos)
						violations = append(violations, fmt.Sprintf(
							"%s:%d: bdpkg.NewClient() assigned to %q in %s: neither BeadsDir nor Server-mode Init seen; embedded mode will break CGO-disabled pods",
							relFromRoot(t, path), p.Line, v, fn.Name.Name))
					}
				}
			}
			return true
		})
	}

	if len(violations) > 0 {
		t.Errorf("found %d bdpkg.NewClient() usages without explicit cluster wiring:\n  %s",
			len(violations), strings.Join(violations, "\n  "))
	}
}

// TestExtractSQLValueCallersStayInAllowlist asserts that every caller of
// config.ExtractSQLValue is in the reviewed allowlist. New call sites must
// land deliberately so the positional-parser contract (post-spi-19v3oa)
// stays auditable.
//
// Why: the positional parser is robust to column aliases, but a new
// caller might produce dolt output in a shape the parser doesn't
// recognise (e.g. multi-row results, non-table output). Gating additions
// on the allowlist keeps the contract reviewable.
func TestExtractSQLValueCallersStayInAllowlist(t *testing.T) {
	root := repoRoot(t)
	// Scan the whole Go source tree (skipping vendor/tests) for callers.
	var violations []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			name := info.Name()
			if name == "vendor" || name == ".git" || name == "node_modules" || name == ".worktrees" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		if strings.HasSuffix(path, "_test.go") {
			return nil
		}
		// Skip the definition itself.
		rel, _ := filepath.Rel(root, path)
		if rel == "pkg/config/tower.go" {
			return nil
		}
		fset, f := parseFile(t, path)
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			if pkg.Name != "config" || sel.Sel.Name != "ExtractSQLValue" {
				return true
			}
			enclosing := enclosingFuncName(f, call.Pos())
			key := rel + ":" + enclosing
			if _, ok := extractSQLValueAllowedCallers[key]; ok {
				return true
			}
			p := fset.Position(call.Pos())
			violations = append(violations, fmt.Sprintf(
				"%s:%d: config.ExtractSQLValue called from %s; add %q to extractSQLValueAllowedCallers with a note on the SQL shape it parses",
				rel, p.Line, enclosing, key))
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	if len(violations) > 0 {
		t.Errorf("found %d unreviewed config.ExtractSQLValue caller(s):\n  %s",
			len(violations), strings.Join(violations, "\n  "))
	}
}

// enclosingFuncName returns the name of the function containing pos, or
// "" if pos is at file-level. Methods are reported as "Recv.Name".
func enclosingFuncName(f *ast.File, pos token.Pos) string {
	var best *ast.FuncDecl
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		if fn.Pos() <= pos && pos <= fn.End() {
			best = fn
			break
		}
	}
	if best == nil {
		return "<file-level>"
	}
	return best.Name.Name
}

// findNewClientAssignment looks for a statement of the form:
//
//	v := bdpkg.NewClient()
//	v := bd.NewClient()
//
// and returns the name of v plus the statement's position. Returns ""
// when no such assignment is present.
func findNewClientAssignment(stmt ast.Stmt) (string, token.Pos) {
	assign, ok := stmt.(*ast.AssignStmt)
	if !ok || len(assign.Lhs) != 1 || len(assign.Rhs) != 1 {
		return "", token.NoPos
	}
	ident, ok := assign.Lhs[0].(*ast.Ident)
	if !ok {
		return "", token.NoPos
	}
	call, ok := assign.Rhs[0].(*ast.CallExpr)
	if !ok {
		return "", token.NoPos
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return "", token.NoPos
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok {
		return "", token.NoPos
	}
	if (pkg.Name == "bdpkg" || pkg.Name == "bd") && sel.Sel.Name == "NewClient" {
		return ident.Name, assign.Pos()
	}
	return "", token.NoPos
}

// functionConfiguresClient reports whether fn assigns a BeadsDir field on
// client, or calls Init(...) with Server: true in its InitOpts literal.
func functionConfiguresClient(fn *ast.FuncDecl, client string) bool {
	var configured bool
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if configured {
			return false
		}
		switch node := n.(type) {
		case *ast.AssignStmt:
			// client.BeadsDir = ...
			if len(node.Lhs) != 1 {
				return true
			}
			sel, ok := node.Lhs[0].(*ast.SelectorExpr)
			if !ok {
				return true
			}
			id, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			if id.Name == client && sel.Sel.Name == "BeadsDir" {
				configured = true
				return false
			}
		case *ast.CallExpr:
			// client.Init(bdpkg.InitOpts{Server: true, ...})
			sel, ok := node.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			id, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			if id.Name != client || sel.Sel.Name != "Init" {
				return true
			}
			if len(node.Args) == 0 {
				return true
			}
			lit, ok := node.Args[0].(*ast.CompositeLit)
			if !ok {
				return true
			}
			for _, elt := range lit.Elts {
				kv, ok := elt.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				key, ok := kv.Key.(*ast.Ident)
				if !ok {
					continue
				}
				if key.Name == "Server" {
					if ident, ok := kv.Value.(*ast.Ident); ok && ident.Name == "true" {
						configured = true
						return false
					}
				}
			}
		}
		return true
	})
	return configured
}
