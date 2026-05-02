package lifecycle

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"runtime"
	"strings"
	"time"

	"github.com/awell-health/spire/pkg/formula"
	"github.com/awell-health/spire/pkg/store"
)

// VerboseTransitions controls whether RecordEvent emits a per-write
// audit log line. Off by default; tooling (steward, CLI) flips it via
// SetVerboseTransitions when surfaced through their own verbosity flags.
var verboseTransitions bool

// SetVerboseTransitions toggles transition log emission. The flag is
// package-scoped because RecordEvent is intended to be the sole writer
// of bead.status — there is no per-call knob and no need for one.
func SetVerboseTransitions(on bool) {
	verboseTransitions = on
}

// serviceDeps is the unexported dependency surface RecordEvent uses. It
// is intentionally narrow: GetBead, ResolveFormula, and a CAS update
// primitive. Tests construct alternate values; production keeps the
// defaults wired to pkg/store + pkg/formula.
type serviceDeps struct {
	GetBead         func(id string) (store.Bead, error)
	ResolveFormula  func(b *store.Bead) (*formula.FormulaStepGraph, error)
	UpdateStatusCAS func(ctx context.Context, beadID, expected, next string) (rowsAffected int64, err error)
}

// defaultServiceDeps wires production implementations. The CAS path uses
// store.ActiveDB (which the dolt backend exposes) so it runs identically
// in local subprocess and cluster pod modes — both paths talk to the
// same logical issues table.
var defaultServiceDeps = serviceDeps{
	GetBead: store.GetBead,
	ResolveFormula: func(b *store.Bead) (*formula.FormulaStepGraph, error) {
		if b == nil {
			return nil, fmt.Errorf("lifecycle: nil bead")
		}
		return formula.ResolveV3(formula.BeadInfo{
			ID:     b.ID,
			Type:   b.Type,
			Labels: b.Labels,
		})
	},
	UpdateStatusCAS: defaultUpdateStatusCAS,
}

// activeServiceDeps is the resolver used by RecordEvent. Tests swap it
// via withServiceDeps; production callers leave it untouched.
var activeServiceDeps = defaultServiceDeps

// withServiceDeps temporarily swaps activeServiceDeps. The returned
// closure restores the prior value. Defined unexported so only
// in-package tests can call it.
func withServiceDeps(d serviceDeps) func() {
	prev := activeServiceDeps
	activeServiceDeps = d
	return func() { activeServiceDeps = prev }
}

// RecordEvent is the narrow entry point through which all bead status
// mutations flow. It loads the bead, resolves the formula, runs
// ApplyEvent, and persists the new status with optimistic CAS so a
// concurrent transition surfaces as ErrTransitionConflict instead of
// silently overwriting.
//
// Returns nil for no-op transitions (newStatus == currentStatus); no
// UPDATE is issued in that case.
//
// Mode portability: every dependency goes through pkg/store /
// pkg/formula abstractions. There are no filesystem paths, no
// local-only assumptions; the same code path runs in local-native
// subprocesses and cluster-native pods.
func RecordEvent(ctx context.Context, beadID string, event Event) error {
	if beadID == "" {
		return fmt.Errorf("lifecycle: RecordEvent: empty beadID")
	}
	if event == nil {
		return fmt.Errorf("lifecycle: RecordEvent: nil event")
	}

	bead, err := activeServiceDeps.GetBead(beadID)
	if err != nil {
		return fmt.Errorf("lifecycle: RecordEvent get %s: %w", beadID, err)
	}

	f, err := activeServiceDeps.ResolveFormula(&bead)
	if err != nil {
		return fmt.Errorf("lifecycle: RecordEvent resolve formula for %s: %w", beadID, err)
	}

	newStatus, err := ApplyEvent(bead.Status, event, f)
	if err != nil {
		return fmt.Errorf("lifecycle: RecordEvent apply %s: %w", beadID, err)
	}

	if newStatus == bead.Status {
		return nil
	}

	rows, err := activeServiceDeps.UpdateStatusCAS(ctx, beadID, bead.Status, newStatus)
	if err != nil {
		return fmt.Errorf("lifecycle: RecordEvent CAS %s %s→%s: %w", beadID, bead.Status, newStatus, err)
	}
	if rows == 0 {
		return ErrTransitionConflict
	}

	caller := callerOutsideLifecycle()
	log.Printf("[lifecycle] bead=%s event=%s from=%s to=%s caller=%s step=%s",
		beadID, eventTypeName(event), bead.Status, newStatus, caller, eventStepName(event))

	if verboseTransitions {
		log.Printf("lifecycle: transition bead=%s old=%s new=%s event=%s step=%s",
			beadID, bead.Status, newStatus, eventTypeName(event), eventStepName(event))
	}

	return nil
}

// callerOutsideLifecycle walks up the call stack and returns "file:line"
// for the first frame whose file is outside pkg/lifecycle. The audit log
// uses this so a `[lifecycle]` line names the actual writer (executor,
// wizard, steward, …) rather than service.go itself.
//
// Returns "?" if no non-lifecycle frame is found within a reasonable
// depth — never panics, never blocks the write path.
func callerOutsideLifecycle() string {
	const maxDepth = 16
	const skipFromCaller = 2 // 0=this fn, 1=RecordEvent
	for skip := skipFromCaller; skip < maxDepth; skip++ {
		_, file, line, ok := runtime.Caller(skip)
		if !ok {
			break
		}
		if strings.Contains(file, "/pkg/lifecycle/") || strings.HasSuffix(file, "/pkg/lifecycle") {
			continue
		}
		return fmt.Sprintf("%s:%d", trimFileForCaller(file), line)
	}
	return "?"
}

// trimFileForCaller drops everything before the repository-relative
// segment so log lines stay grep-friendly ("pkg/wizard/wizard.go:926"
// rather than "/Users/.../spire/pkg/wizard/wizard.go:926").
func trimFileForCaller(file string) string {
	const marker = "/spire/"
	if i := strings.LastIndex(file, marker); i >= 0 {
		return file[i+len(marker):]
	}
	return file
}

// defaultUpdateStatusCAS issues the optimistic CAS update against the
// shared Dolt-backed issues table. RowsAffected == 0 means the
// pre-event status no longer matches — the caller treats that as
// ErrTransitionConflict.
func defaultUpdateStatusCAS(ctx context.Context, beadID, expected, next string) (int64, error) {
	db, ok := store.ActiveDB()
	if !ok || db == nil {
		return 0, fmt.Errorf("lifecycle: no active DB for CAS update")
	}
	res, err := db.ExecContext(ctx,
		`UPDATE issues SET status = ?, updated_at = ? WHERE id = ? AND status = ?`,
		next, time.Now().UTC(), beadID, expected,
	)
	if err != nil {
		return 0, err
	}
	return rowsAffectedSafe(res)
}

// rowsAffectedSafe extracts RowsAffected, mapping driver-side errors to a
// zero count + the original error so RecordEvent can decide between
// ErrTransitionConflict (rows == 0) and a generic write failure.
func rowsAffectedSafe(res sql.Result) (int64, error) {
	if res == nil {
		return 0, fmt.Errorf("nil sql.Result")
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return n, nil
}

// eventTypeName returns the short name used in the verbose transition
// log. Mirrors the concrete type name (Filed, FormulaStepCompleted, …)
// so log readers can grep for an exact event class.
func eventTypeName(ev Event) string {
	switch ev.(type) {
	case Filed:
		return "Filed"
	case ReadyToWork:
		return "ReadyToWork"
	case WizardClaimed:
		return "WizardClaimed"
	case FormulaStepStarted:
		return "FormulaStepStarted"
	case FormulaStepCompleted:
		return "FormulaStepCompleted"
	case FormulaStepFailed:
		return "FormulaStepFailed"
	case Escalated:
		return "Escalated"
	case Closed:
		return "Closed"
	case ApprenticeNoChanges:
		return "ApprenticeNoChanges"
	}
	return fmt.Sprintf("%T", ev)
}

// eventStepName returns the formula step the event references, or "-"
// for core events that carry no step. Used in the audit log line.
func eventStepName(ev Event) string {
	switch e := ev.(type) {
	case FormulaStepStarted:
		return e.Step
	case FormulaStepCompleted:
		return e.Step
	case FormulaStepFailed:
		return e.Step
	}
	return "-"
}
