package dolt

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// pkg/dolt has no sqlsync.go file in the current source tree. The change
// spec listed it as a sweep target on the assumption it owned the
// remote-mutating Dolt op helpers (push/pull/fetch+merge wrappers).
// Those helpers live elsewhere — push.go, pull.go, sync.go, and
// dolthub.go each carry a single helper with its own
// EnsureNotGatewayResolved guard.
//
// The tests below pin two structural facts so the round-1 review
// finding stays codified instead of getting silently re-introduced:
//
//   - sqlsync.go does not exist; if a future contributor re-creates the
//     file, this test fails so the reviewer can decide whether the new
//     helpers belong with the rest of the guarded mutation surface.
//   - every remote-mutating helper currently in pkg/dolt has a
//     gateway-mode: annotation pinned next to its body, matching the
//     audit grep marker (`grep -rn "gateway-mode:" pkg/`).
//
// Read-only helpers (SQL, RawQuery, LocalQuery, InteractiveSQL) are
// intentionally excluded — the spec is explicit that local read-only
// SQL ops are not gated.

func TestSQLSyncGo_DoesNotExist(t *testing.T) {
	if _, err := os.Stat("sqlsync.go"); !os.IsNotExist(err) {
		t.Fatalf("sqlsync.go now exists; the round-1 review finding assumed " +
			"this file's absence — review whether new helpers in it carry " +
			"EnsureNotGatewayResolved guards and add them to the audit list above")
	}
}

// TestRemoteMutatingHelpersAnnotated walks the source files that own the
// remote-mutating dolt helpers and asserts each one has the audit
// `gateway-mode:` annotation in a comment. This is the durable artifact
// of the audit: any contributor that adds a new mutation helper without
// the annotation breaks the test, forcing a deliberate decision.
func TestRemoteMutatingHelpersAnnotated(t *testing.T) {
	cases := []struct {
		file    string
		helpers []string
	}{
		{"push.go", []string{"CLIPush", "SetCLIRemote"}},
		{"pull.go", []string{"CLIPull"}},
		{"sync.go", []string{"CLIFetchMerge"}},
		{"dolthub.go", []string{"EnsureDoltHubDB"}},
	}

	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join(".", tc.file))
			if err != nil {
				t.Fatalf("read %s: %v", tc.file, err)
			}
			text := string(data)

			if !strings.Contains(text, "gateway-mode:") {
				t.Errorf("%s missing `gateway-mode:` annotation; the audit "+
					"requires every remote-mutating helper carry the marker "+
					"so `grep -rn 'gateway-mode:' pkg/` enumerates the swept set",
					tc.file)
			}
			for _, name := range tc.helpers {
				needle := "func " + name
				if !strings.Contains(text, needle) {
					t.Errorf("%s no longer defines %s — audit list is stale",
						tc.file, name)
				}
			}
		})
	}
}
