package repoconfig

import (
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestRuntimeConfigRoundTrip verifies the six-slot apprentice/CI scope
// split (spi-q3lfd3 for test/ci_test, spi-dx5621 for build/ci_build and
// lint/ci_lint) parses and re-marshals cleanly.
func TestRuntimeConfigRoundTrip(t *testing.T) {
	input := []byte(`runtime:
  language: go
  install: ""
  test: go test ./cmd/spire/ -timeout=60s
  ci_test: go test ./... -timeout=60s
  build: go build ./cmd/spire/
  ci_build: go mod tidy && go build ./...
  lint: go vet ./cmd/spire/
  ci_lint: go vet ./... && make verify-rbac
`)

	var cfg RepoConfig
	if err := yaml.Unmarshal(input, &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	got := cfg.Runtime
	want := RuntimeConfig{
		Language: "go",
		Test:     "go test ./cmd/spire/ -timeout=60s",
		CITest:   "go test ./... -timeout=60s",
		Build:    "go build ./cmd/spire/",
		CIBuild:  "go mod tidy && go build ./...",
		Lint:     "go vet ./cmd/spire/",
		CILint:   "go vet ./... && make verify-rbac",
	}
	if got != want {
		t.Fatalf("runtime mismatch:\n got:  %+v\n want: %+v", got, want)
	}

	// Re-marshal and unmarshal to confirm the tags round-trip.
	out, err := yaml.Marshal(&cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var rt RepoConfig
	if err := yaml.Unmarshal(out, &rt); err != nil {
		t.Fatalf("re-unmarshal: %v", err)
	}
	if rt.Runtime != want {
		t.Fatalf("round-trip mismatch:\n got:  %+v\n want: %+v", rt.Runtime, want)
	}
}

// TestRuntimeConfigOmitEmptyCI ensures empty CI* fields stay out of the
// marshaled output — older towers that don't set them must not crash.
func TestRuntimeConfigOmitEmptyCI(t *testing.T) {
	cfg := RepoConfig{Runtime: RuntimeConfig{
		Language: "go",
		Test:     "go test ./cmd/spire/",
		Build:    "go build ./cmd/spire/",
		Lint:     "go vet ./cmd/spire/",
	}}
	out, err := yaml.Marshal(&cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(out)
	for _, key := range []string{"ci_test:", "ci_build:", "ci_lint:"} {
		if containsKey(s, key) {
			t.Errorf("expected %q to be omitted when empty; got:\n%s", key, s)
		}
	}
}

// TestLoadedSpireYAMLHasNarrowApprenticeGate asserts the repo's own
// spire.yaml keeps the narrow apprentice-gate shape. Guards against a
// future contributor "helpfully" re-broadening build/lint (see spi-dx5621).
func TestLoadedSpireYAMLHasNarrowApprenticeGate(t *testing.T) {
	repoRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	cfg, err := Load(repoRoot)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	rt := cfg.Runtime
	if rt.Build != "go build ./cmd/spire/" {
		t.Errorf("apprentice build gate broadened: %q", rt.Build)
	}
	if rt.Lint != "go vet ./cmd/spire/" {
		t.Errorf("apprentice lint gate broadened: %q", rt.Lint)
	}
	if rt.CIBuild == "" {
		t.Error("ci_build must carry the broad build coverage")
	}
	if rt.CILint == "" {
		t.Error("ci_lint must carry the broad lint coverage (verify-rbac, CRD drift, etc.)")
	}
}

func containsKey(s, key string) bool {
	for i := 0; i+len(key) <= len(s); i++ {
		if s[i:i+len(key)] == key {
			return true
		}
	}
	return false
}
