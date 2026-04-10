package wizard

import (
	"strings"
	"testing"

	"github.com/awell-health/spire/pkg/repoconfig"
)

func TestWizardBuildCustomPrompt(t *testing.T) {
	cfg := &repoconfig.RepoConfig{
		Runtime: repoconfig.RuntimeConfig{
			Build: "go build ./...",
			Test:  "go test ./...",
		},
	}

	result := WizardBuildCustomPrompt(
		"wizard-spi-abc",
		"spi-abc",
		cfg,
		"Focus context here",
		`{"id":"spi-abc","title":"Test bead"}`,
		"Do research only.\n\nDo NOT write code.",
	)

	// Verify identity header
	if !strings.Contains(result, "You are wizard-spi-abc, a Spire agent working on bead spi-abc.") {
		t.Error("missing identity header")
	}

	// Verify focus context is included
	if !strings.Contains(result, "Focus context here") {
		t.Error("missing focus context")
	}

	// Verify bead JSON is included
	if !strings.Contains(result, `"id":"spi-abc"`) {
		t.Error("missing bead JSON")
	}

	// Verify repo config
	if !strings.Contains(result, "go build ./...") {
		t.Error("missing build command")
	}
	if !strings.Contains(result, "go test ./...") {
		t.Error("missing test command")
	}

	// Verify custom prompt is in "Your task" section
	if !strings.Contains(result, "## Your task") {
		t.Error("missing 'Your task' section header")
	}
	if !strings.Contains(result, "Do research only.") {
		t.Error("missing custom prompt content")
	}

	// Verify default context paths when none configured
	if !strings.Contains(result, "CLAUDE.md") {
		t.Error("missing default context path CLAUDE.md")
	}
}

func TestWizardBuildCustomPrompt_CustomContextPaths(t *testing.T) {
	cfg := &repoconfig.RepoConfig{
		Context: []string{"docs/ARCH.md", "README.md"},
		Runtime: repoconfig.RuntimeConfig{
			Build: "make build",
		},
	}

	result := WizardBuildCustomPrompt(
		"wizard-test",
		"spi-xyz",
		cfg,
		"context",
		"{}",
		"Custom task instructions",
	)

	if !strings.Contains(result, "docs/ARCH.md") {
		t.Error("missing custom context path docs/ARCH.md")
	}
	if !strings.Contains(result, "README.md") {
		t.Error("missing custom context path README.md")
	}
	// Should not contain default paths when custom ones are set
	if strings.Contains(result, "SPIRE.md") {
		t.Error("should not contain default SPIRE.md when custom context paths are configured")
	}
}

func TestWizardBuildCustomPrompt_EmptyCommands(t *testing.T) {
	cfg := &repoconfig.RepoConfig{}

	result := WizardBuildCustomPrompt(
		"wizard-test",
		"spi-xyz",
		cfg,
		"context",
		"{}",
		"Task here",
	)

	if !strings.Contains(result, "(none)") {
		t.Error("empty commands should render as (none)")
	}
}
