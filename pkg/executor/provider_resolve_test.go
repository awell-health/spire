package executor

import (
	"testing"

	"github.com/awell-health/spire/pkg/repoconfig"
)

func TestRepoProvider_NilRepoConfigFunc(t *testing.T) {
	e := NewForTest("spi-test", "wizard-spi-test", nil, nil, &Deps{
		RepoConfig: nil, // RepoConfig function itself is nil
	})
	got := e.repoProvider()
	if got != "" {
		t.Errorf("repoProvider() = %q, want empty when RepoConfig func is nil", got)
	}
}

func TestRepoProvider_NilRepoConfigReturn(t *testing.T) {
	e := NewForTest("spi-test", "wizard-spi-test", nil, nil, &Deps{
		RepoConfig: func() *repoconfig.RepoConfig { return nil },
	})
	got := e.repoProvider()
	if got != "" {
		t.Errorf("repoProvider() = %q, want empty when RepoConfig returns nil", got)
	}
}

func TestRepoProvider_WithProvider(t *testing.T) {
	e := NewForTest("spi-test", "wizard-spi-test", nil, nil, &Deps{
		RepoConfig: func() *repoconfig.RepoConfig {
			return &repoconfig.RepoConfig{
				Agent: repoconfig.AgentConfig{Provider: "codex"},
			}
		},
	})
	got := e.repoProvider()
	if got != "codex" {
		t.Errorf("repoProvider() = %q, want %q", got, "codex")
	}
}

func TestResolveStepProvider_StepWins(t *testing.T) {
	graph := &FormulaStepGraph{Provider: "cursor"}
	e := NewGraphForTest("spi-test", "wizard-spi-test", graph, nil, &Deps{
		RepoConfig: func() *repoconfig.RepoConfig {
			return &repoconfig.RepoConfig{
				Agent: repoconfig.AgentConfig{Provider: "claude"},
			}
		},
	})
	step := StepConfig{Provider: "codex"}
	got := e.resolveStepProvider(step)
	if got != "codex" {
		t.Errorf("resolveStepProvider() = %q, want %q (step wins)", got, "codex")
	}
}

func TestResolveStepProvider_FormulaWins(t *testing.T) {
	graph := &FormulaStepGraph{Provider: "cursor"}
	e := NewGraphForTest("spi-test", "wizard-spi-test", graph, nil, &Deps{
		RepoConfig: func() *repoconfig.RepoConfig {
			return &repoconfig.RepoConfig{
				Agent: repoconfig.AgentConfig{Provider: "claude"},
			}
		},
	})
	step := StepConfig{} // no step-level provider
	got := e.resolveStepProvider(step)
	if got != "cursor" {
		t.Errorf("resolveStepProvider() = %q, want %q (formula wins)", got, "cursor")
	}
}

func TestResolveStepProvider_RepoWins(t *testing.T) {
	graph := &FormulaStepGraph{} // no formula-level provider
	e := NewGraphForTest("spi-test", "wizard-spi-test", graph, nil, &Deps{
		RepoConfig: func() *repoconfig.RepoConfig {
			return &repoconfig.RepoConfig{
				Agent: repoconfig.AgentConfig{Provider: "codex"},
			}
		},
	})
	step := StepConfig{} // no step-level provider
	got := e.resolveStepProvider(step)
	if got != "codex" {
		t.Errorf("resolveStepProvider() = %q, want %q (repo wins)", got, "codex")
	}
}

func TestResolveStepProvider_DefaultClaude(t *testing.T) {
	e := NewGraphForTest("spi-test", "wizard-spi-test", nil, nil, &Deps{
		RepoConfig: nil,
	})
	step := StepConfig{}
	got := e.resolveStepProvider(step)
	if got != "claude" {
		t.Errorf("resolveStepProvider() = %q, want %q (default)", got, "claude")
	}
}
