package executor

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
	BeadID            string            `json:"bead_id"`
	AgentName         string            `json:"agent_name"`
	Formula           string            `json:"formula"`
	FormulaSource     string            `json:"formula_source,omitempty"` // "embedded", "repo", or "tower"
	StartedAt         string            `json:"started_at"`
	LastActionAt      string            `json:"last_action_at"`
	StagingBranch     string            `json:"staging_branch,omitempty"`
	BaseBranch        string            `json:"base_branch,omitempty"`
	RepoPath          string            `json:"repo_path,omitempty"`
	AttemptBeadID     string            `json:"attempt_bead_id,omitempty"`
	StepBeadIDs       map[string]string `json:"step_bead_ids,omitempty"`        // phase name → step bead ID
	ReviewStepBeadIDs map[string]string `json:"review_step_bead_ids,omitempty"` // formula step name → sub-step bead ID
	WorktreeDir       string            `json:"worktree_dir,omitempty"`         // staging worktree directory path
	LastGraphResult   *GraphResult      `json:"last_graph_result,omitempty"`
	// v3 graph runtime state.
	Workspaces map[string]WorkspaceState `json:"workspaces,omitempty"`
	StepStates map[string]StepState      `json:"step_states,omitempty"`
	Counters   map[string]int            `json:"counters,omitempty"`
}

// Executor drives a bead through its formula's step graph.
type Executor struct {
	beadID    string
	agentName string
	graph     *FormulaStepGraph  // v3 step graph
	graphState *GraphState       // v3 state
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
	// removes state.json instead of writing it.
	terminated bool

	// designPollInterval controls how long wizardValidateDesign sleeps between
	// poll iterations. Defaults to 30s in production; set to a small value in
	// tests to avoid blocking.
	designPollInterval time.Duration
}

// NewGraph creates a v3 graph executor for a bead. It loads or creates
// GraphState, registers with the wizard registry, and returns an executor
// with the graph path active (formula is nil).
func NewGraph(beadID, agentName string, graph *FormulaStepGraph, deps *Deps) (*Executor, error) {
	log := func(format string, a ...interface{}) {
		fmt.Fprintf(os.Stderr, "[%s] %s\n", agentName, fmt.Sprintf(format, a...))
	}

	// Default to file-backed store if not explicitly set.
	if deps.GraphStateStore == nil {
		deps.GraphStateStore = &FileGraphStateStore{ConfigDir: deps.ConfigDir}
	}

	// Load or create graph state.
	state, err := deps.GraphStateStore.Load(agentName)
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

// Run drives the bead through its formula's step graph.
func (e *Executor) Run() error {
	return e.RunGraph(e.graph, e.graphState)
}

// saveState persists the executor state to disk.
func (e *Executor) saveState() error {
	if e.state == nil {
		return nil
	}
	path := StatePath(e.agentName, e.deps.ConfigDir)
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

// SetFormulaSource sets the formula provenance ("embedded", "repo", or "tower")
// so it is persisted across phases and included in agent run records.
// Callers set this after NewGraph returns and before Run.
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

