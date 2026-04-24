// Package beadlifecycle is the exclusive owner of the (task bead status ×
// attempt bead × registry entry) state machine.
//
// Three entrypoints:
//   - BeginWork  — local-summon path only (creates attempt + sets in_progress + upserts registry).
//   - ClaimWork  — local + cluster claim (creates attempt + sets in_progress; skips registry for cluster).
//   - EndWork    — all close/interrupt paths from cmd/spire + steward.
//   - OrphanSweep — daemon tick cleanup; local-native only.
//
// The executor's close path (pkg/executor/graph_actions.go:actionBeadFinish) is
// an explicit carve-out and is NOT called through this package.
package beadlifecycle

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/awell-health/spire/pkg/registry"
	"github.com/awell-health/spire/pkg/store"
	"github.com/steveyegge/beads"
)

// Mode controls whether the registry is written.
type Mode int

const (
	// ModeLocal is the local-native deployment mode. wizards.json is written.
	ModeLocal Mode = iota
	// ModeCluster is the cluster-native deployment mode. wizards.json is NOT written.
	ModeCluster
)

// BeginOpts configures BeginWork and ClaimWork.
type BeginOpts struct {
	Mode      Mode   // ModeLocal or ModeCluster
	Worktree  string // path to the wizard's worktree (used in registry entry)
	Tower     string // tower name (used in registry entry)
	AgentName string // wizard agent name
	Backend   string // "process" or "docker"
	Model     string // model identifier for attempt bead
	Branch    string // git branch for attempt bead
}

// EndResult describes the outcome of a work unit.
type EndResult struct {
	// Status is the outcome label written to the attempt bead.
	// One of: "success", "discarded", "interrupted", "reset".
	Status string

	// CascadeReason is appended to the cascade close comment on
	// recovery + alert children. Empty = "work complete".
	// Set this to "reset" or "resummon" when the close is not a
	// natural completion, so audit trails stay interpretable.
	CascadeReason string

	// ReopenTask instructs EndWork to flip the task bead back to
	// open after closing the attempt. Set for interrupted/reset paths.
	ReopenTask bool

	// StripLabels lists labels to remove from the task bead on close.
	// Primarily used to strip "review-approved" on resummon.
	StripLabels []string
}

// OrphanScope controls what OrphanSweep examines.
type OrphanScope struct {
	// BeadID limits the sweep to a single bead's registry entry.
	// When empty, All must be true.
	BeadID string

	// All sweeps every dead entry in the registry.
	All bool
}

// SweepReport summarizes what OrphanSweep found and fixed.
type SweepReport struct {
	// Examined is the number of registry entries checked (Scan A).
	Examined int

	// Dead is the number of dead entries found (PID not running
	// AND no graph_state.json present — dual-signal check).
	Dead int

	// Cleaned is the count of attempts closed + beads reopened.
	Cleaned int

	// Errors is a list of non-fatal errors encountered.
	// OrphanSweep continues past per-entry errors.
	Errors []error
}

// Deps is the narrow dependency surface for beadlifecycle operations.
type Deps interface {
	GetBead(id string) (store.Bead, error)
	UpdateBead(id string, updates map[string]interface{}) error
	CreateAttemptBead(parentID, agentName, model, branch string) (string, error)
	CloseAttemptBead(attemptID string, resultLabel string) error
	ListAttemptsForBead(beadID string) ([]store.Bead, error)
	RemoveLabel(id, label string) error
	// AlertCascadeClose closes all alert+recovery children of sourceBeadID
	// reachable via any of: caused-by, recovery-for (legacy edge), or related.
	// Implementation: calls pkg/recovery.CloseRelatedDependents with
	// depTypes=[]string{"caused-by", "recovery-for", "related"}.
	AlertCascadeClose(sourceBeadID string) error
	// AddLabel adds a label to a bead (used for dead-letter:orphan labeling).
	AddLabel(id, label string) error
	// ListBeads returns all beads matching the filter (used for Scan B).
	ListBeads(filter beads.IssueFilter) ([]store.Bead, error)
}

// pidLivenessProbe is the function used to test whether a PID is alive.
// Replaceable in tests.
var pidLivenessProbe = func(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}

// graphStateCheck tests whether a graph_state.json file exists at the given worktree path.
// Replaceable in tests.
var graphStateCheck = func(worktreePath string) bool {
	gsPath := filepath.Join(worktreePath, ".spire", "graph_state.json")
	_, err := os.Stat(gsPath)
	return err == nil
}

// nowFunc returns the current UTC time as an RFC3339 string.
// Replaceable in tests.
var nowFunc = func() string {
	return time.Now().UTC().Format(time.RFC3339)
}

// BeginWork establishes the full work state for a local-summon.
//
// Steps (local mode), IN THIS ORDER:
//  1. Calls OrphanSweep(deps, OrphanScope{BeadID: beadID}) to close
//     any in-progress attempts for this bead whose registry entries are dead,
//     AND to close any orphan attempt beads (Scan B: phantom-attempt case).
//  2. Upserts the registry entry FIRST (ModeLocal only).
//     Registry-first ordering means that if step 3 or 4 fails, the entry
//     exists for the next OrphanSweep to find and clean up.
//  3. Creates a new attempt bead via Deps.CreateAttemptBead.
//  4. Flips the task bead to status=in_progress.
//
// Returns the new attempt bead ID.
// Error if the bead is already in_progress with a live owner (caller must
// choose to resummon explicitly).
//
// NOTE: BeginWork is local-summon-only in the current architecture.
// Cluster-native dispatch transitions ready→dispatched in the steward
// without calling BeginWork. Cluster-native claim uses ClaimWork.
func BeginWork(deps Deps, beadID string, opts BeginOpts) (string, error) {
	// Step 1: Run orphan sweep scoped to this bead to close stale attempts.
	_, _ = OrphanSweep(deps, OrphanScope{BeadID: beadID})

	// Check current bead state.
	bead, err := deps.GetBead(beadID)
	if err != nil {
		return "", fmt.Errorf("BeginWork: get bead %s: %w", beadID, err)
	}

	// If already in_progress, check whether the owner is live. If there's a
	// live active attempt, refuse (caller must resummon).
	if bead.Status == "in_progress" {
		active, err := findActiveAttempt(deps, beadID)
		if err != nil {
			return "", fmt.Errorf("BeginWork: check active attempt for %s: %w", beadID, err)
		}
		if active != nil {
			agentLabel := store.HasLabel(*active, "agent:")
			return "", fmt.Errorf("BeginWork: bead %s is already in_progress (attempt %s, agent %s); use resummon to take over",
				beadID, active.ID, agentLabel)
		}
	}

	// Step 2: Upsert registry entry FIRST (ModeLocal only).
	if opts.Mode == ModeLocal && opts.AgentName != "" {
		entry := registry.Entry{
			Name:      opts.AgentName,
			BeadID:    beadID,
			Worktree:  opts.Worktree,
			Tower:     opts.Tower,
			StartedAt: nowFunc(),
		}
		if err := registry.Upsert(entry); err != nil {
			return "", fmt.Errorf("BeginWork: registry upsert for %s: %w", beadID, err)
		}
	}

	// Step 3: Create a new attempt bead.
	attemptID, err := deps.CreateAttemptBead(beadID, opts.AgentName, opts.Model, opts.Branch)
	if err != nil {
		return "", fmt.Errorf("BeginWork: create attempt for %s: %w", beadID, err)
	}

	// Step 4: Flip task bead to in_progress.
	if err := deps.UpdateBead(beadID, map[string]interface{}{"status": "in_progress"}); err != nil {
		return attemptID, fmt.Errorf("BeginWork: transition %s to in_progress: %w", beadID, err)
	}

	return attemptID, nil
}

// ClaimWork is the claim-path entrypoint (both local and cluster).
// Called from cmd/spire/claim.go.
//
// ClaimWork is the MINIMAL STATE MACHINE ONLY:
//  1. Reads the bead status. Accepts: ready, dispatched, in_progress, hooked, open.
//  2. If status is in_progress or hooked:
//     - If an attempt exists owned by the same agentName → reclaim (return existing attemptID).
//     - Otherwise → return error (different agent owns it).
//  3. If status is ready, dispatched, open, or "":
//     - Creates a new attempt bead.
//     - Transitions status → in_progress.
//     - For ModeLocal: upserts registry entry.
//
// Returns the attempt bead ID (new or reclaimed).
//
// IMPORTANT: The following business logic from cmd/spire/claim.go stays IN claim.go,
// NOT in ClaimWork:
//   - Instance ownership verification (IsOwnedByInstance / StampAttemptInstance)
//   - Assignee field stamping on the bead
//   - Hooked-step clearing on human-takeover path
func ClaimWork(deps Deps, beadID string, opts BeginOpts) (string, error) {
	bead, err := deps.GetBead(beadID)
	if err != nil {
		return "", fmt.Errorf("ClaimWork: get bead %s: %w", beadID, err)
	}

	switch bead.Status {
	case "in_progress", "hooked":
		// Reclaim path: find the active attempt.
		active, err := findActiveAttempt(deps, beadID)
		if err != nil {
			return "", fmt.Errorf("ClaimWork: check active attempt for %s: %w", beadID, err)
		}
		if active != nil {
			// Check if the same agent owns it.
			existingAgent := store.HasLabel(*active, "agent:")
			if existingAgent == opts.AgentName {
				return active.ID, nil // reclaim
			}
			return "", fmt.Errorf("ClaimWork: bead %s is owned by agent %q; cannot reclaim as %q",
				beadID, existingAgent, opts.AgentName)
		}
		// No active attempt — fall through to creation.

	case "ready", "dispatched", "open", "":
		// Fall through to creation.

	default:
		return "", fmt.Errorf("ClaimWork: bead %s has unclaimable status %q", beadID, bead.Status)
	}

	// Create attempt bead.
	attemptID, err := deps.CreateAttemptBead(beadID, opts.AgentName, opts.Model, opts.Branch)
	if err != nil {
		return "", fmt.Errorf("ClaimWork: create attempt for %s: %w", beadID, err)
	}

	// Transition to in_progress.
	if err := deps.UpdateBead(beadID, map[string]interface{}{"status": "in_progress"}); err != nil {
		return attemptID, fmt.Errorf("ClaimWork: transition %s to in_progress: %w", beadID, err)
	}

	// For ModeLocal: upsert registry entry.
	if opts.Mode == ModeLocal && opts.AgentName != "" {
		entry := registry.Entry{
			Name:      opts.AgentName,
			BeadID:    beadID,
			Worktree:  opts.Worktree,
			Tower:     opts.Tower,
			StartedAt: nowFunc(),
		}
		if err := registry.Upsert(entry); err != nil {
			// Non-fatal: log but do not block claim.
			_ = err
		}
	}

	return attemptID, nil
}

// EndWork closes the work state.
//
// Steps:
//  1. Closes the current attempt bead with EndResult.Status as the result label.
//  2. Closes alert + recovery children of beadID via Deps.AlertCascadeClose(beadID).
//  3. If ReopenTask: transitions task bead in_progress → open.
//     If !ReopenTask: transitions task bead in_progress → closed.
//  4. Strips EndResult.StripLabels from the task bead.
//  5. Removes registry entry (ModeLocal only; noop for ModeCluster).
//
// Idempotent: safe to call even if parts of the state are already clean.
//
// NOTE: EndWork is called from cmd/spire/ paths (resummon, reset) and pkg/steward.
// It is NOT called from pkg/executor/graph_actions.go:actionBeadFinish — the
// executor's close path is a deliberate carve-out.
func EndWork(deps Deps, beadID string, opts BeginOpts, result EndResult) error {
	// Step 1: Close the current attempt bead.
	active, err := findActiveAttempt(deps, beadID)
	if err == nil && active != nil {
		resultLabel := result.Status
		if resultLabel == "" {
			resultLabel = "interrupted"
		}
		if cerr := deps.CloseAttemptBead(active.ID, resultLabel); cerr != nil {
			// Non-fatal: continue with rest of cleanup.
			_ = cerr
		}
	}

	// Step 2: Close alert + recovery children via cascade.
	if cerr := deps.AlertCascadeClose(beadID); cerr != nil {
		// Non-fatal: continue.
		_ = cerr
	}

	// Step 3: Transition task bead.
	if result.ReopenTask {
		if err := deps.UpdateBead(beadID, map[string]interface{}{"status": "open"}); err != nil {
			return fmt.Errorf("EndWork: reopen %s: %w", beadID, err)
		}
	} else {
		if err := deps.UpdateBead(beadID, map[string]interface{}{"status": "closed"}); err != nil {
			return fmt.Errorf("EndWork: close %s: %w", beadID, err)
		}
	}

	// Step 4: Strip labels from the task bead.
	for _, lbl := range result.StripLabels {
		if err := deps.RemoveLabel(beadID, lbl); err != nil {
			// Non-fatal.
			_ = err
		}
	}

	// Step 5: Remove registry entry (ModeLocal only).
	if opts.Mode == ModeLocal && opts.AgentName != "" {
		if rerr := registry.Remove(opts.AgentName); rerr != nil {
			// Non-fatal.
			_ = rerr
		}
	}

	return nil
}

// OrphanSweep is the canonical cleanup pass. LOCAL-NATIVE ONLY.
// Cluster-native uses CheckBeadHealth + stale heartbeat instead.
//
// OrphanSweep runs TWO complementary scans to close all phantom-attempt cases:
//
// SCAN A — Registry-driven (detects crashed wizards):
//
//	An entry is declared dead when BOTH conditions hold:
//	(a) syscall.Kill(entry.PID, 0) returns an error (PID not running), AND
//	(b) no graph_state.json exists at entry.Worktree/.spire/graph_state.json.
//	Dead PID + graph_state.json present → NOT orphaned (crash-safe resume).
//	Dead PID + no graph_state.json → orphaned; close attempt, reopen bead, remove entry.
//
// SCAN B — Attempt-driven (detects phantom attempts from failed registry writes):
//
//	Scan all in_progress attempt beads whose parent task bead is in_progress.
//	For each, check if there is a live registry entry with the attempt's agent name.
//	If no live registry entry exists (registry write failed during BeginWork):
//	check graph_state.json — if absent, close attempt + reopen parent bead.
//
// Called every tick by the steward daemon AND defensively by BeginWork.
// Idempotent: second call on already-clean state is a no-op.
func OrphanSweep(deps Deps, scope OrphanScope) (SweepReport, error) {
	var report SweepReport

	// Load current registry state once.
	entries, err := registry.List()
	if err != nil {
		return report, fmt.Errorf("OrphanSweep: list registry: %w", err)
	}

	// Build a map of live agents (name → entry) for Scan B.
	liveAgents := make(map[string]registry.Entry)
	for _, e := range entries {
		if pidLivenessProbe(e.PID) {
			liveAgents[e.Name] = e
		}
	}

	// --- SCAN A: registry-driven ---
	var scanAEntries []registry.Entry
	if scope.All {
		scanAEntries = entries
	} else if scope.BeadID != "" {
		for _, e := range entries {
			if e.BeadID == scope.BeadID {
				scanAEntries = append(scanAEntries, e)
			}
		}
	}

	for _, entry := range scanAEntries {
		report.Examined++

		// Dual-signal: PID dead AND no graph_state.json.
		if pidLivenessProbe(entry.PID) {
			continue // still alive
		}
		if graphStateCheck(entry.Worktree) {
			continue // graph state present — crash-safe resume; not orphaned
		}

		// Declared dead: close active attempt + label task bead + reopen bead + remove entry.
		report.Dead++

		// Close active attempt bead.
		active, err := findActiveAttempt(deps, entry.BeadID)
		if err == nil && active != nil {
			if cerr := deps.CloseAttemptBead(active.ID, "interrupted:orphan"); cerr != nil {
				report.Errors = append(report.Errors, fmt.Errorf("scan A: close attempt %s: %w", active.ID, cerr))
			}
		}

		// Add dead-letter:orphan label to task bead.
		if lerr := deps.AddLabel(entry.BeadID, "dead-letter:orphan"); lerr != nil {
			report.Errors = append(report.Errors, fmt.Errorf("scan A: label %s dead-letter:orphan: %w", entry.BeadID, lerr))
		}

		// Reopen task bead (only if still in_progress or open).
		bead, gerr := deps.GetBead(entry.BeadID)
		if gerr == nil && (bead.Status == "in_progress" || bead.Status == "open") {
			if uerr := deps.UpdateBead(entry.BeadID, map[string]interface{}{"status": "open"}); uerr != nil {
				report.Errors = append(report.Errors, fmt.Errorf("scan A: reopen %s: %w", entry.BeadID, uerr))
			}
		}

		// Remove registry entry.
		if rerr := registry.Remove(entry.Name); rerr != nil {
			report.Errors = append(report.Errors, fmt.Errorf("scan A: remove registry entry %s: %w", entry.Name, rerr))
		}

		report.Cleaned++
	}

	// --- SCAN B: attempt-driven (phantom-attempt detection) ---
	// Run Scan B when scope.All is true OR when scope.BeadID is set.
	if scope.All || scope.BeadID != "" {
		allBeads, err := deps.ListBeads(beads.IssueFilter{})
		if err != nil {
			// Non-fatal — report error but return what we have.
			report.Errors = append(report.Errors, fmt.Errorf("scan B: list beads: %w", err))
			return report, nil
		}

		for _, b := range allBeads {
			if !isAttemptBead(b) {
				continue
			}
			if b.Status != "in_progress" && b.Status != "open" {
				continue
			}

			// Scope filter for BeadID mode.
			if scope.BeadID != "" && b.Parent != scope.BeadID {
				continue
			}

			// Get agent name from attempt bead label.
			agentName := store.HasLabel(b, "agent:")
			if agentName == "" {
				continue // cannot identify agent — leave alone
			}

			// Gate A: live registry entry exists → not a phantom.
			if _, ok := liveAgents[agentName]; ok {
				continue
			}

			// Gate B: graph_state.json at the agent's worktree.
			// Look up the registry entry (possibly dead) to find the worktree.
			var worktree string
			for _, e := range entries {
				if e.Name == agentName {
					worktree = e.Worktree
					break
				}
			}
			if worktree != "" && graphStateCheck(worktree) {
				continue // crash-safe resume — not orphaned
			}

			// Phantom attempt confirmed: close it + reopen parent.
			if cerr := deps.CloseAttemptBead(b.ID, "interrupted:orphan"); cerr != nil {
				report.Errors = append(report.Errors, fmt.Errorf("scan B: close attempt %s: %w", b.ID, cerr))
				continue
			}

			// Reopen parent task bead.
			if b.Parent != "" {
				parent, gerr := deps.GetBead(b.Parent)
				if gerr == nil && (parent.Status == "in_progress" || parent.Status == "open") {
					if uerr := deps.UpdateBead(b.Parent, map[string]interface{}{"status": "open"}); uerr != nil {
						report.Errors = append(report.Errors, fmt.Errorf("scan B: reopen parent %s: %w", b.Parent, uerr))
					}
				}
			}

			report.Dead++
			report.Cleaned++
		}
	}

	return report, nil
}

// findActiveAttempt returns the first open/in_progress attempt bead for beadID.
func findActiveAttempt(deps Deps, beadID string) (*store.Bead, error) {
	attempts, err := deps.ListAttemptsForBead(beadID)
	if err != nil {
		return nil, err
	}
	for i, a := range attempts {
		if a.Status == "in_progress" || a.Status == "open" {
			return &attempts[i], nil
		}
	}
	return nil, nil
}

// isAttemptBead returns true for beads carrying the "attempt" label or type.
func isAttemptBead(b store.Bead) bool {
	if b.Type == "attempt" {
		return true
	}
	for _, l := range b.Labels {
		if l == "attempt" {
			return true
		}
	}
	return false
}

