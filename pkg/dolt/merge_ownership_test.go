package dolt

import (
	"strings"
	"testing"
)

func TestCoalesceSkipsSQLNullLiteral(t *testing.T) {
	if got := Coalesce("NULL", "oo-sum", "fallback"); got != "oo-sum" {
		t.Fatalf("Coalesce should skip SQL NULL sentinel, got %q", got)
	}
	if got := Coalesce("", "NULL", "closed"); got != "closed" {
		t.Fatalf("Coalesce should skip empty and SQL NULL sentinels, got %q", got)
	}
}

func TestBuildIssueConflictStatements_DeleteVsModifyConflict(t *testing.T) {
	row := map[string]string{
		"dolt_conflict_id":          "42",
		"base_id":                   "oo-sum",
		"our_id":                    "NULL",
		"their_id":                  "oo-sum",
		"base_created_at":           "2026-04-13 15:20:26",
		"our_created_at":            "NULL",
		"their_created_at":          "2026-04-13 15:20:26",
		"base_created_by":           "spire",
		"our_created_by":            "NULL",
		"their_created_by":          "spire",
		"base_updated_at":           "2026-04-13 15:23:18",
		"our_updated_at":            "NULL",
		"their_updated_at":          "2026-04-13 15:23:18",
		"base_design":               "",
		"our_design":                "NULL",
		"their_design":              "",
		"base_acceptance_criteria":  "",
		"our_acceptance_criteria":   "NULL",
		"their_acceptance_criteria": "",
		"base_notes":                "",
		"our_notes":                 "NULL",
		"their_notes":               "",
		"base_status":               "in_progress",
		"our_status":                "NULL",
		"their_status":              "closed",
		"base_owner":                "",
		"our_owner":                 "",
		"their_owner":               "",
		"base_assignee":             "",
		"our_assignee":              "",
		"their_assignee":            "",
		"base_closed_at":            "",
		"our_closed_at":             "NULL",
		"their_closed_at":           "2026-04-13 17:20:00",
		"base_closed_by_session":    "",
		"our_closed_by_session":     "NULL",
		"their_closed_by_session":   "sess-1",
		"base_title":                "attempt: wizard-oo-b9u",
		"our_title":                 "NULL",
		"their_title":               "attempt: wizard-oo-b9u",
		"base_description":          "",
		"our_description":           "NULL",
		"their_description":         "kept remotely",
		"base_priority":             "2",
		"our_priority":              "NULL",
		"their_priority":            "2",
		"base_issue_type":           "attempt",
		"our_issue_type":            "NULL",
		"their_issue_type":          "attempt",
	}

	id, updateStmt, deleteStmt, ok := buildIssueConflictStatements(row)
	if !ok {
		t.Fatal("expected delete-vs-modify conflict row to produce resolution statements")
	}
	if id != "oo-sum" {
		t.Fatalf("resolved conflict id = %q, want %q", id, "oo-sum")
	}
	if strings.Contains(updateStmt, "'NULL'") {
		t.Fatalf("update statement should not write literal NULL sentinel values: %s", updateStmt)
	}
	for _, want := range []string{
		"UPDATE dolt_conflicts_issues SET",
		"our_id = 'oo-sum'",
		"our_created_at = '2026-04-13 15:20:26'",
		"our_updated_at = '2026-04-13 15:23:18'",
		"our_title = 'attempt: wizard-oo-b9u'",
		"our_description = 'kept remotely'",
		"our_design = ''",
		"our_acceptance_criteria = ''",
		"our_notes = ''",
		"our_status = 'closed'",
		"our_priority = 2",
		"our_issue_type = 'attempt'",
		"our_closed_at = '2026-04-13 17:20:00'",
		"our_closed_by_session = 'sess-1'",
		"WHERE dolt_conflict_id = '42'",
	} {
		if !strings.Contains(updateStmt, want) {
			t.Fatalf("update statement missing %q: %s", want, updateStmt)
		}
	}
	if strings.Contains(deleteStmt, "'NULL'") {
		t.Fatalf("delete statement should target the concrete conflict id, got: %s", deleteStmt)
	}
	if deleteStmt != "DELETE FROM dolt_conflicts_issues WHERE dolt_conflict_id = '42'" {
		t.Fatalf("unexpected delete statement: %s", deleteStmt)
	}
}
