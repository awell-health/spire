package identity

// Boundary tests for DefaultClusterIdentityResolver.
//
// These tests go deeper than the wave-0 smoke test in
// cluster_identity_test.go: they parametrize over a table of registered
// repo prefixes plus one deliberately unregistered prefix, and they
// wire a panicking LocalBindings accessor for every case to prove no
// code path consults it.
//
// The invariants pinned here are the ones cluster-native scheduling
// relies on:
//
//   1. The resolver returns the canonical ClusterRepoIdentity — URL,
//      BaseBranch, Prefix — for a prefix present in shared storage.
//   2. The resolver returns an error matching ErrRepoNotRegistered (via
//      errors.Is) for a prefix not present in shared storage.
//   3. The resolver NEVER dereferences a LocalBindingsAccessor, even
//      when one is wired. Local workspace state is explicitly out of
//      scope for cluster-native identity.
//
// If (3) regresses, cluster scheduling would silently couple back to
// machine-local binding state, which is invalid under multi-replica
// control planes.

import (
	"context"
	"errors"
	"testing"
)

// boundaryRegistry is a table-backed RegistryStore. It holds a map of
// prefix to URL/branch so a single test case list can exercise multiple
// registered repos. Absence of a prefix is modeled as `ok=false, err=nil`
// — the same shape the production SQLRegistryStore returns for
// sql.ErrNoRows.
type boundaryRegistry struct {
	rows map[string]boundaryRow
}

type boundaryRow struct {
	url    string
	branch string
}

func (r *boundaryRegistry) LookupRepo(_ context.Context, prefix string) (string, string, bool, error) {
	row, ok := r.rows[prefix]
	if !ok {
		return "", "", false, nil
	}
	return row.url, row.branch, true, nil
}

// boundaryPanicLocalBindings is the panicking LocalBindingsAccessor wired
// into every test case. If Resolve ever touches the accessor, the test
// panics and fails with a clear message. A separate type (rather than
// reusing the existing panicLocalBindings from cluster_identity_test.go)
// keeps this boundary test self-contained and makes the invariant read
// straight from the test file.
type boundaryPanicLocalBindings struct{}

func (boundaryPanicLocalBindings) Get(string) (LocalBindingSnapshot, bool) {
	panic("identity boundary: Resolve must not consult LocalBindings — cluster identity is registry-only")
}

// TestDefaultClusterIdentityResolver_Boundary_TableDriven parametrizes
// over a mix of registered and unregistered prefixes. Each case wires
// the panicking LocalBindings accessor — if Resolve regresses into
// touching it, every sub-test panics. The table is keyed by prefix so a
// failure attributes cleanly to the offending case.
func TestDefaultClusterIdentityResolver_Boundary_TableDriven(t *testing.T) {
	registry := &boundaryRegistry{
		rows: map[string]boundaryRow{
			"spi": {url: "https://example.test/spire.git", branch: "main"},
			"web": {url: "https://example.test/web-app.git", branch: "trunk"},
			"api": {url: "https://example.test/api-server.git", branch: "develop"},
			"hub": {url: "https://example.test/hub.git", branch: "main"},
		},
	}

	type expectation struct {
		name       string
		prefix     string
		wantErr    bool    // true = want an error; false = want a concrete identity
		wantNotReg bool    // when wantErr, true = expect ErrRepoNotRegistered
		wantURL    string  // only when !wantErr
		wantBranch string  // only when !wantErr
	}

	cases := []expectation{
		{
			name:       "registered: spi",
			prefix:     "spi",
			wantURL:    "https://example.test/spire.git",
			wantBranch: "main",
		},
		{
			name:       "registered: web",
			prefix:     "web",
			wantURL:    "https://example.test/web-app.git",
			wantBranch: "trunk",
		},
		{
			name:       "registered: api",
			prefix:     "api",
			wantURL:    "https://example.test/api-server.git",
			wantBranch: "develop",
		},
		{
			name:       "registered: hub",
			prefix:     "hub",
			wantURL:    "https://example.test/hub.git",
			wantBranch: "main",
		},
		{
			name:       "unregistered: zzz returns ErrRepoNotRegistered",
			prefix:     "zzz",
			wantErr:    true,
			wantNotReg: true,
		},
	}

	for _, tc := range cases {
		tc := tc // capture
		t.Run(tc.name, func(t *testing.T) {
			// Every case wires the panicking LocalBindings — if
			// Resolve touches it, the test panics.
			r := &DefaultClusterIdentityResolver{
				Registry:      registry,
				LocalBindings: boundaryPanicLocalBindings{},
			}

			defer func() {
				if p := recover(); p != nil {
					t.Fatalf("Resolve(%q) touched LocalBindings (panicked): %v", tc.prefix, p)
				}
			}()

			got, err := r.Resolve(context.Background(), tc.prefix)

			if tc.wantErr {
				if err == nil {
					t.Fatalf("Resolve(%q): expected error, got identity %+v", tc.prefix, got)
				}
				if tc.wantNotReg && !errors.Is(err, ErrRepoNotRegistered) {
					t.Fatalf("Resolve(%q): err = %v, want errors.Is(err, ErrRepoNotRegistered)", tc.prefix, err)
				}
				return
			}

			if err != nil {
				t.Fatalf("Resolve(%q): unexpected error: %v", tc.prefix, err)
			}
			want := ClusterRepoIdentity{
				URL:        tc.wantURL,
				BaseBranch: tc.wantBranch,
				Prefix:     tc.prefix,
			}
			if got != want {
				t.Fatalf("Resolve(%q): got %+v, want %+v", tc.prefix, got, want)
			}
		})
	}
}

// TestDefaultClusterIdentityResolver_Boundary_PrefixEchoedEvenWhenEmptyBranch
// pins a subtle field-level property: the returned ClusterRepoIdentity's
// Prefix field is always the caller-supplied prefix, not anything the
// registry might return. A regression that synthesized Prefix from the
// URL (for instance) would fail this case because the registry URL has
// a completely unrelated path segment.
func TestDefaultClusterIdentityResolver_Boundary_PrefixEchoedEvenWhenEmptyBranch(t *testing.T) {
	registry := &boundaryRegistry{
		rows: map[string]boundaryRow{
			"odd": {url: "https://example.test/unrelated-path.git", branch: ""},
		},
	}
	r := &DefaultClusterIdentityResolver{
		Registry:      registry,
		LocalBindings: boundaryPanicLocalBindings{},
	}

	defer func() {
		if p := recover(); p != nil {
			t.Fatalf("Resolve touched LocalBindings: %v", p)
		}
	}()

	got, err := r.Resolve(context.Background(), "odd")
	if err != nil {
		t.Fatalf("Resolve: unexpected error: %v", err)
	}
	if got.Prefix != "odd" {
		t.Errorf("Resolve: Prefix = %q, want %q (must echo the caller-supplied prefix)", got.Prefix, "odd")
	}
	if got.URL != "https://example.test/unrelated-path.git" {
		t.Errorf("Resolve: URL = %q, want canonical registry URL", got.URL)
	}
	// Branch may legitimately be empty when the registry row stores an
	// empty string — the resolver propagates rather than filling in a
	// default. Pin that behavior.
	if got.BaseBranch != "" {
		t.Errorf("Resolve: BaseBranch = %q, want empty (resolver must propagate, not fill in)", got.BaseBranch)
	}
}

// TestDefaultClusterIdentityResolver_Boundary_MultiplePrefixesIndependent
// pins independence between prefix lookups. Resolving one prefix must
// not leak state into the next — the resolver is stateless by design,
// and a regression that cached the last lookup would fail this because
// the second Resolve would return the first prefix's identity.
func TestDefaultClusterIdentityResolver_Boundary_MultiplePrefixesIndependent(t *testing.T) {
	registry := &boundaryRegistry{
		rows: map[string]boundaryRow{
			"first":  {url: "https://example.test/first.git", branch: "main"},
			"second": {url: "https://example.test/second.git", branch: "trunk"},
		},
	}
	r := &DefaultClusterIdentityResolver{
		Registry:      registry,
		LocalBindings: boundaryPanicLocalBindings{},
	}

	defer func() {
		if p := recover(); p != nil {
			t.Fatalf("Resolve touched LocalBindings: %v", p)
		}
	}()

	ctx := context.Background()
	firstIdent, err := r.Resolve(ctx, "first")
	if err != nil {
		t.Fatalf("Resolve(first): %v", err)
	}
	secondIdent, err := r.Resolve(ctx, "second")
	if err != nil {
		t.Fatalf("Resolve(second): %v", err)
	}
	if firstIdent == secondIdent {
		t.Fatalf("Resolve(first) and Resolve(second) returned identical identities: %+v", firstIdent)
	}
	if firstIdent.Prefix != "first" {
		t.Errorf("first.Prefix = %q, want %q", firstIdent.Prefix, "first")
	}
	if secondIdent.Prefix != "second" {
		t.Errorf("second.Prefix = %q, want %q", secondIdent.Prefix, "second")
	}
}

// TestDefaultClusterIdentityResolver_Boundary_NilLocalBindingsAlsoFine
// pins that LocalBindings being NIL (not wired at all) is valid and
// does not trigger a defensive dereference. The production wiring in
// cmd/spire leaves LocalBindings nil; only tests set it to the panic
// stub. If a future change adds a defensive `if r.LocalBindings != nil`
// read, this test will still pass — the invariant is that nil is
// accepted, not that non-nil is mandatory.
func TestDefaultClusterIdentityResolver_Boundary_NilLocalBindingsAlsoFine(t *testing.T) {
	registry := &boundaryRegistry{
		rows: map[string]boundaryRow{
			"spi": {url: "https://example.test/spire.git", branch: "main"},
		},
	}
	r := &DefaultClusterIdentityResolver{
		Registry: registry,
		// LocalBindings is deliberately nil.
	}

	got, err := r.Resolve(context.Background(), "spi")
	if err != nil {
		t.Fatalf("Resolve(nil LocalBindings): %v", err)
	}
	if got.Prefix != "spi" {
		t.Errorf("Resolve(nil LocalBindings).Prefix = %q, want %q", got.Prefix, "spi")
	}
}

// Compile-time conformance for the boundary fakes.
var (
	_ RegistryStore         = (*boundaryRegistry)(nil)
	_ LocalBindingsAccessor = boundaryPanicLocalBindings{}
)
