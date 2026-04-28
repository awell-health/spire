package formula

import (
	"testing"
)

// TestClericDefaultFormula_Loads pins the cleric-default embedded
// formula to load + validate cleanly. The cleric runtime (spi-hhkozk)
// dispatches recovery beads against this formula via
// DefaultV3FormulaMap["recovery"]; a parse / validation regression
// would silently break recovery dispatch in cluster and local mode.
func TestClericDefaultFormula_Loads(t *testing.T) {
	g, err := LoadEmbeddedStepGraph("cleric-default")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if g.Version != 3 {
		t.Errorf("version = %d, want 3", g.Version)
	}
	if g.Entry != "decide" {
		t.Errorf("entry = %q, want decide", g.Entry)
	}
	want := map[string]struct {
		kind     string
		action   string
		terminal bool
	}{
		"decide":               {kind: "op", action: "wizard.run"},
		"publish":              {kind: "op", action: "cleric.publish"},
		"wait_for_gate":        {kind: "wait"},
		"execute":              {kind: "op", action: "cleric.execute"},
		"requeue_after_reject": {kind: "op", action: "cleric.reject"},
		"handle_takeover":      {kind: "op", action: "cleric.takeover", terminal: true},
		"finish":               {kind: "op", action: "cleric.finish", terminal: true},
	}
	for name, expected := range want {
		got, ok := g.Steps[name]
		if !ok {
			t.Errorf("step %q missing", name)
			continue
		}
		if got.Kind != expected.kind {
			t.Errorf("%s.kind = %q, want %q", name, got.Kind, expected.kind)
		}
		if got.Action != expected.action {
			t.Errorf("%s.action = %q, want %q", name, got.Action, expected.action)
		}
		if got.Terminal != expected.terminal {
			t.Errorf("%s.terminal = %v, want %v", name, got.Terminal, expected.terminal)
		}
	}
	// wait_for_gate must declare its produced outputs so the wait
	// interpreter knows which keys to park on.
	if got := g.Steps["wait_for_gate"]; len(got.Produces) == 0 {
		t.Error("wait_for_gate must declare produces")
	}
	// requeue_after_reject must declare resets to the prior three
	// steps so a fresh cleric round runs after rejection.
	if got := g.Steps["requeue_after_reject"]; len(got.Resets) != 3 {
		t.Errorf("requeue_after_reject.resets = %v, want 3 entries (decide, publish, wait_for_gate)", got.Resets)
	}
}

// TestRecoveryFormulaMapping pins the recovery → cleric-default
// resolution. The steward's cleric dispatch path relies on this
// mapping when it spawns an executor against a recovery bead — without
// it, the executor would fall back to task-default (or a confused
// user-type-specific default) and the cleric formula would not run.
func TestRecoveryFormulaMapping(t *testing.T) {
	got := ResolveV3Name(BeadInfo{Type: "recovery"})
	if got != "cleric-default" {
		t.Errorf("ResolveV3Name(recovery) = %q, want cleric-default", got)
	}
}

// TestClericOpcodes pins the cleric.* opcodes are recognized by the
// formula validator. Adding a new opcode is fine; removing or renaming
// any of these breaks the embedded formula.
func TestClericOpcodes(t *testing.T) {
	for _, op := range []string{"cleric.publish", "cleric.execute", "cleric.takeover", "cleric.finish", "cleric.reject"} {
		if !ValidOpcode(op) {
			t.Errorf("opcode %q not valid", op)
		}
	}
}
