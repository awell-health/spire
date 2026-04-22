// Package identity provides the canonical cluster repo-identity resolver.
//
// This resolver is the only canonical source of cluster repo identity.
// LocalBindings.State and LocalBindings.LocalPath must never appear in
// cluster scheduling paths.
//
// Cluster-native scheduling must resolve repo identity solely from the
// shared repo-registration store — the tower's `repos` table, backed by
// pkg/store's dolt connection. Machine-local state (config.TowerConfig's
// LocalBindings map, cfg.Instances, or any filesystem walk) has no meaning
// across cluster replicas and MUST NOT be consulted here.
package identity

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ClusterRepoIdentity is the canonical identity for a repo in cluster-native
// scheduling. Every field is sourced from the shared tower repo registry and
// is therefore consistent across every replica of the control plane.
type ClusterRepoIdentity struct {
	URL        string
	BaseBranch string
	Prefix     string
}

// ClusterIdentityResolver is the seam cluster scheduling code calls to resolve
// a repo prefix to its canonical identity. Implementations MUST resolve from
// shared registration only — never from LocalBindings or cfg.Instances.
type ClusterIdentityResolver interface {
	Resolve(ctx context.Context, repoPrefix string) (ClusterRepoIdentity, error)
}

// ErrRepoNotRegistered is returned by Resolve when the prefix is not present
// in the shared tower repo registry. Callers use errors.Is to distinguish it
// from configuration or I/O errors.
var ErrRepoNotRegistered = errors.New("repo not registered in shared tower registry")

// RegistryStore reads repo rows from the shared repo-registration store.
// In production this is backed by pkg/store's dolt connection (the tower's
// `repos` table); in tests it's an in-memory fake.
//
// Implementations MUST source records from shared (tower-wide) state and
// MUST NOT read LocalBindings, cfg.Instances, or any machine-local config.
type RegistryStore interface {
	// LookupRepo returns the canonical fields for the given prefix. When the
	// prefix is not registered, ok is false and err is nil.
	LookupRepo(ctx context.Context, prefix string) (url, baseBranch string, ok bool, err error)
}

// LocalBindingSnapshot mirrors the shape of config.LocalRepoBinding fields
// that cluster scheduling code must never dereference. It exists purely so
// LocalBindingsAccessor.Get has a concrete return type — there is no
// production call site inside this package.
type LocalBindingSnapshot struct {
	Prefix    string
	LocalPath string
	State     string
}

// LocalBindingsAccessor captures the single method a cluster path would call
// on a LocalBindings map if it were wrong to do so. DefaultClusterIdentityResolver
// accepts one purely so tests can wire a panic stub and assert Resolve never
// dereferences it, pinning the boundary mechanically.
//
// Cluster scheduling code — including this resolver — MUST NEVER call methods
// on a LocalBindingsAccessor.
type LocalBindingsAccessor interface {
	Get(prefix string) (LocalBindingSnapshot, bool)
}

// DefaultClusterIdentityResolver resolves ClusterRepoIdentity from the shared
// repo-registration store. Registry is the sole data source. LocalBindings is
// audit-only: it is carried so tests can assert the resolver never touches it.
type DefaultClusterIdentityResolver struct {
	// Registry reads the shared repo-registration store. Required.
	Registry RegistryStore
	// LocalBindings is audit-only. Resolve MUST NEVER invoke methods on this
	// field. Tests wire a panic stub here to prove the invariant.
	LocalBindings LocalBindingsAccessor
}

// Resolve returns the canonical ClusterRepoIdentity for repoPrefix by reading
// the shared repo-registration store. It never touches r.LocalBindings.
func (r *DefaultClusterIdentityResolver) Resolve(ctx context.Context, repoPrefix string) (ClusterRepoIdentity, error) {
	if r == nil || r.Registry == nil {
		return ClusterRepoIdentity{}, fmt.Errorf("identity: DefaultClusterIdentityResolver has nil Registry")
	}
	if repoPrefix == "" {
		return ClusterRepoIdentity{}, fmt.Errorf("identity: empty repo prefix")
	}
	url, branch, ok, err := r.Registry.LookupRepo(ctx, repoPrefix)
	if err != nil {
		return ClusterRepoIdentity{}, fmt.Errorf("identity: lookup repo %q: %w", repoPrefix, err)
	}
	if !ok {
		return ClusterRepoIdentity{}, fmt.Errorf("%w: prefix %q", ErrRepoNotRegistered, repoPrefix)
	}
	return ClusterRepoIdentity{
		URL:        url,
		BaseBranch: branch,
		Prefix:     repoPrefix,
	}, nil
}

// SQLRegistryStore is the production RegistryStore backed by a *sql.DB
// connected to the tower's dolt database. It reads from the shared `repos`
// table that `spire repo add` populates.
//
// The zero value is not usable — callers must supply a DB via NewSQLRegistryStore.
type SQLRegistryStore struct {
	db *sql.DB
}

// NewSQLRegistryStore returns a RegistryStore that reads the tower's `repos`
// table via the supplied connection. The connection must be open against the
// tower's dolt database (the same database pkg/store uses); no database name
// qualification is applied to queries.
func NewSQLRegistryStore(db *sql.DB) *SQLRegistryStore {
	return &SQLRegistryStore{db: db}
}

// LookupRepo reads the shared `repos` row for prefix and returns its URL and
// base branch. ok is false when no row exists; the error is nil in that case.
func (s *SQLRegistryStore) LookupRepo(ctx context.Context, prefix string) (string, string, bool, error) {
	if s == nil || s.db == nil {
		return "", "", false, fmt.Errorf("identity: SQLRegistryStore has nil DB")
	}
	var url, branch string
	err := s.db.QueryRowContext(ctx,
		`SELECT repo_url, branch FROM repos WHERE prefix = ? LIMIT 1`,
		prefix,
	).Scan(&url, &branch)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, fmt.Errorf("identity: query repos: %w", err)
	}
	return url, branch, true, nil
}
