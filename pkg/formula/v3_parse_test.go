package formula

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// testdataPath returns the absolute path to the testdata directory.
func testdataPath(t *testing.T) string {
	t.Helper()
	_, filename, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(filename), "testdata")
}

func readTestdata(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(testdataPath(t), name))
	if err != nil {
		t.Fatalf("read testdata %s: %v", name, err)
	}
	return data
}

// --- Task fixture parsing ---

func TestParseV3Formula_TaskFixture(t *testing.T) {
	data := readTestdata(t, "v3-task-test.formula.toml")
	f, err := ParseFormulaStepGraph(data)
	if err != nil {
		t.Fatalf("parse task fixture: %v", err)
	}

	if f.Name != "v3-task-test" {
		t.Errorf("name = %q, want %q", f.Name, "v3-task-test")
	}
	if f.Version != 3 {
		t.Errorf("version = %d, want 3", f.Version)
	}
	if f.Entry != "plan" {
		t.Errorf("entry = %q, want %q", f.Entry, "plan")
	}

	// Step count.
	if len(f.Steps) != 5 {
		t.Errorf("step count = %d, want 5", len(f.Steps))
	}

	// Verify expected steps exist.
	for _, name := range []string{"plan", "implement", "review", "merge", "discard"} {
		if _, ok := f.Steps[name]; !ok {
			t.Errorf("missing step %q", name)
		}
	}

	// Terminal steps.
	terminals := 0
	for _, step := range f.Steps {
		if step.Terminal {
			terminals++
		}
	}
	if terminals != 2 {
		t.Errorf("terminal count = %d, want 2", terminals)
	}

	// Workspace declarations.
	if len(f.Workspaces) != 1 {
		t.Errorf("workspace count = %d, want 1", len(f.Workspaces))
	}
	ws, ok := f.Workspaces["feature"]
	if !ok {
		t.Fatal("missing workspace 'feature'")
	}
	if ws.Kind != WorkspaceKindOwnedWorktree {
		t.Errorf("workspace kind = %q, want %q", ws.Kind, WorkspaceKindOwnedWorktree)
	}
	if ws.Branch != "feat/{vars.bead_id}" {
		t.Errorf("workspace branch = %q, want %q", ws.Branch, "feat/{vars.bead_id}")
	}
	if ws.Base != "main" {
		t.Errorf("workspace base = %q, want %q", ws.Base, "main")
	}
	// Defaults should be applied.
	if ws.Scope != WorkspaceScopeRun {
		t.Errorf("workspace scope = %q, want %q", ws.Scope, WorkspaceScopeRun)
	}
	if ws.Cleanup != WorkspaceCleanupTerminal {
		t.Errorf("workspace cleanup = %q, want %q", ws.Cleanup, WorkspaceCleanupTerminal)
	}

	// Var types.
	beadVar, ok := f.Vars["bead_id"]
	if !ok {
		t.Fatal("missing var bead_id")
	}
	if beadVar.Type != VarTypeBeadID {
		t.Errorf("bead_id type = %q, want %q", beadVar.Type, VarTypeBeadID)
	}
	if !beadVar.Required {
		t.Error("bead_id should be required")
	}
}

// --- Review fixture parsing ---

func TestParseV3Formula_ReviewFixture(t *testing.T) {
	data := readTestdata(t, "v3-review-test.formula.toml")
	f, err := ParseFormulaStepGraph(data)
	if err != nil {
		t.Fatalf("parse review fixture: %v", err)
	}

	if f.Name != "v3-review-test" {
		t.Errorf("name = %q, want %q", f.Name, "v3-review-test")
	}
	if len(f.Steps) != 5 {
		t.Errorf("step count = %d, want 5", len(f.Steps))
	}

	// Verify conditions.
	fix := f.Steps["fix"]
	if fix.Condition == "" {
		t.Error("fix step should have a condition")
	}
	if !strings.Contains(fix.Condition, "request_changes") {
		t.Errorf("fix condition should mention request_changes, got %q", fix.Condition)
	}

	arbiter := f.Steps["arbiter"]
	if arbiter.Condition == "" {
		t.Error("arbiter step should have a condition")
	}
	if !strings.Contains(arbiter.Condition, "max_rounds") {
		t.Errorf("arbiter condition should mention max_rounds, got %q", arbiter.Condition)
	}

	// Verify needs.
	merge := f.Steps["merge"]
	if len(merge.Needs) != 2 {
		t.Errorf("merge needs = %v, want [sage-review, arbiter]", merge.Needs)
	}

	// Terminal steps.
	if !merge.Terminal {
		t.Error("merge should be terminal")
	}
	discard := f.Steps["discard"]
	if !discard.Terminal {
		t.Error("discard should be terminal")
	}
}

// --- Error cases ---

func TestParseV3Formula_InvalidVersion(t *testing.T) {
	toml := `
name = "bad-version"
version = 2

[steps.a]
terminal = true
`
	_, err := ParseFormulaStepGraph([]byte(toml))
	if err == nil {
		t.Fatal("expected error for version != 3")
	}
	if !strings.Contains(err.Error(), "version 3") {
		t.Errorf("expected version error, got: %v", err)
	}
}

func TestParseV3Formula_MissingEntry(t *testing.T) {
	// All steps have needs — no implicit entry point and no explicit entry field.
	toml := `
name = "no-entry"
version = 3

[steps.a]
needs = ["b"]
terminal = true

[steps.b]
needs = ["a"]
`
	_, err := ParseFormulaStepGraph([]byte(toml))
	if err == nil {
		t.Fatal("expected error for missing entry step")
	}
	if !strings.Contains(err.Error(), "entry point") {
		t.Errorf("expected entry point error, got: %v", err)
	}
}

func TestParseV3Formula_DanglingNeeds(t *testing.T) {
	toml := `
name = "dangling"
version = 3

[steps.a]
action = "wizard.run"
flow = "plan"

[steps.b]
action = "wizard.run"
needs = ["nonexistent"]
terminal = true
`
	_, err := ParseFormulaStepGraph([]byte(toml))
	if err == nil {
		t.Fatal("expected error for dangling needs reference")
	}
	if !strings.Contains(err.Error(), "nonexistent") {
		t.Errorf("expected error mentioning nonexistent, got: %v", err)
	}
}

func TestParseV3Formula_NoTerminal(t *testing.T) {
	toml := `
name = "no-terminal"
version = 3

[steps.a]
action = "wizard.run"
flow = "plan"

[steps.b]
action = "wizard.run"
needs = ["a"]
`
	_, err := ParseFormulaStepGraph([]byte(toml))
	if err == nil {
		t.Fatal("expected error for no terminal step")
	}
	if !strings.Contains(err.Error(), "terminal") {
		t.Errorf("expected terminal error, got: %v", err)
	}
}

func TestParseV3Formula_WorkspaceRefInvalid(t *testing.T) {
	toml := `
name = "bad-ws-ref"
version = 3

[workspaces.staging]
kind = "staging"
branch = "staging/test"

[steps.a]
action = "wizard.run"
workspace = "nonexistent"
terminal = true
`
	_, err := ParseFormulaStepGraph([]byte(toml))
	if err == nil {
		t.Fatal("expected error for step referencing undefined workspace")
	}
	if !strings.Contains(err.Error(), "workspace") && !strings.Contains(err.Error(), "not declared") {
		t.Errorf("expected workspace error, got: %v", err)
	}
}

func TestParseV3Formula_ConditionExclusion(t *testing.T) {
	toml := `
name = "both-cond"
version = 3

[steps.a]
action = "wizard.run"
flow = "plan"

[steps.b]
action = "bead.finish"
needs = ["a"]
condition = "verdict == approve"
terminal = true

[steps.b.when]
all = [{ left = "verdict", op = "eq", right = "approve" }]
`
	_, err := ParseFormulaStepGraph([]byte(toml))
	if err == nil {
		t.Fatal("expected error for both when and condition")
	}
	if !strings.Contains(err.Error(), "both when and condition") {
		t.Errorf("expected condition exclusion error, got: %v", err)
	}
}
