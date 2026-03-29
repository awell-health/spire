package workshop

import "github.com/awell-health/spire/pkg/formula"

// PhaseDefault holds sensible defaults for a phase given a bead type.
type PhaseDefault struct {
	Role           string
	Model          string
	Timeout        string
	Dispatch       string
	Worktree       bool
	Apprentice     bool
	VerdictOnly    bool
	Judgment       bool
	Auto           bool
	StagingBranch  string
	MergeStrategy  string
	Context        []string
	RevisionPolicy *formula.RevisionPolicy
}

// PhaseDefaultsFor returns preset defaults for a given phase and bead type.
func PhaseDefaultsFor(phase, beadType string) PhaseDefault {
	switch phase {
	case "design":
		return PhaseDefault{
			Role:    "wizard",
			Timeout: "1m",
		}

	case "plan":
		timeout := "5m"
		if beadType == "epic" {
			timeout = "10m"
		}
		return PhaseDefault{
			Role:    "wizard",
			Model:   "claude-opus-4-6",
			Timeout: timeout,
		}

	case "implement":
		timeout := "30m"
		if beadType == "bug" {
			timeout = "10m"
		}
		d := PhaseDefault{
			Role:       "apprentice",
			Model:      "claude-sonnet-4-6",
			Timeout:    timeout,
			Worktree:   true,
			Apprentice: true,
			Context:    []string{"CLAUDE.md", "PLAYBOOK.md"},
		}
		if beadType == "epic" {
			d.Dispatch = "wave"
			d.StagingBranch = "epic/{bead-id}"
		}
		return d

	case "review":
		timeout := "10m"
		d := PhaseDefault{
			Role:        "sage",
			Model:       "claude-opus-4-6",
			Timeout:     timeout,
			VerdictOnly: true,
			RevisionPolicy: &formula.RevisionPolicy{
				MaxRounds:    3,
				ArbiterModel: "claude-opus-4-6",
			},
		}
		if beadType == "epic" {
			d.Timeout = "20m"
			d.Judgment = true
		}
		return d

	case "merge":
		d := PhaseDefault{
			MergeStrategy: "squash",
			Auto:          true,
		}
		if beadType == "epic" {
			d.StagingBranch = "epic/{bead-id}"
		}
		return d

	default:
		return PhaseDefault{}
	}
}

// DefaultPhasesForType returns the phases typically enabled for a bead type.
func DefaultPhasesForType(beadType string) []string {
	switch beadType {
	case "epic":
		return []string{"design", "plan", "implement", "review", "merge"}
	case "bug":
		return []string{"plan", "implement", "review", "merge"}
	case "task", "feature", "chore":
		return []string{"plan", "implement", "review", "merge"}
	default:
		return []string{"plan", "implement", "review", "merge"}
	}
}

// KnownModels returns the list of known Claude models.
func KnownModels() []string {
	return []string{"claude-opus-4-6", "claude-sonnet-4-6", "claude-haiku-4-5"}
}

// KnownBehaviors returns the list of known behavior overrides.
func KnownBehaviors() []string {
	return []string{"validate-design", "sage-review", "merge-to-main", "deploy", "skip"}
}

// KnownRoles returns the list of known phase roles.
func KnownRoles() []string {
	return []string{"human", "apprentice", "sage", "wizard", "skip"}
}

// KnownDispatches returns the list of known dispatch modes.
func KnownDispatches() []string {
	return []string{"direct", "wave", "sequential"}
}

// KnownStrategies returns the list of known merge strategies.
func KnownStrategies() []string {
	return []string{"squash", "merge", "rebase"}
}
