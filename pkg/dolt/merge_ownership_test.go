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

// TestBuildIssueConflictStatements_DeleteVsModifyRestoresFromTheirs verifies
// that when the local side deleted a bead but the cluster modified it, the
// resolver emits an INSERT from the `their_*` snapshot instead of a no-op
// UPDATE against a non-existent row. This is the fix for the orphaned-FK
// bug where the conflict row got deleted but the children
// (labels/comments/events referencing the missing issue_id) were left behind.
func TestBuildIssueConflictStatements_DeleteVsModifyRestoresFromTheirs(t *testing.T) {
	row := map[string]string{
		"base_id":           "oo-sum",
		"our_id":            "NULL",
		"their_id":          "oo-sum",
		"base_status":       "in_progress",
		"our_status":        "NULL",
		"their_status":      "closed",
		"their_title":       "attempt: wizard-oo-b9u",
		"their_description": "kept remotely",
		"their_priority":    "2",
		"their_issue_type":  "attempt",
	}
	cols := []string{"id", "title", "status", "closed_at"}

	stmts, id, kind, ok := buildIssueConflictStatements(row, cols)
	if !ok {
		t.Fatal("expected delete-vs-modify conflict row to produce resolution statements")
	}
	if id != "oo-sum" {
		t.Fatalf("resolved conflict id = %q, want %q", id, "oo-sum")
	}
	if kind != "restore-from-theirs" {
		t.Fatalf("branch = %q, want restore-from-theirs", kind)
	}
	if len(stmts) != 2 {
		t.Fatalf("expected 2 statements (INSERT + DELETE conflict), got %d: %v", len(stmts), stmts)
	}

	insert := stmts[0]
	if !strings.HasPrefix(insert, "INSERT INTO issues") {
		t.Fatalf("first statement must be an INSERT (not UPDATE), got: %s", insert)
	}
	for _, want := range []string{
		"(id, title, status, closed_at)",
		"SELECT their_id, their_title, their_status, their_closed_at",
		"FROM dolt_conflicts_issues",
		"WHERE their_id = 'oo-sum'",
	} {
		if !strings.Contains(insert, want) {
			t.Fatalf("INSERT statement missing %q: %s", want, insert)
		}
	}
	// Crucially, no UPDATE — the old behaviour silently no-op'd.
	for _, stmt := range stmts {
		if strings.HasPrefix(stmt, "UPDATE issues") {
			t.Fatalf("delete-vs-modify must not emit an UPDATE (it would no-op and orphan FK children): %s", stmt)
		}
		if strings.Contains(stmt, "id = 'NULL'") {
			t.Fatalf("no statement should reference the literal NULL id: %s", stmt)
		}
	}

	delConflict := stmts[1]
	if !strings.Contains(delConflict, "DELETE FROM dolt_conflicts_issues") {
		t.Fatalf("second statement must clear the conflict row, got: %s", delConflict)
	}
	if !strings.Contains(delConflict, "our_id = 'oo-sum' OR their_id = 'oo-sum' OR base_id = 'oo-sum'") {
		t.Fatalf("conflict delete did not target resolved id: %s", delConflict)
	}
}

// TestBuildIssueConflictStatements_ModifyVsModifyUsesFieldLevelMerge verifies
// that when both sides modified the bead, the resolver continues to use the
// field-level ownership UPDATE (cluster wins cluster fields, local wins user
// fields).
func TestBuildIssueConflictStatements_ModifyVsModifyUsesFieldLevelMerge(t *testing.T) {
	row := map[string]string{
		"base_id":           "oo-sum",
		"our_id":            "oo-sum",
		"their_id":          "oo-sum",
		"our_status":        "in_progress",
		"their_status":      "closed",
		"our_owner":         "local-owner",
		"their_owner":       "remote-owner",
		"our_closed_at":     "",
		"their_closed_at":   "2026-04-13 17:20:00",
		"our_title":         "local edit",
		"their_title":       "remote edit",
		"our_description":   "local desc",
		"their_description": "remote desc",
		"our_priority":      "1",
		"their_priority":    "3",
		"our_issue_type":    "task",
		"their_issue_type":  "feature",
	}

	stmts, id, kind, ok := buildIssueConflictStatements(row, []string{"id", "title"})
	if !ok {
		t.Fatal("expected modify-vs-modify row to resolve")
	}
	if id != "oo-sum" {
		t.Fatalf("id = %q, want oo-sum", id)
	}
	if kind != "field-merge" {
		t.Fatalf("branch = %q, want field-merge", kind)
	}
	if len(stmts) != 2 {
		t.Fatalf("expected 2 statements (UPDATE + DELETE conflict), got %d", len(stmts))
	}

	update := stmts[0]
	if !strings.HasPrefix(update, "UPDATE issues") {
		t.Fatalf("first statement must be UPDATE, got: %s", update)
	}
	for _, want := range []string{
		"status = 'closed'",           // cluster field → theirs
		"owner = 'remote-owner'",      // cluster field → theirs
		"closed_at = '2026-04-13 17:20:00'",
		"title = 'local edit'",        // user field → ours
		"description = 'local desc'",  // user field → ours
		"priority = 1",                // user field → ours
		"issue_type = 'task'",         // user field → ours
		"WHERE id = 'oo-sum'",
	} {
		if !strings.Contains(update, want) {
			t.Fatalf("UPDATE missing %q: %s", want, update)
		}
	}
}

// TestBuildIssueConflictStatements_ModifyVsDeleteKeepsOurs verifies that when
// the cluster deleted a bead but we modified it locally, the resolver keeps
// the local row untouched and only clears the conflict entry. "Keep ours"
// is the right default because whoever modified the bead recently still
// cares about it.
func TestBuildIssueConflictStatements_ModifyVsDeleteKeepsOurs(t *testing.T) {
	row := map[string]string{
		"base_id":  "oo-sum",
		"our_id":   "oo-sum",
		"their_id": "NULL",
	}

	stmts, id, kind, ok := buildIssueConflictStatements(row, []string{"id", "title"})
	if !ok {
		t.Fatal("expected modify-vs-delete row to resolve")
	}
	if id != "oo-sum" {
		t.Fatalf("id = %q, want oo-sum", id)
	}
	if kind != "keep-ours" {
		t.Fatalf("branch = %q, want keep-ours", kind)
	}
	if len(stmts) != 1 {
		t.Fatalf("expected exactly 1 statement (DELETE conflict), got %d: %v", len(stmts), stmts)
	}
	if !strings.HasPrefix(stmts[0], "DELETE FROM dolt_conflicts_issues") {
		t.Fatalf("only statement must be DELETE from conflicts, got: %s", stmts[0])
	}
}

// TestBuildIssueConflictStatements_BothDeletedJustClearsConflict verifies
// that when both sides deleted the bead, the resolver simply drops the
// conflict row (nothing to apply).
func TestBuildIssueConflictStatements_BothDeletedJustClearsConflict(t *testing.T) {
	row := map[string]string{
		"base_id":  "oo-sum",
		"our_id":   "NULL",
		"their_id": "NULL",
	}

	stmts, id, kind, ok := buildIssueConflictStatements(row, []string{"id"})
	if !ok {
		t.Fatal("expected both-deleted row to resolve")
	}
	if id != "oo-sum" {
		t.Fatalf("id = %q, want oo-sum (from base_id)", id)
	}
	if kind != "both-deleted" {
		t.Fatalf("branch = %q, want both-deleted", kind)
	}
	if len(stmts) != 1 {
		t.Fatalf("expected exactly 1 statement (DELETE conflict), got %d", len(stmts))
	}
	if !strings.HasPrefix(stmts[0], "DELETE FROM dolt_conflicts_issues") {
		t.Fatalf("only statement must be DELETE from conflicts, got: %s", stmts[0])
	}
}

// TestBuildIssueConflictStatements_NoIDReturnsNotOk verifies that a conflict
// row with no usable id on any side is skipped rather than emitting broken
// SQL.
func TestBuildIssueConflictStatements_NoIDReturnsNotOk(t *testing.T) {
	row := map[string]string{
		"base_id":  "",
		"our_id":   "NULL",
		"their_id": "NULL",
	}
	if stmts, _, _, ok := buildIssueConflictStatements(row, []string{"id"}); ok {
		t.Fatalf("expected ok=false for row without any id, got stmts=%v", stmts)
	}
}

// TestBuildRestoreFromTheirsInsertFailsWithoutColumns guards against an
// accidental empty-column list (which would produce invalid SQL).
func TestBuildRestoreFromTheirsInsertFailsWithoutColumns(t *testing.T) {
	if _, ok := buildRestoreFromTheirsInsert("oo-sum", nil); ok {
		t.Fatal("expected ok=false when issue column list is empty")
	}
}
