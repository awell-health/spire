package cleric

import (
	"encoding/json"
	"testing"
)

// TestV1ActionsCatalogContainsAllVerbs ensures the catalog covers every
// verb the desktop and cleric runtime expect. The names are part of the
// public contract; if one disappears, callers break.
func TestV1ActionsCatalogContainsAllVerbs(t *testing.T) {
	want := []string{"resummon", "dismiss", "update_status", "comment_request", "reset_hard"}
	have := map[string]bool{}
	for _, a := range V1Actions {
		have[a.Name] = true
	}
	for _, v := range want {
		if !have[v] {
			t.Errorf("V1Actions missing verb %q", v)
		}
	}
	if len(V1Actions) != len(want) {
		t.Errorf("V1Actions has %d entries, want %d (extras: %v)", len(V1Actions), len(want), have)
	}
}

// TestV1ActionsArgsSchemasParse pins that every entry's ArgsSchema is
// valid JSON. The manifest is served verbatim over HTTP — desktop will
// break if the schema string is malformed.
func TestV1ActionsArgsSchemasParse(t *testing.T) {
	for _, a := range V1Actions {
		var v interface{}
		if err := json.Unmarshal(a.ArgsSchema, &v); err != nil {
			t.Errorf("%s: ArgsSchema is not valid JSON: %v", a.Name, err)
		}
	}
}

// TestFindActionReturnsManifest verifies the lookup helper.
func TestFindActionReturnsManifest(t *testing.T) {
	a := FindAction("resummon")
	if a == nil {
		t.Fatal("FindAction(\"resummon\") = nil, want manifest")
	}
	if a.Name != "resummon" {
		t.Errorf("Name = %q, want resummon", a.Name)
	}
	if FindAction("not-a-verb") != nil {
		t.Error("FindAction on bogus name returned non-nil")
	}
}

// TestDestructiveActionsAreFlagged pins the destructive-flag policy:
// dismiss and reset_hard close beads / nuke worktrees and must be flagged
// so the desktop confirms before firing.
func TestDestructiveActionsAreFlagged(t *testing.T) {
	for _, a := range V1Actions {
		switch a.Name {
		case "dismiss", "reset_hard":
			if !a.Destructive {
				t.Errorf("%s: Destructive = false, want true", a.Name)
			}
		case "resummon", "update_status", "comment_request":
			if a.Destructive {
				t.Errorf("%s: Destructive = true, want false", a.Name)
			}
		}
	}
}

func TestIsValidStatusTransition(t *testing.T) {
	tests := []struct {
		from, to string
		want     bool
	}{
		// Whitelisted hooked → * cases. The "I fixed it manually, take
		// the bead back" path the design calls out.
		{"hooked", "open", true},
		{"hooked", "in_progress", true},
		{"hooked", "awaiting_review", true},
		{"hooked", "closed", true},

		// awaiting_review → * (cleric / human gate outcomes).
		{"awaiting_review", "in_progress", true},
		{"awaiting_review", "closed", true},

		// Idempotent self-transitions are always allowed so re-firing the
		// same verb is a 200 no-op.
		{"hooked", "hooked", true},
		{"open", "open", true},

		// Disallowed: closed → anything else (must use other paths).
		{"closed", "open", false},
		{"closed", "in_progress", false},

		// Unknown statuses fall through to false.
		{"banana", "open", false},
		{"open", "banana", false},
	}
	for _, tc := range tests {
		got := IsValidStatusTransition(tc.from, tc.to)
		if got != tc.want {
			t.Errorf("IsValidStatusTransition(%q, %q) = %v, want %v", tc.from, tc.to, got, tc.want)
		}
	}
}

// TestEndpointPathsAreUnique pins that no two manifest entries share the
// same endpoint — the desktop dropdown groups by endpoint, and a
// duplicate would silently mask one of the verbs.
func TestEndpointPathsAreUnique(t *testing.T) {
	seen := map[string]string{}
	for _, a := range V1Actions {
		if other, ok := seen[a.EndpointPath]; ok {
			t.Errorf("endpoint %q is shared by %q and %q", a.EndpointPath, other, a.Name)
		}
		seen[a.EndpointPath] = a.Name
	}
}
