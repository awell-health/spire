package repoconfig

import (
	"os"
	"testing"
)

func TestResolveModel(t *testing.T) {
	tests := []struct {
		name       string
		phaseModel string
		repoModel  string
		want       string
	}{
		{"zero/zero → system default", "", "", DefaultModel},
		{"zero/repo → repo value", "", "claude-opus-4-6", "claude-opus-4-6"},
		{"phase/zero → phase value", "claude-haiku-4-5-20251001", "", "claude-haiku-4-5-20251001"},
		{"phase/repo → phase wins", "claude-haiku-4-5-20251001", "claude-opus-4-6", "claude-haiku-4-5-20251001"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ResolveModel(tt.phaseModel, tt.repoModel)
			if got != tt.want {
				t.Errorf("ResolveModel(%q, %q) = %q, want %q", tt.phaseModel, tt.repoModel, got, tt.want)
			}
		})
	}
}

func TestResolveTimeout(t *testing.T) {
	tests := []struct {
		name           string
		phaseTimeout   string
		repoTimeout    string
		defaultTimeout string
		want           string
	}{
		{"all empty → system default", "", "", "", DefaultTimeout},
		{"repo set → repo value", "", "20m", "", "20m"},
		{"phase set → phase value", "5m", "", "", "5m"},
		{"phase wins over repo", "5m", "20m", "", "5m"},
		{"custom default used", "", "", "30m", "30m"},
		{"phase wins over all", "5m", "20m", "30m", "5m"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ResolveTimeout(tt.phaseTimeout, tt.repoTimeout, tt.defaultTimeout)
			if got != tt.want {
				t.Errorf("ResolveTimeout(%q, %q, %q) = %q, want %q",
					tt.phaseTimeout, tt.repoTimeout, tt.defaultTimeout, got, tt.want)
			}
		})
	}
}

func TestResolveStale(t *testing.T) {
	tests := []struct {
		name      string
		repoStale string
		want      string
	}{
		{"empty → default", "", DefaultStale},
		{"set → repo value", "5m", "5m"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ResolveStale(tt.repoStale)
			if got != tt.want {
				t.Errorf("ResolveStale(%q) = %q, want %q", tt.repoStale, got, tt.want)
			}
		})
	}
}

func TestResolveBranchBase(t *testing.T) {
	tests := []struct {
		name string
		base string
		want string
	}{
		{"empty → main", "", DefaultBranchBase},
		{"set → value", "develop", "develop"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ResolveBranchBase(tt.base)
			if got != tt.want {
				t.Errorf("ResolveBranchBase(%q) = %q, want %q", tt.base, got, tt.want)
			}
		})
	}
}

func TestResolveBranchPattern(t *testing.T) {
	tests := []struct {
		name    string
		pattern string
		want    string
	}{
		{"empty → default", "", DefaultBranchPattern},
		{"set → value", "feature/{bead-id}", "feature/{bead-id}"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ResolveBranchPattern(tt.pattern)
			if got != tt.want {
				t.Errorf("ResolveBranchPattern(%q) = %q, want %q", tt.pattern, got, tt.want)
			}
		})
	}
}

func TestResolveDesignTimeout(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"empty → default", "", DefaultDesignTimeout},
		{"set → value", "20m", "20m"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ResolveDesignTimeout(tt.input)
			if got != tt.want {
				t.Errorf("ResolveDesignTimeout(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

// TestLoadSparseYAMLReturnsZeroValues verifies that Load() on a sparse spire.yaml
// returns zero values for policy fields (model, stale, timeout, branch).
func TestLoadSparseYAMLReturnsZeroValues(t *testing.T) {
	dir := t.TempDir()

	// Write a minimal spire.yaml with only runtime info.
	yaml := "runtime:\n  language: go\n  test: go test ./...\n"
	if err := writeFile(dir, "spire.yaml", yaml); err != nil {
		t.Fatal(err)
	}
	// Also create go.mod so detectRuntime doesn't override language.
	if err := writeFile(dir, "go.mod", "module test\n"); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	// Policy fields should be zero values (empty strings).
	if cfg.Agent.Model != "" {
		t.Errorf("Agent.Model = %q, want empty", cfg.Agent.Model)
	}
	if cfg.Agent.Stale != "" {
		t.Errorf("Agent.Stale = %q, want empty", cfg.Agent.Stale)
	}
	if cfg.Agent.Timeout != "" {
		t.Errorf("Agent.Timeout = %q, want empty", cfg.Agent.Timeout)
	}
	if cfg.Agent.DesignTimeout != "" {
		t.Errorf("Agent.DesignTimeout = %q, want empty", cfg.Agent.DesignTimeout)
	}
	if cfg.Branch.Base != "" {
		t.Errorf("Branch.Base = %q, want empty", cfg.Branch.Base)
	}
	if cfg.Branch.Pattern != "" {
		t.Errorf("Branch.Pattern = %q, want empty", cfg.Branch.Pattern)
	}

	// Runtime fields should still be filled from the YAML.
	if cfg.Runtime.Language != "go" {
		t.Errorf("Runtime.Language = %q, want %q", cfg.Runtime.Language, "go")
	}
	if cfg.Runtime.Test != "go test ./..." {
		t.Errorf("Runtime.Test = %q, want %q", cfg.Runtime.Test, "go test ./...")
	}
}

// TestLoadFullYAMLPreservesExplicitValues verifies that Load() preserves
// explicit values from a full spire.yaml.
func TestLoadFullYAMLPreservesExplicitValues(t *testing.T) {
	dir := t.TempDir()

	yaml := `runtime:
  language: typescript
  install: pnpm install
  test: pnpm test
agent:
  model: claude-opus-4-6
  stale: 8m
  timeout: 20m
  design-timeout: 12m
branch:
  base: develop
  pattern: "feature/{bead-id}"
`
	if err := writeFile(dir, "spire.yaml", yaml); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if cfg.Agent.Model != "claude-opus-4-6" {
		t.Errorf("Agent.Model = %q, want %q", cfg.Agent.Model, "claude-opus-4-6")
	}
	if cfg.Agent.Stale != "8m" {
		t.Errorf("Agent.Stale = %q, want %q", cfg.Agent.Stale, "8m")
	}
	if cfg.Agent.Timeout != "20m" {
		t.Errorf("Agent.Timeout = %q, want %q", cfg.Agent.Timeout, "20m")
	}
	if cfg.Agent.DesignTimeout != "12m" {
		t.Errorf("Agent.DesignTimeout = %q, want %q", cfg.Agent.DesignTimeout, "12m")
	}
	if cfg.Branch.Base != "develop" {
		t.Errorf("Branch.Base = %q, want %q", cfg.Branch.Base, "develop")
	}
	if cfg.Branch.Pattern != "feature/{bead-id}" {
		t.Errorf("Branch.Pattern = %q, want %q", cfg.Branch.Pattern, "feature/{bead-id}")
	}
}

func writeFile(dir, name, content string) error {
	return os.WriteFile(dir+"/"+name, []byte(content), 0644)
}
