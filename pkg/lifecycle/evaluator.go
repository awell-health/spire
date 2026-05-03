package lifecycle

import (
	"fmt"

	"github.com/awell-health/spire/pkg/formula"
)

// legalStatusSet is the set of bead statuses the evaluator may transition
// into today. Statuses outside this set are rejected so misconfigured
// formulas surface early rather than silently writing unknown values.
//
// Landing 3 of spi-sqqero introduces awaiting_review, needs_changes,
// awaiting_human, and merge_pending — the parking statuses that
// replaced the legacy single-bucket parked state.
var legalStatusSet = map[string]struct{}{
	"open":            {},
	"ready":           {},
	"dispatched":      {},
	"in_progress":     {},
	"blocked":         {},
	"deferred":        {},
	"awaiting_review": {},
	"needs_changes":   {},
	"awaiting_human":  {},
	"merge_pending":   {},
	"closed":          {},
}

// ApplyEvent computes the new bead status produced by applying event to a
// bead currently in currentStatus, given the formula running on the bead.
//
// ApplyEvent is pure Go: no I/O, no globals, no goroutines. The same call
// produces the same result in a local subprocess and a cluster pod.
//
// Core event rules (zero-behavior-change with today's writers):
//
//	Filed                 → "open"
//	ReadyToWork           → "ready"
//	WizardClaimed         → "in_progress" (from "ready" or "open"); else unchanged
//	Escalated             → currentStatus (escalation today only adds labels/alerts)
//	Closed                → "closed"
//	ApprenticeNoChanges   → "open" (from "in_progress", HandoffDone=false); else unchanged
//	                        Mirrors pkg/wizard/wizard.go:926 reopen-as-open semantics
//	                        until Landing 3 introduces needs_changes.
//
// Formula step events (FormulaStepStarted/Completed/Failed) consult
// f.Steps[event.Step].Lifecycle. When Lifecycle is nil the evaluator
// returns currentStatus unchanged so legacy formulas keep their
// executor-driven behavior. When Lifecycle is non-nil:
//
//	FormulaStepStarted   → Lifecycle.OnStart (if non-empty)
//	FormulaStepCompleted → first matching Lifecycle.OnCompleteMatch clause
//	                       (each clause's When evaluated by formula.EvalWhen
//	                       against event.Outputs); falls back to
//	                       Lifecycle.OnComplete if no clause matches
//	FormulaStepFailed    → Lifecycle.OnFail.Status (if set), else if
//	                       OnFail.Event == "Escalated" delegates to the
//	                       Escalated core rule
//
// A computed status outside legalStatusSet returns a descriptive error.
func ApplyEvent(currentStatus string, event Event, f *formula.FormulaStepGraph) (string, error) {
	switch ev := event.(type) {
	case Filed:
		return validateLegal(currentStatus, "open")
	case ReadyToWork:
		return validateLegal(currentStatus, "ready")
	case WizardClaimed:
		if currentStatus == "ready" || currentStatus == "open" {
			return validateLegal(currentStatus, "in_progress")
		}
		return currentStatus, nil
	case Escalated:
		return currentStatus, nil
	case Closed:
		return validateLegal(currentStatus, "closed")
	case ApprenticeNoChanges:
		if ev.HandoffDone {
			return currentStatus, nil
		}
		if currentStatus == "in_progress" {
			return validateLegal(currentStatus, "open")
		}
		return currentStatus, nil
	case FormulaStepStarted:
		cfg := stepLifecycle(f, ev.Step)
		if cfg == nil || cfg.OnStart == "" {
			return currentStatus, nil
		}
		return validateLegal(currentStatus, cfg.OnStart)
	case FormulaStepCompleted:
		cfg := stepLifecycle(f, ev.Step)
		if cfg == nil {
			return currentStatus, nil
		}
		for _, clause := range cfg.OnCompleteMatch {
			ok, err := formula.EvalWhen(clause.When, ev.Outputs)
			if err != nil {
				return currentStatus, fmt.Errorf("lifecycle: step %q on_complete_match %q: %w", ev.Step, clause.When, err)
			}
			if ok {
				return validateLegal(currentStatus, clause.Status)
			}
		}
		if cfg.OnComplete == "" {
			return currentStatus, nil
		}
		return validateLegal(currentStatus, cfg.OnComplete)
	case FormulaStepFailed:
		cfg := stepLifecycle(f, ev.Step)
		if cfg == nil || cfg.OnFail == nil {
			return currentStatus, nil
		}
		if cfg.OnFail.Status != "" {
			return validateLegal(currentStatus, cfg.OnFail.Status)
		}
		if cfg.OnFail.Event == "Escalated" {
			return ApplyEvent(currentStatus, Escalated{}, f)
		}
		return currentStatus, nil
	}
	return "", fmt.Errorf("lifecycle: unknown event type %T", event)
}

// stepLifecycle returns the Lifecycle config for the named step, or nil
// when the formula is nil, the step is missing, or the step declares no
// lifecycle block. Callers treat nil as "executor-driven defaults".
func stepLifecycle(f *formula.FormulaStepGraph, step string) *formula.LifecycleConfig {
	if f == nil || step == "" {
		return nil
	}
	cfg, ok := f.Steps[step]
	if !ok {
		return nil
	}
	return cfg.Lifecycle
}

// validateLegal returns next when next is in legalStatusSet, otherwise an
// error describing the misconfiguration. currentStatus is included so the
// error surfaces both sides of the rejected transition.
func validateLegal(currentStatus, next string) (string, error) {
	if _, ok := legalStatusSet[next]; !ok {
		return currentStatus, fmt.Errorf("lifecycle: status %q is not in today's legal set (transition from %q rejected)", next, currentStatus)
	}
	return next, nil
}
