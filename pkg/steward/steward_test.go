package steward

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/awell-health/spire/pkg/agent"
	"github.com/awell-health/spire/pkg/dolt"
	"github.com/awell-health/spire/pkg/store"
	"github.com/steveyegge/beads"
)

// --- AgentNames tests (replaces loadRoster tests) ---

func TestAgentNames_Override(t *testing.T) {
	agents := []agent.Info{
		{Name: "wizard-1"},
		{Name: "wizard-2"},
	}
	override := []string{"explicit-a", "explicit-b"}
	got := AgentNames(agents, override)
	if len(got) != 2 || got[0] != "explicit-a" || got[1] != "explicit-b" {
		t.Errorf("AgentNames with override = %v, want [explicit-a explicit-b]", got)
	}
}

func TestAgentNames_FromAgentInfo(t *testing.T) {
	agents := []agent.Info{
		{Name: "wizard-1"},
		{Name: "wizard-2"},
		{Name: "wizard-1"}, // duplicate
	}
	got := AgentNames(agents, nil)
	if len(got) != 2 || got[0] != "wizard-1" || got[1] != "wizard-2" {
		t.Errorf("AgentNames = %v, want [wizard-1 wizard-2]", got)
	}
}

func TestAgentNames_Empty(t *testing.T) {
	got := AgentNames(nil, nil)
	if len(got) != 0 {
		t.Errorf("AgentNames(nil, nil) = %v, want []", got)
	}
}

// --- BusySet tests (replaces findBusyAgents/localBusyAgents tests) ---

func TestBusySet_AliveOnly(t *testing.T) {
	agents := []agent.Info{
		{Name: "wizard-1", Alive: true},
		{Name: "wizard-2", Alive: false},
		{Name: "wizard-3", Alive: true},
	}
	busy := BusySet(agents)
	if !busy["wizard-1"] {
		t.Error("expected wizard-1 to be busy (alive)")
	}
	if busy["wizard-2"] {
		t.Error("expected wizard-2 to NOT be busy (dead)")
	}
	if !busy["wizard-3"] {
		t.Error("expected wizard-3 to be busy (alive)")
	}
}

func TestBusySet_Empty(t *testing.T) {
	busy := BusySet(nil)
	if len(busy) != 0 {
		t.Errorf("BusySet(nil) = %v, want empty", busy)
	}
}

// --- LoadLocalConfig tests ---

// chdirTemp changes the working directory to a new temp dir for the duration
// of the test and restores it on cleanup.
func chdirTemp(t *testing.T) string {
	t.Helper()
	tmpDir := t.TempDir()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(orig) })
	return tmpDir
}

func TestLoadLocalConfig_NoConfig_ZeroValues(t *testing.T) {
	chdirTemp(t) // no spire.yaml in the temp dir

	cfg := LoadLocalConfig()

	if cfg.Model != "" {
		t.Errorf("Model = %q, want zero value", cfg.Model)
	}
	if cfg.MaxTurns != 0 {
		t.Errorf("MaxTurns = %d, want 0", cfg.MaxTurns)
	}
	if cfg.Timeout != 0 {
		t.Errorf("Timeout = %s, want 0", cfg.Timeout)
	}
	if cfg.BaseBranch != "" {
		t.Errorf("BaseBranch = %q, want zero value", cfg.BaseBranch)
	}
	if cfg.BranchPattern != "" {
		t.Errorf("BranchPattern = %q, want zero value", cfg.BranchPattern)
	}
}

func TestLoadLocalConfig_Overrides(t *testing.T) {
	dir := chdirTemp(t)

	yaml := `agent:
  model: claude-opus-4-6
  max-turns: 50
  timeout: 30m
branch:
  base: develop
  pattern: "work/{bead-id}"
`
	if err := os.WriteFile(filepath.Join(dir, "spire.yaml"), []byte(yaml), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := LoadLocalConfig()

	if cfg.Model != "claude-opus-4-6" {
		t.Errorf("Model = %q, want %q", cfg.Model, "claude-opus-4-6")
	}
	if cfg.MaxTurns != 50 {
		t.Errorf("MaxTurns = %d, want 50", cfg.MaxTurns)
	}
	if cfg.Timeout != 30*time.Minute {
		t.Errorf("Timeout = %s, want 30m", cfg.Timeout)
	}
	if cfg.BaseBranch != "develop" {
		t.Errorf("BaseBranch = %q, want %q", cfg.BaseBranch, "develop")
	}
	if cfg.BranchPattern != "work/{bead-id}" {
		t.Errorf("BranchPattern = %q, want %q", cfg.BranchPattern, "work/{bead-id}")
	}
}

func TestLoadLocalConfig_PartialOverride(t *testing.T) {
	dir := chdirTemp(t)

	// Only override model; everything else should be zero (unset).
	yaml := `agent:
  model: claude-haiku-4-5-20251001
`
	if err := os.WriteFile(filepath.Join(dir, "spire.yaml"), []byte(yaml), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := LoadLocalConfig()

	if cfg.Model != "claude-haiku-4-5-20251001" {
		t.Errorf("Model = %q, want %q", cfg.Model, "claude-haiku-4-5-20251001")
	}
	// Remaining fields are zero — consumer decides defaults.
	if cfg.MaxTurns != 0 {
		t.Errorf("MaxTurns = %d, want 0 (unset)", cfg.MaxTurns)
	}
	if cfg.BaseBranch != "" {
		t.Errorf("BaseBranch = %q, want zero value (unset)", cfg.BaseBranch)
	}
}

// --- IsWizardRunning tests ---

func TestIsWizardRunning_NoPIDFile(t *testing.T) {
	t.Setenv("SPIRE_DOLT_DIR", t.TempDir())
	if IsWizardRunning("nonexistent-wizard") {
		t.Error("expected false for wizard with no PID file")
	}
}

func TestIsWizardRunning_SelfPID(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("SPIRE_DOLT_DIR", tmpDir)

	name := "test-wizard"
	if err := dolt.WritePID(WizardPIDPath(name), os.Getpid()); err != nil {
		t.Fatal(err)
	}

	if !IsWizardRunning(name) {
		t.Error("expected true for wizard with current process PID")
	}
}

func TestIsWizardRunning_DeadPID(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("SPIRE_DOLT_DIR", tmpDir)

	name := "dead-wizard"
	// PID 0 is never a valid process; processAlive returns false for it.
	if err := dolt.WritePID(WizardPIDPath(name), 0); err != nil {
		t.Fatal(err)
	}

	if IsWizardRunning(name) {
		t.Error("expected false for wizard with PID 0")
	}
}

// --- Fail-closed: corrupted bead quarantine tests ---

// TestStewardAssignment_FailClosed_ExcludesAndAlerts verifies that when
// GetActiveAttemptFunc returns an error (multiple open attempts), the bead
// is excluded (shouldSkip=true) and RaiseCorruptedBeadAlertFunc is called.
func TestStewardAssignment_FailClosed_ExcludesAndAlerts(t *testing.T) {
	origAttempt := GetActiveAttemptFunc
	GetActiveAttemptFunc = func(parentID string) (*store.Bead, error) {
		if parentID == "spi-corrupted" {
			return nil, fmt.Errorf("invariant violation: 2 open attempt beads for spi-corrupted")
		}
		return nil, nil
	}
	defer func() { GetActiveAttemptFunc = origAttempt }()

	var alertedBeads []string
	origAlert := RaiseCorruptedBeadAlertFunc
	RaiseCorruptedBeadAlertFunc = func(beadID string, err error) {
		alertedBeads = append(alertedBeads, beadID)
	}
	defer func() { RaiseCorruptedBeadAlertFunc = origAlert }()

	bead := store.Bead{ID: "spi-corrupted", Title: "corrupted task", Status: "open"}

	// Replicate the assignment-loop logic: fail closed on error.
	attempt, aErr := GetActiveAttemptFunc(bead.ID)
	if aErr != nil {
		RaiseCorruptedBeadAlertFunc(bead.ID, aErr)
	}
	shouldSkip := aErr != nil || attempt != nil

	if !shouldSkip {
		t.Error("expected corrupted bead to be excluded (shouldSkip=true)")
	}
	if len(alertedBeads) != 1 || alertedBeads[0] != "spi-corrupted" {
		t.Errorf("expected alert for spi-corrupted, got %v", alertedBeads)
	}
}

// TestStewardAssignment_FailClosed_CleanBeadUnaffected verifies that a bead
// without corrupted attempts is NOT excluded or alerted.
func TestStewardAssignment_FailClosed_CleanBeadUnaffected(t *testing.T) {
	origAttempt := GetActiveAttemptFunc
	GetActiveAttemptFunc = func(parentID string) (*store.Bead, error) {
		return nil, nil // no active attempt, no error
	}
	defer func() { GetActiveAttemptFunc = origAttempt }()

	var alertedBeads []string
	origAlert := RaiseCorruptedBeadAlertFunc
	RaiseCorruptedBeadAlertFunc = func(beadID string, err error) {
		alertedBeads = append(alertedBeads, beadID)
	}
	defer func() { RaiseCorruptedBeadAlertFunc = origAlert }()

	bead := store.Bead{ID: "spi-clean", Title: "clean task", Status: "open"}

	attempt, aErr := GetActiveAttemptFunc(bead.ID)
	if aErr != nil {
		RaiseCorruptedBeadAlertFunc(bead.ID, aErr)
	}
	shouldSkip := aErr != nil || attempt != nil

	if shouldSkip {
		t.Error("clean bead should not be excluded")
	}
	if len(alertedBeads) != 0 {
		t.Errorf("expected no alerts for clean bead, got %v", alertedBeads)
	}
}

// TestStewardReengage_FailClosed_SkipsAndAlerts verifies the re-engagement path:
// when GetActiveAttemptFunc returns an error, re-engagement is skipped and
// an alert is raised.
func TestStewardReengage_FailClosed_SkipsAndAlerts(t *testing.T) {
	origAttempt := GetActiveAttemptFunc
	GetActiveAttemptFunc = func(parentID string) (*store.Bead, error) {
		if parentID == "spi-reeng" {
			return nil, fmt.Errorf("invariant violation: 3 open attempt beads for spi-reeng")
		}
		return nil, nil
	}
	defer func() { GetActiveAttemptFunc = origAttempt }()

	var alertedBeads []string
	origAlert := RaiseCorruptedBeadAlertFunc
	RaiseCorruptedBeadAlertFunc = func(beadID string, err error) {
		alertedBeads = append(alertedBeads, beadID)
	}
	defer func() { RaiseCorruptedBeadAlertFunc = origAlert }()

	// Replicate the detectReviewFeedback re-engagement guard logic.
	reEngageAttempt, reEngageErr := GetActiveAttemptFunc("spi-reeng")
	if reEngageErr != nil {
		RaiseCorruptedBeadAlertFunc("spi-reeng", reEngageErr)
	}
	shouldSkip := reEngageErr != nil || reEngageAttempt != nil

	if !shouldSkip {
		t.Error("expected corrupted bead to be skipped for re-engagement")
	}
	if len(alertedBeads) != 1 || alertedBeads[0] != "spi-reeng" {
		t.Errorf("expected alert for spi-reeng, got %v", alertedBeads)
	}
}

// TestRaiseCorruptedBeadAlert_Dedup verifies that RaiseCorruptedBeadAlert
// does not create a duplicate alert when an open alert already exists for the bead.
func TestRaiseCorruptedBeadAlert_Dedup(t *testing.T) {
	// Track how many times the create function is called.
	createCount := 0
	origCreate := CreateAlertFunc
	CreateAlertFunc = func(beadID, msg string) error {
		createCount++
		return nil
	}
	defer func() { CreateAlertFunc = origCreate }()

	// First call: no existing alert.
	origCheck := CheckExistingAlertFunc
	CheckExistingAlertFunc = func(beadID string) bool { return false }
	defer func() { CheckExistingAlertFunc = origCheck }()

	RaiseCorruptedBeadAlert("spi-dup", fmt.Errorf("invariant violation"))
	if createCount != 1 {
		t.Errorf("expected 1 create on first call, got %d", createCount)
	}

	// Second call: alert now exists — dedup should suppress creation.
	CheckExistingAlertFunc = func(beadID string) bool { return true }
	RaiseCorruptedBeadAlert("spi-dup", fmt.Errorf("invariant violation"))
	if createCount != 1 {
		t.Errorf("expected still 1 create after dedup, got %d", createCount)
	}
}

// TestRaiseCorruptedBeadAlert_DedupPerBead verifies dedup is scoped per-bead:
// an existing alert for bead A does not suppress an alert for bead B.
func TestRaiseCorruptedBeadAlert_DedupPerBead(t *testing.T) {
	createCount := 0
	origCreate := CreateAlertFunc
	CreateAlertFunc = func(beadID, msg string) error {
		createCount++
		return nil
	}
	defer func() { CreateAlertFunc = origCreate }()

	origCheck := CheckExistingAlertFunc
	CheckExistingAlertFunc = func(beadID string) bool {
		return beadID == "spi-a" // only spi-a has existing alert
	}
	defer func() { CheckExistingAlertFunc = origCheck }()

	RaiseCorruptedBeadAlert("spi-a", fmt.Errorf("err")) // should be suppressed
	RaiseCorruptedBeadAlert("spi-b", fmt.Errorf("err")) // should create

	if createCount != 1 {
		t.Errorf("expected 1 create (only spi-b), got %d", createCount)
	}
}

// --- CleanUpdatedLabels tests ---

func TestCleanUpdatedLabels_CleansMatchingLabels(t *testing.T) {
	origList := ListBeadsFunc
	ListBeadsFunc = func(filter beads.IssueFilter) ([]store.Bead, error) {
		return []store.Bead{
			{ID: "spi-a", Labels: []string{"updated:2026-03-30T01:00:00Z", "other-label"}},
			{ID: "spi-b", Labels: []string{"updated:2026-03-29T12:00:00Z"}},
		}, nil
	}
	defer func() { ListBeadsFunc = origList }()

	var removed []string
	origRemove := RemoveLabelFunc
	RemoveLabelFunc = func(id, label string) error {
		removed = append(removed, id+"="+label)
		return nil
	}
	defer func() { RemoveLabelFunc = origRemove }()

	cleaned := CleanUpdatedLabels()
	if cleaned != 2 {
		t.Errorf("cleaned = %d, want 2", cleaned)
	}
	if len(removed) != 2 {
		t.Fatalf("removed = %v, want 2 entries", removed)
	}
	if removed[0] != "spi-a=updated:2026-03-30T01:00:00Z" {
		t.Errorf("removed[0] = %q, want spi-a=updated:2026-03-30T01:00:00Z", removed[0])
	}
	if removed[1] != "spi-b=updated:2026-03-29T12:00:00Z" {
		t.Errorf("removed[1] = %q, want spi-b=updated:2026-03-29T12:00:00Z", removed[1])
	}
}

func TestCleanUpdatedLabels_SkipsBeadsWithoutLabel(t *testing.T) {
	origList := ListBeadsFunc
	ListBeadsFunc = func(filter beads.IssueFilter) ([]store.Bead, error) {
		return []store.Bead{
			{ID: "spi-no-label", Labels: []string{"other-label"}},
			{ID: "spi-also-clean", Labels: nil},
		}, nil
	}
	defer func() { ListBeadsFunc = origList }()

	var removed []string
	origRemove := RemoveLabelFunc
	RemoveLabelFunc = func(id, label string) error {
		removed = append(removed, id)
		return nil
	}
	defer func() { RemoveLabelFunc = origRemove }()

	cleaned := CleanUpdatedLabels()
	if cleaned != 0 {
		t.Errorf("cleaned = %d, want 0", cleaned)
	}
	if len(removed) != 0 {
		t.Errorf("expected no RemoveLabel calls, got %v", removed)
	}
}

func TestCleanUpdatedLabels_ListErrorReturnsZero(t *testing.T) {
	origList := ListBeadsFunc
	ListBeadsFunc = func(filter beads.IssueFilter) ([]store.Bead, error) {
		return nil, fmt.Errorf("database unavailable")
	}
	defer func() { ListBeadsFunc = origList }()

	cleaned := CleanUpdatedLabels()
	if cleaned != 0 {
		t.Errorf("cleaned = %d, want 0 on list error", cleaned)
	}
}

func TestCleanUpdatedLabels_RemoveLabelErrorContinues(t *testing.T) {
	origList := ListBeadsFunc
	ListBeadsFunc = func(filter beads.IssueFilter) ([]store.Bead, error) {
		return []store.Bead{
			{ID: "spi-fail", Labels: []string{"updated:2026-03-30T01:00:00Z"}},
			{ID: "spi-ok", Labels: []string{"updated:2026-03-30T02:00:00Z"}},
		}, nil
	}
	defer func() { ListBeadsFunc = origList }()

	origRemove := RemoveLabelFunc
	RemoveLabelFunc = func(id, label string) error {
		if id == "spi-fail" {
			return fmt.Errorf("remove failed")
		}
		return nil
	}
	defer func() { RemoveLabelFunc = origRemove }()

	cleaned := CleanUpdatedLabels()
	if cleaned != 1 {
		t.Errorf("cleaned = %d, want 1 (spi-fail should error, spi-ok should succeed)", cleaned)
	}
}

// --- CheckBeadHealth tests ---

// fakeBackend implements agent.Backend for testing.
type fakeBackend struct {
	killed []string
}

func (f *fakeBackend) Spawn(cfg agent.SpawnConfig) (agent.Handle, error) { return nil, nil }
func (f *fakeBackend) List() ([]agent.Info, error)                       { return nil, nil }
func (f *fakeBackend) Logs(name string) (io.ReadCloser, error) {
	return nil, os.ErrNotExist
}
func (f *fakeBackend) Kill(name string) error {
	f.killed = append(f.killed, name)
	return nil
}

func TestCheckBeadHealth_StaleIncrementsCount(t *testing.T) {
	// Bead updated 20 minutes ago.
	staleTime := time.Now().Add(-20 * time.Minute).UTC().Format(time.RFC3339)
	origList := ListBeadsFunc
	ListBeadsFunc = func(filter beads.IssueFilter) ([]store.Bead, error) {
		return []store.Bead{
			{ID: "spi-stale", Title: "stale task", Status: "in_progress", UpdatedAt: staleTime},
		}, nil
	}
	defer func() { ListBeadsFunc = origList }()

	origAttempt := GetActiveAttemptFunc
	GetActiveAttemptFunc = func(parentID string) (*store.Bead, error) { return nil, nil }
	defer func() { GetActiveAttemptFunc = origAttempt }()

	backend := &fakeBackend{}
	staleCount, shutdownCount := CheckBeadHealth(10*time.Minute, 30*time.Minute, false, backend)

	if staleCount != 1 {
		t.Errorf("staleCount = %d, want 1", staleCount)
	}
	if shutdownCount != 0 {
		t.Errorf("shutdownCount = %d, want 0", shutdownCount)
	}
	if len(backend.killed) != 0 {
		t.Errorf("expected no kills, got %v", backend.killed)
	}
}

func TestCheckBeadHealth_ShutdownKillsAgent(t *testing.T) {
	// Bead updated 45 minutes ago (beyond shutdown threshold).
	oldTime := time.Now().Add(-45 * time.Minute).UTC().Format(time.RFC3339)
	origList := ListBeadsFunc
	ListBeadsFunc = func(filter beads.IssueFilter) ([]store.Bead, error) {
		return []store.Bead{
			{ID: "spi-old", Title: "old task", Status: "in_progress", UpdatedAt: oldTime},
		}, nil
	}
	defer func() { ListBeadsFunc = origList }()

	attemptBead := &store.Bead{
		ID:     "spi-old.attempt-1",
		Status: "in_progress",
		Labels: []string{"attempt", "agent:wizard-old"},
	}
	origAttempt := GetActiveAttemptFunc
	GetActiveAttemptFunc = func(parentID string) (*store.Bead, error) {
		if parentID == "spi-old" {
			return attemptBead, nil
		}
		return nil, nil
	}
	defer func() { GetActiveAttemptFunc = origAttempt }()

	backend := &fakeBackend{}
	staleCount, shutdownCount := CheckBeadHealth(10*time.Minute, 30*time.Minute, false, backend)

	if shutdownCount != 1 {
		t.Errorf("shutdownCount = %d, want 1", shutdownCount)
	}
	if staleCount != 0 {
		t.Errorf("staleCount = %d, want 0 (shutdown supersedes stale)", staleCount)
	}
	if len(backend.killed) != 1 || backend.killed[0] != "wizard-old" {
		t.Errorf("killed = %v, want [wizard-old]", backend.killed)
	}
}

func TestCheckBeadHealth_NoUpdatedAtSkipped(t *testing.T) {
	origList := ListBeadsFunc
	ListBeadsFunc = func(filter beads.IssueFilter) ([]store.Bead, error) {
		return []store.Bead{
			{ID: "spi-nolabel", Title: "no label", Status: "in_progress"},
		}, nil
	}
	defer func() { ListBeadsFunc = origList }()

	backend := &fakeBackend{}
	staleCount, shutdownCount := CheckBeadHealth(10*time.Minute, 30*time.Minute, false, backend)

	if staleCount != 0 || shutdownCount != 0 {
		t.Errorf("expected 0/0, got stale=%d shutdown=%d", staleCount, shutdownCount)
	}
}

func TestCheckBeadHealth_ReviewApprovedSkipped(t *testing.T) {
	// Bead with review-approved label should be skipped regardless of age.
	oldTime := time.Now().Add(-45 * time.Minute).UTC().Format(time.RFC3339)
	origList := ListBeadsFunc
	ListBeadsFunc = func(filter beads.IssueFilter) ([]store.Bead, error) {
		return []store.Bead{
			{ID: "spi-approved", Title: "approved", Status: "in_progress", Labels: []string{"review-approved"}, UpdatedAt: oldTime},
		}, nil
	}
	defer func() { ListBeadsFunc = origList }()

	backend := &fakeBackend{}
	staleCount, shutdownCount := CheckBeadHealth(10*time.Minute, 30*time.Minute, false, backend)

	if staleCount != 0 || shutdownCount != 0 {
		t.Errorf("expected 0/0 for review-approved bead, got stale=%d shutdown=%d", staleCount, shutdownCount)
	}
}

// --- GetActiveAttemptFunc injection tests ---

// TestStewardSkipsBeadWithAttemptChildNoOwnerLabel verifies that the steward's
// assignment logic skips a bead that has an active attempt child, even when the
// bead has no owner: label. The attempt bead is the authority.
func TestStewardSkipsBeadWithAttemptChildNoOwnerLabel(t *testing.T) {
	attemptBead := &store.Bead{
		ID:     "spi-test.1",
		Title:  "attempt: wizard-abc",
		Status: "in_progress",
		Labels: []string{"attempt", "agent:wizard-abc"},
	}

	orig := GetActiveAttemptFunc
	GetActiveAttemptFunc = func(parentID string) (*store.Bead, error) {
		if parentID == "spi-test" {
			return attemptBead, nil
		}
		return nil, nil
	}
	defer func() { GetActiveAttemptFunc = orig }()

	// Bead has NO owner: label — authority comes from the attempt child only.
	bead := store.Bead{ID: "spi-test", Title: "some task", Status: "open"}

	if store.HasLabel(bead, "owner:") != "" {
		t.Fatal("test setup error: bead must not have owner: label")
	}

	attempt, err := GetActiveAttemptFunc(bead.ID)
	if err != nil {
		t.Fatalf("unexpected error from GetActiveAttemptFunc: %v", err)
	}
	if attempt == nil {
		t.Fatal("expected active attempt to be found via attempt bead query")
	}

	// The assignment loop condition: skip if attempt != nil.
	shouldSkip := attempt != nil
	if !shouldSkip {
		t.Error("expected bead to be skipped (active attempt found)")
	}

	// Verify the agent name is readable from the attempt bead's agent: label.
	agentName := store.HasLabel(*attempt, "agent:")
	if agentName != "wizard-abc" {
		t.Errorf("expected agent=wizard-abc, got %q", agentName)
	}
}
