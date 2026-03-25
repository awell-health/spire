package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	toml "github.com/pelletier/go-toml/v2"

	"github.com/awell-health/spire/pkg/repoconfig"
)

// FormulaV2 represents a v2 formula that configures the universal phase pipeline.
type FormulaV2 struct {
	Name        string                 `toml:"name"`
	Description string                 `toml:"description"`
	Version     int                    `toml:"version"`
	Phases      map[string]PhaseConfig `toml:"phases"`
	Vars        map[string]FormulaVar  `toml:"vars"`
}

// PhaseConfig configures a single phase in the pipeline.
type PhaseConfig struct {
	Timeout        string          `toml:"timeout,omitempty"`
	Model          string          `toml:"model,omitempty"`
	Context        []string        `toml:"context,omitempty"`
	RevisionPolicy *RevisionPolicy `toml:"revision_policy,omitempty"`
	// Execution directives
	Role          string `toml:"role,omitempty"`           // human | apprentice | sage | skip
	Dispatch      string `toml:"dispatch,omitempty"`       // direct | wave
	VerdictOnly   bool   `toml:"verdict_only,omitempty"`   // sage: produce verdict only
	Judgment      bool   `toml:"judgment,omitempty"`        // executor judges review feedback
	StagingBranch string `toml:"staging_branch,omitempty"` // branch pattern for wave merges
	MergeStrategy string `toml:"strategy,omitempty"`       // squash | merge | rebase
	Auto          bool   `toml:"auto,omitempty"`           // auto-execute without human gate
	NoHandoff     bool   `toml:"no_handoff,omitempty"`     // apprentice: skip review handoff
	Worktree      bool   `toml:"worktree,omitempty"`       // run in isolated worktree
}

// GetRole returns the phase role, defaulting to "apprentice".
func (pc PhaseConfig) GetRole() string {
	if pc.Role != "" {
		return pc.Role
	}
	return "apprentice"
}

// GetDispatch returns the dispatch mode, defaulting to "direct".
func (pc PhaseConfig) GetDispatch() string {
	if pc.Dispatch != "" {
		return pc.Dispatch
	}
	return "direct"
}

// GetMergeStrategy returns the merge strategy, defaulting to "squash".
func (pc PhaseConfig) GetMergeStrategy() string {
	if pc.MergeStrategy != "" {
		return pc.MergeStrategy
	}
	return "squash"
}

// RevisionPolicy configures review loop behavior (review phase only).
type RevisionPolicy struct {
	MaxRounds    int    `toml:"max_rounds"`
	ArbiterModel string `toml:"arbiter_model,omitempty"`
}

// FormulaVar defines a variable accepted by the formula.
type FormulaVar struct {
	Description string `toml:"description"`
	Required    bool   `toml:"required"`
}

// LoadFormulaV2 reads and parses a v2 formula from a TOML file.
func LoadFormulaV2(path string) (*FormulaV2, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read formula: %w", err)
	}
	return ParseFormulaV2(data)
}

// ParseFormulaV2 parses v2 formula from TOML bytes.
func ParseFormulaV2(data []byte) (*FormulaV2, error) {
	var f FormulaV2
	if err := toml.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("parse formula v2: %w", err)
	}
	if f.Version != 2 {
		return nil, fmt.Errorf("expected formula version 2, got %d", f.Version)
	}
	// Validate phase names
	for name := range f.Phases {
		if !isValidPhase(name) {
			return nil, fmt.Errorf("unknown phase %q in formula", name)
		}
	}
	return &f, nil
}

// EnabledPhases returns the ordered list of enabled phases for this formula.
// Order follows validPhases (design, plan, implement, review, merge).
func (f *FormulaV2) EnabledPhases() []string {
	var enabled []string
	for _, p := range validPhases {
		if _, ok := f.Phases[p]; ok {
			enabled = append(enabled, p)
		}
	}
	return enabled
}

// PhaseEnabled checks if a specific phase is enabled in this formula.
func (f *FormulaV2) PhaseEnabled(phase string) bool {
	_, ok := f.Phases[phase]
	return ok
}

// GetRevisionPolicy returns the revision policy for the review phase.
// Returns default values if not configured.
func (f *FormulaV2) GetRevisionPolicy() RevisionPolicy {
	if review, ok := f.Phases["review"]; ok && review.RevisionPolicy != nil {
		rp := *review.RevisionPolicy
		if rp.MaxRounds == 0 {
			rp.MaxRounds = 3
		}
		if rp.ArbiterModel == "" {
			rp.ArbiterModel = "claude-opus-4-6"
		}
		return rp
	}
	return RevisionPolicy{MaxRounds: 3, ArbiterModel: "claude-opus-4-6"}
}

// DefaultFormulaMap maps bead types to default formula names.
// Can be overridden by tower config in the future.
var DefaultFormulaMap = map[string]string{
	"task":    "spire-agent-work",
	"bug":     "spire-bugfix",
	"epic":    "spire-epic",
	"chore":   "spire-agent-work",
	"feature": "spire-agent-work",
}

// ResolveFormula determines which formula to use for a bead.
// Resolution order:
//  1. Bead label formula:<name> (explicit override)
//  2. Bead type → DefaultFormulaMap
//  3. spire.yaml agent.formula
//  4. Fall back to "spire-agent-work"
func ResolveFormula(bead Bead) (*FormulaV2, error) {
	name := resolveFormulaName(bead)
	path, err := FindFormula(name)
	if err != nil {
		// If the resolved formula doesn't exist, fall back to default
		if name != "spire-agent-work" {
			path, err = FindFormula("spire-agent-work")
			if err != nil {
				return nil, fmt.Errorf("resolve formula for %s: %w", bead.ID, err)
			}
		} else {
			return nil, fmt.Errorf("resolve formula for %s: %w", bead.ID, err)
		}
	}
	return LoadFormulaV2(path)
}

// resolveFormulaName returns the formula name for a bead without loading it.
func resolveFormulaName(bead Bead) string {
	// 1. Check bead labels for formula:<name>
	for _, l := range bead.Labels {
		if strings.HasPrefix(l, "formula:") {
			return l[len("formula:"):]
		}
	}

	// 2. Check bead type → formula mapping
	if name, ok := DefaultFormulaMap[bead.Type]; ok {
		return name
	}

	// 3. Check spire.yaml agent.formula
	// Load repo config from CWD (best effort)
	if cfg, err := repoconfig.Load("."); err == nil && cfg.Agent.Formula != "" {
		return cfg.Agent.Formula
	}

	// 4. Default
	return "spire-agent-work"
}

// FindFormula locates a formula file in the .beads/formulas directory.
func FindFormula(name string) (string, error) {
	beadsDir := os.Getenv("BEADS_DIR")
	if beadsDir == "" {
		// Try common locations
		candidates := []string{
			".beads/formulas",
			filepath.Join(os.Getenv("HOME"), ".beads/formulas"),
		}
		for _, c := range candidates {
			path := filepath.Join(c, name+".formula.toml")
			if _, err := os.Stat(path); err == nil {
				return path, nil
			}
		}
		return "", fmt.Errorf("formula %q not found", name)
	}
	path := filepath.Join(beadsDir, "formulas", name+".formula.toml")
	if _, err := os.Stat(path); err != nil {
		return "", fmt.Errorf("formula %q not found at %s", name, path)
	}
	return path, nil
}
