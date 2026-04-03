package workshop

import (
	"strings"
	"testing"
)

// helper to build a minimal v3 TOML with custom steps section
func v3TOML(stepsSection string) []byte {
	return []byte(`name = "test"
description = "test formula"
version = 3

` + stepsSection)
}

func TestValidateV3_InvalidStepKind(t *testing.T) {
	// invalid kind is caught by ParseFormulaStepGraph -> ValidateGraph
	data := v3TOML(`[steps.a]
kind = "invalid"
terminal = true
`)
	issues := validateV3(data)
	if len(issues) == 0 {
		t.Fatal("expected issues for invalid step kind")
	}
	found := false
	for _, iss := range issues {
		if strings.Contains(iss.Message, "invalid kind") || strings.Contains(iss.Message, "v3 parse error") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected kind error, got: %v", issues)
	}
}

func TestValidateV3_UnknownOpcode(t *testing.T) {
	data := v3TOML(`[steps.a]
action = "nonexistent.opcode"
terminal = true
`)
	issues := validateV3(data)
	if len(issues) == 0 {
		t.Fatal("expected issues for unknown opcode")
	}
}

func TestValidateV3_DanglingWorkspaceRef(t *testing.T) {
	data := v3TOML(`[steps.a]
workspace = "missing"
terminal = true
`)
	issues := validateV3(data)
	if len(issues) == 0 {
		t.Fatal("expected issues for dangling workspace ref")
	}
}

func TestValidateV3_ValidFormula(t *testing.T) {
	data := []byte(`name = "valid"
description = "valid formula"
version = 3

[workspaces.staging]
kind = "staging"
branch = "staging/test"
scope = "run"
ownership = "owned"
cleanup = "terminal"

[steps.start]
kind = "op"
action = "check.design-linked"

[steps.finish]
kind = "op"
action = "bead.finish"
needs = ["start"]
terminal = true
workspace = "staging"
`)
	issues := validateV3(data)
	// Filter out warnings — there should be no errors
	for _, iss := range issues {
		if iss.Level == "error" {
			t.Fatalf("unexpected error: %s [%s]", iss.Message, iss.Phase)
		}
	}
}

func TestValidateV3_CallStepMissingGraph(t *testing.T) {
	data := []byte(`name = "call-test"
description = "test"
version = 3

[steps.entry]
kind = "op"
action = "check.design-linked"

[steps.review]
kind = "call"
needs = ["entry"]
terminal = true
`)
	issues := validateV3(data)
	found := false
	for _, iss := range issues {
		if strings.Contains(iss.Message, "call step requires graph") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected call-without-graph error, got: %v", issues)
	}
}

func TestValidateV3_DispatchMissingWithFields(t *testing.T) {
	data := []byte(`name = "dispatch-test"
description = "test"
version = 3

[steps.entry]
kind = "dispatch"
action = "dispatch.children"
terminal = true
`)
	issues := validateV3(data)
	foundChildren := false
	foundStrategy := false
	for _, iss := range issues {
		if strings.Contains(iss.Message, "with.children") {
			foundChildren = true
		}
		if strings.Contains(iss.Message, "with.strategy") {
			foundStrategy = true
		}
	}
	if !foundChildren {
		t.Fatal("expected warning about missing with.children")
	}
	if !foundStrategy {
		t.Fatal("expected warning about missing with.strategy")
	}
}

func TestValidateV3_WizardRunMissingFlow(t *testing.T) {
	data := []byte(`name = "flow-test"
description = "test"
version = 3

[steps.start]
kind = "op"
action = "wizard.run"
terminal = true
`)
	issues := validateV3(data)
	found := false
	for _, iss := range issues {
		if strings.Contains(iss.Message, "wizard.run") && strings.Contains(iss.Message, "flow") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected wizard.run-without-flow warning, got: %v", issues)
	}
}

func TestValidateV3_RequiredVarNoDefault(t *testing.T) {
	data := []byte(`name = "var-test"
description = "test"
version = 3

[vars.bead_id]
type = "bead_id"
required = true

[steps.start]
terminal = true
`)
	issues := validateV3(data)
	found := false
	for _, iss := range issues {
		if strings.Contains(iss.Message, "required variable") && strings.Contains(iss.Message, "no default") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected required-var warning, got: %v", issues)
	}
}

func TestValidateV3_EmptyProduces(t *testing.T) {
	data := []byte(`name = "produces-test"
description = "test"
version = 3

[steps.start]
kind = "op"
action = "check.design-linked"
produces = ["verdict", ""]
terminal = true
`)
	issues := validateV3(data)
	found := false
	for _, iss := range issues {
		if strings.Contains(iss.Message, "produces[1] is empty") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected empty-produces error, got: %v", issues)
	}
}

func TestValidateV3_WhenPredicateMissingOperand(t *testing.T) {
	data := []byte(`name = "when-test"
description = "test"
version = 3

[steps.entry]
kind = "op"

[steps.check]
needs = ["entry"]
terminal = true

[[steps.check.when.all]]
left = "verdict"
op = "eq"
right = ""
`)
	issues := validateV3(data)
	found := false
	for _, iss := range issues {
		if strings.Contains(iss.Message, "missing right operand") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected missing-right-operand error, got: %v", issues)
	}
}
