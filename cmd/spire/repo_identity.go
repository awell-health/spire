// repo_identity.go resolves the canonical RepoIdentity for CLI commands
// that need to address a specific tower/prefix pair.
//
// Background: before chunk 3 of the runtime-contract migration (spi-ypoqx),
// pkg/executor.ResolveGraphStateStore looked up the dolt database name by
// walking from os.Getwd() to .beads/metadata.json and then fell back to a
// hardcoded "spire" string. That ambient-CWD behavior is gone:
// ResolveGraphStateStore now requires a RepoIdentity and returns
// executor.ErrNoTowerBound on a zero value.
//
// This file centralizes the CLI-side identity resolution so every caller
// reaches the same answer:
//
//  1. Active tower comes from config.ActiveTowerConfig() — which honors
//     the --tower flag (via SPIRE_TOWER env set in root.go) and then
//     falls back to CWD-based instance matching.
//  2. Prefix comes from the tower's registered-repo set. If the tower
//     has exactly one bound / registered repo, that prefix is used
//     automatically. Otherwise the caller must supply --prefix or we
//     return executor.ErrAmbiguousPrefix.
//
// See docs/design/spi-xplwy-runtime-contract.md §1.1 and §7.3 for the
// policy decisions this encodes.
package main

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/awell-health/spire/pkg/config"
	"github.com/awell-health/spire/pkg/dolt"
	"github.com/awell-health/spire/pkg/executor"
	"github.com/awell-health/spire/pkg/repoconfig"
	"github.com/awell-health/spire/pkg/store"
)

// resolveRepoIdentity returns the canonical RepoIdentity for the active
// tower plus the requested prefix. An empty prefixOverride triggers
// auto-pick when the tower has exactly one registered repo; otherwise a
// typed error wrapping executor.ErrAmbiguousPrefix is returned so the
// caller can prompt the user for --prefix.
//
// If the active tower cannot be resolved (no SPIRE_TOWER set, CWD not
// in a registered spire instance, no tower configs on disk), the error
// wraps executor.ErrNoTowerBound so callers can errors.Is it and print
// the uniform "no tower bound" message.
func resolveRepoIdentity(prefixOverride string) (executor.RepoIdentity, error) {
	tower, err := config.ActiveTowerConfig()
	if err != nil {
		return executor.RepoIdentity{}, fmt.Errorf("%w: %v", executor.ErrNoTowerBound, err)
	}
	if tower == nil || tower.Database == "" {
		return executor.RepoIdentity{}, executor.ErrNoTowerBound
	}

	prefixes := collectTowerPrefixes(tower)
	picked, err := pickPrefix(prefixOverride, prefixes, tower)
	if err != nil {
		return executor.RepoIdentity{}, err
	}

	id := executor.RepoIdentity{
		TowerName:  tower.Database,
		TowerID:    tower.ProjectID,
		Prefix:     picked,
		BaseBranch: repoconfig.DefaultBranchBase,
	}

	// Enrich with RepoURL and BaseBranch from the tower's shared repos
	// table when available. Best-effort: a missing repos row is not an
	// error — the fields on RepoIdentity are informational for the
	// runtime-contract consumers, not load-bearing for graph-state
	// resolution (which only needs TowerName).
	if picked != "" {
		if tower.LocalBindings != nil {
			if binding, ok := tower.LocalBindings[picked]; ok && binding != nil {
				if binding.RepoURL != "" {
					id.RepoURL = binding.RepoURL
				}
				if binding.SharedBranch != "" {
					id.BaseBranch = binding.SharedBranch
				}
			}
		}
		// Fall back to the shared repos table if LocalBindings is sparse
		// (e.g. freshly installed machine where reconcileSharedRepos has
		// not run yet).
		if id.RepoURL == "" {
			if url, branch := lookupSharedRepo(tower.Database, picked); url != "" {
				id.RepoURL = url
				if branch != "" {
					id.BaseBranch = branch
				}
			}
		}
	}

	return id, nil
}

// collectTowerPrefixes returns the set of registered / bound repo
// prefixes the tower knows about. Source of truth is the local bindings
// map; the shared repos table in the dolt database is used as a
// fallback if bindings are absent (or if the caller is on a fresh
// machine before reconcile has run).
func collectTowerPrefixes(tower *config.TowerConfig) []string {
	seen := make(map[string]struct{})
	for _, b := range tower.LocalBindings {
		if b == nil {
			continue
		}
		// Include any prefix the tower has heard of, not just bound. A
		// tower with a single "unbound" prefix is still unambiguous.
		if b.Prefix != "" {
			seen[b.Prefix] = struct{}{}
		}
	}
	// Fallback: shared repos table. Best-effort.
	if len(seen) == 0 {
		for _, p := range listSharedRepoPrefixes(tower.Database) {
			seen[p] = struct{}{}
		}
	}
	// Also include the tower's own HubPrefix if nothing else is known.
	if len(seen) == 0 && tower.HubPrefix != "" {
		seen[tower.HubPrefix] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// pickPrefix chooses a prefix given the tower's registered set and an
// optional caller-supplied override.
//
//   - override non-empty → use it (we trust the caller); no validation
//     against the registered set, since commands may legitimately
//     target a prefix that is not yet bound on this machine.
//   - override empty + single-prefix tower → auto-pick.
//   - override empty + multi-prefix tower → ErrAmbiguousPrefix.
//   - override empty + zero-prefix tower (fresh install) → empty string
//     is returned without error. The caller gets a RepoIdentity with
//     TowerName set but Prefix empty; graph-state resolution works
//     because only TowerName is load-bearing for that path.
func pickPrefix(override string, prefixes []string, tower *config.TowerConfig) (string, error) {
	if override != "" {
		return override, nil
	}
	switch len(prefixes) {
	case 0:
		// No registered repos yet — graph-state resolution only needs
		// TowerName, so accept an empty prefix rather than forcing the
		// user to bind a repo just to run the steward.
		return "", nil
	case 1:
		return prefixes[0], nil
	default:
		return "", fmt.Errorf(
			"%w: tower %q has prefixes [%s] — rerun with --prefix=<one>",
			executor.ErrAmbiguousPrefix,
			tower.Name,
			strings.Join(prefixes, ", "),
		)
	}
}

// lookupSharedRepo queries the tower's shared repos table for the
// given prefix and returns (repo_url, branch). Best-effort: returns
// empty strings on any error.
func lookupSharedRepo(database, prefix string) (string, string) {
	if database == "" || prefix == "" {
		return "", ""
	}
	sql := fmt.Sprintf(
		"SELECT repo_url, branch FROM `%s`.repos WHERE prefix = '%s' LIMIT 1",
		database, escapeSQLLiteral(prefix),
	)
	out, err := dolt.RawQuery(sql)
	if err != nil {
		return "", ""
	}
	rows := dolt.ParseDoltRows(out, []string{"repo_url", "branch"})
	if len(rows) == 0 {
		return "", ""
	}
	return rows[0]["repo_url"], rows[0]["branch"]
}

// listSharedRepoPrefixes queries the tower's shared repos table for
// all registered prefixes. Best-effort: returns nil on any error.
func listSharedRepoPrefixes(database string) []string {
	if database == "" {
		return nil
	}
	sql := fmt.Sprintf("SELECT prefix FROM `%s`.repos ORDER BY prefix", database)
	out, err := dolt.RawQuery(sql)
	if err != nil {
		return nil
	}
	rows := dolt.ParseDoltRows(out, []string{"prefix"})
	result := make([]string, 0, len(rows))
	for _, r := range rows {
		if p := r["prefix"]; p != "" {
			result = append(result, p)
		}
	}
	return result
}

// escapeSQLLiteral escapes a single-quoted SQL string literal by
// doubling embedded single quotes. Inputs are tower-registered prefixes
// (alphanumerics), so the primary defense is type-shape, but we still
// escape to satisfy principled-code review.
func escapeSQLLiteral(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

// resolveGraphStateStoreForCLI is the cmd/spire-facing wrapper around
// executor.ResolveGraphStateStore. It resolves RepoIdentity from the
// active tower (honoring --prefix when supplied) and calls through.
// Callers that need cluster-mode persistence (e.g. the steward daemon)
// should use this and surface the error so the user gets a clear
// "no tower bound" message.
func resolveGraphStateStoreForCLI(prefixOverride string) (executor.GraphStateStore, error) {
	id, err := resolveRepoIdentity(prefixOverride)
	if err != nil {
		return nil, err
	}
	store, err := executor.ResolveGraphStateStore(id, configDir)
	if err != nil {
		return nil, err
	}
	return store, nil
}

// resolveGraphStateStoreOrLocal returns a GraphStateStore for use inside
// buildExecutorDeps (which cannot itself return an error because it's
// part of a Deps struct literal).
//
// Semantics:
//
//   - Happy path: resolve identity from the active tower and call
//     executor.ResolveGraphStateStore. In cluster mode this yields a
//     DoltGraphStateStore; locally, a FileGraphStateStore keyed by
//     configDir.
//   - No tower bound / ambiguous prefix: log the error and fall back
//     to a bare FileGraphStateStore keyed by configDir. The executor
//     can still persist graph state locally, and the dispatch site
//     surfaces the underlying identity problem through other paths
//     (tower lookups, backend selection, etc.).
//
// Callers that MUST have cluster-mode persistence (the steward) should
// use resolveGraphStateStoreForCLI instead and refuse to start on error.
func resolveGraphStateStoreOrLocal(prefixOverride string) executor.GraphStateStore {
	store, err := resolveGraphStateStoreForCLI(prefixOverride)
	if err == nil {
		return store
	}
	// Log once — operators need to see this — then fall back to the
	// local file store so the caller keeps working.
	fmt.Fprintf(os.Stderr,
		"[spire] graph-state store running in local-only mode: %s\n",
		friendlyIdentityError(err),
	)
	return &executor.FileGraphStateStore{ConfigDir: configDir}
}

// resolveGraphStateStoreForBead resolves the graph-state store for a
// specific bead using the bead ID's prefix segment (see store.PrefixFromID).
// This is the per-bead form of the resolver: it treats an empty beadID as
// a programmer error (hard fail), because the whole point of this helper
// is to avoid the silent cluster-mode write-loss that happens when the
// prefix is absent on a multi-prefix tower.
//
// When the bead ID is non-empty but has no prefix separator (e.g. passed
// in malformed), the underlying resolver returns ErrAmbiguousPrefix or
// similar on multi-prefix towers; single-prefix towers auto-pick.
//
// Callers that sweep all beads in a tower (steward daemon) MUST use
// resolveGlobalGraphStateStore instead — passing "" here is rejected.
func resolveGraphStateStoreForBead(beadID string) (executor.GraphStateStore, error) {
	if beadID == "" {
		return nil, fmt.Errorf("resolveGraphStateStoreForBead: beadID must be non-empty (use resolveGlobalGraphStateStore for tower-global sweeps)")
	}
	prefix := store.PrefixFromID(beadID)
	return resolveGraphStateStoreForCLI(prefix)
}

// resolveGraphStateStoreForBeadOrLocal is the non-error-returning form of
// resolveGraphStateStoreForBead, suitable for use inside a Deps struct
// literal. It panics on an empty beadID — that's a programmer error, not
// an operator misconfiguration.
func resolveGraphStateStoreForBeadOrLocal(beadID string) executor.GraphStateStore {
	if beadID == "" {
		panic("resolveGraphStateStoreForBeadOrLocal: beadID must be non-empty (use resolveGlobalGraphStateStore for tower-global sweeps)")
	}
	prefix := store.PrefixFromID(beadID)
	s, err := resolveGraphStateStoreForCLI(prefix)
	if err == nil {
		return s
	}
	fmt.Fprintf(os.Stderr,
		"[spire] graph-state store running in local-only mode: %s\n",
		friendlyIdentityError(err),
	)
	return &executor.FileGraphStateStore{ConfigDir: configDir}
}

// resolveGlobalGraphStateStore is the tower-global form of the resolver,
// used by the steward daemon that sweeps every agent's graph state
// regardless of prefix. Empty prefix is intentional here — the steward
// is tower-scoped, not bead-scoped. This is a rename of the historical
// resolveGraphStateStoreForCLI("") idiom so the intentional case is typed
// distinctly from the accidental empty-prefix one that Bug C fixes.
func resolveGlobalGraphStateStore() (executor.GraphStateStore, error) {
	return resolveGraphStateStoreForCLI("")
}

// resolveGlobalGraphStateStoreOrLocal mirrors
// resolveGraphStateStoreOrLocal but for the tower-global resolver — used
// by the gateway server constructor where a missing tower binding must
// not crash the gateway. Spi-skfsia finding 4: gateway needs to share
// the executor/steward graph-state store so the cleric HITL gate hits
// the canonical runtime dir; falling back to a bare FileGraphStateStore
// keyed by configDir is acceptable because the gate handler itself
// surfaces 409 when a particular agent's graph state is missing.
func resolveGlobalGraphStateStoreOrLocal() executor.GraphStateStore {
	store, err := resolveGlobalGraphStateStore()
	if err == nil {
		return store
	}
	fmt.Fprintf(os.Stderr,
		"[spire] gateway graph-state store running in local-only mode: %s\n",
		friendlyIdentityError(err),
	)
	return &executor.FileGraphStateStore{ConfigDir: configDir}
}

// friendlyIdentityError converts the typed errors from
// resolveRepoIdentity / ResolveGraphStateStore into messages suitable
// for end-user CLI output.
func friendlyIdentityError(err error) string {
	switch {
	case errors.Is(err, executor.ErrNoTowerBound):
		return "no tower bound for this command. Run `spire tower create` to create one, " +
			"or `spire repo add` inside a registered repo. Use --tower <name> to target " +
			"a specific tower when multiple exist."
	case errors.Is(err, executor.ErrAmbiguousPrefix):
		return err.Error()
	default:
		return err.Error()
	}
}
