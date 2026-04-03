package formula

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	toml "github.com/pelletier/go-toml/v2"

	"github.com/awell-health/spire/pkg/formula/embedded"
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
	// Behavior override — dispatched before role. When set, the executor
	// calls the named behavior handler instead of role-based dispatch.
	// See docs/wizard-workflow-dag.md for available behaviors per phase.
	Behavior string `toml:"behavior,omitempty"`
	Deploy   string `toml:"deploy,omitempty"` // deploy command (deploy behavior)
	// Execution directives
	Role     string `toml:"role,omitempty"`     // human | apprentice | sage | wizard | skip
	Dispatch string `toml:"dispatch,omitempty"` // direct | wave | sequential
	VerdictOnly   bool   `toml:"verdict_only,omitempty"`   // sage: produce verdict only
	Judgment      bool   `toml:"judgment,omitempty"`        // executor judges review feedback
	StagingBranch string `toml:"staging_branch,omitempty"` // branch pattern for wave merges
	MergeStrategy string `toml:"strategy,omitempty"`       // squash | merge | rebase
	Auto          bool   `toml:"auto,omitempty"`           // auto-execute without human gate
	Apprentice    bool   `toml:"apprentice,omitempty"`     // run as apprentice (no phase labels, no review handoff)
	Worktree      bool   `toml:"worktree,omitempty"`       // run in isolated worktree
	Build         string `toml:"build,omitempty"`          // build command to verify after wave/merge
	Test          string `toml:"test,omitempty"`           // test command to verify after rebase/merge
	MaxBuildFixRounds int      `toml:"max_build_fix_rounds,omitempty"` // max build-fix attempts per wave (default 2)
	OnBuildFailure    string   `toml:"on_build_failure,omitempty"`     // "retry" (default) | "escalate" | "fail"
	DocPatterns       []string `toml:"doc_patterns" json:"doc_patterns,omitempty"` // glob patterns for doc files to review on merge
	Graph             string   `toml:"graph,omitempty"`                            // step-graph formula name for graph-based phases (e.g. review)
}

// GetBehavior returns the behavior override, or "" for role-based dispatch.
func (pc PhaseConfig) GetBehavior() string {
	return pc.Behavior
}

// GetMaxTurns returns the max turns for this phase.
// Returns 0 (unlimited) if not set in the formula — the timeout is the gate.
// Set max_turns explicitly in the formula TOML to enforce a turn budget.
func (pc PhaseConfig) GetMaxTurns() int {
	return pc.MaxTurns
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
	Entry       string                   `toml:"entry,omitempty"` // explicit entry step (falls back to EntryStep())
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
		if !IsValidPhase(name) {
			return nil, fmt.Errorf("unknown phase %q in formula", name)
		}
	}
	return &f, nil
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

// ParseFormulaAny peeks at the version field and parses as V2 or V3.
// Returns the parsed formula (either *FormulaV2 or *FormulaStepGraph),
// the version number, and any error.
func ParseFormulaAny(data []byte) (interface{}, int, error) {
	var peek struct {
		Version int `toml:"version"`
	}
	if err := toml.Unmarshal(data, &peek); err != nil {
		return nil, 0, fmt.Errorf("peek version: %w", err)
	}
	switch peek.Version {
	case 2:
		f, err := ParseFormulaV2(data)
		return f, 2, err
	case 3:
		f, err := ParseFormulaStepGraph(data)
		return f, 3, err
	default:
		return nil, 0, fmt.Errorf("unsupported formula version %d", peek.Version)
	}
}

// --- FormulaV2 methods ---

// EnabledPhases returns the ordered list of enabled phases for this formula.
// Order follows ValidPhases (design, plan, implement, review, merge).
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

// --- Loading ---

// LoadFormulaV2 reads and parses a v2 formula from a TOML file.
func LoadFormulaV2(path string) (*FormulaV2, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read formula: %w", err)
	}
	return ParseFormulaV2(data)
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

// LoadReviewPhaseFormula loads the embedded review-phase step-graph formula.
// Used by the executor to pour the review molecule on entering the review phase.
func LoadReviewPhaseFormula() (*FormulaStepGraph, error) {
	return LoadStepGraphByName("review-phase")
}

// LoadStepGraphByName loads a step-graph formula with layered resolution:
//  1. On-disk (.beads/formulas/<name>.formula.toml) — user/project override
//  2. Embedded default (compiled into binary)
func LoadStepGraphByName(name string) (*FormulaStepGraph, error) {
	if path, err := FindFormula(name); err == nil {
		return LoadStepGraphFromFile(path)
	}
	return LoadEmbeddedStepGraph(name)
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

// DefaultFormulaMap maps bead types to default v2 formula names.
// Can be overridden by tower config in the future.
var DefaultFormulaMap = map[string]string{
	"task":    "spire-agent-work",
	"bug":     "spire-bugfix",
	"epic":    "spire-epic",
	"chore":   "spire-agent-work",
	"feature": "spire-agent-work",
}

// DefaultV3FormulaMap maps bead types to default v3 formula names.
// Used by ResolveAny when a v3 formula is explicitly requested via
// the "formula-version:3" label on a bead.
var DefaultV3FormulaMap = map[string]string{
	"task":    "spire-agent-work-v3",
	"bug":     "spire-bugfix-v3",
	"epic":    "spire-epic-v3",
	"chore":   "spire-agent-work-v3",
	"feature": "spire-agent-work-v3",
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

// Resolve determines which formula to use for a bead.
// Resolution order:
//  1. Bead label formula:<name> (explicit override)
//  2. RepoFormulaNameFunc callback (spire.yaml agent.formula)
//  3. Bead type -> DefaultFormulaMap
//  4. Fall back to "spire-agent-work"
func Resolve(bead BeadInfo) (*FormulaV2, error) {
	name := ResolveName(bead)
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

// ResolveName returns the formula name for a bead without loading it.
// Resolution order: label override > repo config (spire.yaml) > compiled-in map > fallback.
func ResolveName(bead BeadInfo) string {
	// 1. Check bead labels for formula:<name>
	for _, l := range bead.Labels {
		if strings.HasPrefix(l, "formula:") {
			return l[len("formula:"):]
		}
	}

	// 2. Check repo-level formula via callback (spire.yaml agent.formula)
	if RepoFormulaNameFunc != nil {
		if name := RepoFormulaNameFunc(bead.ID); name != "" {
			return name
		}
	}

	// 3. Check bead type -> formula mapping
	if name, ok := DefaultFormulaMap[bead.Type]; ok {
		return name
	}

	// 4. Default
	return "spire-agent-work"
}

// WantsV2 returns true if the bead has a "formula-version:2" label,
// explicitly requesting the legacy v2 phase pipeline.
func WantsV2(bead BeadInfo) bool {
	for _, l := range bead.Labels {
		if l == "formula-version:2" {
			return true
		}
	}
	return false
}

// ResolveV3Name returns the v3 formula name for a bead without loading it.
// Resolution order: formula:<name> label > v3 default map > fallback.
func ResolveV3Name(bead BeadInfo) string {
	// 1. Check bead labels for formula:<name> (explicit override)
	for _, l := range bead.Labels {
		if strings.HasPrefix(l, "formula:") {
			return l[len("formula:"):]
		}
	}

	// 2. Check repo-level formula via callback
	if RepoFormulaNameFunc != nil {
		if name := RepoFormulaNameFunc(bead.ID); name != "" {
			return name
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
		// Fall back to default v3 formula
		if name != "spire-agent-work-v3" {
			g, err = LoadStepGraphByName("spire-agent-work-v3")
			if err != nil {
				return nil, fmt.Errorf("resolve v3 formula for %s: %w", bead.ID, err)
			}
			return g, nil
		}
		return nil, fmt.Errorf("resolve v3 formula for %s: %w", bead.ID, err)
	}
	return g, nil
}

// ResolveAny determines which formula to use for a bead, supporting both
// v2 and v3 formulas. It returns either a *FormulaV2 or *FormulaStepGraph,
// the version number, and any error.
//
// Resolution order:
//  1. If the bead has a "formula-version:2" label, resolve as v2 (legacy opt-in).
//  2. If the bead has a "formula:<name>" label, try v3 then v2.
//  3. Default: v3 resolution.
func ResolveAny(bead BeadInfo) (interface{}, int, error) {
	// Check if v2 is explicitly requested (legacy opt-in).
	if WantsV2(bead) {
		f, err := Resolve(bead)
		if err != nil {
			return nil, 0, err
		}
		return f, 2, nil
	}

	// Check if the formula:<name> label points to a specific formula.
	for _, l := range bead.Labels {
		if strings.HasPrefix(l, "formula:") {
			name := l[len("formula:"):]
			// Try v3 first, fall back to v2.
			if g, err := LoadStepGraphByName(name); err == nil {
				return g, 3, nil
			}
			if f, err := LoadFormulaByName(name); err == nil {
				return f, 2, nil
			}
			break
		}
	}

	// Default: v3 resolution.
	g, err := ResolveV3(bead)
	if err != nil {
		return nil, 0, err
	}
	return g, 3, nil
}
