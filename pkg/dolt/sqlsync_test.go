package dolt

import (
	"strings"
	"testing"
)

func TestSQLEscape(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"", ""},
		{"main", "main"},
		{"o'rigin", "o''rigin"},
		{"''", "''''"},
		{"a'b'c", "a''b''c"},
	}
	for _, tt := range tests {
		got := sqlEscape(tt.in)
		if got != tt.want {
			t.Errorf("sqlEscape(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestSQLEscapeIdent(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"", ""},
		{"beads", "beads"},
		{"b`ad", "b``ad"},
		{"``", "````"},
		{"a`b`c", "a``b``c"},
	}
	for _, tt := range tests {
		got := sqlEscapeIdent(tt.in)
		if got != tt.want {
			t.Errorf("sqlEscapeIdent(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestBuildSyncQuery(t *testing.T) {
	t.Run("requires dbName", func(t *testing.T) {
		if _, err := buildSyncQuery("DOLT_PULL", "", "origin", "main"); err == nil {
			t.Fatal("expected error for empty dbName, got nil")
		}
	})

	t.Run("defaults remote and branch", func(t *testing.T) {
		got, err := buildSyncQuery("DOLT_PULL", "beads", "", "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := "USE `beads`; CALL DOLT_PULL('origin', 'main')"
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("pull and push use same shape", func(t *testing.T) {
		pull, _ := buildSyncQuery("DOLT_PULL", "beads", "origin", "main")
		push, _ := buildSyncQuery("DOLT_PUSH", "beads", "origin", "main")
		if !strings.Contains(pull, "CALL DOLT_PULL(") {
			t.Errorf("pull missing DOLT_PULL: %q", pull)
		}
		if !strings.Contains(push, "CALL DOLT_PUSH(") {
			t.Errorf("push missing DOLT_PUSH: %q", push)
		}
	})

	t.Run("escapes backticks in dbName", func(t *testing.T) {
		got, err := buildSyncQuery("DOLT_PULL", "weird`db", "origin", "main")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := "USE `weird``db`; CALL DOLT_PULL('origin', 'main')"
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("escapes single quotes in remote/branch", func(t *testing.T) {
		got, err := buildSyncQuery("DOLT_PUSH", "beads", "o'rigin", "ma'in")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := "USE `beads`; CALL DOLT_PUSH('o''rigin', 'ma''in')"
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})
}

func TestSQLPullPushValidation(t *testing.T) {
	// Empty dbName short-circuits before RawQuery, so these run without
	// a live server.
	if err := SQLPull("", "origin", "main"); err == nil {
		t.Error("SQLPull with empty dbName: expected error, got nil")
	}
	if err := SQLPush("", "origin", "main"); err == nil {
		t.Error("SQLPush with empty dbName: expected error, got nil")
	}
}
