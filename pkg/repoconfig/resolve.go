package repoconfig

import "time"

// System-wide default constants. Defined once here so that repoconfig,
// wizard, executor, and steward all share the same values.
const (
	// DefaultModel is the default Claude model for agent work.
	DefaultModel = "claude-sonnet-4-6"

	// DefaultReviewModel is the default model for code review (opus for quality).
	DefaultReviewModel = "claude-opus-4-6"

	// DefaultTimeout is the default fatal timeout for agent work.
	DefaultTimeout = "15m"

	// DefaultDesignTimeout is the default timeout for design phases.
	DefaultDesignTimeout = "10m"

	// DefaultStale is the default stale warning threshold.
	DefaultStale = "10m"

	// DefaultBranchBase is the default base branch name.
	DefaultBranchBase = "main"

	// DefaultBranchPattern is the default branch naming pattern.
	DefaultBranchPattern = "feat/{bead-id}"

	// DefaultStaleDuration is DefaultStale as a time.Duration.
	DefaultStaleDuration = 10 * time.Minute

	// DefaultTimeoutDuration is DefaultTimeout as a time.Duration.
	DefaultTimeoutDuration = 15 * time.Minute
)

// ResolveModel returns the first non-empty value in the precedence chain:
// formula phase model > spire.yaml model > system default.
func ResolveModel(phaseModel, repoModel string) string {
	if phaseModel != "" {
		return phaseModel
	}
	if repoModel != "" {
		return repoModel
	}
	return DefaultModel
}

// ResolveTimeout returns the first non-empty value in the precedence chain:
// formula phase timeout > spire.yaml timeout > provided default.
func ResolveTimeout(phaseTimeout, repoTimeout, defaultTimeout string) string {
	if phaseTimeout != "" {
		return phaseTimeout
	}
	if repoTimeout != "" {
		return repoTimeout
	}
	if defaultTimeout != "" {
		return defaultTimeout
	}
	return DefaultTimeout
}

// ResolveStale returns the stale threshold from spire.yaml, or the system default.
func ResolveStale(repoStale string) string {
	if repoStale != "" {
		return repoStale
	}
	return DefaultStale
}

// ResolveBranchBase returns the base branch from spire.yaml, or "main".
func ResolveBranchBase(repoBranchBase string) string {
	if repoBranchBase != "" {
		return repoBranchBase
	}
	return DefaultBranchBase
}

// ResolveBranchPattern returns the branch pattern from spire.yaml, or "feat/{bead-id}".
func ResolveBranchPattern(repoPattern string) string {
	if repoPattern != "" {
		return repoPattern
	}
	return DefaultBranchPattern
}

// ResolveDesignTimeout returns the design timeout from spire.yaml, or the system default.
func ResolveDesignTimeout(repoDesignTimeout string) string {
	if repoDesignTimeout != "" {
		return repoDesignTimeout
	}
	return DefaultDesignTimeout
}

// ResolveDesignRequireApproval returns whether design beads require human
// approval. Returns true (the default) when the pointer is nil (config
// section absent or field omitted).
func ResolveDesignRequireApproval(ptr *bool) bool {
	if ptr != nil {
		return *ptr
	}
	return true
}
