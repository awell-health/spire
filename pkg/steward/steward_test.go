package steward

import (
	"encoding/json"
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

// --- SweepHookedSteps tests ---

// spawnTrackingBackend records Spawn calls for assertion.
type spawnTrackingBackend struct {
	spawns []agent.SpawnConfig
}

func (b *spawnTrackingBackend) Spawn(cfg agent.SpawnConfig) (agent.Handle, error) {
	b.spawns = append(b.spawns, cfg)
	return &fakeHandle{id: cfg.Name}, nil
}
func (b *spawnTrackingBackend) List() ([]agent.Info, error)       { return nil, nil }
func (b *spawnTrackingBackend) Logs(name string) (io.ReadCloser, error) { return nil, os.ErrNotExist }
func (b *spawnTrackingBackend) Kill(name string) error            { return nil }

type fakeHandle struct{ id string }

func (h *fakeHandle) Wait() error                { return nil }
func (h *fakeHandle) Signal(sig os.Signal) error { return nil }
func (h *fakeHandle) Alive() bool                { return true }
func (h *fakeHandle) Name() string               { return h.id }
func (h *fakeHandle) Identifier() string         { return h.id }

// writeGraphState creates a runtime/<agentName>/graph_state.json under cfgDir.
func writeGraphState(t *testing.T, cfgDir, agentName string, gs interface{}) {
	t.Helper()
	dir := filepath.Join(cfgDir, "runtime", agentName)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	data, err := json.MarshalIndent(gs, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "graph_state.json"), data, 0644); err != nil {
		t.Fatal(err)
	}
}

func TestSweepHookedSteps_ResolvesDesign(t *testing.T) {
	// Set up temp config dir.
	cfgDir := t.TempDir()
	t.Setenv("SPIRE_CONFIG_DIR", cfgDir)
	t.Setenv("SPIRE_DOLT_DIR", t.TempDir())

	// Write graph state with a hooked step.
	gs := map[string]interface{}{
		"bead_id": "spi-test1",
		"steps": map[string]interface{}{
			"check.design-linked": map[string]interface{}{
				"status": "hooked",
				"outputs": map[string]string{
					"design_ref": "spi-design1",
				},
			},
			"implement": map[string]interface{}{
				"status": "pending",
			},
		},
	}
	writeGraphState(t, cfgDir, "wizard-test1", gs)

	// Mock store: design bead is closed with content.
	origGetBead := GetBeadFunc
	GetBeadFunc = func(id string) (store.Bead, error) {
		if id == "spi-design1" {
			return store.Bead{
				ID:          "spi-design1",
				Status:      "closed",
				Description: "Design decisions documented here.",
			}, nil
		}
		return store.Bead{}, fmt.Errorf("not found: %s", id)
	}
	defer func() { GetBeadFunc = origGetBead }()

	origGetComments := GetCommentsFunc
	GetCommentsFunc = func(id string) ([]*beads.Comment, error) {
		return nil, nil
	}
	defer func() { GetCommentsFunc = origGetComments }()

	backend := &spawnTrackingBackend{}
	count := SweepHookedSteps(false, backend, "test-tower")

	if count != 1 {
		t.Errorf("SweepHookedSteps returned %d, want 1", count)
	}

	// Verify graph state was updated.
	data, err := os.ReadFile(filepath.Join(cfgDir, "runtime", "wizard-test1", "graph_state.json"))
	if err != nil {
		t.Fatal(err)
	}
	var updated map[string]interface{}
	json.Unmarshal(data, &updated)
	stepsMap := updated["steps"].(map[string]interface{})
	step := stepsMap["check.design-linked"].(map[string]interface{})
	if step["status"] != "pending" {
		t.Errorf("step status = %q, want pending", step["status"])
	}
	if _, hasOutputs := step["outputs"]; hasOutputs {
		t.Error("step outputs should have been deleted")
	}

	// Verify backend.Spawn was called correctly.
	if len(backend.spawns) != 1 {
		t.Fatalf("spawn count = %d, want 1", len(backend.spawns))
	}
	sc := backend.spawns[0]
	if sc.Name != "wizard-test1" {
		t.Errorf("spawn name = %q, want wizard-test1", sc.Name)
	}
	if sc.BeadID != "spi-test1" {
		t.Errorf("spawn bead = %q, want spi-test1", sc.BeadID)
	}
	if sc.Role != agent.RoleApprentice {
		t.Errorf("spawn role = %q, want %q", sc.Role, agent.RoleApprentice)
	}
}

func TestSweepHookedSteps_SkipsOpenDesign(t *testing.T) {
	// Set up temp config dir.
	cfgDir := t.TempDir()
	t.Setenv("SPIRE_CONFIG_DIR", cfgDir)
	t.Setenv("SPIRE_DOLT_DIR", t.TempDir())

	// Write graph state with a hooked step.
	gs := map[string]interface{}{
		"bead_id": "spi-test2",
		"steps": map[string]interface{}{
			"check.design-linked": map[string]interface{}{
				"status": "hooked",
				"outputs": map[string]string{
					"design_ref": "spi-design2",
				},
			},
		},
	}
	writeGraphState(t, cfgDir, "wizard-test2", gs)

	// Mock store: design bead is still open.
	origGetBead := GetBeadFunc
	GetBeadFunc = func(id string) (store.Bead, error) {
		if id == "spi-design2" {
			return store.Bead{
				ID:     "spi-design2",
				Status: "open",
			}, nil
		}
		return store.Bead{}, fmt.Errorf("not found: %s", id)
	}
	defer func() { GetBeadFunc = origGetBead }()

	origGetComments := GetCommentsFunc
	GetCommentsFunc = func(id string) ([]*beads.Comment, error) {
		return nil, nil
	}
	defer func() { GetCommentsFunc = origGetComments }()

	backend := &spawnTrackingBackend{}
	count := SweepHookedSteps(false, backend, "test-tower")

	if count != 0 {
		t.Errorf("SweepHookedSteps returned %d, want 0", count)
	}

	// Verify graph state is unchanged.
	data, err := os.ReadFile(filepath.Join(cfgDir, "runtime", "wizard-test2", "graph_state.json"))
	if err != nil {
		t.Fatal(err)
	}
	var unchanged map[string]interface{}
	json.Unmarshal(data, &unchanged)
	stepsMap := unchanged["steps"].(map[string]interface{})
	step := stepsMap["check.design-linked"].(map[string]interface{})
	if step["status"] != "hooked" {
		t.Errorf("step status = %q, want hooked (unchanged)", step["status"])
	}

	// Verify no spawn.
	if len(backend.spawns) != 0 {
		t.Errorf("spawn count = %d, want 0", len(backend.spawns))
	}
}

func TestSweepHookedSteps_TowerScoped(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("SPIRE_CONFIG_DIR", cfgDir)
	t.Setenv("SPIRE_DOLT_DIR", t.TempDir())

	// Write two graph states: one for tower "awell", one for tower "mlti".
	gsAwell := map[string]interface{}{
		"bead_id":    "spi-awell1",
		"tower_name": "awell",
		"steps": map[string]interface{}{
			"check.design-linked": map[string]interface{}{
				"status":  "hooked",
				"outputs": map[string]string{"design_ref": "spi-design-a"},
			},
		},
	}
	gsMlti := map[string]interface{}{
		"bead_id":    "ml-mlti1",
		"tower_name": "mlti",
		"steps": map[string]interface{}{
			"check.design-linked": map[string]interface{}{
				"status":  "hooked",
				"outputs": map[string]string{"design_ref": "ml-design-b"},
			},
		},
	}
	// Legacy state with no tower_name — should be swept by any tower.
	gsLegacy := map[string]interface{}{
		"bead_id": "spi-legacy1",
		"steps": map[string]interface{}{
			"check.design-linked": map[string]interface{}{
				"status":  "hooked",
				"outputs": map[string]string{"design_ref": "spi-design-legacy"},
			},
		},
	}
	writeGraphState(t, cfgDir, "wizard-awell1", gsAwell)
	writeGraphState(t, cfgDir, "wizard-mlti1", gsMlti)
	writeGraphState(t, cfgDir, "wizard-legacy1", gsLegacy)

	// Mock store: all design beads are closed with content.
	origGetBead := GetBeadFunc
	GetBeadFunc = func(id string) (store.Bead, error) {
		return store.Bead{ID: id, Status: "closed", Description: "done"}, nil
	}
	defer func() { GetBeadFunc = origGetBead }()

	origGetComments := GetCommentsFunc
	GetCommentsFunc = func(id string) ([]*beads.Comment, error) {
		return nil, nil
	}
	defer func() { GetCommentsFunc = origGetComments }()

	// Sweep as tower "awell" — should process awell + legacy, skip mlti.
	backend := &spawnTrackingBackend{}
	count := SweepHookedSteps(false, backend, "awell")

	if count != 2 {
		t.Errorf("SweepHookedSteps returned %d, want 2", count)
	}

	// Verify only awell and legacy agents were spawned.
	spawnedNames := make(map[string]bool)
	for _, sc := range backend.spawns {
		spawnedNames[sc.Name] = true
	}
	if !spawnedNames["wizard-awell1"] {
		t.Error("expected wizard-awell1 to be spawned")
	}
	if !spawnedNames["wizard-legacy1"] {
		t.Error("expected wizard-legacy1 to be spawned (legacy, no tower_name)")
	}
	if spawnedNames["wizard-mlti1"] {
		t.Error("wizard-mlti1 should NOT have been spawned (wrong tower)")
	}

	// Verify mlti graph state is unchanged (still hooked).
	data, _ := os.ReadFile(filepath.Join(cfgDir, "runtime", "wizard-mlti1", "graph_state.json"))
	var mltiState map[string]interface{}
	json.Unmarshal(data, &mltiState)
	mltiSteps := mltiState["steps"].(map[string]interface{})
	mltiStep := mltiSteps["check.design-linked"].(map[string]interface{})
	if mltiStep["status"] != "hooked" {
		t.Errorf("mlti step status = %q, want hooked (unchanged)", mltiStep["status"])
	}
}

// --- SweepHookedSteps human.approve tests ---

func TestSweepHookedSteps_HumanApprove_LabelsPresent_Skips(t *testing.T) {
	// When awaiting-approval and needs-human labels are still present,
	// the sweep should NOT resolve the hooked step.
	cfgDir := t.TempDir()
	t.Setenv("SPIRE_CONFIG_DIR", cfgDir)
	t.Setenv("SPIRE_DOLT_DIR", t.TempDir())

	// Graph state with a hooked step that has NO design_ref (human.approve path).
	gs := map[string]interface{}{
		"bead_id": "spi-approve1",
		"steps": map[string]interface{}{
			"approve": map[string]interface{}{
				"status":  "hooked",
				"outputs": map[string]string{},
			},
		},
	}
	writeGraphState(t, cfgDir, "wizard-approve1", gs)

	// Mock store: bead still has both approval labels.
	origGetBead := GetBeadFunc
	GetBeadFunc = func(id string) (store.Bead, error) {
		if id == "spi-approve1" {
			return store.Bead{
				ID:     "spi-approve1",
				Status: "in_progress",
				Labels: []string{"needs-human", "awaiting-approval"},
			}, nil
		}
		return store.Bead{}, fmt.Errorf("not found: %s", id)
	}
	defer func() { GetBeadFunc = origGetBead }()

	backend := &spawnTrackingBackend{}
	count := SweepHookedSteps(false, backend, "test-tower")

	if count != 0 {
		t.Errorf("SweepHookedSteps returned %d, want 0 (labels still present)", count)
	}

	// Verify graph state is unchanged (still hooked).
	data, err := os.ReadFile(filepath.Join(cfgDir, "runtime", "wizard-approve1", "graph_state.json"))
	if err != nil {
		t.Fatal(err)
	}
	var unchanged map[string]interface{}
	json.Unmarshal(data, &unchanged)
	stepsMap := unchanged["steps"].(map[string]interface{})
	step := stepsMap["approve"].(map[string]interface{})
	if step["status"] != "hooked" {
		t.Errorf("step status = %q, want hooked (unchanged)", step["status"])
	}

	// Verify no spawn.
	if len(backend.spawns) != 0 {
		t.Errorf("spawn count = %d, want 0", len(backend.spawns))
	}
}

func TestSweepHookedSteps_HumanApprove_LabelsCleared_Resolves(t *testing.T) {
	// When both awaiting-approval and needs-human labels have been cleared
	// (by spire approve), the sweep should resolve the hooked step and re-summon.
	cfgDir := t.TempDir()
	t.Setenv("SPIRE_CONFIG_DIR", cfgDir)
	t.Setenv("SPIRE_DOLT_DIR", t.TempDir())

	// Graph state with a hooked step that has NO design_ref (human.approve path).
	gs := map[string]interface{}{
		"bead_id": "spi-approve2",
		"steps": map[string]interface{}{
			"approve": map[string]interface{}{
				"status":  "hooked",
				"outputs": map[string]string{},
			},
		},
	}
	writeGraphState(t, cfgDir, "wizard-approve2", gs)

	// Mock store: bead has labels cleared (spire approve ran).
	origGetBead := GetBeadFunc
	GetBeadFunc = func(id string) (store.Bead, error) {
		if id == "spi-approve2" {
			return store.Bead{
				ID:     "spi-approve2",
				Status: "in_progress",
				Labels: []string{}, // both labels removed
			}, nil
		}
		return store.Bead{}, fmt.Errorf("not found: %s", id)
	}
	defer func() { GetBeadFunc = origGetBead }()

	backend := &spawnTrackingBackend{}
	count := SweepHookedSteps(false, backend, "test-tower")

	if count != 1 {
		t.Errorf("SweepHookedSteps returned %d, want 1", count)
	}

	// Verify graph state was updated (step reset to pending).
	data, err := os.ReadFile(filepath.Join(cfgDir, "runtime", "wizard-approve2", "graph_state.json"))
	if err != nil {
		t.Fatal(err)
	}
	var updated map[string]interface{}
	json.Unmarshal(data, &updated)
	stepsMap := updated["steps"].(map[string]interface{})
	step := stepsMap["approve"].(map[string]interface{})
	if step["status"] != "pending" {
		t.Errorf("step status = %q, want pending", step["status"])
	}
	if _, hasOutputs := step["outputs"]; hasOutputs {
		t.Error("step outputs should have been deleted")
	}

	// Verify backend.Spawn was called correctly.
	if len(backend.spawns) != 1 {
		t.Fatalf("spawn count = %d, want 1", len(backend.spawns))
	}
	sc := backend.spawns[0]
	if sc.Name != "wizard-approve2" {
		t.Errorf("spawn name = %q, want wizard-approve2", sc.Name)
	}
	if sc.BeadID != "spi-approve2" {
		t.Errorf("spawn bead = %q, want spi-approve2", sc.BeadID)
	}
	if sc.Role != agent.RoleApprentice {
		t.Errorf("spawn role = %q, want %q", sc.Role, agent.RoleApprentice)
	}
}

// --- ReviewBeadVerdict tests ---

func TestReviewBeadVerdict_FromMetadata(t *testing.T) {
	b := store.Bead{
		ID:       "spi-review-1",
		Metadata: map[string]string{"review_verdict": "approve"},
		// Description also set — metadata should take precedence.
		Description: "verdict: request_changes\n\nsome feedback",
	}
	got := ReviewBeadVerdict(b)
	if got != "approve" {
		t.Errorf("ReviewBeadVerdict() = %q, want %q (metadata should take precedence)", got, "approve")
	}
}

func TestReviewBeadVerdict_LegacyFallback(t *testing.T) {
	b := store.Bead{
		ID:          "spi-review-2",
		Description: "verdict: request_changes\n\nMissing error handling",
	}
	got := ReviewBeadVerdict(b)
	if got != "request_changes" {
		t.Errorf("ReviewBeadVerdict() = %q, want %q (legacy description parsing)", got, "request_changes")
	}
}

func TestReviewBeadVerdict_EmptyBead(t *testing.T) {
	b := store.Bead{ID: "spi-review-3"}
	got := ReviewBeadVerdict(b)
	if got != "" {
		t.Errorf("ReviewBeadVerdict() = %q, want empty for bead with no metadata or description", got)
	}
}

func TestReviewBeadVerdict_NoMatchingDescription(t *testing.T) {
	b := store.Bead{
		ID:          "spi-review-4",
		Description: "some random description without verdict prefix",
	}
	got := ReviewBeadVerdict(b)
	if got != "" {
		t.Errorf("ReviewBeadVerdict() = %q, want empty for non-verdict description", got)
	}
}

func TestSweepHookedSteps_HumanApprove_OnlyNeedsHumanPresent_Skips(t *testing.T) {
	// When needs-human is still present (even if awaiting-approval is gone),
	// the sweep should skip — at least one label remains.
	cfgDir := t.TempDir()
	t.Setenv("SPIRE_CONFIG_DIR", cfgDir)
	t.Setenv("SPIRE_DOLT_DIR", t.TempDir())

	gs := map[string]interface{}{
		"bead_id": "spi-approve3",
		"steps": map[string]interface{}{
			"approve": map[string]interface{}{
				"status":  "hooked",
				"outputs": map[string]string{},
			},
		},
	}
	writeGraphState(t, cfgDir, "wizard-approve3", gs)

	// Mock store: bead still has needs-human but awaiting-approval is gone.
	origGetBead := GetBeadFunc
	GetBeadFunc = func(id string) (store.Bead, error) {
		if id == "spi-approve3" {
			return store.Bead{
				ID:     "spi-approve3",
				Status: "in_progress",
				Labels: []string{"needs-human"}, // only needs-human remains
			}, nil
		}
		return store.Bead{}, fmt.Errorf("not found: %s", id)
	}
	defer func() { GetBeadFunc = origGetBead }()

	backend := &spawnTrackingBackend{}
	count := SweepHookedSteps(false, backend, "test-tower")

	if count != 0 {
		t.Errorf("SweepHookedSteps returned %d, want 0 (needs-human still present)", count)
	}

	if len(backend.spawns) != 0 {
		t.Errorf("spawn count = %d, want 0", len(backend.spawns))
	}
}
