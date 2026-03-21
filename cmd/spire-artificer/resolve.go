package main

import (
	"strings"

	"github.com/awell-health/spire/pkg/repoconfig"
)

// resolveTargetBranch returns the branch to merge into.
// Priority: bead label "branch:" > parent label "branch:" > cfg.Branch.Base
func resolveTargetBranch(bead *Bead, parent *Bead, cfg *repoconfig.RepoConfig) string {
	if b := beadLabel(bead, "branch:"); b != "" {
		return b
	}
	if parent != nil {
		if b := beadLabel(parent, "branch:"); b != "" {
			return b
		}
	}
	return cfg.Branch.Base
}

// resolveMergeMode returns "merge" or "pr".
// Priority: bead label "merge-mode:" > parent label "merge-mode:" > "merge"
func resolveMergeMode(bead *Bead, parent *Bead) string {
	if m := beadLabel(bead, "merge-mode:"); m != "" {
		return m
	}
	if parent != nil {
		if m := beadLabel(parent, "merge-mode:"); m != "" {
			return m
		}
	}
	return "merge"
}

// beadLabel returns the suffix of the first label matching the given prefix.
func beadLabel(b *Bead, prefix string) string {
	if b == nil {
		return ""
	}
	for _, l := range b.Labels {
		if strings.HasPrefix(l, prefix) {
			return l[len(prefix):]
		}
	}
	return ""
}
