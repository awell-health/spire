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

	// DefaultProvider is the default AI provider for agent work.
	DefaultProvider = "claude"

	// DefaultStaleDuration is DefaultStale as a time.Duration.
	DefaultStaleDuration = 10 * time.Minute

	// DefaultTimeoutDuration is DefaultTimeout as a time.Duration.
	DefaultTimeoutDuration = 15 * time.Minute

	// DefaultClericPromotionThreshold is the global default for the number
	// of consecutive clean agentic recoveries (each carrying a
	// mechanical_recipe) before a failure_signature promotes to mechanical
	// execution. Per-signature overrides can raise or lower this via the
	// cleric.promotion_overrides map in spire.yaml.
	DefaultClericPromotionThreshold = 3
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

// ResolveProvider returns the first non-empty value in the precedence chain:
// step provider > formula provider > spire.yaml provider > system default ("claude").
func ResolveProvider(stepProvider, formulaProvider, repoProvider string) string {
	if stepProvider != "" {
		return stepProvider
	}
	if formulaProvider != "" {
		return formulaProvider
	}
	if repoProvider != "" {
		return repoProvider
	}
	return DefaultProvider
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

// ResolveClericPromotionThreshold returns the effective promotion threshold
// for a given failure signature. Resolution order:
//  1. cleric.promotion_overrides[failureSig] (positive value wins)
//  2. cleric.promotion_threshold (if positive)
//  3. DefaultClericPromotionThreshold
//
// Non-positive configured values fall through to the next source, which
// means an operator can't accidentally disable promotion with 0 or -1.
func ResolveClericPromotionThreshold(cfg ClericConfig, failureSig string) int {
	if failureSig != "" {
		if v, ok := cfg.PromotionOverrides[failureSig]; ok && v > 0 {
			return v
		}
	}
	if cfg.PromotionThreshold > 0 {
		return cfg.PromotionThreshold
	}
	return DefaultClericPromotionThreshold
}
