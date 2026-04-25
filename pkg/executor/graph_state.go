package executor

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/awell-health/spire/pkg/config"
)

// GraphState is the generic step-oriented state model for v3 graph execution.
// It replaces the hardcoded ReviewRounds/BuildFixRounds/Phase/Wave fields with
// a graph-generic representation that tracks step status, outputs, counters,
// workspace state, and vars.
type GraphState struct {
	BeadID     string                     `json:"bead_id"`
	AgentName  string                     `json:"agent_name"`
	Formula       string                  `json:"formula"`
	FormulaSource string                  `json:"formula_source,omitempty"` // "embedded", "repo", or "tower"
	Entry         string                  `json:"entry"`
	Steps      map[string]StepState       `json:"steps"`
	Counters   map[string]int             `json:"counters"`
	Workspaces map[string]WorkspaceState  `json:"workspaces"`
	Vars       map[string]string          `json:"vars"`
	ActiveStep string                     `json:"active_step"`

	// Bookkeeping (carried over from State for compatibility)
	StartedAt     string            `json:"started_at"`
	LastActionAt  string            `json:"last_action_at"`
	StagingBranch string            `json:"staging_branch,omitempty"`
	BaseBranch    string            `json:"base_branch,omitempty"`
	TowerName     string            `json:"tower_name,omitempty"`
	RepoPath      string            `json:"repo_path,omitempty"`
	AttemptBeadID string            `json:"attempt_bead_id,omitempty"`
	StepBeadIDs   map[string]string `json:"step_bead_ids,omitempty"`
	WorktreeDir   string            `json:"worktree_dir,omitempty"`
	InjectedTasks []string          `json:"injected_tasks,omitempty"`

	// Auth is the per-run selected auth context (spi-rs8sb2). Populated by
	// `spire summon`'s selection logic and read by downstream spawn/HTTP
	// code. Persisted with the rest of GraphState so a resumed wizard picks
	// up the same credential slot the original summon selected. The parent
	// directory uses 0700 and this file is written with 0600 (see Save) to
	// keep the embedded secret off world-readable state.
	Auth *config.AuthContext `json:"auth,omitempty"`
}

// StepState tracks the status and outputs of a single graph step.
type StepState struct {
	Status         string            `json:"status"` // pending, active, completed, failed, hooked, skipped
	Outputs        map[string]string `json:"outputs,omitempty"`
	StartedAt      string            `json:"started_at,omitempty"`
	CompletedAt    string            `json:"completed_at,omitempty"`
	CompletedCount int               `json:"completed_count,omitempty"` // mechanical counter: how many times this step has completed

	// RepairAttempts is the audit trail of in-wizard recovery cycles run against this
	// step. A new entry is appended each time runRecoveryCycle fires, whether the cycle
	// repaired, escalated, or was interrupted mid-flight and resumed as a new cycle.
	// Missing field in persisted graph state loads as empty (back-compat).
	RepairAttempts []RepairAttempt `json:"repair_attempts,omitempty"`
	// CurrentRepair is non-nil when a recovery cycle is in flight for this step. It
	// carries the phase the wizard is in so a mid-cycle crash can be resumed correctly
	// on next startup. See resumeInFlightRepairs for the resume policy.
	CurrentRepair *InFlightRepair `json:"current_repair,omitempty"`
}

// CrashPhase names a checkpoint inside a recovery cycle. The wizard persists
// graph state every time it advances the phase so a crash at any point leaves
// CurrentRepair.Phase pointing at exactly the work that was in flight.
type CrashPhase string

const (
	PhaseCreateRecoveryBead   CrashPhase = "create_recovery_bead"
	PhaseDiagnose             CrashPhase = "diagnose"
	PhaseDecide               CrashPhase = "decide"
	PhaseExecuteMechanical    CrashPhase = "execute_mechanical"
	PhaseExecuteMergeConflict CrashPhase = "execute_merge_conflict"
	PhaseExecuteWorker        CrashPhase = "execute_worker"
	PhaseApplyBundle          CrashPhase = "apply_bundle"
	PhaseRewindStep           CrashPhase = "rewind_step"
	PhaseRedispatch           CrashPhase = "redispatch"
)

// RecoveryOutcomeKind is the terminal classification of a single recovery cycle.
// It is what runRecoveryCycle returns to the interpreter and is what gets
// appended to StepState.RepairAttempts.
type RecoveryOutcomeKind string

const (
	// RecoveryRepaired means the cycle ran, executed a repair, and the step
	// should be rewound to pending so the interpreter can re-dispatch it.
	RecoveryRepaired RecoveryOutcomeKind = "repaired"
	// RecoveryEscalated means the cycle decided the failure needs human
	// intervention. The interpreter hooks the bead and exits.
	RecoveryEscalated RecoveryOutcomeKind = "escalated"
	// RecoveryBudgetExhausted means the per-step round budget was reached
	// before this cycle could run. The interpreter hooks the bead and exits.
	RecoveryBudgetExhausted RecoveryOutcomeKind = "budget_exhausted"
	// RecoveryInterrupted means the cycle was started but did not complete,
	// typically because the wizard crashed mid-flight. resumeInFlightRepairs
	// translates an abandoned CurrentRepair into this outcome.
	RecoveryInterrupted RecoveryOutcomeKind = "interrupted"
	// RecoveryFailed means the cycle ran but the chosen repair failed to apply
	// cleanly. Counts toward the round budget; next loop iteration retries.
	RecoveryFailed RecoveryOutcomeKind = "failed"
	// RecoveryNoop means decide returned Mode=noop — no repair needed; resume
	// the hooked step as-is.
	RecoveryNoop RecoveryOutcomeKind = "noop"
)

// RepairAttempt is the durable audit record for a single recovery cycle. The
// wizard appends one RepairAttempt per cycle when the cycle finishes, regardless
// of outcome. Interrupted cycles also produce a RepairAttempt so the round
// counter stays honest across crashes (seams 14-15).
type RepairAttempt struct {
	Round          int                 `json:"round"`
	Mode           string              `json:"mode,omitempty"`   // recovery.RepairMode value
	Action         string              `json:"action,omitempty"` // mechanical fn, recipe id, or worker role
	Outcome        RecoveryOutcomeKind `json:"outcome"`
	StartedAt      string              `json:"started_at,omitempty"`
	EndedAt        string              `json:"ended_at,omitempty"`
	RecoveryBeadID string              `json:"recovery_bead_id,omitempty"`
	// FinalPhase records the last phase the wizard reached before the cycle
	// ended (useful for reasoning about interrupted cycles).
	FinalPhase CrashPhase `json:"final_phase,omitempty"`
	// Error carries the error text for Failed cycles (empty otherwise).
	Error string `json:"error,omitempty"`
}

// InFlightRepair captures the live state of a recovery cycle that has not yet
// finished. When the wizard resumes, resumeInFlightRepairs reads this to decide
// whether to honor an in-flight apprentice, close the cycle as interrupted and
// start a new round, or complete a post-repair bookkeeping step.
type InFlightRepair struct {
	Round           int        `json:"round"`
	Mode            string     `json:"mode,omitempty"`
	Action          string     `json:"action,omitempty"`
	Phase           CrashPhase `json:"phase"`
	RecoveryBeadID  string     `json:"recovery_bead_id,omitempty"`
	StartedAt       string     `json:"started_at,omitempty"`
	WorkerSignalKey string     `json:"worker_signal_key,omitempty"` // non-empty iff Mode==worker
}

// WorkspaceState is the persisted runtime state for a single declared workspace.
type WorkspaceState struct {
	Name       string          `json:"name,omitempty"`       // matches the key in formula [workspaces]
	Kind       string          `json:"kind,omitempty"`        // resolved kind from WorkspaceDecl
	Dir        string          `json:"dir,omitempty"`         // absolute path (worktree types only)
	Branch     string          `json:"branch,omitempty"`      // resolved branch name
	BaseBranch string          `json:"base_branch,omitempty"` // resolved base branch
	StartSHA   string          `json:"start_sha,omitempty"`   // session baseline SHA
	Status     string          `json:"status,omitempty"`      // "pending", "active", "closed"
	Scope      string          `json:"scope,omitempty"`       // "run" or "step"
	Ownership  string          `json:"ownership,omitempty"`   // "owned" or "borrowed"
	Cleanup    string          `json:"cleanup,omitempty"`     // "always", "terminal", "never"
	Origin     WorkspaceOrigin `json:"origin,omitempty"`      // how substrate was produced; missing in older persisted shape → defaults to local-bind on load
}

// UnmarshalJSON implements back-compatible decoding: older persisted
// WorkspaceState shapes lack `origin`, so an empty Origin is defaulted
// to WorkspaceOriginLocalBind. Runtime code must therefore never treat
// a zero-value Origin as meaningful — the only valid zero on disk was
// produced by a pre-Origin shape, which we interpret as local-bind.
func (s *WorkspaceState) UnmarshalJSON(data []byte) error {
	type alias WorkspaceState
	var a alias
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	*s = WorkspaceState(a)
	if s.Origin == "" {
		s.Origin = WorkspaceOriginLocalBind
	}
	return nil
}

// Handle converts the persisted WorkspaceState into an exported
// WorkspaceHandle — the contract piece pkg/agent backends consume when
// materializing a worker's substrate. Borrowed is derived from the
// persisted Ownership string (ownership="borrowed"). An empty Origin
// is normalized to WorkspaceOriginLocalBind to keep the zero-value
// contract consistent with UnmarshalJSON.
func (s *WorkspaceState) Handle() WorkspaceHandle {
	origin := s.Origin
	if origin == "" {
		origin = WorkspaceOriginLocalBind
	}
	return WorkspaceHandle{
		Name:       s.Name,
		Kind:       WorkspaceKind(s.Kind),
		Branch:     s.Branch,
		BaseBranch: s.BaseBranch,
		Path:       s.Dir,
		Origin:     origin,
		Borrowed:   s.Ownership == "borrowed",
	}
}

// NewGraphState creates a fresh GraphState from a graph definition, initializing
// all steps to "pending".
func NewGraphState(graph *FormulaStepGraph, beadID, agentName string) *GraphState {
	steps := make(map[string]StepState, len(graph.Steps))
	for name := range graph.Steps {
		steps[name] = StepState{Status: "pending"}
	}

	entry := graph.Entry
	if entry == "" {
		// Fall back to the step with no needs.
		for name, step := range graph.Steps {
			if len(step.Needs) == 0 {
				entry = name
				break
			}
		}
	}

	return &GraphState{
		BeadID:      beadID,
		AgentName:   agentName,
		Formula:     graph.Name,
		Entry:       entry,
		Steps:       steps,
		Counters:    make(map[string]int),
		Workspaces:  make(map[string]WorkspaceState),
		Vars:        make(map[string]string),
		StepBeadIDs: make(map[string]string),
		StartedAt:   time.Now().UTC().Format(time.RFC3339),
	}
}

// GraphStatePath returns the path to the graph state file for the given agent.
func GraphStatePath(agentName string, configDirFn func() (string, error)) string {
	dir, _ := configDirFn()
	return filepath.Join(dir, "runtime", agentName, "graph_state.json")
}

// LoadGraphState loads the graph state from disk for the given agent.
// Returns nil, nil when no state file exists (fresh start).
func LoadGraphState(agentName string, configDirFn func() (string, error)) (*GraphState, error) {
	path := GraphStatePath(agentName, configDirFn)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var state GraphState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("parse graph state: %w", err)
	}
	return &state, nil
}

// Save persists the graph state to disk. Uses 0700 on the parent directory
// and 0600 on the file itself because Auth embeds subscription/api-key
// secrets (spi-rs8sb2); the rest of the state is per-user runtime state
// that has no business being world-readable either.
func (s *GraphState) Save(agentName string, configDirFn func() (string, error)) error {
	path := GraphStatePath(agentName, configDirFn)
	os.MkdirAll(filepath.Dir(path), 0700)
	s.LastActionAt = time.Now().UTC().Format(time.RFC3339)
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal graph state: %w", err)
	}
	return os.WriteFile(path, data, 0600)
}

// HasHookedSteps returns true if any step in the graph has status "hooked".
func (s *GraphState) HasHookedSteps() bool {
	for _, ss := range s.Steps {
		if ss.Status == "hooked" {
			return true
		}
	}
	return false
}

// RemoveGraphState deletes the graph state file on terminal success.
func RemoveGraphState(agentName string, configDirFn func() (string, error)) {
	path := GraphStatePath(agentName, configDirFn)
	os.Remove(path)
}
