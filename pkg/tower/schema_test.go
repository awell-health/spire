package tower

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/awell-health/spire/pkg/store"
)

// TestReposTableSQL pins the repos-table DDL to the shape callers rely on.
// `spire repo add` is the smoke-test canary for attach-cluster bootstrap —
// if this DDL drifts or stops producing the expected column set, the
// cluster-native install regresses.
func TestReposTableSQL(t *testing.T) {
	if ReposTableSQL == "" {
		t.Fatal("ReposTableSQL is empty")
	}
	for _, fragment := range []string{
		"CREATE TABLE",
		"IF NOT EXISTS",
		"repos",
		"prefix",
		"repo_url",
		"branch",
		"PRIMARY KEY",
	} {
		if !strings.Contains(ReposTableSQL, fragment) {
			t.Errorf("ReposTableSQL missing %q", fragment)
		}
	}
}

// TestAgentRunsTableSQL guards the metrics-pipeline DDL. Every
// spireMigrations row in cmd/spire/tower.go assumes a column from this
// DDL already exists on first boot; drift here would cascade into
// migration failures on every tower startup.
func TestAgentRunsTableSQL(t *testing.T) {
	if AgentRunsTableSQL == "" {
		t.Fatal("AgentRunsTableSQL is empty")
	}
	for _, fragment := range []string{
		"CREATE TABLE",
		"IF NOT EXISTS",
		"agent_runs",
		"id VARCHAR",
		"bead_id",
		"PRIMARY KEY",
	} {
		if !strings.Contains(AgentRunsTableSQL, fragment) {
			t.Errorf("AgentRunsTableSQL missing %q", fragment)
		}
	}
}

// TestApplySpireExtensions_CreatesAllExtensionTables verifies the helper
// issues one CREATE TABLE statement per Spire extension table, in order,
// scoped to the caller's database via `USE`. This is the contract both
// `spire tower create` and `attach-cluster --bootstrap-if-blank` depend on.
func TestApplySpireExtensions_CreatesAllExtensionTables(t *testing.T) {
	var calls []string
	exec := func(q string) (string, error) {
		calls = append(calls, q)
		// SHOW TABLES returns empty → fresh tower (local-only tables absent),
		// exercising the dolt_ignore-before-create path.
		return "", nil
	}
	if err := ApplySpireExtensions(exec, "smoke"); err != nil {
		t.Fatalf("ApplySpireExtensions: %v", err)
	}
	wantOrdered := []struct {
		name string
		ddl  string
	}{
		{"repos", ReposTableSQL},
		{"agent_runs", AgentRunsTableSQL},
		{"bead_lifecycle", store.BeadLifecycleTableSQL},
		{"agent_log_artifacts", store.AgentLogArtifactsTableSQL},
	}
	// Each extension table is created exactly once, scoped via USE, in order.
	createIdx := make(map[string]int, len(wantOrdered))
	var prev = -1
	for _, want := range wantOrdered {
		found := -1
		for j, c := range calls {
			if strings.Contains(c, want.ddl) {
				if !strings.Contains(c, "USE `smoke`") {
					t.Errorf("%s CREATE missing USE clause; got %q", want.name, c)
				}
				found = j
				break
			}
		}
		if found < 0 {
			t.Fatalf("no CREATE call for %s", want.name)
		}
		if found <= prev {
			t.Errorf("%s CREATE (call %d) is not after the previous table's CREATE (call %d)", want.name, found, prev)
		}
		prev = found
		createIdx[want.name] = found
	}
	// Fresh-tower contract: local-only tables are registered in dolt_ignore
	// BEFORE they are created (dolt_ignore has no effect post-commit).
	ignoreIdx := -1
	for j, c := range calls {
		if strings.Contains(c, "dolt_ignore") && strings.Contains(c, "DOLT_ADD") {
			ignoreIdx = j
			break
		}
	}
	if ignoreIdx < 0 {
		t.Fatal("expected a dolt_ignore registration call before the local-only CREATEs")
	}
	for _, lt := range LocalOnlyTables {
		if ci, ok := createIdx[lt]; ok && ignoreIdx >= ci {
			t.Errorf("dolt_ignore registered at call %d, not before %s CREATE at call %d", ignoreIdx, lt, ci)
		}
	}
}

// TestApplySpireExtensions_ErrorAttribution confirms error messages name
// the failing table so production debug output identifies the bad DDL
// without a log-dive. spi-2xf158 traced the original incident via error
// messages — keeping attribution prevents a regression in that signal.
func TestApplySpireExtensions_ErrorAttribution(t *testing.T) {
	tests := []struct {
		name      string
		ddl       string
		wantTable string
	}{
		{"fail on repos", ReposTableSQL, "repos"},
		{"fail on agent_runs", AgentRunsTableSQL, "agent_runs"},
		{"fail on bead_lifecycle", store.BeadLifecycleTableSQL, "bead_lifecycle"},
		{"fail on agent_log_artifacts", store.AgentLogArtifactsTableSQL, "agent_log_artifacts"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sentinel := fmt.Errorf("sentinel-%s", tc.wantTable)
			// Fail on the specific table's CREATE (matched by content, robust to
			// the dolt_ignore pre-registration calls that now precede the loop).
			exec := func(q string) (string, error) {
				if strings.Contains(q, tc.ddl) {
					return "", sentinel
				}
				return "", nil
			}
			err := ApplySpireExtensions(exec, "spi")
			if err == nil {
				t.Fatalf("expected error")
			}
			if !errors.Is(err, sentinel) {
				t.Errorf("error chain should wrap sentinel; got %v", err)
			}
			if !strings.Contains(err.Error(), tc.wantTable) {
				t.Errorf("error should name failing table %q; got %v", tc.wantTable, err)
			}
		})
	}
}

// TestApplySpireExtensions_Validation ensures the helper rejects inputs
// that would produce corrupt DDL (empty database name would emit
// `USE \`\“ which dolt rejects, but catching it up-front gives a
// clearer error than dolt's parser).
func TestApplySpireExtensions_Validation(t *testing.T) {
	exec := func(q string) (string, error) { return "", nil }
	if err := ApplySpireExtensions(nil, "spi"); err == nil || !strings.Contains(err.Error(), "exec is required") {
		t.Errorf("want 'exec is required' error, got %v", err)
	}
	if err := ApplySpireExtensions(exec, ""); err == nil || !strings.Contains(err.Error(), "database is required") {
		t.Errorf("want 'database is required' error, got %v", err)
	}
}
