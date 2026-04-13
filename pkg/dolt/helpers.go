package dolt

import (
	"fmt"
	"strings"
)

// trimSpace trims whitespace from a string (internal helper to avoid
// importing strings in every file just for TrimSpace).
func trimSpace(s string) string {
	return strings.TrimSpace(s)
}

// SQLEscape escapes single quotes in a string for safe SQL insertion.
func SQLEscape(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

// SQLAssign returns a SQL assignment expression for string / datetime columns.
// Empty and SQL NULL sentinel values are assigned as SQL NULL.
func SQLAssign(field, value string) string {
	if value == "" || strings.EqualFold(value, "NULL") {
		return field + " = NULL"
	}
	return fmt.Sprintf("%s = '%s'", field, SQLEscape(value))
}

// SQLAssignNonNull returns a SQL assignment expression for required string fields.
// Empty and SQL NULL sentinel values are normalized to the empty string.
func SQLAssignNonNull(field, value string) string {
	if value == "" || strings.EqualFold(value, "NULL") {
		value = ""
	}
	return fmt.Sprintf("%s = '%s'", field, SQLEscape(value))
}

// Coalesce returns the first non-empty, non-SQL-NULL string.
// Dolt's tabular CLI output renders SQL NULL values as the literal text "NULL".
// For conflict resolution we treat that sentinel as an absent value.
func Coalesce(vals ...string) string {
	for _, v := range vals {
		if v != "" && !strings.EqualFold(v, "NULL") {
			return v
		}
	}
	return ""
}

// ExtractCountValue parses a COUNT(*) result from dolt tabular output.
func ExtractCountValue(output string) int {
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "+") || strings.HasPrefix(line, "| c") {
			continue
		}
		if strings.HasPrefix(line, "|") {
			for _, p := range strings.Split(line, "|") {
				p = strings.TrimSpace(p)
				if p != "" && p != "c" {
					n := 0
					fmt.Sscanf(p, "%d", &n)
					return n
				}
			}
		}
	}
	return 0
}

// ParseDoltRows parses dolt's tabular output into a slice of maps keyed by column name.
//
// Expected format:
//
//	+--------+----------+
//	| prefix | repo_url |
//	+--------+----------+
//	| spi    | https... |
//	+--------+----------+
//
// Separator lines (+---+) and the header row (first | ... | line) are skipped.
func ParseDoltRows(out string, columns []string) []map[string]string {
	lines := strings.Split(strings.TrimSpace(out), "\n")

	var rows []map[string]string
	headerSkipped := false
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "+") {
			continue
		}
		// First pipe-delimited line is the header — skip it
		if !headerSkipped {
			headerSkipped = true
			continue
		}
		// Parse data row
		parts := strings.Split(line, "|")
		var cells []string
		for _, p := range parts {
			cells = append(cells, strings.TrimSpace(p))
		}
		// Strip leading/trailing empty boundary cells from "| a | b |"
		if len(cells) > 0 && cells[0] == "" {
			cells = cells[1:]
		}
		if len(cells) > 0 && cells[len(cells)-1] == "" {
			cells = cells[:len(cells)-1]
		}

		row := make(map[string]string)
		for i, col := range columns {
			if i < len(cells) {
				row[col] = cells[i]
			} else {
				row[col] = ""
			}
		}
		rows = append(rows, row)
	}
	return rows
}

// SQLNullableSet returns a SQL SET clause for a nullable field.
//
//   - authoritative = "NULL" -> field = NULL  (explicit clear, fallback ignored)
//   - authoritative = "val"  -> field = 'val' (fallback ignored)
//   - authoritative = ""     -> use fallback  (authoritative side absent from conflict)
func SQLNullableSet(field, authoritative, fallback string) string {
	if authoritative == "NULL" {
		// Authoritative side explicitly set NULL — honor it.
		return field + " = NULL"
	}
	if authoritative != "" {
		return fmt.Sprintf("%s = '%s'", field, SQLEscape(authoritative))
	}
	// Authoritative side absent — use fallback.
	if fallback == "" || fallback == "NULL" {
		return field + " = NULL"
	}
	return fmt.Sprintf("%s = '%s'", field, SQLEscape(fallback))
}
