package dolt

import (
	"fmt"
	"strings"
)

// SQLPull triggers a server-side `CALL DOLT_PULL('origin', 'main')` against
// the running dolt server for the given database. Used by the cluster
// gateway, where no local dolt repo exists — the syncer pod has only a
// .beads/ workspace and must drive the remote sync through the server
// process on the dolt StatefulSet (which has the JWK creds).
//
// Returns nil when the pull succeeds cleanly or is already up-to-date.
// A "non-fast-forward" or conflict error is returned verbatim so callers
// can decide whether to retry, merge, or surface.
func SQLPull(dbName, remote, branch string) error {
	if dbName == "" {
		return fmt.Errorf("SQLPull: dbName is required")
	}
	if remote == "" {
		remote = "origin"
	}
	if branch == "" {
		branch = "main"
	}
	q := fmt.Sprintf("USE `%s`; CALL DOLT_PULL('%s', '%s')", dbName, sqlEscape(remote), sqlEscape(branch))
	out, err := RawQuery(q)
	if err != nil {
		return fmt.Errorf("dolt pull via SQL: %w", err)
	}
	if strings.Contains(strings.ToLower(out), "error") {
		return fmt.Errorf("dolt pull via SQL: %s", strings.TrimSpace(out))
	}
	return nil
}

// SQLPush triggers a server-side `CALL DOLT_PUSH('origin', 'main')`. Same
// rationale as SQLPull — the dolt server has the creds and the DB, the
// gateway just drives the call.
func SQLPush(dbName, remote, branch string) error {
	if dbName == "" {
		return fmt.Errorf("SQLPush: dbName is required")
	}
	if remote == "" {
		remote = "origin"
	}
	if branch == "" {
		branch = "main"
	}
	q := fmt.Sprintf("USE `%s`; CALL DOLT_PUSH('%s', '%s')", dbName, sqlEscape(remote), sqlEscape(branch))
	out, err := RawQuery(q)
	if err != nil {
		return fmt.Errorf("dolt push via SQL: %w", err)
	}
	if strings.Contains(strings.ToLower(out), "error") {
		return fmt.Errorf("dolt push via SQL: %s", strings.TrimSpace(out))
	}
	return nil
}

func sqlEscape(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}
