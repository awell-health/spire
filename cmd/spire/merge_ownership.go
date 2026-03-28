// merge_ownership.go provides backward-compatible wrappers delegating to pkg/dolt.
package main

import (
	"github.com/awell-health/spire/pkg/dolt"
)

// Type alias for backward compatibility.
type clusterRegression = dolt.ClusterRegression

// Re-export the cluster fields map for any internal references.
var clusterFields = dolt.ClusterFields

func isClusterField(field string) bool    { return dolt.IsClusterField(field) }
func isStatusRegression(from, to string) bool { return dolt.IsStatusRegression(from, to) }

func applyMergeOwnership(dbName, preCommit string) error {
	return dolt.ApplyMergeOwnership(dbName, preCommit)
}

func resolveIssueConflicts(dbName string) (int, error) {
	return dolt.ResolveIssueConflicts(dbName)
}

func scanClusterRegressions(dbName, preCommit string) ([]clusterRegression, error) {
	return dolt.ScanClusterRegressions(dbName, preCommit)
}

func repairClusterRegressions(dbName string, regressions []clusterRegression) error {
	return dolt.RepairClusterRegressions(dbName, regressions)
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

// doltSQLWithDB runs a SQL query against a specific database using --use-db.
func doltSQLWithDB(dbName, query string) (string, error) {
	return dolt.SQL(query, false, dbName, nil)
}
