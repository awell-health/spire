// Package beadlifecycle is the exclusive owner of the (task bead status ×
// attempt bead × wizard-registry entry) state machine.
//
// Three entrypoints:
//   - BeginWork   — local-summon path only (creates attempt + sets in_progress + upserts registry).
//   - ClaimWork   — local + cluster claim (creates attempt + sets in_progress; skips registry write for cluster).
//   - EndWork     — all close/interrupt paths from cmd/spire + steward.
//   - OrphanSweep — daemon tick cleanup; mode-portable.
//
// Liveness is delegated to the [wizardregistry.Registry] interface. The
// race-safety guarantee documented on that interface is the load-bearing
// contract for the spi-5bzu9r incident: every liveness decision in this
// package goes through a fresh [wizardregistry.Registry.IsAlive] call,
// never a snapshot.
//
// The executor's close path (pkg/executor/graph_actions.go:actionBeadFinish) is
// an explicit carve-out and is NOT called through this package.
package beadlifecycle

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	spireconfig "github.com/awell-health/spire/pkg/config"
	"github.com/awell-health/spire/pkg/store"
	"github.com/awell-health/spire/pkg/wizardregistry"
	"github.com/steveyegge/beads"
)

// Mode controls whether the registry is written.
type Mode int

const (
	// ModeLocal is the local-native deployment mode. The wizard-registry
	// is written via [wizardregistry.Registry.Upsert].
	ModeLocal Mode = iota
	// ModeCluster is the cluster-native deployment mode. The wizard-registry
	// is operator-owned and read-only from clients; client-side Upsert/Remove
	// returns [wizardregistry.ErrReadOnly] which is ignored here.
	ModeCluster
)

// BeginOpts configures BeginWork and ClaimWork.
type BeginOpts struct {
	Mode      Mode   // ModeLocal or ModeCluster
	Worktree  string // path to the wizard's worktree (used in registry entry; informational only after migration to wizardregistry)
	Tower     string // tower name (informational; not stored in wizardregistry.Wizard)
	AgentName string // wizard agent name (used as wizardregistry.Wizard.ID)
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

	// Dead is the number of dead entries found.
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
	// GetAttemptHeartbeat reads the active attempt's last_seen_at execution
	// heartbeat written by the executor. Returns (lastSeen, true, nil) when
	// the heartbeat metadata is present, (zero, false, nil) when absent
	// (e.g., a brand-new attempt that has not yet emitted a heartbeat),
	// and (zero, false, err) on read failure. OrphanSweep consults this
	// before closing an attempt to avoid orphaning a wizard whose heartbeat
	// is fresh even when local registry probes report dead/missing
	// (spi-p2ou7v).
	GetAttemptHeartbeat(attemptID string) (time.Time, bool, error)
}

// heartbeatFreshness is the window within which an attempt's last_seen_at
// metadata makes the wizard "definitively alive" for OrphanSweep purposes.
// If the heartbeat is more recent than this, OrphanSweep skips the close
// even when the registry says the wizard is dead/missing — registry blips,
// stale PIDs, and PID-probe false negatives must not orphan a live wizard.
//
// Set to mirror the steward's default ShutdownThreshold (60m, see
// cmd/spire/steward.go default) so OrphanSweep and the steward agree on
// which heartbeats count as "still alive": a heartbeat the steward would
// not yet kill on must not be orphaned here either (spi-9ixgqy +
// spi-p2ou7v).
const heartbeatFreshness = spireconfig.DefaultAgentShutdownThreshold

// nowTimeFunc returns the current UTC time. Replaceable in tests so the
// heartbeat-freshness gate can be exercised deterministically.
var nowTimeFunc = func() time.Time { return time.Now().UTC() }

// attemptHeartbeatFresh reports whether an attempt's heartbeat is fresh
// enough that OrphanSweep should skip closing it as orphan. Returns
// (skip, lastSeen, present): when skip=true the caller MUST NOT orphan
// the attempt; when skip=false the existing orphan policy applies.
//
// !present (no heartbeat metadata) and read errors fall through to the
// existing conservative behavior — never widen orphan-skip on missing
// data, since a brand-new attempt may not have heartbeated yet on the
// first sweep tick.
func attemptHeartbeatFresh(deps Deps, attemptID string) (skip bool, lastSeen time.Time, present bool) {
	lastSeen, present, err := deps.GetAttemptHeartbeat(attemptID)
	if err != nil {
		return false, time.Time{}, false
	}
	if !present {
		return false, time.Time{}, false
	}
	if nowTimeFunc().Sub(lastSeen) < heartbeatFreshness {
		return true, lastSeen, true
	}
	return false, lastSeen, true
}

func orphanSweepAttemptID(attempt *store.Bead) string {
	if attempt == nil || attempt.ID == "" {
		return "-"
	}
	return attempt.ID
}

func orphanSweepTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.UTC().Format(time.RFC3339)
}

// nowFunc returns the current UTC time as an RFC3339 string.
// Replaceable in tests.
var nowFunc = func() string {
	return time.Now().UTC().Format(time.RFC3339)
}

// upsertWizard registers (or refreshes) the wizard entry for opts in reg.
// Read-only backends (cluster mode) silently ignore the call.
func upsertWizard(reg wizardregistry.Registry, beadID string, opts BeginOpts) error {
	if reg == nil || opts.AgentName == "" {
		return nil
	}
	w := wizardregistry.Wizard{
		ID:        opts.AgentName,
		Mode:      modeToRegistry(opts.Mode),
		BeadID:    beadID,
		StartedAt: time.Now().UTC(),
	}
	err := reg.Upsert(context.Background(), w)
	if errors.Is(err, wizardregistry.ErrReadOnly) {
		return nil
	}
	return err
}

// removeWizard deletes the wizard entry for opts in reg.
// Read-only backends (cluster mode) and ErrNotFound are silently ignored.
func removeWizard(reg wizardregistry.Registry, opts BeginOpts) error {
	if reg == nil || opts.AgentName == "" {
		return nil
	}
	err := reg.Remove(context.Background(), opts.AgentName)
	if errors.Is(err, wizardregistry.ErrReadOnly) || errors.Is(err, wizardregistry.ErrNotFound) {
		return nil
	}
	return err
}

func modeToRegistry(m Mode) wizardregistry.Mode {
	if m == ModeCluster {
		return wizardregistry.ModeCluster
	}
	return wizardregistry.ModeLocal
}

// BeginWork establishes the full work state for a local-summon.
//
// Steps (local mode), IN THIS ORDER:
//  1. Calls OrphanSweep(deps, reg, OrphanScope{BeadID: beadID}) to close
//     any in-progress attempts for this bead whose registered wizards are
//     no longer alive (Scan A) OR have no live registry entry (Scan B).
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
func BeginWork(deps Deps, reg wizardregistry.Registry, beadID string, opts BeginOpts) (string, error) {
	// Step 1: Run orphan sweep scoped to this bead to close stale attempts.
	_, _ = OrphanSweep(deps, reg, OrphanScope{BeadID: beadID})

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
	if opts.Mode == ModeLocal {
		if err := upsertWizard(reg, beadID, opts); err != nil {
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
func ClaimWork(deps Deps, reg wizardregistry.Registry, beadID string, opts BeginOpts) (string, error) {
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

	// For ModeLocal: upsert registry entry. Errors are non-fatal for claim.
	if opts.Mode == ModeLocal {
		_ = upsertWizard(reg, beadID, opts)
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
//  5. Removes registry entry (ModeLocal only; cluster Remove returns
//     ErrReadOnly which is silently ignored).
//
// Idempotent: safe to call even if parts of the state are already clean.
//
// NOTE: EndWork is called from cmd/spire/ paths (resummon, reset) and pkg/steward.
// It is NOT called from pkg/executor/graph_actions.go:actionBeadFinish — the
// executor's close path is a deliberate carve-out.
func EndWork(deps Deps, reg wizardregistry.Registry, beadID string, opts BeginOpts, result EndResult) error {
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
		// Successful terminal close: strip any stale dead-letter:orphan
		// label written by an earlier (false) OrphanSweep verdict. Without
		// this, a wizard that survived a transient orphan would close
		// successfully but leave the parent labeled dead-letter:orphan,
		// producing the contradictory final state seen on spd-3lhw
		// (spi-p2ou7v).
		if result.Status == "success" {
			if err := deps.RemoveLabel(beadID, "dead-letter:orphan"); err != nil {
				// Non-fatal.
				_ = err
			}
		}
	}

	// Step 4: Strip labels from the task bead.
	for _, lbl := range result.StripLabels {
		if err := deps.RemoveLabel(beadID, lbl); err != nil {
			// Non-fatal.
			_ = err
		}
	}

	// Step 5: Remove registry entry (ModeLocal only). Errors are non-fatal.
	if opts.Mode == ModeLocal {
		_ = removeWizard(reg, opts)
	}

	return nil
}

// OrphanSweep is the canonical cleanup pass. MODE-PORTABLE.
//
// OrphanSweep delegates every liveness decision to a fresh
// [wizardregistry.Registry.IsAlive] call — there is no snapshot of the
// wizard set. The race-safety contract on the [wizardregistry.Registry]
// interface guarantees that a wizard upserted concurrently with a sweep
// cannot be mis-classified as dead. This closes the spi-5bzu9r incident
// at the architectural layer rather than via per-impl workarounds.
//
// OrphanSweep runs TWO complementary scans:
//
// SCAN A — Registry-driven (detects crashed wizards):
//
//	Iterate the wizards reported by Registry.List. For each entry, ask
//	Registry.IsAlive. When the answer is "dead", close the active attempt
//	for the entry's bead, label the bead dead-letter:orphan, reopen the
//	bead, and remove the registry entry. Transient IsAlive errors fail
//	open — never close an attempt because the registry briefly couldn't
//	answer.
//
// SCAN B — Attempt-driven (detects phantom attempts and stale entries):
//
//	Scan every in_progress attempt bead. For each, look up the agent name
//	from the attempt label and ask Registry.IsAlive. ErrNotFound means
//	the attempt has no registered wizard at all (phantom — close it).
//	A "dead" answer means the wizard once existed but is no longer live
//	(close it). A "live" answer leaves the attempt alone.
//
// Called every tick by the steward daemon AND defensively by BeginWork.
// Idempotent: a second call on already-clean state is a no-op.
func OrphanSweep(deps Deps, reg wizardregistry.Registry, scope OrphanScope) (SweepReport, error) {
	var report SweepReport
	if reg == nil {
		return report, fmt.Errorf("OrphanSweep: nil registry")
	}
	ctx := context.Background()

	// Load current registry state. Sweep is the read; per-entry liveness is
	// the verdict and uses fresh IsAlive calls.
	entries, err := reg.List(ctx)
	if err != nil {
		return report, fmt.Errorf("OrphanSweep: list registry: %w", err)
	}

	// --- SCAN A: registry-driven ---
	var scanAEntries []wizardregistry.Wizard
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

		alive, err := reg.IsAlive(ctx, entry.ID)
		registryVerdict := "dead"
		if err != nil && !errors.Is(err, wizardregistry.ErrNotFound) {
			// Transient failure — fail open. Mis-classifying a registry-read
			// blip as "dead" is exactly the spi-5bzu9r failure mode.
			log.Printf("OrphanSweep: scan A: IsAlive(%s) failed, skipping: %v", entry.ID, err)
			continue
		}
		if errors.Is(err, wizardregistry.ErrNotFound) {
			registryVerdict = "missing"
		}
		if alive {
			continue
		}

		// Heartbeat-freshness gate (spi-p2ou7v): the registry says
		// dead/missing, but if the active attempt is still heartbeating
		// the execution owner is alive and orphaning would falsely
		// reopen the parent bead. The attempt heartbeat is the
		// execution-owner truth surface; the local registry is only
		// the local process lookup.
		active, aerr := findActiveAttempt(deps, entry.BeadID)
		heartbeatState := "no-attempt"
		lastSeenAt := time.Time{}
		if aerr != nil {
			heartbeatState = "attempt-lookup-error"
			log.Printf("OrphanSweep: scan A: findActiveAttempt bead=%s agent=%s: %v", entry.BeadID, entry.ID, aerr)
		}
		if aerr == nil && active != nil {
			if skip, lastSeen, present := attemptHeartbeatFresh(deps, active.ID); skip {
				log.Printf("OrphanSweep: scan A: skip-fresh-heartbeat agent=%s attempt=%s last_seen_at=%s registry_verdict=%s",
					entry.ID, active.ID, lastSeen.Format(time.RFC3339), registryVerdict)
				continue
			} else if present {
				heartbeatState = "stale"
				lastSeenAt = lastSeen
			} else {
				heartbeatState = "missing-or-error"
			}
		}

		log.Printf("OrphanSweep: scan A: orphaning bead=%s agent=%s attempt=%s registry_verdict=%s heartbeat_state=%s last_seen_at=%s",
			entry.BeadID, entry.ID, orphanSweepAttemptID(active), registryVerdict, heartbeatState, orphanSweepTime(lastSeenAt))

		// Declared dead.
		report.Dead++

		// Close active attempt bead.
		if aerr == nil && active != nil {
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

		// Remove registry entry. ErrReadOnly (cluster) and ErrNotFound are
		// expected and silently absorbed.
		if rerr := reg.Remove(ctx, entry.ID); rerr != nil &&
			!errors.Is(rerr, wizardregistry.ErrReadOnly) &&
			!errors.Is(rerr, wizardregistry.ErrNotFound) {
			report.Errors = append(report.Errors, fmt.Errorf("scan A: remove registry entry %s: %w", entry.ID, rerr))
		}

		report.Cleaned++
	}

	// --- SCAN B: attempt-driven (phantom-attempt detection + stale-entry mop-up) ---
	if scope.All || scope.BeadID != "" {
		allBeads, err := deps.ListBeads(beads.IssueFilter{})
		if err != nil {
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

			agentName := store.HasLabel(b, "agent:")
			if agentName == "" {
				continue // cannot identify agent — leave alone
			}

			// Fresh authoritative read. ErrNotFound → no registered wizard
			// (phantom). Other errors → fail open (never close on a transient
			// read failure).
			alive, err := reg.IsAlive(ctx, agentName)
			registryVerdict := "dead"
			if errors.Is(err, wizardregistry.ErrNotFound) {
				// Phantom attempt: close it + reopen parent (subject to
				// heartbeat-freshness gate below).
				registryVerdict = "missing"
			} else if err != nil {
				log.Printf("OrphanSweep: scan B: IsAlive(%s) failed, skipping: %v", agentName, err)
				continue
			} else if alive {
				continue
			}

			// Heartbeat-freshness gate (spi-p2ou7v): registry says
			// dead/missing but a fresh attempt heartbeat means the
			// execution owner is alive. Skip rather than orphan a live
			// wizard.
			heartbeatState := "missing-or-error"
			lastSeenAt := time.Time{}
			if skip, lastSeen, present := attemptHeartbeatFresh(deps, b.ID); skip {
				log.Printf("OrphanSweep: scan B: skip-fresh-heartbeat agent=%s attempt=%s last_seen_at=%s registry_verdict=%s",
					agentName, b.ID, lastSeen.Format(time.RFC3339), registryVerdict)
				continue
			} else if present {
				heartbeatState = "stale"
				lastSeenAt = lastSeen
			}

			log.Printf("OrphanSweep: scan B: orphaning parent=%s agent=%s attempt=%s registry_verdict=%s heartbeat_state=%s last_seen_at=%s",
				b.Parent, agentName, b.ID, registryVerdict, heartbeatState, orphanSweepTime(lastSeenAt))

			// Phantom or dead-but-still-listed: close attempt + reopen parent.
			if cerr := deps.CloseAttemptBead(b.ID, "interrupted:orphan"); cerr != nil {
				report.Errors = append(report.Errors, fmt.Errorf("scan B: close attempt %s: %w", b.ID, cerr))
				continue
			}

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
