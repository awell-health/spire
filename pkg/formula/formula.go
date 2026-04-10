package formula

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	toml "github.com/pelletier/go-toml/v2"

	"github.com/awell-health/spire/pkg/formula/embedded"
)

// FormulaV2 represents a v2 formula that configures the universal phase pipeline.
// Retained for executor v2 compat (testing, record, executor_dag); pending full removal.
type FormulaV2 struct {
	Name        string                 `toml:"name"`
	Description string                 `toml:"description"`
	Version     int                    `toml:"version"`
	Phases      map[string]PhaseConfig `toml:"phases"`
	Vars        map[string]FormulaVar  `toml:"vars"`
}

// EnabledPhases returns the ordered list of enabled phases for this formula.
func (f *FormulaV2) EnabledPhases() []string {
	var enabled []string
	for _, p := range ValidPhases {
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

// PhaseConfig configures a single phase in the pipeline.
type PhaseConfig struct {
	Timeout        string          `toml:"timeout,omitempty"`
	Model          string          `toml:"model,omitempty"`
	MaxTurns       int             `toml:"max_turns,omitempty"`
	Context        []string        `toml:"context,omitempty"`
	RevisionPolicy *RevisionPolicy `toml:"revision_policy,omitempty"`
	Behavior      string `toml:"behavior,omitempty"`
	Deploy        string `toml:"deploy,omitempty"`
	Role          string `toml:"role,omitempty"`
	Dispatch      string `toml:"dispatch,omitempty"`
	VerdictOnly   bool   `toml:"verdict_only,omitempty"`
	Judgment      bool   `toml:"judgment,omitempty"`
	StagingBranch string `toml:"staging_branch,omitempty"`
	MergeStrategy string `toml:"strategy,omitempty"`
	Auto          bool   `toml:"auto,omitempty"`
	Apprentice    bool   `toml:"apprentice,omitempty"`
	Worktree      bool   `toml:"worktree,omitempty"`
	Build         string `toml:"build,omitempty"`
	Test          string `toml:"test,omitempty"`
	MaxBuildFixRounds int      `toml:"max_build_fix_rounds,omitempty"`
	OnBuildFailure    string   `toml:"on_build_failure,omitempty"`
	DocPatterns       []string `toml:"doc_patterns" json:"doc_patterns,omitempty"`
	Graph             string   `toml:"graph,omitempty"`
}

// GetBehavior returns the behavior override, or "" for role-based dispatch.
func (pc PhaseConfig) GetBehavior() string { return pc.Behavior }

// GetMaxTurns returns the max turns for this phase.
func (pc PhaseConfig) GetMaxTurns() int { return pc.MaxTurns }

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

// GetMaxBuildFixRounds returns the max build-fix attempts per wave, defaulting to 2.
func (pc PhaseConfig) GetMaxBuildFixRounds() int {
	if pc.MaxBuildFixRounds > 0 {
		return pc.MaxBuildFixRounds
	}
	return 2
}

// GetOnBuildFailure returns the build-failure policy, defaulting to "retry".
func (pc PhaseConfig) GetOnBuildFailure() string {
	if pc.OnBuildFailure != "" {
		return pc.OnBuildFailure
	}
	return "retry"
}

// RevisionPolicy configures review loop behavior (review phase only).
type RevisionPolicy struct {
	MaxRounds    int    `toml:"max_rounds"`
	ArbiterModel string `toml:"arbiter_model,omitempty"`
}

// RetryPolicy configures retry behavior for a v3 step.
type RetryPolicy struct {
	Max    int    `toml:"max"`              // maximum retry attempts
	Action string `toml:"action,omitempty"` // opcode to run on retry (e.g. "wizard.run")
	Flow   string `toml:"flow,omitempty"`   // flow for retry action (e.g. "build-fix")
}

// FormulaVar defines a variable accepted by the formula.
type FormulaVar struct {
	Description string `toml:"description"`
	Type        string `toml:"type,omitempty"` // "string" (default), "int", "bool", "bead_id"
	Required    bool   `toml:"required"`
	Default     string `toml:"default,omitempty"`
}

// OutputDecl declares a graph output that terminal steps populate into GraphResult.Outputs.
type OutputDecl struct {
	Type        string   `toml:"type"`                  // "string", "enum", "int"
	Description string   `toml:"description,omitempty"`
	Values      []string `toml:"values,omitempty"` // valid values for enum type
}

// FormulaStepGraph is a version 3 formula that declares a step graph with conditional routing.
// Unlike FormulaV2 (which declares sequential phases), FormulaStepGraph declares individual
// steps with dependency edges and runtime conditions. Used for the review phase molecule:
// the executor pours this formula as a molecule, creating step beads, then walks the graph
// — closing each step bead as it progresses.
type FormulaStepGraph struct {
	Name        string                   `toml:"name"`
	Description string                   `toml:"description"`
	Version     int                      `toml:"version"`
	Provider    string                   `toml:"provider,omitempty"` // formula-level default AI provider (claude, codex, cursor)
	Entry       string                   `toml:"entry,omitempty"`    // explicit entry step (falls back to EntryStep())
	Steps       map[string]StepConfig    `toml:"steps"`
	Workspaces  map[string]WorkspaceDecl `toml:"workspaces"`
	Vars        map[string]FormulaVar    `toml:"vars"`
}

// StepConfig configures a single step in a FormulaStepGraph.
type StepConfig struct {
	// Existing fields — kept for backward compat with current review formulas.
	Role        string   `toml:"role,omitempty"`         // sage | apprentice | arbiter | executor (optional in v3 opcode steps)
	Title       string   `toml:"title,omitempty"`        // human-readable title for the step bead
	Timeout     string   `toml:"timeout,omitempty"`      // e.g. "10m"
	Model       string   `toml:"model,omitempty"`        // model override for agent phases
	Provider    string   `toml:"provider,omitempty"`     // per-step AI provider override (claude, codex, cursor)
	MaxTurns    int      `toml:"max_turns,omitempty"`    // turn budget for agent invocations (0 = unlimited/timeout-gated)
	VerdictOnly bool     `toml:"verdict_only,omitempty"` // sage: produce verdict only, no edits
	// Graph edges
	Needs     []string `toml:"needs,omitempty"`     // predecessor steps (OR semantics: any one satisfies)
	Condition string   `toml:"condition,omitempty"` // runtime gate, e.g. "verdict == request_changes"
	Terminal  bool     `toml:"terminal,omitempty"`  // step enforces branch-lifecycle invariant on completion
	// v3 action fields
	Kind      string               `toml:"kind,omitempty"`      // op | dispatch | call
	Action    string               `toml:"action,omitempty"`    // executor opcode: wizard.run, git.merge_to_main, etc.
	When      *StructuredCondition `toml:"when,omitempty"`      // structured replacement for Condition
	Workspace string               `toml:"workspace,omitempty"` // named workspace reference
	With      map[string]string    `toml:"with,omitempty"`      // typed inputs for the action
	Produces  []string             `toml:"produces,omitempty"`  // declared output keys
	Retry     *RetryPolicy         `toml:"retry,omitempty"`     // optional retry policy
	Resets    []string             `toml:"resets,omitempty"`    // steps to reset to pending after this step completes
	Flow      string               `toml:"flow,omitempty"`      // for wizard.run: task-plan, implement, etc.
	Graph     string               `toml:"graph,omitempty"`     // graph.run: nested graph formula name
}

// --- Parsing ---

// ParseFormulaStepGraph parses a version 3 step-graph formula from TOML bytes.
func ParseFormulaStepGraph(data []byte) (*FormulaStepGraph, error) {
	var f FormulaStepGraph
	if err := toml.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("parse step-graph formula: %w", err)
	}
	if f.Version != 3 {
		return nil, fmt.Errorf("expected step-graph formula version 3, got %d", f.Version)
	}
	// Apply defaults to workspace declarations.
	for name, ws := range f.Workspaces {
		DefaultWorkspaceDecl(&ws)
		f.Workspaces[name] = ws
	}
	if err := ValidateGraph(&f); err != nil {
		return nil, fmt.Errorf("validate step-graph formula: %w", err)
	}
	return &f, nil
}


// --- Loading ---

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

// LoadReviewPhaseFormula loads the embedded review-phase step-graph formula.
// Used by the executor to pour the review molecule on entering the review phase.
func LoadReviewPhaseFormula() (*FormulaStepGraph, error) {
	return LoadStepGraphByName("review-phase")
}

// TowerFetcher is an optional injection point for tower-level formula lookup.
// When set, it is called with a formula name and returns the TOML content
// from the tower's dolt database. Set by cmd/spire at startup to avoid an
// import cycle between pkg/formula and pkg/store.
// Nil-safe: skipped when unset.
var TowerFetcher func(name string) (string, error)

// LoadStepGraphByName loads a step-graph formula with layered resolution:
//  1. Tower-level (dolt database, via TowerFetcher) — shared team defaults
//  2. On-disk (.beads/formulas/<name>.formula.toml) — repo-level customization
//  3. Embedded default (compiled into binary)
func LoadStepGraphByName(name string) (*FormulaStepGraph, error) {
	g, _, err := LoadStepGraphByNameWithSource(name)
	return g, err
}

// LoadStepGraphByNameWithSource loads a step-graph formula and returns its
// source: "tower", "repo", or "embedded". Used by agent_runs tracking to
// record formula provenance.
func LoadStepGraphByNameWithSource(name string) (*FormulaStepGraph, string, error) {
	// --- tier 1: tower-level (dolt database) ---
	if TowerFetcher != nil {
		if content, err := TowerFetcher(name); err == nil && content != "" {
			g, err := ParseFormulaStepGraph([]byte(content))
			if err != nil {
				// Malformed tower formula — log and fall through, don't hard-fail.
				log.Printf("warn: tower formula %q invalid, falling through: %v", name, err)
			} else {
				return g, "tower", nil
			}
		}
		// TowerFetcher returning error (dolt unreachable) → silent fall-through.
	}

	// --- tier 2: repo-level (.beads/formulas/) ---
	if path, err := FindFormula(name); err == nil {
		g, err := LoadStepGraphFromFile(path)
		if err != nil {
			return nil, "", fmt.Errorf("repo formula %q: %w", name, err)
		}
		return g, "repo", nil
	}

	// --- tier 3: embedded defaults ---
	g, err := LoadEmbeddedStepGraph(name)
	if err != nil {
		return nil, "", fmt.Errorf("formula %q not found in tower, repo, or embedded", name)
	}
	return g, "embedded", nil
}

// LoadStepGraphFromFile reads and parses a step-graph formula from a TOML file.
func LoadStepGraphFromFile(path string) (*FormulaStepGraph, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read step graph: %w", err)
	}
	return ParseFormulaStepGraph(data)
}

// LoadEmbeddedStepGraph loads a step-graph formula from the embedded defaults.
func LoadEmbeddedStepGraph(name string) (*FormulaStepGraph, error) {
	filename := "formulas/" + name + ".formula.toml"
	data, err := embedded.Formulas.ReadFile(filename)
	if err != nil {
		return nil, fmt.Errorf("embedded step graph %q not found", name)
	}
	return ParseFormulaStepGraph(data)
}

// --- Resolution ---

// DefaultV3FormulaMap maps bead types to default v3 formula names.
var DefaultV3FormulaMap = map[string]string{
	"task":     "spire-agent-work-v3",
	"bug":      "spire-bugfix-v3",
	"epic":     "spire-epic-v3",
	"chore":    "spire-agent-work-v3",
	"feature":  "spire-agent-work-v3",
	"recovery": "spire-recovery-v3",
}

// BeadInfo carries the bead fields needed for formula resolution.
// This avoids importing pkg/store into pkg/formula.
type BeadInfo struct {
	ID     string
	Type   string
	Labels []string
}

// RepoFormulaNameFunc is a callback that resolves a repo-level formula name
// override for a bead. Returns the formula name or "" if none configured.
// Injected by cmd/spire to bridge repoconfig + wizard logic.
var RepoFormulaNameFunc func(beadID string) string

// legacyV2NameMap translates v2 formula names to their v3 equivalents.
// Used when a bead label or repo config references a v2 name.
var legacyV2NameMap = map[string]string{
	"spire-agent-work": "spire-agent-work-v3",
	"spire-bugfix":     "spire-bugfix-v3",
	"spire-epic":       "spire-epic-v3",
}

// translateLegacyName returns the v3 equivalent if name is a known v2 formula,
// otherwise returns name unchanged.
func translateLegacyName(name string) string {
	if v3, ok := legacyV2NameMap[name]; ok {
		return v3
	}
	return name
}

// ResolveV3Name returns the v3 formula name for a bead without loading it.
// Resolution order: formula:<name> label > repo config > bead type map > fallback.
// Legacy v2 names (e.g. "spire-bugfix") are translated to v3 equivalents.
func ResolveV3Name(bead BeadInfo) string {
	// 1. Check bead labels for formula:<name> (explicit override)
	for _, l := range bead.Labels {
		if strings.HasPrefix(l, "formula:") {
			return translateLegacyName(l[len("formula:"):])
		}
	}

	// 2. Check repo-level formula via callback
	if RepoFormulaNameFunc != nil {
		if name := RepoFormulaNameFunc(bead.ID); name != "" {
			return translateLegacyName(name)
		}
	}

	// 3. Check bead type -> v3 formula mapping
	if name, ok := DefaultV3FormulaMap[bead.Type]; ok {
		return name
	}

	// 4. Default
	return "spire-agent-work-v3"
}

// ResolveV3 determines which v3 formula to use for a bead.
// Returns nil and an error if no v3 formula can be found.
func ResolveV3(bead BeadInfo) (*FormulaStepGraph, error) {
	name := ResolveV3Name(bead)
	g, err := LoadStepGraphByName(name)
	if err != nil {
		// Fall back to bead-type default, not blindly to spire-agent-work-v3.
		typeName, ok := DefaultV3FormulaMap[bead.Type]
		if !ok {
			typeName = "spire-agent-work-v3"
		}
		if name != typeName {
			g, err = LoadStepGraphByName(typeName)
			if err != nil {
				return nil, fmt.Errorf("resolve v3 formula for %s: %w", bead.ID, err)
			}
			return g, nil
		}
		return nil, fmt.Errorf("resolve v3 formula for %s: %w", bead.ID, err)
	}
	return g, nil
}

