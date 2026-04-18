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
	q, err := buildSyncQuery("DOLT_PULL", dbName, remote, branch)
	if err != nil {
		return fmt.Errorf("SQLPull: %w", err)
	}
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
	q, err := buildSyncQuery("DOLT_PUSH", dbName, remote, branch)
	if err != nil {
		return fmt.Errorf("SQLPush: %w", err)
	}
	out, err := RawQuery(q)
	if err != nil {
		return fmt.Errorf("dolt push via SQL: %w", err)
	}
	if strings.Contains(strings.ToLower(out), "error") {
		return fmt.Errorf("dolt push via SQL: %s", strings.TrimSpace(out))
	}
	return nil
}

// buildSyncQuery composes a `USE <db>; CALL <proc>('<remote>', '<branch>')`
// statement with proper identifier validation and literal escaping. proc is
// the dolt stored procedure name (DOLT_PULL / DOLT_PUSH). dbName is
// validated (rejects empty, backticks, NUL) rather than escaped, since it
// comes from controlled internal config; remote/branch default to
// origin/main and are single-quote-escaped.
func buildSyncQuery(proc, dbName, remote, branch string) (string, error) {
	if err := validateIdentifier("dbName", dbName); err != nil {
		return "", err
	}
	if remote == "" {
		remote = "origin"
	}
	if branch == "" {
		branch = "main"
	}
	return fmt.Sprintf(
		"USE `%s`; CALL %s('%s', '%s')",
		dbName, proc, sqlEscape(remote), sqlEscape(branch),
	), nil
}

// sqlEscape escapes a string literal (single-quoted).
func sqlEscape(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

// validateIdentifier guards values interpolated into backtick-quoted SQL
// identifier positions (USE `%s`). A backtick in such a value would close
// the quote and inject arbitrary SQL; sqlEscape only handles single quotes.
// We take the strict path — reject identifiers with backticks or the NUL
// byte outright rather than attempt backtick-doubling, since dbName values
// come from config and should never legitimately contain either.
func validateIdentifier(name, v string) error {
	if v == "" {
		return fmt.Errorf("%s is required", name)
	}
	if strings.ContainsAny(v, "`\x00") {
		return fmt.Errorf("%s contains a disallowed character (backtick or NUL)", name)
	}
	return nil
}
