// Package repoconfig reads and resolves spire.yaml — repo-level configuration
// that agents, the sidecar, and the wizard import to know
// how to install, test, build, and submit work for a given repository.
package repoconfig

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// RepoConfig is the top-level schema for spire.yaml.
type RepoConfig struct {
	Runtime RuntimeConfig `yaml:"runtime"`
	Agent   AgentConfig   `yaml:"agent"`
	Branch  BranchConfig  `yaml:"branch"`
	PR      PRConfig      `yaml:"pr"`
	Design  DesignConfig  `yaml:"design"`
	Cleric  ClericConfig  `yaml:"cleric"`
	Context []string      `yaml:"context"`
}

// ClericConfig controls the cleric (recovery agent) promotion policy.
//
// The cleric runs agentic by default: each new failure_signature dispatches
// an apprentice with full context. Once PromotionThreshold clean agentic
// resolutions accumulate for the same signature (and each carries a
// mechanical_recipe), subsequent recoveries short-circuit to the codified
// recipe. A single failure of a promoted recipe demotes the signature back
// to agentic.
//
// PromotionOverrides lets specific failure_signature values override the
// global threshold. Keys are failure_signature strings as produced by
// pkg/recovery (e.g. "step-failure:merge", "build-failure:implement").
type ClericConfig struct {
	// PromotionThreshold is the number of consecutive clean recoveries with
	// a mechanical_recipe required before a failure_signature is promoted
	// to the mechanical path. Defaults to DefaultClericPromotionThreshold
	// (3) when zero or negative.
	PromotionThreshold int `yaml:"promotion_threshold"`
	// PromotionOverrides maps specific failure_signature strings to a
	// per-class threshold. When a key matches the current signature, its
	// value wins over PromotionThreshold. Non-positive values fall back to
	// the global threshold.
	PromotionOverrides map[string]int `yaml:"promotion_overrides"`
}

// DesignConfig controls design bead creation behaviour.
type DesignConfig struct {
	// RequireApproval controls whether design beads are created with
	// in_progress status and a needs-human label (default: true).
	// When false, design beads are created as open without needs-human —
	// useful for automated pipelines where the agent closes its own designs.
	RequireApproval *bool `yaml:"require_approval"`
}

// RuntimeConfig describes how to install, test, build, and lint the repo.
//
// Apprentice vs CI scopes: Test/Build/Lint are the apprentice validation
// gate — they run inside sandboxed cluster pods with a ~10m wizard stale
// timeout and a cold module cache, so they must stay narrow to keep the
// implement phase under 2 min (see spi-dx5621). CITest/CIBuild/CILint are
// the broader surfaces invoked by CI/build tooling to catch cross-module
// drift (operator submodule breakage, go.mod normalization, CRD/RBAC
// drift, etc.). If a CI* field is empty, callers that want the broader
// scope fall back to the narrow variant. See spi-q3lfd3 for the original
// test split and spi-dx5621 for the build/lint parallel.
type RuntimeConfig struct {
	Language string `yaml:"language"`           // go, typescript, python, rust
	Install  string `yaml:"install"`            // e.g. "pnpm install"
	Test     string `yaml:"test"`               // apprentice gate; narrow, sandbox-safe
	CITest   string `yaml:"ci_test,omitempty"`  // CI-scope tests; optional, falls back to Test
	Build    string `yaml:"build,omitempty"`    // apprentice gate; narrow, sandbox-safe
	CIBuild  string `yaml:"ci_build,omitempty"` // CI-scope build; optional, falls back to Build
	Lint     string `yaml:"lint,omitempty"`     // apprentice gate; narrow, sandbox-safe
	CILint   string `yaml:"ci_lint,omitempty"`  // CI-scope lint; optional, falls back to Lint
}

// AgentConfig controls autonomous agent behaviour.
type AgentConfig struct {
	Backend        string       `yaml:"backend"`              // execution backend: "process", "docker", "k8s"
	Model          string       `yaml:"model"`                // default model for this repo
	Provider       string       `yaml:"provider,omitempty"`   // default AI provider: "claude", "codex", "cursor"
	MaxTurns       int          `yaml:"max-turns"`            // safety limit
	MaxApprentices int          `yaml:"max-apprentices"`      // cap on concurrent apprentices per wizard (0 = unset; resolves to DefaultMaxApprentices)
	Stale          string       `yaml:"stale"`                // warning: wizard exceeded guidelines (e.g. "10m")
	Timeout        string       `yaml:"timeout"`              // fatal: tower kills the wizard (e.g. "15m")
	DesignTimeout  string       `yaml:"design-timeout"`       // timeout for design phase (e.g. "10m")
	Docker         DockerConfig `yaml:"docker"`               // Docker spawner configuration
	Formula        string       `yaml:"formula,omitempty"`    // default formula name
}

// DockerConfig controls Docker-based agent spawning.
type DockerConfig struct {
	Image        string   `yaml:"image"`         // container image (default: ghcr.io/awell-health/spire-agent:latest)
	Network      string   `yaml:"network"`       // Docker network mode (default: "host")
	ExtraVolumes []string `yaml:"extra-volumes"` // additional -v mounts (host:container)
	ExtraEnv     []string `yaml:"extra-env"`     // additional -e KEY=VALUE entries
}

// BranchConfig controls branch naming.
type BranchConfig struct {
	Base    string `yaml:"base"`    // default: "main"
	Pattern string `yaml:"pattern"` // default: "feat/{bead-id}"
}

// PRConfig controls pull request creation.
type PRConfig struct {
	AutoMerge bool     `yaml:"auto-merge"`
	Reviewers []string `yaml:"reviewers,omitempty"`
	Labels    []string `yaml:"labels,omitempty"`
}

// Load reads spire.yaml from the given directory, walking up the directory
// tree to find it (like .gitignore resolution). If no file is found, it
// returns sensible defaults based on auto-detection of the repo's runtime.
func Load(dir string) (*RepoConfig, error) {
	// Resolve to absolute path
	abs, err := filepath.Abs(dir)
	if err != nil {
		abs = dir
	}

	// Walk up looking for spire.yaml
	cfg, foundDir, err := findAndParse(abs)
	if err != nil {
		return nil, err
	}

	// If we found a file, the detection dir is where it lives; otherwise use original dir
	detectDir := abs
	if foundDir != "" {
		detectDir = foundDir
	}

	// Apply defaults (auto-detected values fill in blanks)
	applyDefaults(cfg, detectDir)

	if err := validate(cfg); err != nil {
		return nil, err
	}

	return cfg, nil
}

// validate checks config invariants.
func validate(cfg *RepoConfig) error {
	if cfg.Agent.Stale != "" && cfg.Agent.Timeout != "" {
		stale, err1 := time.ParseDuration(cfg.Agent.Stale)
		timeout, err2 := time.ParseDuration(cfg.Agent.Timeout)
		if err1 == nil && err2 == nil && stale >= timeout {
			return fmt.Errorf("spire.yaml: agent.stale (%s) must be less than agent.timeout (%s)", cfg.Agent.Stale, cfg.Agent.Timeout)
		}
	}
	return nil
}

// findAndParse walks up from dir looking for spire.yaml. Returns the parsed
// config and the directory where it was found. If not found, returns an
// empty config with foundDir = "".
func findAndParse(dir string) (*RepoConfig, string, error) {
	current := dir
	for {
		candidate := filepath.Join(current, "spire.yaml")
		data, err := os.ReadFile(candidate)
		if err == nil {
			var cfg RepoConfig
			if err := yaml.Unmarshal(data, &cfg); err != nil {
				return nil, "", err
			}
			return &cfg, current, nil
		}

		parent := filepath.Dir(current)
		if parent == current {
			// Reached filesystem root — no spire.yaml found
			return &RepoConfig{}, "", nil
		}
		current = parent
	}
}

// applyDefaults fills in zero-value fields with auto-detected or hardcoded defaults.
func applyDefaults(cfg *RepoConfig, dir string) {
	detected := detectRuntime(dir)

	// Runtime defaults from detection
	if cfg.Runtime.Language == "" {
		cfg.Runtime.Language = detected.Language
	}
	if cfg.Runtime.Install == "" {
		cfg.Runtime.Install = detected.Install
	}
	if cfg.Runtime.Test == "" {
		cfg.Runtime.Test = detected.Test
	}

	// Policy defaults (model, stale, timeout, branch) are NOT set here.
	// Load() returns zero values for unset fields; consumers use the
	// Resolve*() functions from resolve.go to apply the precedence chain
	// (formula phase > spire.yaml > system default).
}

// detectRuntime inspects the given directory for known project files and
// returns a RuntimeConfig with language, install, and test commands.
func detectRuntime(dir string) RuntimeConfig {
	// Go
	if fileExists(filepath.Join(dir, "go.mod")) {
		return RuntimeConfig{
			Language: "go",
			Install:  "",
			Test:     "go test ./...",
		}
	}

	// Node.js / TypeScript
	if fileExists(filepath.Join(dir, "package.json")) {
		lang := "typescript"
		// Could check for tsconfig.json to be sure, but typescript is the
		// common case for agent-managed repos.

		switch {
		case fileExists(filepath.Join(dir, "pnpm-lock.yaml")):
			return RuntimeConfig{Language: lang, Install: "pnpm install", Test: "pnpm test"}
		case fileExists(filepath.Join(dir, "yarn.lock")):
			return RuntimeConfig{Language: lang, Install: "yarn", Test: "yarn test"}
		default:
			return RuntimeConfig{Language: lang, Install: "npm install", Test: "npm test"}
		}
	}

	// Python
	if fileExists(filepath.Join(dir, "pyproject.toml")) || fileExists(filepath.Join(dir, "requirements.txt")) {
		return RuntimeConfig{
			Language: "python",
			Install:  "pip install -r requirements.txt",
			Test:     "pytest",
		}
	}

	// Rust
	if fileExists(filepath.Join(dir, "Cargo.toml")) {
		return RuntimeConfig{
			Language: "rust",
			Install:  "",
			Test:     "cargo test",
		}
	}

	return RuntimeConfig{Language: "unknown"}
}

// GenerateYAML renders a spire.yaml string from auto-detected defaults for
// the given directory. Used by `spire repo add` to write a starter config.
func GenerateYAML(dir string) string {
	return GenerateYAMLFromValues(DetectDefaults(dir))
}

// YAMLValues holds the configurable fields for spire.yaml generation.
// Used by GenerateYAMLFromValues to render the file.
type YAMLValues struct {
	Language string
	Install  string
	Test     string
	Build    string
	Lint     string
	Model    string
	Timeout  string
	// DetectedHint is a human-readable string describing what was detected
	// (e.g. "typescript (package.json + pnpm-lock.yaml)"). Informational only.
	DetectedHint string
}

// DetectDefaults inspects the given directory and returns YAMLValues
// populated with auto-detected defaults. Callers can override individual
// fields before passing to GenerateYAMLFromValues.
func DetectDefaults(dir string) YAMLValues {
	rt := detectRuntime(dir)
	return YAMLValues{
		Language:     rt.Language,
		Install:      rt.Install,
		Test:         rt.Test,
		Build:        rt.Build,
		Lint:         rt.Lint,
		Model:        DefaultModel,
		Timeout:      DefaultTimeout,
		DetectedHint: detectHint(dir),
	}
}

// detectHint returns a human-readable string describing what project markers
// were found (e.g. "typescript (package.json + pnpm-lock.yaml)").
func detectHint(dir string) string {
	type marker struct {
		file string
		lang string
	}
	markers := []marker{
		{"go.mod", "go"},
		{"Cargo.toml", "rust"},
		{"pyproject.toml", "python"},
		{"requirements.txt", "python"},
		{"package.json", "typescript"},
	}

	for _, m := range markers {
		if !fileExists(filepath.Join(dir, m.file)) {
			continue
		}
		files := m.file
		// For Node.js, include lock file in the hint
		if m.file == "package.json" {
			switch {
			case fileExists(filepath.Join(dir, "pnpm-lock.yaml")):
				files += " + pnpm-lock.yaml"
			case fileExists(filepath.Join(dir, "yarn.lock")):
				files += " + yarn.lock"
			case fileExists(filepath.Join(dir, "package-lock.json")):
				files += " + package-lock.json"
			}
		}
		return m.lang + " (" + files + ")"
	}
	return "unknown"
}

// GenerateYAMLFromValues renders a spire.yaml string from explicit values.
func GenerateYAMLFromValues(v YAMLValues) string {
	install := v.Install
	if install == "" {
		install = "# (none needed)"
	}

	var s string
	s += "runtime:\n"
	s += "  language: " + v.Language + "\n"
	s += "  install: " + install + "\n"
	s += "  test: " + v.Test + "\n"
	if v.Build != "" {
		s += "  build: " + v.Build + "\n"
	} else {
		s += "  # build:\n"
	}
	if v.Lint != "" {
		s += "  lint: " + v.Lint + "\n"
	} else {
		s += "  # lint:\n"
	}
	s += "\n"
	s += "agent:\n"
	s += "  model: " + v.Model + "\n"
	s += "  stale: 10m\n"
	s += "  timeout: " + v.Timeout + "\n"
	s += "\n"
	s += "branch:\n"
	s += "  base: main\n"
	s += "  pattern: \"feat/{bead-id}\"\n"
	s += "\n"
	s += "pr:\n"
	s += "  auto-merge: false\n"
	s += "  # reviewers: []\n"
	s += "  # labels: []\n"
	s += "\n"
	s += "design:\n"
	s += "  require_approval: true  # set false for automated pipelines\n"
	s += "\n"
	s += "context:\n"
	s += "  - CLAUDE.md\n"
	s += "  - SPIRE.md\n"

	return s
}

// FormatResolved renders the fully-resolved config as human-readable YAML-like output.
func FormatResolved(cfg *RepoConfig) string {
	var s string

	s += "runtime:\n"
	s += "  language: " + cfg.Runtime.Language + "\n"
	install := cfg.Runtime.Install
	if install == "" {
		install = "(none)"
	}
	s += "  install: " + install + "\n"
	s += "  test: " + cfg.Runtime.Test + "\n"
	if cfg.Runtime.Build != "" {
		s += "  build: " + cfg.Runtime.Build + "\n"
	}
	if cfg.Runtime.Lint != "" {
		s += "  lint: " + cfg.Runtime.Lint + "\n"
	}

	s += "agent:\n"
	s += "  model: " + ResolveModel("", cfg.Agent.Model) + "\n"
	if cfg.Agent.MaxTurns > 0 {
		s += "  max-turns: " + itoa(cfg.Agent.MaxTurns) + "\n"
	}
	s += "  stale: " + ResolveStale(cfg.Agent.Stale) + "\n"
	s += "  timeout: " + ResolveTimeout("", cfg.Agent.Timeout, DefaultTimeout) + "\n"

	s += "branch:\n"
	s += "  base: " + ResolveBranchBase(cfg.Branch.Base) + "\n"
	s += "  pattern: " + ResolveBranchPattern(cfg.Branch.Pattern) + "\n"

	s += "pr:\n"
	if cfg.PR.AutoMerge {
		s += "  auto-merge: true\n"
	} else {
		s += "  auto-merge: false\n"
	}
	if len(cfg.PR.Reviewers) > 0 {
		s += "  reviewers: [" + joinStrings(cfg.PR.Reviewers) + "]\n"
	}
	if len(cfg.PR.Labels) > 0 {
		s += "  labels: [" + joinStrings(cfg.PR.Labels) + "]\n"
	}

	s += "design:\n"
	if ResolveDesignRequireApproval(cfg.Design.RequireApproval) {
		s += "  require_approval: true\n"
	} else {
		s += "  require_approval: false\n"
	}

	if len(cfg.Context) > 0 {
		s += "context:\n"
		for _, c := range cfg.Context {
			s += "  - " + c + "\n"
		}
	}

	return s
}

// ResolveBranch returns the branch name for a bead by substituting {bead-id}
// into the config's Branch.Pattern. If the pattern is empty, it falls back to
// "feat/{bead-id}".
func (c *RepoConfig) ResolveBranch(beadID string) string {
	return ResolveBranchName(beadID, c.Branch.Pattern)
}

// ResolveBranchName substitutes {bead-id} into the given pattern.
// If pattern is empty, it defaults to "feat/{bead-id}".
func ResolveBranchName(beadID, pattern string) string {
	if pattern == "" {
		pattern = DefaultBranchPattern
	}
	return strings.ReplaceAll(pattern, "{bead-id}", beadID)
}

// BranchGlob returns a glob pattern that matches branches created by the
// configured Branch.Pattern. For example, "feat/{bead-id}" yields "feat/*".
// Used by doctor.go to scan for stale branches regardless of the configured
// pattern.
func (c *RepoConfig) BranchGlob() string {
	pattern := c.Branch.Pattern
	if pattern == "" {
		pattern = DefaultBranchPattern
	}
	return strings.ReplaceAll(pattern, "{bead-id}", "*")
}

// BranchPrefix returns the static prefix before the {bead-id} placeholder, or
// "" if the pattern has no prefix. Used to extract bead IDs from branch names.
func (c *RepoConfig) BranchPrefix() string {
	pattern := c.Branch.Pattern
	if pattern == "" {
		pattern = DefaultBranchPattern
	}
	idx := strings.Index(pattern, "{bead-id}")
	if idx < 0 {
		return ""
	}
	return pattern[:idx]
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func itoa(n int) string {
	return fmt.Sprintf("%d", n)
}

func joinStrings(ss []string) string {
	result := ""
	for i, s := range ss {
		if i > 0 {
			result += ", "
		}
		result += s
	}
	return result
}
