package executor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestEscalationLogTail verifies the stopgap that enriches terminal-escalate
// comments with the tail of the run's wizard log (releases/v0.53.0). The
// escalate branch of bead.finish has no handle on the upstream step's failure
// detail, so without this the archmage gets a content-free "formula requested
// escalation" comment and has to dig the real error out of the wizard log by
// hand.
func TestEscalationLogTail(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SPIRE_DOLT_DIR", dir)

	wizards := filepath.Join(dir, "wizards")
	if err := os.MkdirAll(wizards, 0o755); err != nil {
		t.Fatal(err)
	}
	// The agent backend resolves "<name>.log" first, and the wizard agent
	// name is "wizard-<bead>", so the log file is "wizard-oo-test.log".
	logPath := filepath.Join(wizards, "wizard-oo-test.log")
	content := "line-a\nbuild failed: ECONNREFUSED 127.0.0.1:8103\nline-c\nline-d\n"
	if err := os.WriteFile(logPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	e := &Executor{agentName: "wizard-oo-test", beadID: "oo-test"}

	t.Run("returns the last n lines", func(t *testing.T) {
		got := e.escalationLogTail(2)
		want := "line-c\nline-d"
		if got != want {
			t.Fatalf("escalationLogTail(2) = %q, want %q", got, want)
		}
	})

	t.Run("includes the failure detail when n covers it", func(t *testing.T) {
		got := e.escalationLogTail(40)
		if !strings.Contains(got, "ECONNREFUSED 127.0.0.1:8103") {
			t.Fatalf("escalationLogTail(40) = %q, want it to contain the build error", got)
		}
	})

	t.Run("falls back to bead ID when agent name is empty", func(t *testing.T) {
		// wizard-<bead>.log is the third candidate the backend tries, so a
		// lookup keyed on the bare bead ID still resolves the wizard log.
		eNoAgent := &Executor{beadID: "oo-test"}
		if got := eNoAgent.escalationLogTail(1); got != "line-d" {
			t.Fatalf("escalationLogTail(1) with empty agent = %q, want %q", got, "line-d")
		}
	})

	t.Run("returns empty when no log exists", func(t *testing.T) {
		eMissing := &Executor{agentName: "wizard-oo-absent", beadID: "oo-absent"}
		if got := eMissing.escalationLogTail(10); got != "" {
			t.Fatalf("escalationLogTail with no log = %q, want empty", got)
		}
	})
}
