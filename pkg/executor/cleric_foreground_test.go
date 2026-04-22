package executor

import (
	"errors"
	"testing"
)

func TestBuildPhaseEvent(t *testing.T) {
	cases := []struct {
		name    string
		step    string
		outputs map[string]string
		stepErr error
		check   func(t *testing.T, phase string, details map[string]any,
			branch, action, reason, verdict, errStr string, confidence float64)
	}{
		{
			name: "collect_context copies class/source/failed_step/attempts",
			step: "collect_context",
			outputs: map[string]string{
				"failure_class":       "merge-failure",
				"source_bead":         "spi-origin",
				"source_attempt_id":   "spi-attempt",
				"failed_step":         "merge",
				"verification_status": "pending",
				"total_attempts":      "3",
			},
			check: func(t *testing.T, phase string, details map[string]any, _, _, _, _, _ string, _ float64) {
				if phase != "collect_context" {
					t.Errorf("phase = %q", phase)
				}
				if details["class"] != "merge-failure" {
					t.Errorf("class = %v", details["class"])
				}
				if details["source_bead"] != "spi-origin" {
					t.Errorf("source_bead = %v", details["source_bead"])
				}
				if details["source_attempt_id"] != "spi-attempt" {
					t.Errorf("source_attempt_id = %v", details["source_attempt_id"])
				}
				if details["failed_step"] != "merge" {
					t.Errorf("failed_step = %v", details["failed_step"])
				}
				if details["verification_status"] != "pending" {
					t.Errorf("verification_status = %v", details["verification_status"])
				}
				if details["attempts"] != "3" {
					t.Errorf("attempts = %v", details["attempts"])
				}
			},
		},
		{
			name: "decide surfaces branch/reasoning/confidence and optional details",
			step: "decide",
			outputs: map[string]string{
				"decide_branch": "promoted-recipe",
				"reasoning":     "prior recipe matched",
				"confidence":    "0.87",
				"needs_human":   "false",
				"promoted":      "true",
			},
			check: func(t *testing.T, _ string, details map[string]any, branch, _, reason, _, _ string, confidence float64) {
				if branch != "promoted-recipe" {
					t.Errorf("branch = %q", branch)
				}
				if reason != "prior recipe matched" {
					t.Errorf("reason = %q", reason)
				}
				if confidence != 0.87 {
					t.Errorf("confidence = %v, want 0.87", confidence)
				}
				if details["needs_human"] != "false" {
					t.Errorf("needs_human = %v", details["needs_human"])
				}
				if details["promoted"] != "true" {
					t.Errorf("promoted = %v", details["promoted"])
				}
			},
		},
		{
			name: "decide tolerates malformed confidence without panic",
			step: "decide",
			outputs: map[string]string{
				"decide_branch": "claude",
				"confidence":    "not-a-number",
			},
			check: func(t *testing.T, _ string, _ map[string]any, branch, _, _, _, _ string, confidence float64) {
				if branch != "claude" {
					t.Errorf("branch = %q", branch)
				}
				if confidence != 0 {
					t.Errorf("confidence should default to 0 on parse failure; got %v", confidence)
				}
			},
		},
		{
			name: "execute maps action/mode/worker_attempt_id/handoff_mode/reason/status",
			step: "execute",
			outputs: map[string]string{
				"action":            "rebase-onto-base",
				"mode":              "mechanical",
				"worker_attempt_id": "spi-worker1",
				"handoff_mode":      "clone",
				"reason":            "retry with fresh worker",
				"status":            "partial",
			},
			check: func(t *testing.T, _ string, details map[string]any, _, action, reason, _, _ string, _ float64) {
				if action != "rebase-onto-base" {
					t.Errorf("action = %q", action)
				}
				if reason != "retry with fresh worker" {
					t.Errorf("reason = %q", reason)
				}
				if details["mode"] != "mechanical" {
					t.Errorf("mode = %v", details["mode"])
				}
				if details["apprentice"] != "spi-worker1" {
					t.Errorf("apprentice = %v", details["apprentice"])
				}
				if details["handoff"] != "clone" {
					t.Errorf("handoff = %v", details["handoff"])
				}
				if details["status"] != "partial" {
					t.Errorf("status = %v", details["status"])
				}
			},
		},
		{
			name: "execute suppresses status=success from Details",
			step: "execute",
			outputs: map[string]string{
				"action": "noop",
				"status": "success",
			},
			check: func(t *testing.T, _ string, details map[string]any, _, action, _, _, _ string, _ float64) {
				if action != "noop" {
					t.Errorf("action = %q", action)
				}
				if _, ok := details["status"]; ok {
					t.Errorf("status=success should be suppressed from details; got %v", details)
				}
			},
		},
		{
			name: "verify prefers verdict over verification_status",
			step: "verify",
			outputs: map[string]string{
				"verdict":             "pass",
				"verification_status": "fail",
				"verify_kind":         "build",
				"failed_step":         "merge",
			},
			check: func(t *testing.T, _ string, details map[string]any, _, _, _, verdict, _ string, _ float64) {
				if verdict != "pass" {
					t.Errorf("verdict = %q; should prefer verdict over verification_status", verdict)
				}
				if details["kind"] != "build" {
					t.Errorf("kind = %v", details["kind"])
				}
				if details["failed_step"] != "merge" {
					t.Errorf("failed_step = %v", details["failed_step"])
				}
			},
		},
		{
			name: "verify falls back to verification_status when verdict empty",
			step: "verify",
			outputs: map[string]string{
				"verification_status": "timeout",
			},
			check: func(t *testing.T, _ string, _ map[string]any, _, _, _, verdict, _ string, _ float64) {
				if verdict != "timeout" {
					t.Errorf("verdict = %q, want timeout from verification_status fallback", verdict)
				}
			},
		},
		{
			name: "learn copies repair metadata",
			step: "learn",
			outputs: map[string]string{
				"repair_mode":    "mechanical",
				"repair_action":  "rebase-onto-base",
				"decision":       "resume",
				"verify_verdict": "pass",
				"outcome":        "repaired",
			},
			check: func(t *testing.T, _ string, details map[string]any, _, _, _, _, _ string, _ float64) {
				if details["mode"] != "mechanical" {
					t.Errorf("mode = %v", details["mode"])
				}
				if details["recipe"] != "rebase-onto-base" {
					t.Errorf("recipe = %v", details["recipe"])
				}
				if details["decision"] != "resume" {
					t.Errorf("decision = %v", details["decision"])
				}
				if details["verdict"] != "pass" {
					t.Errorf("verdict = %v", details["verdict"])
				}
				if details["outcome"] != "repaired" {
					t.Errorf("outcome = %v", details["outcome"])
				}
			},
		},
		{
			name: "finish carries action/status/outcome",
			step: "finish",
			outputs: map[string]string{
				"action":  "resume",
				"status":  "closed",
				"outcome": "resume",
			},
			check: func(t *testing.T, phase string, details map[string]any, _, action, _, _, _ string, _ float64) {
				if phase != "finish" {
					t.Errorf("phase = %q", phase)
				}
				if action != "resume" {
					t.Errorf("action = %q", action)
				}
				if details["status"] != "closed" {
					t.Errorf("status = %v", details["status"])
				}
				if details["outcome"] != "resume" {
					t.Errorf("outcome = %v", details["outcome"])
				}
			},
		},
		{
			name: "finish_needs_human shares finish handling",
			step: "finish_needs_human",
			outputs: map[string]string{
				"action": "escalate",
				"status": "needs_human",
			},
			check: func(t *testing.T, phase string, details map[string]any, _, action, _, _, _ string, _ float64) {
				if phase != "finish_needs_human" {
					t.Errorf("phase = %q", phase)
				}
				if action != "escalate" {
					t.Errorf("action = %q", action)
				}
				if details["status"] != "needs_human" {
					t.Errorf("status = %v", details["status"])
				}
			},
		},
		{
			name: "default arm copies raw outputs",
			step: "retry_on_error",
			outputs: map[string]string{
				"attempt":    "2",
				"last_error": "timeout",
			},
			check: func(t *testing.T, phase string, details map[string]any, _, _, _, _, _ string, _ float64) {
				if phase != "retry_on_error" {
					t.Errorf("phase = %q", phase)
				}
				if details["attempt"] != "2" {
					t.Errorf("attempt = %v", details["attempt"])
				}
				if details["last_error"] != "timeout" {
					t.Errorf("last_error = %v", details["last_error"])
				}
			},
		},
		{
			name:    "step error is recorded in ev.Err",
			step:    "execute",
			outputs: map[string]string{"action": "noop"},
			stepErr: errors.New("boom"),
			check: func(t *testing.T, _ string, _ map[string]any, _, _, _, _ string, errStr string, _ float64) {
				if errStr != "boom" {
					t.Errorf("errStr = %q, want boom", errStr)
				}
			},
		},
		{
			name:    "nil outputs returns base event with no populated fields",
			step:    "verify",
			outputs: nil,
			check: func(t *testing.T, phase string, details map[string]any, branch, action, reason, verdict, errStr string, confidence float64) {
				if phase != "verify" {
					t.Errorf("phase = %q", phase)
				}
				if len(details) != 0 {
					t.Errorf("details should be empty for nil outputs; got %v", details)
				}
				if branch != "" || action != "" || reason != "" || verdict != "" || errStr != "" || confidence != 0 {
					t.Errorf("expected all scalar fields empty; got branch=%q action=%q reason=%q verdict=%q err=%q conf=%v",
						branch, action, reason, verdict, errStr, confidence)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := buildPhaseEvent(tc.step, tc.outputs, tc.stepErr)

			// Phase and Step are always set from stepName.
			if ev.Phase != tc.step {
				t.Errorf("Phase = %q, want %q", ev.Phase, tc.step)
			}
			if ev.Step != tc.step {
				t.Errorf("Step = %q, want %q", ev.Step, tc.step)
			}
			// Ts is always stamped.
			if ev.Ts.IsZero() {
				t.Errorf("Ts should be stamped; got zero")
			}
			tc.check(t, ev.Phase, ev.Details, ev.Branch, ev.Action, ev.Reason, ev.Verdict, ev.Err, ev.Confidence)
		})
	}
}
