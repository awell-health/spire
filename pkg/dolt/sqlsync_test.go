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
		{"it's", "it''s"},
	}
	for _, tt := range tests {
		got := sqlEscape(tt.in)
		if got != tt.want {
			t.Errorf("sqlEscape(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestValidateIdentifier(t *testing.T) {
	cases := []struct {
		name    string
		v       string
		wantErr bool
	}{
		{"empty", "", true},
		{"plain", "beads_acm", false},
		{"with-dash", "beads-acm", false},
		{"with-dot", "beads.acm", false},
		{"backtick", "bad`name", true},
		{"nul", "bad\x00name", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateIdentifier("dbName", tc.v)
			if tc.wantErr && err == nil {
				t.Errorf("validateIdentifier(%q) = nil, want error", tc.v)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("validateIdentifier(%q) = %v, want nil", tc.v, err)
			}
		})
	}
}

func TestBuildSyncQuery(t *testing.T) {
	t.Run("requires dbName", func(t *testing.T) {
		if _, err := buildSyncQuery("DOLT_PULL", "", "origin", "main"); err == nil {
			t.Fatal("expected error for empty dbName, got nil")
		}
	})

	t.Run("rejects backtick in dbName", func(t *testing.T) {
		if _, err := buildSyncQuery("DOLT_PULL", "weird`db", "origin", "main"); err == nil {
			t.Fatal("expected error for backtick in dbName, got nil")
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

func TestSQLPull_RejectsInvalidDBName(t *testing.T) {
	// Empty and backtick-containing dbNames must fail the validator before
	// we ever touch RawQuery, so the test does not need a live dolt server.
	cases := []struct {
		name   string
		dbName string
	}{
		{"empty", ""},
		{"backtick", "evil`"},
		{"nul", "bad\x00"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := SQLPull(tc.dbName, "origin", "main")
			if err == nil {
				t.Fatalf("SQLPull(%q) = nil, want error", tc.dbName)
			}
			if !strings.Contains(err.Error(), "SQLPull") {
				t.Errorf("SQLPull(%q) err = %v, want wrapped with SQLPull context", tc.dbName, err)
			}
		})
	}
}

func TestSQLPush_RejectsInvalidDBName(t *testing.T) {
	cases := []struct {
		name   string
		dbName string
	}{
		{"empty", ""},
		{"backtick", "evil`"},
		{"nul", "bad\x00"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := SQLPush(tc.dbName, "origin", "main")
			if err == nil {
				t.Fatalf("SQLPush(%q) = nil, want error", tc.dbName)
			}
			if !strings.Contains(err.Error(), "SQLPush") {
				t.Errorf("SQLPush(%q) err = %v, want wrapped with SQLPush context", tc.dbName, err)
			}
		})
	}
}
