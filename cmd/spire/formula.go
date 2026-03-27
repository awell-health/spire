package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	toml "github.com/pelletier/go-toml/v2"

	"github.com/awell-health/spire/cmd/spire/embedded"
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
	MaxTurns       int             `toml:"max_turns,omitempty"`
	Context        []string        `toml:"context,omitempty"`
	RevisionPolicy *RevisionPolicy `toml:"revision_policy,omitempty"`
	// Execution directives
	Behavior      string `toml:"behavior,omitempty"`       // validate-design | generate-subtasks | enrich-subtasks | sage-review | merge-to-main
	Role          string `toml:"role,omitempty"`           // human | apprentice | sage | wizard | skip
	Dispatch      string `toml:"dispatch,omitempty"`       // direct | wave
	VerdictOnly   bool   `toml:"verdict_only,omitempty"`   // sage: produce verdict only
	Judgment      bool   `toml:"judgment,omitempty"`        // executor judges review feedback
	StagingBranch string `toml:"staging_branch,omitempty"` // branch pattern for wave merges
	MergeStrategy string `toml:"strategy,omitempty"`       // squash | merge | rebase
	Auto          bool   `toml:"auto,omitempty"`           // auto-execute without human gate
	Apprentice    bool   `toml:"apprentice,omitempty"`     // run as apprentice (no phase labels, no review handoff)
	Worktree      bool   `toml:"worktree,omitempty"`       // run in isolated worktree
	Build         string `toml:"build,omitempty"`          // build command to verify after wave/merge
}

// GetMaxTurns returns the max turns for this phase, with sensible defaults per role.
func (pc PhaseConfig) GetMaxTurns() int {
	if pc.MaxTurns > 0 {
		return pc.MaxTurns
	}
	switch pc.GetRole() {
	case "wizard":
		return 5 // wizard phases are judgment/planning, not exploration
	case "sage":
		return 10
	case "apprentice":
		return 25
	default:
		return 10
	}
}

// GetRole returns the phase role, defaulting to "apprentice".
func (pc PhaseConfig) GetRole() string {
	if pc.Role != "" {
		return pc.Role
	}
	return "apprentice"
}

// GetBehavior returns the explicit behavior for this phase, or empty string if unset.
// When non-empty, the executor dispatches on behavior before role-based dispatch.
// Known behaviors: validate-design, generate-subtasks, enrich-subtasks, sage-review, merge-to-main.
func (pc PhaseConfig) GetBehavior() string {
	return pc.Behavior
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
	Default     string `toml:"default,omitempty"`
}

// FormulaStepGraph is a version 3 formula that declares a step graph with conditional routing.
// Unlike FormulaV2 (which declares sequential phases), FormulaStepGraph declares individual
// steps with dependency edges and runtime conditions. Used for the review phase molecule:
// the executor pours this formula as a molecule, creating step beads, then walks the graph
// — closing each step bead as it progresses.
type FormulaStepGraph struct {
	Name        string                `toml:"name"`
	Description string                `toml:"description"`
	Version     int                   `toml:"version"`
	Steps       map[string]StepConfig `toml:"steps"`
	Vars        map[string]FormulaVar `toml:"vars"`
}

// StepConfig configures a single step in a FormulaStepGraph.
type StepConfig struct {
	Role        string   `toml:"role"`                   // sage | apprentice | arbiter | executor
	Title       string   `toml:"title,omitempty"`        // human-readable title for the step bead
	Timeout     string   `toml:"timeout,omitempty"`      // e.g. "10m"
	Model       string   `toml:"model,omitempty"`        // model override for agent phases
	VerdictOnly bool     `toml:"verdict_only,omitempty"` // sage: produce verdict only, no edits
	// Graph edges
	Needs     []string `toml:"needs,omitempty"`     // predecessor steps (OR semantics: any one satisfies)
	Condition string   `toml:"condition,omitempty"` // runtime gate, e.g. "verdict == request_changes"
	Terminal  bool     `toml:"terminal,omitempty"`  // step enforces branch-lifecycle invariant on completion
}

// ParseFormulaStepGraph parses a version 3 step-graph formula from TOML bytes.
func ParseFormulaStepGraph(data []byte) (*FormulaStepGraph, error) {
	var f FormulaStepGraph
	if err := toml.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("parse step-graph formula: %w", err)
	}
	if f.Version != 3 {
		return nil, fmt.Errorf("expected step-graph formula version 3, got %d", f.Version)
	}
	return &f, nil
}

// LoadReviewPhaseFormula loads the embedded review-phase step-graph formula.
// Used by the executor to pour the review molecule on entering the review phase.
func LoadReviewPhaseFormula() (*FormulaStepGraph, error) {
	data, err := embedded.Formulas.ReadFile("formulas/review-phase.formula.toml")
	if err != nil {
		return nil, fmt.Errorf("embedded review-phase formula not found: %w", err)
	}
	return ParseFormulaStepGraph(data)
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
	f, err := LoadFormulaByName(name)
	if err != nil {
		// If the resolved formula doesn't exist, fall back to default
		if name != "spire-agent-work" {
			f, err = LoadFormulaByName("spire-agent-work")
			if err != nil {
				return nil, fmt.Errorf("resolve formula for %s: %w", bead.ID, err)
			}
			return f, nil
		}
		return nil, fmt.Errorf("resolve formula for %s: %w", bead.ID, err)
	}
	return f, nil
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
	// Try bead's repo first, fall back to CWD
	repoPath, _, _, _ := wizardResolveRepo(bead.ID)
	if repoPath == "" {
		repoPath = "."
	}
	if cfg, err := repoconfig.Load(repoPath); err == nil && cfg.Agent.Formula != "" {
		return cfg.Agent.Formula
	}

	// 4. Default
	return "spire-agent-work"
}

// FindFormula locates a formula file on disk in the .beads/formulas directory.
// Returns empty string and error if not found on disk — callers should
// fall back to LoadEmbeddedFormula for built-in defaults.
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
		return "", fmt.Errorf("formula %q not found on disk", name)
	}
	path := filepath.Join(beadsDir, "formulas", name+".formula.toml")
	if _, err := os.Stat(path); err != nil {
		return "", fmt.Errorf("formula %q not found at %s", name, path)
	}
	return path, nil
}

// LoadEmbeddedFormula loads a formula from the embedded defaults compiled into the binary.
func LoadEmbeddedFormula(name string) (*FormulaV2, error) {
	filename := "formulas/" + name + ".formula.toml"
	data, err := embedded.Formulas.ReadFile(filename)
	if err != nil {
		return nil, fmt.Errorf("embedded formula %q not found", name)
	}
	return ParseFormulaV2(data)
}

// LoadFormulaByName loads a formula with layered resolution:
//  1. On-disk (.beads/formulas/ or ~/.beads/formulas/) — user/project override
//  2. Embedded default (compiled into binary)
func LoadFormulaByName(name string) (*FormulaV2, error) {
	// Try disk first (project or user override)
	if path, err := FindFormula(name); err == nil {
		return LoadFormulaV2(path)
	}
	// Fall back to embedded default
	return LoadEmbeddedFormula(name)
}
