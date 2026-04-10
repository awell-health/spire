package executor

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	spgit "github.com/awell-health/spire/pkg/git"
	"github.com/awell-health/spire/pkg/repoconfig"
)

// GraphResult is the typed return from executing a step-graph formula.
// It captures which terminal step fired and the declared output values,
// so outer workflows can route mechanically without inspecting ad hoc state.
type GraphResult struct {
	GraphName    string            `json:"graph_name"`
	TerminalStep string            `json:"terminal_step"`
	Outputs      map[string]string `json:"outputs"`
}

// State is the persistent state for a formula executor.
type State struct {
	BeadID        string                  `json:"bead_id"`
	AgentName     string                  `json:"agent_name"`
	Formula       string                  `json:"formula"`
	FormulaSource string                  `json:"formula_source,omitempty"` // "embedded", "repo", or "tower"
	Phase         string                  `json:"phase"`
	Wave          int                     `json:"wave"`
	Subtasks      map[string]SubtaskState `json:"subtasks"`
	ReviewRounds  int                     `json:"review_rounds"`
	BuildFixRounds int                    `json:"build_fix_rounds,omitempty"`
	StartedAt     string                  `json:"started_at"`
	LastActionAt  string                  `json:"last_action_at"`
	StagingBranch string                  `json:"staging_branch,omitempty"`
	BaseBranch    string                  `json:"base_branch,omitempty"`
	RepoPath      string                  `json:"repo_path,omitempty"`
	AttemptBeadID string                  `json:"attempt_bead_id,omitempty"`
	StepBeadIDs       map[string]string       `json:"step_bead_ids,omitempty"`        // phase name → step bead ID
	ReviewStepBeadIDs map[string]string       `json:"review_step_bead_ids,omitempty"` // formula step name → sub-step bead ID
	WorktreeDir       string                  `json:"worktree_dir,omitempty"`         // staging worktree directory path
	LastGraphResult   *GraphResult            `json:"last_graph_result,omitempty"`
	// v3 graph runtime state — coexist with v2 fields until migration is complete.
	Workspaces map[string]WorkspaceState `json:"workspaces,omitempty"`
	StepStates map[string]StepState      `json:"step_states,omitempty"`
	Counters   map[string]int            `json:"counters,omitempty"`
}

// Executor drives a bead through its formula's phase pipeline (v2) or step
// graph (v3). When graph is non-nil, Run() delegates to RunGraph().
type Executor struct {
	beadID    string
	agentName string
	formula   *FormulaV2         // v2 phase pipeline (nil when running v3)
	graph     *FormulaStepGraph  // v3 step graph (nil when running v2)
	graphState *GraphState       // v3 state (nil when running v2)
	state     *State
	deps      *Deps
	log       func(string, ...interface{})

	// currentRunID is the run ID of this executor's own agent_run record.
	// Used as ParentRunID for child spawns (apprentice, sage).
	currentRunID string

	// Single staging worktree shared across all phases (implement, review, merge).
	// Created once by ensureStagingWorktree(), cleaned up by Run() on exit.
	stagingWt *spgit.StagingWorktree

	// terminated is set to true on terminal success paths (merge complete,
	// bead closed, all phases done). When true, the deferred saveState()
	// removes state.json instead of writing it, preventing stale state
	// from lingering after successful completion.
	terminated bool

	// designPollInterval controls how long wizardValidateDesign sleeps between
	// poll iterations. Defaults to 30s in production; set to a small value in
	// tests to avoid blocking.
	designPollInterval time.Duration
}

// New creates a formula executor for a bead.
// It loads or creates state, registers with the wizard registry, and resolves the formula.
func New(beadID, agentName string, formula *FormulaV2, deps *Deps) (*Executor, error) {
	log := func(format string, a ...interface{}) {
		fmt.Fprintf(os.Stderr, "[%s] %s\n", agentName, fmt.Sprintf(format, a...))
	}

	// Load or create state
	state, err := LoadState(agentName, deps.ConfigDir)
	if err != nil {
		return nil, fmt.Errorf("load state: %w", err)
	}
	if state == nil {
		// Detect current phase from bead
		bead, err := deps.GetBead(beadID)
		if err != nil {
			return nil, fmt.Errorf("get bead: %w", err)
		}
		phase := deps.GetPhase(bead)
		if phase == "" {
			// Start at first enabled phase
			enabled := formula.EnabledPhases()
			if len(enabled) > 0 {
				phase = enabled[0]
			} else {
				return nil, fmt.Errorf("formula %s has no enabled phases", formula.Name)
			}
		}
		state = &State{
			BeadID:     beadID,
			AgentName:  agentName,
			Formula:    formula.Name,
			Phase:      phase,
			Subtasks:   make(map[string]SubtaskState),
			Workspaces: make(map[string]WorkspaceState),
			StepStates: make(map[string]StepState),
			Counters:   make(map[string]int),
			StartedAt:  time.Now().UTC().Format(time.RFC3339),
		}
	}

	return &Executor{
		beadID:             beadID,
		agentName:          agentName,
		formula:            formula,
		state:              state,
		deps:               deps,
		log:                log,
		designPollInterval: 30 * time.Second,
	}, nil
}

// NewGraph creates a v3 graph executor for a bead. It loads or creates
// GraphState, registers with the wizard registry, and returns an executor
// with the graph path active (formula is nil).
func NewGraph(beadID, agentName string, graph *FormulaStepGraph, deps *Deps) (*Executor, error) {
	log := func(format string, a ...interface{}) {
		fmt.Fprintf(os.Stderr, "[%s] %s\n", agentName, fmt.Sprintf(format, a...))
	}

	// Load or create graph state.
	state, err := LoadGraphState(agentName, deps.ConfigDir)
	if err != nil {
		return nil, fmt.Errorf("load graph state: %w", err)
	}
	if state == nil {
		state = NewGraphState(graph, beadID, agentName)
		// Tag with tower name so sweep/resolve can filter by tower.
		if deps.ActiveTowerConfig != nil {
			if tc, err := deps.ActiveTowerConfig(); err == nil && tc != nil {
				state.TowerName = tc.Name
			}
		}
	}

	return &Executor{
		beadID:     beadID,
		agentName:  agentName,
		graph:      graph,
		graphState: state,
		deps:       deps,
		log:        log,
	}, nil
}

// resolveBranchState resolves repo path, base branch, and staging branch once
// and stores them in the executor state. All git operations read from state
// instead of computing these independently.
func (e *Executor) resolveBranchState() error {
	// Already resolved (e.g. resumed from persisted state) — skip.
	if e.state.RepoPath != "" && e.state.BaseBranch != "" {
		e.log("branch state loaded from persisted state: repo=%s base=%s staging=%s",
			e.state.RepoPath, e.state.BaseBranch, e.state.StagingBranch)
		return nil
	}

	repoPath, _, baseBranch, err := e.deps.ResolveRepo(e.beadID)
	if err != nil {
		return fmt.Errorf("resolve repo: %w", err)
	}
	if repoPath == "" {
		repoPath = "."
	}
	// baseBranch default ("main") is set by ResolveRepo — no duplicate fallback here.

	e.state.RepoPath = repoPath
	e.state.BaseBranch = baseBranch

	// Bead-level base-branch override (from spire file --branch) takes
	// precedence over repo defaults. This ensures the executor respects
	// the branch intent stamped at filing time without mutating the
	// tower-wide repo default.
	if bead, berr := e.deps.GetBead(e.beadID); berr == nil {
		if bb := e.deps.HasLabel(bead, "base-branch:"); bb != "" {
			e.log("using bead base-branch override: %s (was: %s)", bb, e.state.BaseBranch)
			e.state.BaseBranch = bb
		}
	}

	// Resolve staging branch from the implement phase config (if any).
	for _, phaseName := range e.formula.EnabledPhases() {
		pc, ok := e.formula.Phases[phaseName]
		if ok && pc.StagingBranch != "" {
			e.state.StagingBranch = strings.ReplaceAll(pc.StagingBranch, "{bead-id}", e.beadID)
			break
		}
	}

	// Default staging branch to staging/<bead-id> if no formula override.
	if e.state.StagingBranch == "" {
		e.state.StagingBranch = "staging/" + e.beadID
	}

	e.log("branch state resolved: repo=%s base=%s staging=%s",
		e.state.RepoPath, e.state.BaseBranch, e.state.StagingBranch)
	return e.saveState()
}

// Run drives the bead through its formula's phase pipeline until all phases
// are complete or the bead is closed. For v3 graphs, delegates to RunGraph.
func (e *Executor) Run() error {
	if e.graph != nil {
		return e.RunGraph(e.graph, e.graphState)
	}

	// Register with wizard registry inside Run() — paired with the deferred
	// cleanup below so registration and cleanup are always atomic.
	// Previously in New(), where a failure between New() and Run() would
	// orphan the registry entry.
	cleanup := e.deps.RegisterSelf(e.agentName, e.beadID, e.state.Phase)
	defer cleanup()
	defer e.saveState()

	// Clean up the staging worktree on exit. Deferred early so it runs
	// before state save (defers execute LIFO).
	defer e.closeStagingWorktree()

	// Create attempt bead at start (or resume existing one).
	if err := e.ensureAttemptBead(); err != nil {
		e.log("warning: create attempt bead: %s", err)
		// Non-fatal — proceed without attempt tracking.
	}

	// Ensure attempt is closed on all exit paths (success, failure, panic).
	// The recover-then-repanic guard ensures that even if another defer or
	// the function body panics, the attempt bead and step beads are still
	// cleaned up before the panic propagates.
	defer func() {
		var panicVal interface{}
		if r := recover(); r != nil {
			panicVal = r
			e.log("executor cleanup panic: %v", r)
		}
		if !e.terminated {
			e.closeAllOpenStepBeads()
		}
		if e.state.AttemptBeadID != "" {
			if cerr := e.deps.CloseAttemptBead(e.state.AttemptBeadID, "executor exited"); cerr != nil {
				e.log("warning: close attempt bead: %s", cerr)
			}
			e.state.AttemptBeadID = ""
		}
		if panicVal != nil {
			panic(panicVal)
		}
	}()

	// Resolve repo path, base branch, and staging branch once at startup.
	if err := e.resolveBranchState(); err != nil {
		e.closeAttempt("failure: repo-resolution: " + err.Error())
		EscalateHumanFailure(e.beadID, e.agentName, "repo-resolution", err.Error(), e.deps)
		return fmt.Errorf("resolve branch state: %w", err)
	}

	// Create workflow step beads for each formula phase (or resume existing ones).
	if err := e.ensureStepBeads(); err != nil {
		e.log("warning: create step beads: %s", err)
		// Non-fatal — proceed without step bead tracking.
	}

	// Record the executor's own top-level run before any child spawns,
	// so e.currentRunID is available as ParentRunID for child agent runs.
	e.currentRunID = e.recordAgentRun(e.agentName, e.beadID, "", e.repoModel(), "wizard", "execute", time.Now(), nil)

	for {
		phase := e.state.Phase
		pc, ok := e.formula.Phases[phase]
		if !ok {
			e.closeAllOpenStepBeads()
			e.closeAttempt(fmt.Sprintf("failure: unknown phase %q", phase))
			return fmt.Errorf("phase %q not in formula %s", phase, e.formula.Name)
		}

		e.log("phase: %s (role: %s)", phase, pc.GetRole())
		e.saveState()

		// Dispatch by behavior first (if set), then fall through to role.
		var err error
		behavior := pc.GetBehavior()

		switch {
		// --- Behavior-based dispatch (formula-driven) ---
		case behavior == "validate-design":
			err = e.wizardValidateDesign()
		case behavior == "epic-plan":
			bead, berr := e.deps.GetBead(e.beadID)
			if berr != nil {
				err = fmt.Errorf("get bead for epic-plan: %w", berr)
			} else {
				err = e.wizardPlanEpic(bead, pc)
			}
		case behavior == "task-plan":
			bead, berr := e.deps.GetBead(e.beadID)
			if berr != nil {
				err = fmt.Errorf("get bead for task-plan: %w", berr)
			} else {
				err = e.wizardPlanTask(bead, pc)
			}
		case behavior == "enrich-subtasks":
			enrichStarted := time.Now()
			children, _ := e.deps.GetChildren(e.beadID)
			err = e.enrichSubtasksWithChangeSpecs(children, "", "", pc)
			e.recordAgentRun(e.agentName, e.beadID, "", pc.Model, "wizard", "enrich-subtasks", enrichStarted, err)
		case behavior == "sage-review":
			var graphResult *GraphResult
			graphResult, err = e.executeReview(phase, pc)
			if err == nil && graphResult != nil {
				e.state.LastGraphResult = graphResult
				e.saveState()
			}
		case behavior == "auto-approve":
			e.log("auto-approve: skipping review")
			e.recordAgentRun(e.agentName, e.beadID, "", "", "wizard", "auto-approve", time.Now(), nil,
				withResult("skipped"),
				withSkipReason("auto-approve: no human review required by formula config"))
		case behavior == "merge-to-main":
			if mergeErr := e.executeMerge(pc); mergeErr != nil {
				e.closeAllOpenStepBeads()
				e.closeAttempt("failure: merge: " + mergeErr.Error())
				EscalateHumanFailure(e.beadID, e.agentName, "merge-failure", mergeErr.Error(), e.deps)
				return fmt.Errorf("phase %s: %w", phase, mergeErr)
			}
			e.transitionStepBead(phase, "")
			e.closeAttempt("success: merged")
			e.terminated = true
			return nil // merge is terminal
		case behavior == "skip":
			e.log("skipping phase %s (behavior: skip)", phase)
			e.recordAgentRun(e.agentName, e.beadID, "", "", "wizard", phase, time.Now(), nil,
				withResult("skipped"),
				withSkipReason(fmt.Sprintf("behavior: skip for phase %s", phase)))

		// --- Role-based dispatch (legacy / default) ---
		case behavior == "" && phase == "merge":
			// Default merge behavior when no behavior set
			if mergeErr := e.executeMerge(pc); mergeErr != nil {
				e.closeAllOpenStepBeads()
				e.closeAttempt("failure: merge: " + mergeErr.Error())
				EscalateHumanFailure(e.beadID, e.agentName, "merge-failure", mergeErr.Error(), e.deps)
				return fmt.Errorf("phase merge: %w", mergeErr)
			}
			e.transitionStepBead(phase, "")
			e.closeAttempt("success: merged")
			e.terminated = true
			return nil // merge is terminal
		case behavior == "":
			switch pc.GetRole() {
			case "human":
				err = e.waitForHuman(phase)
			case "apprentice":
				dispatchMode, dispatchSource := e.resolveDispatch(pc)
				e.log("dispatch mode: %s (source: %s)", dispatchMode, dispatchSource)
				switch dispatchMode {
				case "wave":
					err = e.executeWave(phase, pc)
				case "sequential":
					err = e.executeSequential(phase, pc)
				default:
					err = e.executeDirect(phase, pc)
				}
			case "sage":
				var graphResult *GraphResult
				graphResult, err = e.executeReview(phase, pc)
				if err == nil && graphResult != nil {
					e.state.LastGraphResult = graphResult
					e.saveState()
				}
			case "skip":
				e.log("skipping phase %s", phase)
				e.recordAgentRun(e.agentName, e.beadID, "", "", "wizard", phase, time.Now(), nil,
					withResult("skipped"),
					withSkipReason(fmt.Sprintf("role: skip for phase %s", phase)))
			default:
				err = fmt.Errorf("unknown role %q for phase %s", pc.GetRole(), phase)
			}
		default:
			err = fmt.Errorf("unknown behavior %q for phase %s", behavior, phase)
		}

		if err != nil {
			e.closeAllOpenStepBeads()
			e.closeAttempt(fmt.Sprintf("failure: phase %s: %s", phase, err.Error()))
			return fmt.Errorf("phase %s: %w", phase, err)
		}

		// After implement phase: check if staging has any diff vs base.
		// If the apprentice produced no code changes, skip review and escalate.
		if phase == "implement" && e.stagingWt != nil {
			hasNew, err := e.stagingWt.HasNewCommits()
			if err != nil {
				e.log("warning: could not check for new commits: %s", err)
				// Don't escalate — assume commits may exist
			} else if !hasNew {
				e.log("implement phase produced no code changes — escalating")
				EscalateEmptyImplement(e.beadID, e.agentName, e.deps)
				e.closeAllOpenStepBeads()
				e.closeAttempt("escalated: empty implement — no code changes")
				e.terminated = true
				return nil
			}
		}

		// Advance to next phase
		prevPhase := e.state.Phase
		if !e.advancePhase() {
			// Close the final step bead.
			e.transitionStepBead(prevPhase, "")
			break // no more phases
		}
		// Transition step beads: close previous, activate next.
		e.transitionStepBead(prevPhase, e.state.Phase)

		// Check if bead is closed (e.g. by executeMerge from within review phase).
		bead, err := e.deps.GetBead(e.beadID)
		if err != nil {
			e.closeAllOpenStepBeads()
			e.closeAttempt("failure: check bead: " + err.Error())
			return fmt.Errorf("check bead: %w", err)
		}
		if bead.Status == "closed" {
			e.log("bead closed — exiting")
			e.closeAllOpenStepBeads()
			e.closeAttempt("success: bead closed")
			e.terminated = true
			return nil
		}
	}

	e.log("all phases complete")
	e.closeAttempt("success: all phases complete")
	e.terminated = true
	return nil
}

// waitForHuman blocks the executor until the human transitions the phase.
func (e *Executor) waitForHuman(phase string) error {
	started := time.Now()
	e.log("phase %s requires human action", phase)
	e.log("when ready, transition the phase and re-run:")
	e.log("  bd label remove %s \"phase:%s\"", e.beadID, phase)
	next := e.nextPhase(phase)
	if next != "" {
		e.log("  bd label add %s \"phase:%s\"", e.beadID, next)
	}
	waitErr := fmt.Errorf("waiting for human to complete %s phase", phase)
	e.recordAgentRun(e.agentName, e.beadID, "", "", "wizard", phase, started, waitErr,
		withResult("waiting"))
	return waitErr
}

// --- State persistence ---

// StatePath returns the path to the executor state file for the given agent.
func StatePath(agentName string, configDirFn func() (string, error)) string {
	dir, _ := configDirFn()
	return filepath.Join(dir, "runtime", agentName, "state.json")
}

// LoadState loads the executor state from disk for the given agent.
// Returns nil, nil when no state file exists (fresh start).
func LoadState(agentName string, configDirFn func() (string, error)) (*State, error) {
	path := StatePath(agentName, configDirFn)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, err
	}
	return &state, nil
}

func (e *Executor) saveState() error {
	path := StatePath(e.agentName, e.deps.ConfigDir)

	// On terminal success paths, remove state.json instead of writing it.
	// This prevents the deferred saveState() from re-creating the file
	// after a terminal return has already cleaned up.
	if e.terminated {
		os.Remove(path)
		return nil
	}

	os.MkdirAll(filepath.Dir(path), 0755)
	e.state.LastActionAt = time.Now().UTC().Format(time.RFC3339)
	data, err := json.MarshalIndent(e.state, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

// --- Phase navigation ---

// advancePhase moves to the next enabled phase in the formula.
// Returns false if there are no more phases.
func (e *Executor) advancePhase() bool {
	next := e.nextPhase(e.state.Phase)
	if next == "" {
		return false
	}
	e.state.Phase = next
	return true
}

// nextPhase returns the next enabled phase after the given one, or "".
func (e *Executor) nextPhase(current string) string {
	enabled := e.formula.EnabledPhases()
	for i, p := range enabled {
		if p == current && i+1 < len(enabled) {
			return enabled[i+1]
		}
	}
	return ""
}

// SetFormulaSource sets the formula provenance ("embedded", "repo", or "tower")
// on both v2 and v3 state, so it is persisted across phases and included in
// agent run records. Callers set this after NewGraph/New returns and before Run.
func (e *Executor) SetFormulaSource(source string) {
	if e.state != nil {
		e.state.FormulaSource = source
	}
	if e.graphState != nil {
		e.graphState.FormulaSource = source
	}
}

// --- Accessors for testing ---

// State returns the executor's current state. Used by tests.
func (e *Executor) State() *State {
	return e.state
}

// GraphState returns the executor's v3 graph state. Used by tests.
func (e *Executor) GraphSt() *GraphState {
	return e.graphState
}

// Terminated returns whether the executor reached a terminal state.
func (e *Executor) Terminated() bool {
	return e.terminated
}

// BeadID returns the executor's bead ID.
func (e *Executor) BeadID() string {
	return e.beadID
}

// AgentName returns the executor's agent name.
func (e *Executor) AgentName() string {
	return e.agentName
}

// resolveBranch returns the branch name for a bead using the injected
// ResolveBranch dep, falling back to "feat/<beadID>" if the dep is nil.
func (e *Executor) resolveBranch(beadID string) string {
	if e.deps.ResolveBranch != nil {
		return e.deps.ResolveBranch(beadID)
	}
	return "feat/" + beadID
}

// repoModel returns the agent model from the repo config, or "" if unavailable.
func (e *Executor) repoModel() string {
	if e.deps.RepoConfig == nil {
		return ""
	}
	cfg := e.deps.RepoConfig()
	if cfg == nil {
		return ""
	}
	return cfg.Agent.Model
}

// repoProvider returns the agent provider from the repo config, or "" if unavailable.
func (e *Executor) repoProvider() string {
	if e.deps.RepoConfig == nil {
		return ""
	}
	cfg := e.deps.RepoConfig()
	if cfg == nil {
		return ""
	}
	return cfg.Agent.Provider
}

// resolveStepProvider returns the resolved AI provider for a step, applying the
// precedence chain: step provider > formula provider > repo config provider > "claude".
func (e *Executor) resolveStepProvider(step StepConfig) string {
	var formulaProvider string
	if e.graph != nil {
		formulaProvider = e.graph.Provider
	}
	return repoconfig.ResolveProvider(step.Provider, formulaProvider, e.repoProvider())
}

// resolveDispatch returns the effective dispatch mode and its source.
// It checks for a dispatch:<mode> label override on the bead first;
// if none is found, it falls back to the formula's per-phase dispatch.
func (e *Executor) resolveDispatch(pc PhaseConfig) (mode, source string) {
	bead, err := e.deps.GetBead(e.beadID)
	if err == nil {
		var found []string
		for _, l := range bead.Labels {
			if strings.HasPrefix(l, "dispatch:") {
				found = append(found, l[len("dispatch:"):])
			}
		}
		if len(found) > 1 {
			e.log("warning: multiple dispatch: labels found, using first (%s)", found[0])
		}
		if len(found) > 0 {
			return found[0], "override"
		}
	}
	return pc.GetDispatch(), "formula"
}
