// merge_ownership.go provides backward-compatible wrappers delegating to pkg/dolt.
// Dead wrappers removed: resolveIssueConflicts, scanClusterRegressions,
// repairClusterRegressions, doltSQLWithDB, clusterFields — no callers in cmd/spire.
package main

import (
	"github.com/awell-health/spire/pkg/dolt"
)

// Type alias for backward compatibility (used by test assertions).
type clusterRegression = dolt.ClusterRegression

func isClusterField(field string) bool         { return dolt.IsClusterField(field) }
func isStatusRegression(from, to string) bool  { return dolt.IsStatusRegression(from, to) }

func applyMergeOwnership(dbName, preCommit string) error {
	return dolt.ApplyMergeOwnership(dbName, preCommit)
}

func sqlNullableSet(field, authoritative, fallback string) string {
	return dolt.SQLNullableSet(field, authoritative, fallback)
}

func coalesce(vals ...string) string {
	return dolt.Coalesce(vals...)
}

func extractCountValue(output string) int {
	return dolt.ExtractCountValue(output)
}

func getCurrentCommitHash(dbName string) string {
	return dolt.GetCurrentCommitHash(dbName)
}
