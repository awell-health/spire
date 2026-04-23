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

// TestApplySpireExtensions_CreatesAllThreeTables verifies the helper
// issues one CREATE TABLE statement per Spire extension table, in order,
// scoped to the caller's database via `USE`. This is the contract both
// `spire tower create` and `attach-cluster --bootstrap-if-blank` depend on.
func TestApplySpireExtensions_CreatesAllThreeTables(t *testing.T) {
	var calls []string
	exec := func(q string) (string, error) {
		calls = append(calls, q)
		return "", nil
	}
	if err := ApplySpireExtensions(exec, "smoke"); err != nil {
		t.Fatalf("ApplySpireExtensions: %v", err)
	}
	if len(calls) != 3 {
		t.Fatalf("want 3 CREATE statements, got %d", len(calls))
	}
	wantOrdered := []struct {
		name string
		ddl  string
	}{
		{"repos", ReposTableSQL},
		{"agent_runs", AgentRunsTableSQL},
		{"bead_lifecycle", store.BeadLifecycleTableSQL},
	}
	for i, want := range wantOrdered {
		if !strings.Contains(calls[i], "USE `smoke`") {
			t.Errorf("call[%d] missing USE clause; got %q", i, calls[i])
		}
		if !strings.Contains(calls[i], want.ddl) {
			t.Errorf("call[%d] does not contain expected %s DDL", i, want.name)
		}
	}
}

// TestApplySpireExtensions_ErrorAttribution confirms error messages name
// the failing table so production debug output identifies the bad DDL
// without a log-dive. spi-2xf158 traced the original incident via error
// messages — keeping attribution prevents a regression in that signal.
func TestApplySpireExtensions_ErrorAttribution(t *testing.T) {
	tests := []struct {
		name       string
		failOnNth  int
		wantTable  string
	}{
		{"fail on repos", 1, "repos"},
		{"fail on agent_runs", 2, "agent_runs"},
		{"fail on bead_lifecycle", 3, "bead_lifecycle"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sentinel := fmt.Errorf("sentinel-%s", tc.wantTable)
			var n int
			exec := func(q string) (string, error) {
				n++
				if n == tc.failOnNth {
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
// `USE \`\`` which dolt rejects, but catching it up-front gives a
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
