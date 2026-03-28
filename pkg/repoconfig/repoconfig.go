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
	Context []string      `yaml:"context"`
}

// RuntimeConfig describes how to install, test, build, and lint the repo.
type RuntimeConfig struct {
	Language string `yaml:"language"`        // go, typescript, python, rust
	Install  string `yaml:"install"`         // e.g. "pnpm install"
	Test     string `yaml:"test"`            // e.g. "pnpm test"
	Build    string `yaml:"build,omitempty"` // optional
	Lint     string `yaml:"lint,omitempty"`  // optional
}

// AgentConfig controls autonomous agent behaviour.
type AgentConfig struct {
	Backend       string       `yaml:"backend"`           // execution backend: "process", "docker", "k8s"
	Model         string       `yaml:"model"`             // default model for this repo
	MaxTurns      int          `yaml:"max-turns"`         // safety limit
	Stale         string       `yaml:"stale"`             // warning: wizard exceeded guidelines (e.g. "10m")
	Timeout       string       `yaml:"timeout"`           // fatal: tower kills the wizard (e.g. "15m")
	DesignTimeout string       `yaml:"design-timeout"`    // timeout for design phase (e.g. "10m")
	Docker        DockerConfig `yaml:"docker"`            // Docker spawner configuration
	Formula       string       `yaml:"formula,omitempty"` // default formula name
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

	// Agent defaults
	if cfg.Agent.Model == "" {
		cfg.Agent.Model = "claude-sonnet-4-6"
	}
	// MaxTurns 0 means unlimited — the timeout is the only gate.
	// Don't default to 30; let wizard.go handle the fallback.
	if cfg.Agent.Stale == "" {
		cfg.Agent.Stale = "10m"
	}
	if cfg.Agent.Timeout == "" {
		cfg.Agent.Timeout = "15m"
	}
	if cfg.Agent.DesignTimeout == "" {
		cfg.Agent.DesignTimeout = "10m"
	}

	// Branch defaults
	if cfg.Branch.Base == "" {
		cfg.Branch.Base = "main"
	}
	if cfg.Branch.Pattern == "" {
		cfg.Branch.Pattern = "feat/{bead-id}"
	}

	// PR defaults: auto-merge defaults to false (zero value), so nothing to do
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
	rt := detectRuntime(dir)

	install := rt.Install
	if install == "" {
		install = "# (none needed)"
	}

	var s string
	s += "runtime:\n"
	s += "  language: " + rt.Language + "\n"
	s += "  install: " + install + "\n"
	s += "  test: " + rt.Test + "\n"
	s += "  # build:\n"
	s += "  # lint:\n"
	s += "\n"
	s += "agent:\n"
	s += "  model: claude-opus-4-6\n"
	s += "  stale: 10m\n"
	s += "  timeout: 15m\n"
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
	s += "  model: " + cfg.Agent.Model + "\n"
	if cfg.Agent.MaxTurns > 0 {
		s += "  max-turns: " + itoa(cfg.Agent.MaxTurns) + "\n"
	}
	s += "  stale: " + cfg.Agent.Stale + "\n"
	s += "  timeout: " + cfg.Agent.Timeout + "\n"

	s += "branch:\n"
	s += "  base: " + cfg.Branch.Base + "\n"
	s += "  pattern: " + cfg.Branch.Pattern + "\n"

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
		pattern = "feat/{bead-id}"
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
		pattern = "feat/{bead-id}"
	}
	return strings.ReplaceAll(pattern, "{bead-id}", "*")
}

// BranchPrefix returns the static prefix before the {bead-id} placeholder, or
// "" if the pattern has no prefix. Used to extract bead IDs from branch names.
func (c *RepoConfig) BranchPrefix() string {
	pattern := c.Branch.Pattern
	if pattern == "" {
		pattern = "feat/{bead-id}"
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
