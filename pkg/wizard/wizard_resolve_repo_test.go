package wizard

import (
	"strings"
	"testing"

	"github.com/awell-health/spire/pkg/config"
)

// TestResolveRepo_UnboundPrefixReturnsError is the layer-1 guard for
// the silent-fallback chain documented in spi-rpuzs6. ResolveRepo must
// refuse to return an empty path when a prefix has no local binding.
// This is the inner-most fence; the executor bridge and graph
// interpreter backstop it, but all three must hold independently.
func TestResolveRepo_UnboundPrefixReturnsError(t *testing.T) {
	deps := &Deps{
		LoadConfig: func() (*config.SpireConfig, error) {
			return &config.SpireConfig{
				Instances: map[string]*config.Instance{
					// "spd" prefix is intentionally missing — simulates an
					// unbound prefix. The tower's repos table has the
					// remote URL but no local checkout is registered.
					"spi": {Prefix: "spi", Path: "/tmp/spire"},
				},
			}, nil
		},
		ResolveDatabase: func(cfg *config.SpireConfig) (string, bool) {
			return "test_db", false
		},
		RawDoltQuery:  func(sql string) (string, error) { return "", nil },
		ParseDoltRows: func(out string, cols []string) []map[string]string { return nil },
		SQLEscape:     func(s string) string { return s },
	}

	_, _, _, err := ResolveRepo("spd-1jd", deps)
	if err == nil {
		t.Fatal("ResolveRepo(\"spd-1jd\") = nil error, want error (prefix is unbound)")
	}
	if !strings.Contains(err.Error(), "spd") {
		t.Errorf("error %q should name the unbound prefix 'spd'", err)
	}
}

// TestResolveRepo_BoundPrefixReturnsPath confirms the happy path still
// works: a prefix registered in Instances resolves to its local path.
func TestResolveRepo_BoundPrefixReturnsPath(t *testing.T) {
	deps := &Deps{
		LoadConfig: func() (*config.SpireConfig, error) {
			return &config.SpireConfig{
				Instances: map[string]*config.Instance{
					"spi": {Prefix: "spi", Path: "/tmp/spire"},
				},
			}, nil
		},
		ResolveDatabase: func(cfg *config.SpireConfig) (string, bool) {
			return "test_db", false
		},
		RawDoltQuery: func(sql string) (string, error) {
			// Return a fake repos row so repoURL + baseBranch populate.
			return "| repo_url | branch |\n| git@github.com:example/spire.git | main |", nil
		},
		ParseDoltRows: func(out string, cols []string) []map[string]string {
			return []map[string]string{{"repo_url": "git@github.com:example/spire.git", "branch": "main"}}
		},
		SQLEscape: func(s string) string { return s },
	}

	path, url, branch, err := ResolveRepo("spi-abc", deps)
	if err != nil {
		t.Fatalf("ResolveRepo(\"spi-abc\") err = %v, want nil", err)
	}
	if path != "/tmp/spire" {
		t.Errorf("path = %q, want /tmp/spire", path)
	}
	if url != "git@github.com:example/spire.git" {
		t.Errorf("url = %q, want git@github.com:example/spire.git", url)
	}
	if branch != "main" {
		t.Errorf("branch = %q, want main", branch)
	}
}

// TestResolveRepo_SharedRepoLookupIsCached verifies the repos-table lookup
// is memoized: a second resolve for the same (database, prefix) within the
// cache TTL must not re-query. Uncached, every steward cycle forked one
// `dolt sql` subprocess per ready bead just to re-read a row that never
// changes mid-run.
func TestResolveRepo_SharedRepoLookupIsCached(t *testing.T) {
	calls := 0
	deps := &Deps{
		LoadConfig: func() (*config.SpireConfig, error) {
			return &config.SpireConfig{
				Instances: map[string]*config.Instance{
					"cch": {Prefix: "cch", Path: "/tmp/cached-repo"},
				},
			}, nil
		},
		ResolveDatabase: func(cfg *config.SpireConfig) (string, bool) {
			return "cache_test_db", false
		},
		RawDoltQuery: func(sql string) (string, error) {
			calls++
			return "| repo_url | branch |", nil
		},
		ParseDoltRows: func(out string, cols []string) []map[string]string {
			return []map[string]string{{"repo_url": "git@github.com:example/cached.git", "branch": "main"}}
		},
		SQLEscape: func(s string) string { return s },
	}

	for i := 0; i < 3; i++ {
		_, url, branch, err := ResolveRepo("cch-abc", deps)
		if err != nil {
			t.Fatalf("resolve %d: %v", i, err)
		}
		if url != "git@github.com:example/cached.git" || branch != "main" {
			t.Fatalf("resolve %d returned (%q, %q), want cached row", i, url, branch)
		}
	}
	if calls != 1 {
		t.Errorf("RawDoltQuery called %d times, want 1 (cache should serve repeats)", calls)
	}
}
