package identity

import (
	"context"
	"errors"
	"testing"
)

// fakeRegistry is an in-memory RegistryStore used to prove the resolver
// consults only shared registration data.
type fakeRegistry struct {
	rows map[string]struct {
		url    string
		branch string
	}
}

func (f *fakeRegistry) LookupRepo(_ context.Context, prefix string) (string, string, bool, error) {
	r, ok := f.rows[prefix]
	if !ok {
		return "", "", false, nil
	}
	return r.url, r.branch, true, nil
}

// panicLocalBindings is a LocalBindingsAccessor whose Get panics if called.
// Wiring it into DefaultClusterIdentityResolver proves Resolve never
// dereferences LocalBindings — if it did, tests would panic.
type panicLocalBindings struct{}

func (panicLocalBindings) Get(string) (LocalBindingSnapshot, bool) {
	panic("identity: cluster resolver must never consult LocalBindings")
}

func TestDefaultClusterIdentityResolver_Resolve_Success(t *testing.T) {
	reg := &fakeRegistry{rows: map[string]struct {
		url    string
		branch string
	}{
		"spi": {url: "https://github.com/awell-health/spire", branch: "main"},
	}}
	r := &DefaultClusterIdentityResolver{Registry: reg}

	got, err := r.Resolve(context.Background(), "spi")
	if err != nil {
		t.Fatalf("Resolve: unexpected error: %v", err)
	}
	want := ClusterRepoIdentity{
		URL:        "https://github.com/awell-health/spire",
		BaseBranch: "main",
		Prefix:     "spi",
	}
	if got != want {
		t.Fatalf("Resolve: got %+v, want %+v", got, want)
	}
}

func TestDefaultClusterIdentityResolver_Resolve_NotRegistered(t *testing.T) {
	reg := &fakeRegistry{rows: map[string]struct {
		url    string
		branch string
	}{}}
	r := &DefaultClusterIdentityResolver{Registry: reg}

	_, err := r.Resolve(context.Background(), "missing")
	if err == nil {
		t.Fatal("Resolve: expected error for unregistered prefix, got nil")
	}
	if !errors.Is(err, ErrRepoNotRegistered) {
		t.Fatalf("Resolve: expected ErrRepoNotRegistered, got %v", err)
	}
}

// TestDefaultClusterIdentityResolver_Resolve_NeverTouchesLocalBindings is the
// headline invariant test: wire a LocalBindings stub whose Get method panics,
// then show Resolve succeeds without triggering it. A failure here means a
// regression has introduced a LocalBindings read into the cluster path.
func TestDefaultClusterIdentityResolver_Resolve_NeverTouchesLocalBindings(t *testing.T) {
	reg := &fakeRegistry{rows: map[string]struct {
		url    string
		branch string
	}{
		"spi": {url: "https://example.test/repo.git", branch: "trunk"},
	}}
	r := &DefaultClusterIdentityResolver{
		Registry:      reg,
		LocalBindings: panicLocalBindings{},
	}

	defer func() {
		if p := recover(); p != nil {
			t.Fatalf("Resolve touched LocalBindings (panicked): %v", p)
		}
	}()

	got, err := r.Resolve(context.Background(), "spi")
	if err != nil {
		t.Fatalf("Resolve: unexpected error: %v", err)
	}
	if got.URL != "https://example.test/repo.git" || got.BaseBranch != "trunk" || got.Prefix != "spi" {
		t.Fatalf("Resolve: got %+v, want URL/branch/prefix from shared registry", got)
	}
}

func TestDefaultClusterIdentityResolver_Resolve_NilRegistry(t *testing.T) {
	r := &DefaultClusterIdentityResolver{}
	_, err := r.Resolve(context.Background(), "spi")
	if err == nil {
		t.Fatal("Resolve: expected error for nil Registry, got nil")
	}
}

func TestDefaultClusterIdentityResolver_Resolve_EmptyPrefix(t *testing.T) {
	reg := &fakeRegistry{}
	r := &DefaultClusterIdentityResolver{Registry: reg}
	_, err := r.Resolve(context.Background(), "")
	if err == nil {
		t.Fatal("Resolve: expected error for empty prefix, got nil")
	}
}

// Compile-time assertion that DefaultClusterIdentityResolver satisfies the
// ClusterIdentityResolver interface. If this line stops compiling, the
// interface-or-impl has drifted.
var _ ClusterIdentityResolver = (*DefaultClusterIdentityResolver)(nil)
