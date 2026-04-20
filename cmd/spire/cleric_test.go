package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// clericTestHarness captures every write the cleric commands make so tests
// can assert on the effect set. We stub the store lookup and metadata
// write seams; nothing touches a real dolt instance.
type clericTestHarness struct {
	mu       sync.Mutex
	bead     Bead
	metadata map[string]string
}

func newClericHarness(t *testing.T, bead Bead) (*clericTestHarness, func()) {
	t.Helper()

	h := &clericTestHarness{bead: bead, metadata: map[string]string{}}

	origGetBead := clericGetBeadFunc
	origSetMeta := clericSetBeadMetadataMapFunc
	origNow := clericNowFunc
	origGetwd := clericGetwdFunc

	clericGetBeadFunc = func(id string) (Bead, error) {
		h.mu.Lock()
		defer h.mu.Unlock()
		if id != h.bead.ID {
			return Bead{}, fmt.Errorf("unexpected bead id %q", id)
		}
		return h.bead, nil
	}
	clericSetBeadMetadataMapFunc = func(id string, m map[string]string) error {
		h.mu.Lock()
		defer h.mu.Unlock()
		if id != h.bead.ID {
			return fmt.Errorf("unexpected bead id %q", id)
		}
		for k, v := range m {
			h.metadata[k] = v
		}
		return nil
	}
	clericNowFunc = func() time.Time {
		return time.Date(2026, 4, 20, 12, 0, 0, 0, time.UTC)
	}

	cleanup := func() {
		clericGetBeadFunc = origGetBead
		clericSetBeadMetadataMapFunc = origSetMeta
		clericNowFunc = origNow
		clericGetwdFunc = origGetwd
	}
	return h, cleanup
}

// --- Registration -------------------------------------------------------

func TestClericCmdRegistered(t *testing.T) {
	children := map[string]bool{}
	for _, c := range clericCmd.Commands() {
		children[c.Name()] = true
	}
	for _, want := range []string{"diagnose", "execute", "learn"} {
		if !children[want] {
			t.Errorf("clericCmd missing child %q; have %v", want, keysOfBool(children))
		}
	}

	// And the parent must itself be registered on rootCmd so `spire cleric`
	// actually dispatches.
	foundParent := false
	for _, c := range rootCmd.Commands() {
		if c.Name() == "cleric" {
			foundParent = true
			break
		}
	}
	if !foundParent {
		t.Error("rootCmd is missing the cleric parent command")
	}
}

// --- diagnose -----------------------------------------------------------

func TestClericDiagnose_HappyPath(t *testing.T) {
	bead := Bead{ID: "spi-diag", Title: "diag"}
	h, cleanup := newClericHarness(t, bead)
	defer cleanup()

	if err := cmdClericDiagnose("spi-diag", "retry with fresh worktree"); err != nil {
		t.Fatalf("diagnose: %v", err)
	}

	if got := h.metadata["cleric_state"]; got != "diagnosed" {
		t.Errorf("cleric_state = %q, want diagnosed", got)
	}
	if got := h.metadata["cleric_decision"]; got != "retry with fresh worktree" {
		t.Errorf("cleric_decision = %q, want retry with fresh worktree", got)
	}
	if h.metadata["cleric_diagnosed_at"] == "" {
		t.Error("cleric_diagnosed_at not set")
	}
}

// --- execute ------------------------------------------------------------

func TestClericExecute_HappyPath(t *testing.T) {
	bead := Bead{ID: "spi-exec", Title: "exec"}
	h, cleanup := newClericHarness(t, bead)
	defer cleanup()

	if err := cmdClericExecute("rebase-onto-base", "spi-exec"); err != nil {
		t.Fatalf("execute: %v", err)
	}

	if got := h.metadata["cleric_state"]; got != "executed" {
		t.Errorf("cleric_state = %q, want executed", got)
	}
	if got := h.metadata["cleric_action"]; got != "rebase-onto-base" {
		t.Errorf("cleric_action = %q, want rebase-onto-base", got)
	}
	if h.metadata["cleric_executed_at"] == "" {
		t.Error("cleric_executed_at not set")
	}
}

func TestClericExecute_MissingActionFlag(t *testing.T) {
	// Direct path: cmdClericExecute returns an explicit error when action
	// is empty. This mirrors what cobra's MarkFlagRequired produces at the
	// dispatch layer but lets the test run without the full cobra harness.
	err := cmdClericExecute("", "spi-nope")
	if err == nil {
		t.Fatal("expected error when --action is empty")
	}
	if !strings.Contains(err.Error(), "action") {
		t.Errorf("error %q does not mention 'action'", err.Error())
	}

	// Cobra-level guard: MarkFlagRequired must be configured on the command.
	if _, ok := clericExecuteCmd.Flags().Lookup("action").Annotations["cobra_annotation_bash_completion_one_required_flag"]; !ok {
		t.Error("action flag is not marked required on clericExecuteCmd")
	}
}

func TestClericExecute_BeadAutoDetect(t *testing.T) {
	bead := Bead{ID: "spi-auto", Title: "auto"}
	h, cleanup := newClericHarness(t, bead)
	defer cleanup()

	// Point the cwd seam at a path whose basename matches the bead ID.
	// filepath.Join keeps the test portable across platforms.
	fakeCwd := filepath.Join(t.TempDir(), "spi-auto")
	if err := os.MkdirAll(fakeCwd, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	clericGetwdFunc = func() (string, error) { return fakeCwd, nil }

	// Ensure no stale SPIRE_BEAD env leaks in.
	t.Setenv("SPIRE_BEAD", "")

	if err := cmdClericExecute("rerun-plan", ""); err != nil {
		t.Fatalf("execute (auto-detect): %v", err)
	}

	if got := h.metadata["cleric_action"]; got != "rerun-plan" {
		t.Errorf("cleric_action = %q, want rerun-plan", got)
	}
	if got := h.metadata["cleric_state"]; got != "executed" {
		t.Errorf("cleric_state = %q, want executed", got)
	}
}

// --- learn --------------------------------------------------------------

func TestClericLearn_HappyPath(t *testing.T) {
	bead := Bead{ID: "spi-learn", Title: "learn"}
	h, cleanup := newClericHarness(t, bead)
	defer cleanup()

	if err := cmdClericLearn("spi-learn", "conflicts came from stale base"); err != nil {
		t.Fatalf("learn: %v", err)
	}

	if got := h.metadata["cleric_state"]; got != "finished" {
		t.Errorf("cleric_state = %q, want finished", got)
	}
	if got := h.metadata["cleric_learning"]; got != "conflicts came from stale base" {
		t.Errorf("cleric_learning = %q, want 'conflicts came from stale base'", got)
	}
	if h.metadata["cleric_learned_at"] == "" {
		t.Error("cleric_learned_at not set")
	}
}

func keysOfBool(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
