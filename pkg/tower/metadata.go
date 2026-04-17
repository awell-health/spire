package tower

import (
	"fmt"

	"github.com/awell-health/spire/pkg/config"
)

// SQLExec runs a SQL query and returns the raw output. Callers wire this to
// whichever dolt entry point fits their context: `dolt.LocalQuery` for
// on-disk-file reads against a freshly cloned database the running server
// has not yet discovered, or `dolt.RawQuery` for server reads.
type SQLExec func(query string) (string, error)

// ReadMetadata returns the tower's project_id and prefix from its `metadata`
// table. When database is non-empty, queries are scoped as `<database>.metadata`
// (server mode); when empty, queries use the bare `metadata` table (local mode
// against a specific data dir).
//
// project_id is authoritative — if absent, the database was not created with
// `spire tower create` and is not a valid Spire tower; an error is returned.
// prefix is optional — the caller decides what to fall back to.
func ReadMetadata(exec SQLExec, database string) (projectID, prefix string, err error) {
	tableExpr := "metadata"
	if database != "" {
		tableExpr = "`" + database + "`.metadata"
	}

	pidOut, err := exec(fmt.Sprintf("SELECT `value` FROM %s WHERE `key` = '_project_id'", tableExpr))
	if err != nil {
		return "", "", fmt.Errorf("query project_id: %w", err)
	}
	projectID = config.ExtractSQLValue(pidOut)
	if projectID == "" {
		return "", "", fmt.Errorf("no project_id found in tower database — was it created with `spire tower create`?")
	}

	prefixOut, err := exec(fmt.Sprintf("SELECT `value` FROM %s WHERE `key` = 'prefix'", tableExpr))
	if err == nil {
		prefix = config.ExtractSQLValue(prefixOut)
	}

	return projectID, prefix, nil
}
