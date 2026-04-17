package tower

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// pipeOutput mimics what `dolt sql -q` emits so ExtractSQLValue can pluck the
// value from the data row.
func pipeOutput(value string) string {
	return strings.Join([]string{
		"+-----------+",
		"| value     |",
		"+-----------+",
		fmt.Sprintf("| %-9s |", value),
		"+-----------+",
	}, "\n")
}

func stubExec(responses map[string]struct {
	out string
	err error
}) SQLExec {
	return func(query string) (string, error) {
		for substr, resp := range responses {
			if strings.Contains(query, substr) {
				return resp.out, resp.err
			}
		}
		return "", fmt.Errorf("stubExec: no response for query %q", query)
	}
}

func TestReadMetadata_ScopedVsUnscoped(t *testing.T) {
	tests := []struct {
		name     string
		database string
		wantExpr string
	}{
		{"unscoped", "", "metadata"},
		{"scoped",   "beads_t1", "`beads_t1`.metadata"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var seen []string
			exec := func(q string) (string, error) {
				seen = append(seen, q)
				if strings.Contains(q, "_project_id") {
					return pipeOutput("proj-42"), nil
				}
				return pipeOutput("spi"), nil
			}

			projectID, prefix, err := ReadMetadata(exec, tc.database)
			if err != nil {
				t.Fatalf("ReadMetadata: %v", err)
			}
			if projectID != "proj-42" {
				t.Errorf("projectID = %q, want proj-42", projectID)
			}
			if prefix != "spi" {
				t.Errorf("prefix = %q, want spi", prefix)
			}
			if len(seen) != 2 {
				t.Fatalf("expected 2 queries, got %d: %v", len(seen), seen)
			}
			for _, q := range seen {
				if !strings.Contains(q, "FROM "+tc.wantExpr+" ") {
					t.Errorf("query %q should reference table expr %q", q, tc.wantExpr)
				}
			}
		})
	}
}

func TestReadMetadata_ProjectIDErrorBubblesUp(t *testing.T) {
	sentinel := errors.New("connection refused")
	exec := stubExec(map[string]struct {
		out string
		err error
	}{
		"_project_id": {err: sentinel},
	})

	_, _, err := ReadMetadata(exec, "")
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("error chain should wrap sentinel; got %v", err)
	}
}

func TestReadMetadata_MissingProjectIDIsFatal(t *testing.T) {
	exec := stubExec(map[string]struct {
		out string
		err error
	}{
		"_project_id": {out: ""},
		"prefix":      {out: pipeOutput("spi")},
	})

	_, _, err := ReadMetadata(exec, "")
	if err == nil || !strings.Contains(err.Error(), "no project_id") {
		t.Fatalf("want 'no project_id' error, got %v", err)
	}
}

func TestReadMetadata_PrefixQueryErrorIsTolerated(t *testing.T) {
	exec := stubExec(map[string]struct {
		out string
		err error
	}{
		"_project_id": {out: pipeOutput("proj-7")},
		"prefix":      {err: errors.New("table missing")},
	})

	projectID, prefix, err := ReadMetadata(exec, "")
	if err != nil {
		t.Fatalf("prefix error should not fail ReadMetadata: %v", err)
	}
	if projectID != "proj-7" {
		t.Errorf("projectID = %q, want proj-7", projectID)
	}
	if prefix != "" {
		t.Errorf("prefix should be empty on query error; got %q", prefix)
	}
}

func TestReadMetadata_EmptyPrefixRowYieldsEmptyString(t *testing.T) {
	exec := stubExec(map[string]struct {
		out string
		err error
	}{
		"_project_id": {out: pipeOutput("proj-8")},
		"prefix":      {out: "\n\n"},
	})

	projectID, prefix, err := ReadMetadata(exec, "")
	if err != nil {
		t.Fatalf("ReadMetadata: %v", err)
	}
	if projectID != "proj-8" {
		t.Errorf("projectID = %q, want proj-8", projectID)
	}
	if prefix != "" {
		t.Errorf("prefix = %q, want empty", prefix)
	}
}
