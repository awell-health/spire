package tower

import (
	"fmt"
	"strings"
	"testing"
)

// recordingExec returns a SQLExec that records every query and delegates the
// return value to resp(query). resp may return an error to simulate failures.
func recordingExec(calls *[]string, resp func(q string) (string, error)) SQLExec {
	return func(q string) (string, error) {
		*calls = append(*calls, q)
		if resp != nil {
			return resp(q)
		}
		return "", nil
	}
}

func firstIndexContaining(calls []string, subs ...string) int {
	for i, c := range calls {
		all := true
		for _, s := range subs {
			if !strings.Contains(c, s) {
				all = false
				break
			}
		}
		if all {
			return i
		}
	}
	return -1
}

// TestPreRegisterLocalOnlyIgnore_FreshTower: when the local-only tables do not
// yet exist, each is registered in dolt_ignore and the change is committed —
// the fresh-tower path that makes them untracked on creation.
func TestPreRegisterLocalOnlyIgnore_FreshTower(t *testing.T) {
	var calls []string
	exec := recordingExec(&calls, func(q string) (string, error) {
		return "", nil // SHOW TABLES empty => tables absent
	})
	if err := preRegisterLocalOnlyIgnore(exec, "smoke"); err != nil {
		t.Fatalf("preRegisterLocalOnlyIgnore: %v", err)
	}
	for _, lt := range LocalOnlyTables {
		if firstIndexContaining(calls, "dolt_ignore", lt) < 0 {
			t.Errorf("no dolt_ignore registration for %q", lt)
		}
	}
	if firstIndexContaining(calls, "DOLT_ADD('dolt_ignore')") < 0 {
		t.Error("expected DOLT_ADD('dolt_ignore') staging call")
	}
	if firstIndexContaining(calls, "DOLT_COMMIT") < 0 {
		t.Error("expected a DOLT_COMMIT of the dolt_ignore change")
	}
}

// TestPreRegisterLocalOnlyIgnore_ExistingTower: when the tables already exist,
// no dolt_ignore patterns are registered (UntrackLocalOnlyTables owns that
// case — registering here would be a no-op on a committed table).
func TestPreRegisterLocalOnlyIgnore_ExistingTower(t *testing.T) {
	var calls []string
	exec := recordingExec(&calls, func(q string) (string, error) {
		if strings.Contains(q, "SHOW TABLES LIKE") {
			// Echo the requested table name so tableExists reports "present".
			for _, lt := range LocalOnlyTables {
				if strings.Contains(q, lt) {
					return lt, nil
				}
			}
		}
		return "", nil
	})
	if err := preRegisterLocalOnlyIgnore(exec, "smoke"); err != nil {
		t.Fatalf("preRegisterLocalOnlyIgnore: %v", err)
	}
	if i := firstIndexContaining(calls, "REPLACE INTO dolt_ignore"); i >= 0 {
		t.Errorf("did not expect a dolt_ignore registration for existing tables; got call %q", calls[i])
	}
}

// TestUntrackLocalOnlyTables_DropBeforeIgnore is the load-bearing ordering
// invariant: for a tracked table, the DROP is committed BEFORE the dolt_ignore
// pattern is added (Dolt refuses to stage a drop once the table is ignored),
// and the table is recreated after the ignore.
func TestUntrackLocalOnlyTables_DropBeforeIgnore(t *testing.T) {
	var calls []string
	exec := recordingExec(&calls, func(q string) (string, error) {
		// AS OF 'HEAD' probe succeeds => table is tracked.
		return "", nil
	})
	if err := UntrackLocalOnlyTables(exec, "smoke"); err != nil {
		t.Fatalf("UntrackLocalOnlyTables: %v", err)
	}
	for _, lt := range LocalOnlyTables {
		dropIdx := firstIndexContaining(calls, "DROP TABLE `"+lt+"`")
		ignoreIdx := firstIndexContaining(calls, "REPLACE INTO dolt_ignore VALUES ('"+lt+"'")
		ddl, _ := localOnlyTableDDL(lt)
		recreateIdx := firstIndexContaining(calls, ddl)
		if dropIdx < 0 {
			t.Errorf("%s: expected a DROP TABLE", lt)
			continue
		}
		if ignoreIdx < 0 {
			t.Errorf("%s: expected a dolt_ignore registration", lt)
			continue
		}
		if dropIdx >= ignoreIdx {
			t.Errorf("%s: DROP (call %d) must come before ignore (call %d)", lt, dropIdx, ignoreIdx)
		}
		if recreateIdx < 0 || recreateIdx <= ignoreIdx {
			t.Errorf("%s: recreate (call %d) must come after ignore (call %d)", lt, recreateIdx, ignoreIdx)
		}
	}
}

// TestUntrackLocalOnlyTables_SkipsUntracked: when the AS OF 'HEAD' probe errors
// (table absent from HEAD = already untracked), the destructive drop is skipped
// — making the migration safe to re-run.
func TestUntrackLocalOnlyTables_SkipsUntracked(t *testing.T) {
	var calls []string
	exec := recordingExec(&calls, func(q string) (string, error) {
		if strings.Contains(q, "AS OF 'HEAD'") {
			return "", fmt.Errorf("table not found: %s", q)
		}
		return "", nil
	})
	if err := UntrackLocalOnlyTables(exec, "smoke"); err != nil {
		t.Fatalf("UntrackLocalOnlyTables: %v", err)
	}
	if i := firstIndexContaining(calls, "DROP TABLE"); i >= 0 {
		t.Errorf("must not DROP an already-untracked table; got %q", calls[i])
	}
}
