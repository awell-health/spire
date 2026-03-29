package workshop

import (
	"strings"
	"testing"

	"github.com/awell-health/spire/pkg/formula"
)

func TestNewBuilder(t *testing.T) {
	b := NewBuilder("test-formula")
	if b.name != "test-formula" {
		t.Fatalf("expected name test-formula, got %s", b.name)
	}
	if len(b.phases) != 0 {
		t.Fatalf("expected 0 phases, got %d", len(b.phases))
	}
}

func TestEnablePhase_Valid(t *testing.T) {
	b := NewBuilder("test")
	if err := b.EnablePhase("plan"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(b.phases) != 1 || b.phases[0] != "plan" {
		t.Fatalf("expected [plan], got %v", b.phases)
	}
	// Enabling again should be a no-op
	if err := b.EnablePhase("plan"); err != nil {
		t.Fatalf("unexpected error on re-enable: %v", err)
	}
	if len(b.phases) != 1 {
		t.Fatalf("expected 1 phase after re-enable, got %d", len(b.phases))
	}
}

func TestEnablePhase_Invalid(t *testing.T) {
	b := NewBuilder("test")
	if err := b.EnablePhase("bogus"); err == nil {
		t.Fatal("expected error for invalid phase")
	}
}

func TestDisablePhase(t *testing.T) {
	b := NewBuilder("test")
	b.EnablePhase("plan")
	b.EnablePhase("implement")
	b.DisablePhase("plan")
	if len(b.phases) != 1 || b.phases[0] != "implement" {
		t.Fatalf("expected [implement], got %v", b.phases)
	}
	if _, ok := b.phaseConfigs["plan"]; ok {
		t.Fatal("plan config should have been removed")
	}
}

func TestConfigurePhase_NotEnabled(t *testing.T) {
	b := NewBuilder("test")
	err := b.ConfigurePhase("plan", formula.PhaseConfig{})
	if err == nil {
		t.Fatal("expected error when configuring non-enabled phase")
	}
}

func TestBuild_NoPhases(t *testing.T) {
	b := NewBuilder("test")
	_, err := b.Build()
	if err == nil {
		t.Fatal("expected error with no phases")
	}
}

func TestBuild_WaveWithoutStagingBranch(t *testing.T) {
	b := NewBuilder("test")
	b.EnablePhase("implement")
	b.ConfigurePhase("implement", formula.PhaseConfig{
		Dispatch: "wave",
	})
	_, err := b.Build()
	if err == nil {
		t.Fatal("expected error for wave without staging_branch")
	}
}

func TestBuild_Valid(t *testing.T) {
	b := NewBuilder("my-formula")
	b.SetDescription("A test formula")
	b.SetBeadType("task")
	b.EnablePhase("plan")
	b.EnablePhase("implement")
	b.EnablePhase("review")
	b.EnablePhase("merge")

	f, err := b.Build()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if f.Name != "my-formula" {
		t.Fatalf("expected name my-formula, got %s", f.Name)
	}
	if f.Version != 2 {
		t.Fatalf("expected version 2, got %d", f.Version)
	}
	if len(f.Phases) != 4 {
		t.Fatalf("expected 4 phases, got %d", len(f.Phases))
	}
	if _, ok := f.Phases["plan"]; !ok {
		t.Fatal("expected plan phase")
	}
}

func TestBuild_DefaultsByBeadType(t *testing.T) {
	b := NewBuilder("epic-formula")
	b.SetBeadType("epic")
	b.EnablePhase("plan")
	b.EnablePhase("implement")

	cfg, ok := b.PhaseConfig("plan")
	if !ok {
		t.Fatal("expected plan config")
	}
	if cfg.Timeout != "10m" {
		t.Fatalf("expected plan timeout 10m for epic, got %s", cfg.Timeout)
	}

	cfg, ok = b.PhaseConfig("implement")
	if !ok {
		t.Fatal("expected implement config")
	}
	if cfg.Dispatch != "wave" {
		t.Fatalf("expected implement dispatch wave for epic, got %s", cfg.Dispatch)
	}
	if cfg.StagingBranch != "epic/{bead-id}" {
		t.Fatalf("expected staging_branch epic/{bead-id}, got %s", cfg.StagingBranch)
	}
}

func TestAddRemoveVar(t *testing.T) {
	b := NewBuilder("test")
	b.EnablePhase("plan")
	b.AddVar("task", formula.FormulaVar{Description: "The bead ID", Required: true})
	b.AddVar("extra", formula.FormulaVar{Description: "Extra var"})
	b.RemoveVar("extra")

	f, err := b.Build()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(f.Vars) != 1 {
		t.Fatalf("expected 1 var, got %d", len(f.Vars))
	}
	if f.Vars["task"].Description != "The bead ID" {
		t.Fatalf("unexpected var description: %s", f.Vars["task"].Description)
	}
}

func TestMarshalTOML_Order(t *testing.T) {
	b := NewBuilder("test-formula")
	b.SetDescription("Test description")
	// Add in reverse order to verify canonical ordering
	b.EnablePhase("merge")
	b.EnablePhase("review")
	b.EnablePhase("plan")

	data, err := b.MarshalTOML()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	s := string(data)

	// Verify header
	if !strings.Contains(s, `name = "test-formula"`) {
		t.Fatal("missing name in TOML output")
	}
	if !strings.Contains(s, `version = 2`) {
		t.Fatal("missing version in TOML output")
	}

	// Verify phase order: plan before review before merge
	planIdx := strings.Index(s, "[phases.plan]")
	reviewIdx := strings.Index(s, "[phases.review]")
	mergeIdx := strings.Index(s, "[phases.merge]")
	if planIdx < 0 || reviewIdx < 0 || mergeIdx < 0 {
		t.Fatalf("missing phase sections in output:\n%s", s)
	}
	if planIdx >= reviewIdx || reviewIdx >= mergeIdx {
		t.Fatalf("phases not in canonical order: plan=%d review=%d merge=%d", planIdx, reviewIdx, mergeIdx)
	}
}

func TestMarshalTOML_RoundTrip(t *testing.T) {
	b := NewBuilder("roundtrip-test")
	b.SetDescription("Round trip test formula")
	b.SetBeadType("task")
	b.EnablePhase("plan")
	b.EnablePhase("implement")
	b.EnablePhase("review")
	b.EnablePhase("merge")
	b.AddVar("task", formula.FormulaVar{Description: "The bead ID", Required: true})

	data, err := b.MarshalTOML()
	if err != nil {
		t.Fatalf("marshal error: %v", err)
	}

	// Parse the output back
	f, err := formula.ParseFormulaV2(data)
	if err != nil {
		t.Fatalf("round-trip parse error: %v\nTOML:\n%s", err, data)
	}

	if f.Name != "roundtrip-test" {
		t.Fatalf("expected name roundtrip-test, got %s", f.Name)
	}
	if f.Version != 2 {
		t.Fatalf("expected version 2, got %d", f.Version)
	}
	if len(f.Phases) != 4 {
		t.Fatalf("expected 4 phases, got %d", len(f.Phases))
	}
	if f.Phases["plan"].Role != "wizard" {
		t.Fatalf("expected plan role wizard, got %s", f.Phases["plan"].Role)
	}
	if f.Phases["review"].RevisionPolicy == nil {
		t.Fatal("expected review revision_policy to survive round-trip")
	}
	if f.Vars["task"].Required != true {
		t.Fatal("expected task var to be required")
	}
}

func TestMarshalTOML_RevisionPolicy(t *testing.T) {
	b := NewBuilder("test")
	b.SetBeadType("task")
	b.EnablePhase("review")

	data, err := b.MarshalTOML()
	if err != nil {
		t.Fatalf("marshal error: %v", err)
	}

	s := string(data)
	if !strings.Contains(s, "[phases.review.revision_policy]") {
		t.Fatalf("missing revision_policy section:\n%s", s)
	}
	if !strings.Contains(s, "max_rounds = 3") {
		t.Fatalf("missing max_rounds:\n%s", s)
	}
}
