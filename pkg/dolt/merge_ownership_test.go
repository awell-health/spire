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
		"base_id":                 "oo-sum",
		"our_id":                  "NULL",
		"their_id":                "oo-sum",
		"base_status":             "in_progress",
		"our_status":              "NULL",
		"their_status":            "closed",
		"base_owner":              "",
		"our_owner":               "",
		"their_owner":             "",
		"base_assignee":           "",
		"our_assignee":            "",
		"their_assignee":          "",
		"base_closed_at":          "",
		"our_closed_at":           "NULL",
		"their_closed_at":         "2026-04-13 17:20:00",
		"base_closed_by_session":  "",
		"our_closed_by_session":   "NULL",
		"their_closed_by_session": "sess-1",
		"base_title":              "attempt: wizard-oo-b9u",
		"our_title":               "NULL",
		"their_title":             "attempt: wizard-oo-b9u",
		"base_description":        "",
		"our_description":         "NULL",
		"their_description":       "kept remotely",
		"base_priority":           "2",
		"our_priority":            "NULL",
		"their_priority":          "2",
		"base_issue_type":         "attempt",
		"our_issue_type":          "NULL",
		"their_issue_type":        "attempt",
	}

	id, updateStmt, deleteStmt, ok := buildIssueConflictStatements(row)
	if !ok {
		t.Fatal("expected delete-vs-modify conflict row to produce resolution statements")
	}
	if id != "oo-sum" {
		t.Fatalf("resolved conflict id = %q, want %q", id, "oo-sum")
	}
	if strings.Contains(updateStmt, "id = 'NULL'") {
		t.Fatalf("update statement targeted literal NULL id: %s", updateStmt)
	}
	for _, want := range []string{
		"status = 'closed'",
		"title = 'attempt: wizard-oo-b9u'",
		"description = 'kept remotely'",
		"priority = 2",
		"issue_type = 'attempt'",
		"closed_at = '2026-04-13 17:20:00'",
		"closed_by_session = 'sess-1'",
		"WHERE id = 'oo-sum'",
	} {
		if !strings.Contains(updateStmt, want) {
			t.Fatalf("update statement missing %q: %s", want, updateStmt)
		}
	}
	if strings.Contains(deleteStmt, "'NULL'") {
		t.Fatalf("delete statement should target the real bead id, got: %s", deleteStmt)
	}
	if !strings.Contains(deleteStmt, "our_id = 'oo-sum' OR their_id = 'oo-sum' OR base_id = 'oo-sum'") {
		t.Fatalf("delete statement did not target resolved conflict id: %s", deleteStmt)
	}
}
