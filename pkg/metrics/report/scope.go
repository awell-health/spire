package report

import "strings"

// Scope selects either the whole tower ("all") or a single repo by
// prefix (e.g. "spi", "spd"). The TypeScript contract represents this
// as a plain string — "all" means no filter, anything else is treated
// as a repo prefix.
type Scope struct {
	Prefix string // empty means "all"
}

// ParseScope maps the raw query-param string to a Scope. "all" and
// empty string both mean no filter; any other value is taken as a
// repo prefix with trailing hyphens stripped so "spi-" and "spi" are
// equivalent.
func ParseScope(raw string) Scope {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "all" {
		return Scope{}
	}
	return Scope{Prefix: strings.TrimSuffix(raw, "-")}
}

// IsAll reports whether this scope targets every bead/repo.
func (s Scope) IsAll() bool { return s.Prefix == "" }

// String returns the frontend-facing scope label — "all" or the repo
// prefix. Used to echo the scope back in the response.
func (s Scope) String() string {
	if s.IsAll() {
		return "all"
	}
	return s.Prefix
}

// beadIDClause returns a SQL fragment + bound arg for filtering a
// bead_id column to the scope. For "all", returns ("", nil). For a
// repo scope, returns (" AND <col> LIKE ?", "prefix-%").
func (s Scope) beadIDClause(col string) (string, []any) {
	if s.IsAll() {
		return "", nil
	}
	return " AND " + col + " LIKE ?", []any{s.Prefix + "-%"}
}

// repoClause returns a SQL fragment + bound arg for filtering the
// `repo` column on agent_runs_olap. repoFromBeadID in pkg/olap ETL
// sets repo to the bead_id prefix, so the prefix lookup is a direct
// equality, not a LIKE.
func (s Scope) repoClause(col string) (string, []any) {
	if s.IsAll() {
		return "", nil
	}
	return " AND " + col + " = ?", []any{s.Prefix}
}
