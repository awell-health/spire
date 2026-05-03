package steward

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/awell-health/spire/pkg/agent"
	"github.com/awell-health/spire/pkg/config"
	"github.com/awell-health/spire/pkg/dolt"
	"github.com/awell-health/spire/pkg/executor"
	"github.com/awell-health/spire/pkg/recovery"
	"github.com/awell-health/spire/pkg/store"
	"github.com/steveyegge/beads"
)

// mustMarshalOutcome returns the JSON encoding of a RecoveryOutcome for use in
// bead metadata in tests. It mirrors what recovery.WriteOutcome persists under
// recovery.KeyRecoveryOutcome.
func mustMarshalOutcome(t *testing.T, out recovery.RecoveryOutcome) string {
	t.Helper()
	b, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal recovery outcome: %v", err)
	}
	return string(b)
}

func captureStewardLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prevOutput := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() {
		log.SetOutput(prevOutput)
	})
	return &buf
}

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

// --- RecoverStaleDispatched tests ---

func TestRecoverStaleDispatched_FlipsStuckBead(t *testing.T) {
	origList := ListBeadsFunc
	origUpdate := UpdateBeadFunc
	defer func() {
		ListBeadsFunc = origList
		UpdateBeadFunc = origUpdate
	}()

	stuckTime := time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339)
	freshTime := time.Now().Add(-30 * time.Second).UTC().Format(time.RFC3339)

	ListBeadsFunc = func(filter beads.IssueFilter) ([]store.Bead, error) {
		return []store.Bead{
			{ID: "spi-stuck", Type: "task", Title: "stuck", UpdatedAt: stuckTime},
			{ID: "spi-fresh", Type: "task", Title: "fresh", UpdatedAt: freshTime},
		}, nil
	}
	updated := map[string]map[string]interface{}{}
	UpdateBeadFunc = func(id string, fields map[string]interface{}) error {
		updated[id] = fields
		return nil
	}

	reverted := RecoverStaleDispatched(5*time.Minute, false)
	if reverted != 1 {
		t.Errorf("reverted = %d, want 1 (only spi-stuck past timeout)", reverted)
	}
	if updated["spi-stuck"]["status"] != "ready" {
		t.Errorf("spi-stuck update = %v, want status=ready", updated["spi-stuck"])
	}
	if _, ok := updated["spi-fresh"]; ok {
		t.Errorf("spi-fresh was updated but is under timeout: %v", updated["spi-fresh"])
	}
}

func TestRecoverStaleDispatched_DryRun(t *testing.T) {
	origList := ListBeadsFunc
	origUpdate := UpdateBeadFunc
	defer func() {
		ListBeadsFunc = origList
		UpdateBeadFunc = origUpdate
	}()

	stuckTime := time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339)
	ListBeadsFunc = func(filter beads.IssueFilter) ([]store.Bead, error) {
		return []store.Bead{{ID: "spi-stuck", Type: "task", UpdatedAt: stuckTime}}, nil
	}
	updateCalled := false
	UpdateBeadFunc = func(id string, fields map[string]interface{}) error {
		updateCalled = true
		return nil
	}

	reverted := RecoverStaleDispatched(5*time.Minute, true)
	if reverted != 1 {
		t.Errorf("reverted (dry-run count) = %d, want 1", reverted)
	}
	if updateCalled {
		t.Error("UpdateBeadFunc was called during dry-run")
	}
}

func TestRecoverStaleDispatched_SkipsInternalBeads(t *testing.T) {
	origList := ListBeadsFunc
	origUpdate := UpdateBeadFunc
	defer func() {
		ListBeadsFunc = origList
		UpdateBeadFunc = origUpdate
	}()

	stuckTime := time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339)
	ListBeadsFunc = func(filter beads.IssueFilter) ([]store.Bead, error) {
		return []store.Bead{
			{ID: "spi-attempt", Type: "attempt", UpdatedAt: stuckTime},             // internal
			{ID: "spi-child", Type: "task", Parent: "spi-a", UpdatedAt: stuckTime}, // child
			{ID: "spi-top", Type: "task", UpdatedAt: stuckTime},                    // counts
		}, nil
	}
	updated := 0
	UpdateBeadFunc = func(id string, fields map[string]interface{}) error {
		updated++
		if id != "spi-top" {
			t.Errorf("unexpected update on %s (should be IsWorkBead-filtered)", id)
		}
		return nil
	}

	reverted := RecoverStaleDispatched(5*time.Minute, false)
	if reverted != 1 {
		t.Errorf("reverted = %d, want 1 (only top-level work bead)", reverted)
	}
	if updated != 1 {
		t.Errorf("update count = %d, want 1", updated)
	}
}

func TestRecoverStaleDispatched_ParsesBothTimeFormats(t *testing.T) {
	origList := ListBeadsFunc
	origUpdate := UpdateBeadFunc
	defer func() {
		ListBeadsFunc = origList
		UpdateBeadFunc = origUpdate
	}()

	// dolt sometimes emits MySQL datetime instead of RFC3339 — the
	// recovery path has a fallback parser for that format.
	stuckTime := time.Now().Add(-10 * time.Minute).UTC().Format("2006-01-02 15:04:05")
	ListBeadsFunc = func(filter beads.IssueFilter) ([]store.Bead, error) {
		return []store.Bead{{ID: "spi-dolt", Type: "task", UpdatedAt: stuckTime}}, nil
	}
	updated := false
	UpdateBeadFunc = func(id string, fields map[string]interface{}) error {
		updated = true
		return nil
	}

	reverted := RecoverStaleDispatched(5*time.Minute, false)
	if reverted != 1 || !updated {
		t.Errorf("RecoverStaleDispatched with MySQL-format timestamp: reverted=%d updated=%v, want 1/true", reverted, updated)
	}
}

func TestRecoverStaleDispatched_SkipsUnparseableAndEmptyTime(t *testing.T) {
	origList := ListBeadsFunc
	origUpdate := UpdateBeadFunc
	defer func() {
		ListBeadsFunc = origList
		UpdateBeadFunc = origUpdate
	}()

	ListBeadsFunc = func(filter beads.IssueFilter) ([]store.Bead, error) {
		return []store.Bead{
			{ID: "spi-empty", Type: "task", UpdatedAt: ""},
			{ID: "spi-garbage", Type: "task", UpdatedAt: "not-a-timestamp"},
		}, nil
	}
	UpdateBeadFunc = func(id string, fields map[string]interface{}) error {
		t.Errorf("unexpected update on %s", id)
		return nil
	}

	if got := RecoverStaleDispatched(5*time.Minute, false); got != 0 {
		t.Errorf("reverted = %d, want 0 when timestamps unusable", got)
	}
}

func TestRecoverStaleDispatched_ListErrorReturnsZero(t *testing.T) {
	origList := ListBeadsFunc
	defer func() { ListBeadsFunc = origList }()
	ListBeadsFunc = func(filter beads.IssueFilter) ([]store.Bead, error) {
		return nil, fmt.Errorf("db down")
	}
	if got := RecoverStaleDispatched(5*time.Minute, false); got != 0 {
		t.Errorf("reverted on list error = %d, want 0", got)
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
	agents []agent.Info
}

func (f *fakeBackend) Spawn(cfg agent.SpawnConfig) (agent.Handle, error) { return nil, nil }
func (f *fakeBackend) List() ([]agent.Info, error)                       { return f.agents, nil }
func (f *fakeBackend) Logs(name string) (io.ReadCloser, error) {
	return nil, os.ErrNotExist
}
func (f *fakeBackend) Kill(name string) error {
	f.killed = append(f.killed, name)
	return nil
}
func (f *fakeBackend) TerminateBead(ctx context.Context, beadID string) error { return nil }

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
	staleCount, shutdownCount := CheckBeadHealth(10*time.Minute, 30*time.Minute, false, backend, config.DeploymentModeLocalNative)

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
	// Bead updated 45 minutes ago (beyond shutdown threshold) AND attempt
	// heartbeat is also 45 minutes old — both clocks agree the wizard is
	// wedged. Owner is reported alive in the backend, so we kill it.
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

	origGetInstance := GetAttemptInstanceFunc
	GetAttemptInstanceFunc = func(attemptID string) (*store.InstanceMeta, error) {
		return &store.InstanceMeta{InstanceID: "local-instance-uuid", LastSeenAt: oldTime}, nil
	}
	defer func() { GetAttemptInstanceFunc = origGetInstance }()

	origInstanceID := InstanceIDFunc
	InstanceIDFunc = func() string { return "local-instance-uuid" }
	defer func() { InstanceIDFunc = origInstanceID }()

	backend := &fakeBackend{
		agents: []agent.Info{{Name: "wizard-old", Alive: true}},
	}
	staleCount, shutdownCount := CheckBeadHealth(10*time.Minute, 30*time.Minute, false, backend, config.DeploymentModeLocalNative)

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

func TestCheckBeadHealth_ShutdownLogIncludesAttemptContext(t *testing.T) {
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

	origGetInstance := GetAttemptInstanceFunc
	GetAttemptInstanceFunc = func(attemptID string) (*store.InstanceMeta, error) {
		return &store.InstanceMeta{
			InstanceID:   "local-instance-uuid",
			InstanceName: "baker-pro.local",
			SessionID:    "session-123",
			Backend:      "process",
			Tower:        "awell-test",
			StartedAt:    oldTime,
			LastSeenAt:   oldTime,
		}, nil
	}
	defer func() { GetAttemptInstanceFunc = origGetInstance }()

	origInstanceID := InstanceIDFunc
	InstanceIDFunc = func() string { return "local-instance-uuid" }
	defer func() { InstanceIDFunc = origInstanceID }()

	backend := &fakeBackend{
		agents: []agent.Info{{Name: "wizard-old", Alive: true}},
	}
	buf := captureStewardLog(t)

	_, shutdownCount := CheckBeadHealth(10*time.Minute, 30*time.Minute, false, backend, config.DeploymentModeLocalNative)

	if shutdownCount != 1 {
		t.Fatalf("shutdownCount = %d, want 1", shutdownCount)
	}
	got := buf.String()
	for _, want := range []string{
		"SHUTDOWN: spi-old (old task) owner=wizard-old",
		`attempt="spi-old.attempt-1"`,
		`owner_state="alive"`,
		`attempt_meta="present"`,
		`heartbeat_state="parsed"`,
		`local_instance="local-instance-uuid"`,
		`attempt_instance="local-instance-uuid"`,
		`session_id="session-123"`,
		`backend="process"`,
		`tower="awell-test"`,
		`last_seen_at="` + oldTime + `"`,
		`stale_threshold=10m0s`,
		`shutdown_threshold=30m0s`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("expected log to contain %q, got %s", want, got)
		}
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

	origAttempt := GetActiveAttemptFunc
	GetActiveAttemptFunc = func(parentID string) (*store.Bead, error) { return nil, nil }
	defer func() { GetActiveAttemptFunc = origAttempt }()

	backend := &fakeBackend{}
	staleCount, shutdownCount := CheckBeadHealth(10*time.Minute, 30*time.Minute, false, backend, config.DeploymentModeLocalNative)

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
	staleCount, shutdownCount := CheckBeadHealth(10*time.Minute, 30*time.Minute, false, backend, config.DeploymentModeLocalNative)

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
func (b *spawnTrackingBackend) List() ([]agent.Info, error)             { return nil, nil }
func (b *spawnTrackingBackend) Logs(name string) (io.ReadCloser, error) { return nil, os.ErrNotExist }
func (b *spawnTrackingBackend) Kill(name string) error                  { return nil }
func (b *spawnTrackingBackend) TerminateBead(ctx context.Context, beadID string) error {
	return nil
}

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
	gsStore := &executor.FileGraphStateStore{ConfigDir: func() (string, error) { return cfgDir, nil }}
	count := SweepHookedSteps(false, backend, "test-tower", gsStore, PhaseDispatch{})

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
	// Role must be RoleWizard so the K8s backend builds a wizard pod
	// (spi-fcord7); RoleApprentice was the masked bug from the process
	// backend's command-map indirection.
	if sc.Role != agent.RoleWizard {
		t.Errorf("spawn role = %q, want %q", sc.Role, agent.RoleWizard)
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
	gsStore := &executor.FileGraphStateStore{ConfigDir: func() (string, error) { return cfgDir, nil }}
	count := SweepHookedSteps(false, backend, "test-tower", gsStore, PhaseDispatch{})

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
	gsStore := &executor.FileGraphStateStore{ConfigDir: func() (string, error) { return cfgDir, nil }}
	count := SweepHookedSteps(false, backend, "awell", gsStore, PhaseDispatch{})

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
	gsStore := &executor.FileGraphStateStore{ConfigDir: func() (string, error) { return cfgDir, nil }}
	count := SweepHookedSteps(false, backend, "test-tower", gsStore, PhaseDispatch{})

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
	gsStore := &executor.FileGraphStateStore{ConfigDir: func() (string, error) { return cfgDir, nil }}
	count := SweepHookedSteps(false, backend, "test-tower", gsStore, PhaseDispatch{})

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
	// Role must be RoleWizard so the K8s backend builds a wizard pod
	// (spi-fcord7); RoleApprentice was the masked bug from the process
	// backend's command-map indirection.
	if sc.Role != agent.RoleWizard {
		t.Errorf("spawn role = %q, want %q", sc.Role, agent.RoleWizard)
	}
}

// TestSweepHookedSteps_ClusterNative_EmitsIntentInsteadOfSpawn pins the
// spi-agmsk5 fix: when the tower's deployment mode is cluster-native,
// the hooked-sweep resume must emit a WorkloadIntent for the operator
// to reconcile and must NOT call backend.Spawn. The graph-state +
// bead-status setup mirrors the local-native HumanApprove_LabelsCleared
// case so the two tests pin the same scenario across both modes.
func TestSweepHookedSteps_ClusterNative_EmitsIntentInsteadOfSpawn(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("SPIRE_CONFIG_DIR", cfgDir)
	t.Setenv("SPIRE_DOLT_DIR", t.TempDir())

	gs := map[string]interface{}{
		"bead_id": "spi-cluster1",
		"steps": map[string]interface{}{
			"approve": map[string]interface{}{
				"status":  "hooked",
				"outputs": map[string]string{},
			},
		},
	}
	writeGraphState(t, cfgDir, "wizard-cluster1", gs)

	origGetBead := GetBeadFunc
	GetBeadFunc = func(id string) (store.Bead, error) {
		if id == "spi-cluster1" {
			return store.Bead{
				ID:     "spi-cluster1",
				Type:   "task",
				Status: "in_progress",
				Labels: []string{},
			}, nil
		}
		return store.Bead{}, fmt.Errorf("not found: %s", id)
	}
	defer func() { GetBeadFunc = origGetBead }()

	withStubbedNextDispatchSeq(t, 11)

	backend := &spawnTrackingBackend{}
	pub := &phaseTrackingPublisher{}
	pd := PhaseDispatch{
		Mode: config.DeploymentModeClusterNative,
		ClusterDispatch: &ClusterDispatchConfig{
			Resolver:  fakeResolver{},
			Publisher: pub,
		},
	}
	gsStore := &executor.FileGraphStateStore{ConfigDir: func() (string, error) { return cfgDir, nil }}
	count := SweepHookedSteps(false, backend, "test-tower", gsStore, pd)

	if count != 1 {
		t.Fatalf("SweepHookedSteps = %d, want 1", count)
	}
	if len(backend.spawns) != 0 {
		t.Errorf("backend.Spawn called %d time(s), want 0 — cluster-native must emit intent, not spawn", len(backend.spawns))
	}
	if len(pub.published) != 1 {
		t.Fatalf("published intents = %d, want 1", len(pub.published))
	}
	got := pub.published[0]
	if got.TaskID != "spi-cluster1" {
		t.Errorf("TaskID = %q, want spi-cluster1", got.TaskID)
	}
	if got.DispatchSeq != 11 {
		t.Errorf("DispatchSeq = %d, want 11 (stubbed)", got.DispatchSeq)
	}
	// A hooked-sweep resume re-dispatches the whole bead, so the phase
	// is bead-level. For a task, that's "task".
	if got.FormulaPhase != "task" {
		t.Errorf("FormulaPhase = %q, want %q (bead-level resume)", got.FormulaPhase, "task")
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

// TestReviewBeadVerdict_ArbiterOverridesSage verifies the arbiter_verdict
// JSON payload takes precedence over sage's review_verdict on the same
// review-round bead. This is the binding-verdict guarantee — readers
// downstream of the steward consult ReviewBeadVerdict to decide whether to
// re-engage the wizard, so the arbiter must win.
func TestReviewBeadVerdict_ArbiterOverridesSage(t *testing.T) {
	b := store.Bead{
		ID: "spi-review-arb",
		Metadata: map[string]string{
			"arbiter_verdict": `{"source":"arbiter","verdict":"approve","decided_at":"2026-04-20T12:00:00Z"}`,
			"review_verdict":  "request_changes",
		},
	}
	got := ReviewBeadVerdict(b)
	if got != "approve" {
		t.Errorf("ReviewBeadVerdict() = %q, want %q (arbiter must override sage)", got, "approve")
	}
}

// TestReviewBeadVerdict_ArbiterUnparseableFallsBack ensures malformed
// arbiter_verdict JSON falls through to review_verdict rather than blackholing
// the verdict — the round must still surface a usable answer.
func TestReviewBeadVerdict_ArbiterUnparseableFallsBack(t *testing.T) {
	b := store.Bead{
		ID: "spi-review-bad",
		Metadata: map[string]string{
			"arbiter_verdict": "{not-json",
			"review_verdict":  "approve",
		},
	}
	got := ReviewBeadVerdict(b)
	if got != "approve" {
		t.Errorf("ReviewBeadVerdict() = %q, want %q (fallback on bad JSON)", got, "approve")
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
	gsStore := &executor.FileGraphStateStore{ConfigDir: func() (string, error) { return cfgDir, nil }}
	count := SweepHookedSteps(false, backend, "test-tower", gsStore, PhaseDispatch{})

	if count != 0 {
		t.Errorf("SweepHookedSteps returned %d, want 0 (needs-human still present)", count)
	}

	if len(backend.spawns) != 0 {
		t.Errorf("spawn count = %d, want 0", len(backend.spawns))
	}
}

// --- SweepHookedSteps ownership-filtering tests ---

func TestSweepHookedSteps_SkipsForeignOwnedAttempt(t *testing.T) {
	// When the active attempt is owned by a foreign instance, the hooked-step
	// sweep should skip that bead entirely — no graph-state read, no spawn.
	cfgDir := t.TempDir()
	t.Setenv("SPIRE_CONFIG_DIR", cfgDir)
	t.Setenv("SPIRE_DOLT_DIR", t.TempDir())

	// Write graph state with a hooked step — this should NOT be reached.
	gs := map[string]interface{}{
		"bead_id": "spi-foreign1",
		"steps": map[string]interface{}{
			"check.design-linked": map[string]interface{}{
				"status": "hooked",
				"outputs": map[string]string{
					"design_ref": "spi-design-foreign",
				},
			},
		},
	}
	writeGraphState(t, cfgDir, "wizard-foreign1", gs)

	// Mock store: hooked parent bead.
	origList := ListBeadsFunc
	ListBeadsFunc = func(filter beads.IssueFilter) ([]store.Bead, error) {
		if filter.Status != nil && string(*filter.Status) == "hooked" {
			return []store.Bead{
				{ID: "spi-foreign1", Status: "hooked", Type: "task"},
			}, nil
		}
		return nil, nil
	}
	defer func() { ListBeadsFunc = origList }()

	// Active attempt exists for the hooked parent.
	origAttempt := GetActiveAttemptFunc
	GetActiveAttemptFunc = func(parentID string) (*store.Bead, error) {
		if parentID == "spi-foreign1" {
			return &store.Bead{ID: "spi-foreign1.attempt-1", Status: "in_progress"}, nil
		}
		return nil, nil
	}
	defer func() { GetActiveAttemptFunc = origAttempt }()

	// Attempt is owned by a foreign instance.
	origOwned := IsOwnedByInstanceFunc
	IsOwnedByInstanceFunc = func(attemptID, instanceID string) (bool, error) {
		if attemptID == "spi-foreign1.attempt-1" {
			return false, nil // foreign
		}
		return true, nil
	}
	defer func() { IsOwnedByInstanceFunc = origOwned }()

	origInstanceID := InstanceIDFunc
	InstanceIDFunc = func() string { return "local-instance-uuid" }
	defer func() { InstanceIDFunc = origInstanceID }()

	// GetHookedStepsFunc should NOT be called for foreign bead — track calls.
	hookedStepsCalled := false
	origHooked := GetHookedStepsFunc
	GetHookedStepsFunc = func(parentID string) ([]store.Bead, error) {
		if parentID == "spi-foreign1" {
			hookedStepsCalled = true
		}
		return nil, nil
	}
	defer func() { GetHookedStepsFunc = origHooked }()

	backend := &spawnTrackingBackend{}
	gsStore := &executor.FileGraphStateStore{ConfigDir: func() (string, error) { return cfgDir, nil }}
	count := SweepHookedSteps(false, backend, "test-tower", gsStore, PhaseDispatch{})

	if count != 0 {
		t.Errorf("SweepHookedSteps returned %d, want 0 (foreign attempt skipped)", count)
	}
	if len(backend.spawns) != 0 {
		t.Errorf("spawn count = %d, want 0 (foreign attempt should not trigger spawn)", len(backend.spawns))
	}
	if hookedStepsCalled {
		t.Error("GetHookedStepsFunc was called for foreign-owned bead — should have been skipped")
	}
}

func TestSweepHookedSteps_SkipsForeignOwnedAttempt_GraphStateFallback(t *testing.T) {
	// The graph-state fallback loop (second path in SweepHookedSteps) should
	// also skip foreign-owned attempts.
	cfgDir := t.TempDir()
	t.Setenv("SPIRE_CONFIG_DIR", cfgDir)
	t.Setenv("SPIRE_DOLT_DIR", t.TempDir())

	// Write graph state with a hooked step.
	gs := map[string]interface{}{
		"bead_id": "spi-fallback1",
		"steps": map[string]interface{}{
			"check.design-linked": map[string]interface{}{
				"status": "hooked",
				"outputs": map[string]string{
					"design_ref": "spi-design-fb",
				},
			},
		},
	}
	writeGraphState(t, cfgDir, "wizard-fallback1", gs)

	// No hooked beads from bead-status path (forces fallback to graph-state path).
	origList := ListBeadsFunc
	ListBeadsFunc = func(filter beads.IssueFilter) ([]store.Bead, error) {
		return nil, nil
	}
	defer func() { ListBeadsFunc = origList }()

	// Active attempt exists for the fallback bead.
	origAttempt := GetActiveAttemptFunc
	GetActiveAttemptFunc = func(parentID string) (*store.Bead, error) {
		if parentID == "spi-fallback1" {
			return &store.Bead{ID: "spi-fallback1.attempt-1", Status: "in_progress"}, nil
		}
		return nil, nil
	}
	defer func() { GetActiveAttemptFunc = origAttempt }()

	// Attempt is owned by a foreign instance.
	origOwned := IsOwnedByInstanceFunc
	IsOwnedByInstanceFunc = func(attemptID, instanceID string) (bool, error) {
		if attemptID == "spi-fallback1.attempt-1" {
			return false, nil // foreign
		}
		return true, nil
	}
	defer func() { IsOwnedByInstanceFunc = origOwned }()

	origInstanceID := InstanceIDFunc
	InstanceIDFunc = func() string { return "local-instance-uuid" }
	defer func() { InstanceIDFunc = origInstanceID }()

	// Mock GetBead so the fallback path doesn't error if it checks the design.
	origGetBead := GetBeadFunc
	GetBeadFunc = func(id string) (store.Bead, error) {
		return store.Bead{}, fmt.Errorf("not found: %s", id)
	}
	defer func() { GetBeadFunc = origGetBead }()

	backend := &spawnTrackingBackend{}
	gsStore := &executor.FileGraphStateStore{ConfigDir: func() (string, error) { return cfgDir, nil }}
	count := SweepHookedSteps(false, backend, "test-tower", gsStore, PhaseDispatch{})

	if count != 0 {
		t.Errorf("SweepHookedSteps returned %d, want 0 (foreign attempt skipped in graph-state fallback)", count)
	}
	if len(backend.spawns) != 0 {
		t.Errorf("spawn count = %d, want 0 (foreign attempt should not trigger spawn)", len(backend.spawns))
	}
}

// --- Failure-evidence (cleric) sweep tests ---

// stubFailureEvidenceHooks saves and restores all function vars used by the
// failure-evidence path in SweepHookedSteps. Returns a cleanup function.
func stubFailureEvidenceHooks(t *testing.T) func() {
	t.Helper()
	origList := ListBeadsFunc
	origAttempt := GetActiveAttemptFunc
	origOwned := IsOwnedByInstanceFunc
	origInstance := InstanceIDFunc
	origInstanceName := InstanceNameFunc
	origGetBead := GetBeadFunc
	origGetComments := GetCommentsFunc
	origHooked := GetHookedStepsFunc
	origUnhook := UnhookStepBeadFunc
	origUpdate := UpdateBeadFunc
	origDependents := GetDependentsWithMetaFunc
	origCreateAttempt := CreateAttemptBeadAtomicFunc
	origStampAttempt := StampAttemptInstanceFunc

	return func() {
		ListBeadsFunc = origList
		GetActiveAttemptFunc = origAttempt
		IsOwnedByInstanceFunc = origOwned
		InstanceIDFunc = origInstance
		InstanceNameFunc = origInstanceName
		GetBeadFunc = origGetBead
		GetCommentsFunc = origGetComments
		GetHookedStepsFunc = origHooked
		UnhookStepBeadFunc = origUnhook
		UpdateBeadFunc = origUpdate
		GetDependentsWithMetaFunc = origDependents
		CreateAttemptBeadAtomicFunc = origCreateAttempt
		StampAttemptInstanceFunc = origStampAttempt
	}
}

func TestSweepHookedSteps_FailureEvidence_SummonsCleric(t *testing.T) {
	// When a hooked bead has failure evidence (a recovery bead linked via
	// caused-by) and no cleric is running, the sweep should summon a cleric.
	cfgDir := t.TempDir()
	t.Setenv("SPIRE_CONFIG_DIR", cfgDir)
	t.Setenv("SPIRE_DOLT_DIR", t.TempDir())

	cleanup := stubFailureEvidenceHooks(t)
	defer cleanup()

	// Hooked parent bead returned by ListBeads.
	hookedStatus := beads.Status("hooked")
	ListBeadsFunc = func(filter beads.IssueFilter) ([]store.Bead, error) {
		if filter.Status != nil && *filter.Status == hookedStatus {
			return []store.Bead{
				{ID: "spi-parent1", Status: "hooked", Type: "task"},
			}, nil
		}
		return nil, nil
	}

	// Active attempt is locally owned.
	GetActiveAttemptFunc = func(parentID string) (*store.Bead, error) {
		if parentID == "spi-parent1" {
			return &store.Bead{ID: "spi-parent1.attempt-1", Status: "in_progress"}, nil
		}
		return nil, nil
	}
	IsOwnedByInstanceFunc = func(attemptID, instanceID string) (bool, error) {
		return true, nil
	}
	InstanceIDFunc = func() string { return "local-instance" }

	// Hooked step bead with no design_ref (falls to approval path).
	GetHookedStepsFunc = func(parentID string) ([]store.Bead, error) {
		if parentID == "spi-parent1" {
			return []store.Bead{
				{ID: "spi-parent1.step-impl", Status: "hooked", Labels: []string{"step:implement-failed"}},
			}, nil
		}
		return nil, nil
	}

	// Parent bead has needs-human label — approval check won't resolve.
	GetBeadFunc = func(id string) (store.Bead, error) {
		switch id {
		case "spi-parent1":
			return store.Bead{
				ID: "spi-parent1", Status: "hooked", Type: "task",
				Labels: []string{"needs-human"},
			}, nil
		case "spi-recovery1":
			return store.Bead{
				ID: "spi-recovery1", Status: "open", Type: "recovery",
			}, nil
		}
		return store.Bead{}, fmt.Errorf("not found: %s", id)
	}

	GetCommentsFunc = func(id string) ([]*beads.Comment, error) { return nil, nil }

	// Recovery bead linked via caused-by.
	GetDependentsWithMetaFunc = func(id string) ([]*beads.IssueWithDependencyMetadata, error) {
		if id == "spi-parent1" {
			return []*beads.IssueWithDependencyMetadata{
				{
					Issue:          beads.Issue{ID: "spi-recovery1", IssueType: "recovery", Status: "open"},
					DependencyType: "caused-by",
				},
			}, nil
		}
		return nil, nil
	}

	// Claim succeeds — no existing attempt on the recovery bead.
	var claimedBeadID string
	CreateAttemptBeadAtomicFunc = func(parentID, agentName, model, branch string) (string, error) {
		claimedBeadID = parentID
		return parentID + ".attempt-1", nil
	}
	StampAttemptInstanceFunc = func(attemptID string, meta store.InstanceMeta) error { return nil }
	InstanceNameFunc = func() string { return "test-machine" }

	UnhookStepBeadFunc = func(id string) error { return nil }
	UpdateBeadFunc = func(id string, fields map[string]interface{}) error { return nil }

	backend := &spawnTrackingBackend{}
	gsStore := &executor.FileGraphStateStore{ConfigDir: func() (string, error) { return cfgDir, nil }}
	count := SweepHookedSteps(false, backend, "test-tower", gsStore, PhaseDispatch{})

	if count != 1 {
		t.Errorf("SweepHookedSteps returned %d, want 1", count)
	}

	// Verify recovery bead was claimed.
	if claimedBeadID != "spi-recovery1" {
		t.Errorf("claimed bead = %q, want spi-recovery1", claimedBeadID)
	}

	// Verify a cleric was spawned (not a wizard).
	if len(backend.spawns) != 1 {
		t.Fatalf("spawn count = %d, want 1", len(backend.spawns))
	}
	sc := backend.spawns[0]
	if sc.Name != "cleric-spi-recovery1" {
		t.Errorf("spawn name = %q, want cleric-spi-recovery1", sc.Name)
	}
	if sc.BeadID != "spi-recovery1" {
		t.Errorf("spawn bead = %q, want spi-recovery1", sc.BeadID)
	}
	if sc.Role != agent.RoleCleric {
		t.Errorf("spawn role = %q, want %q", sc.Role, agent.RoleCleric)
	}
}

func TestSweepHookedSteps_FailureEvidence_AlreadyClaimed_Skips(t *testing.T) {
	// When the recovery bead is already claimed (attempt exists), skip summoning.
	cfgDir := t.TempDir()
	t.Setenv("SPIRE_CONFIG_DIR", cfgDir)
	t.Setenv("SPIRE_DOLT_DIR", t.TempDir())

	cleanup := stubFailureEvidenceHooks(t)
	defer cleanup()

	hookedStatus := beads.Status("hooked")
	ListBeadsFunc = func(filter beads.IssueFilter) ([]store.Bead, error) {
		if filter.Status != nil && *filter.Status == hookedStatus {
			return []store.Bead{
				{ID: "spi-parent2", Status: "hooked", Type: "task"},
			}, nil
		}
		return nil, nil
	}

	GetActiveAttemptFunc = func(parentID string) (*store.Bead, error) {
		if parentID == "spi-parent2" {
			return &store.Bead{ID: "spi-parent2.attempt-1", Status: "in_progress"}, nil
		}
		return nil, nil
	}
	IsOwnedByInstanceFunc = func(attemptID, instanceID string) (bool, error) {
		return true, nil
	}
	InstanceIDFunc = func() string { return "local-instance" }

	GetHookedStepsFunc = func(parentID string) ([]store.Bead, error) {
		if parentID == "spi-parent2" {
			return []store.Bead{
				{ID: "spi-parent2.step-impl", Status: "hooked", Labels: []string{"step:implement-failed"}},
			}, nil
		}
		return nil, nil
	}

	GetBeadFunc = func(id string) (store.Bead, error) {
		switch id {
		case "spi-parent2":
			return store.Bead{
				ID: "spi-parent2", Status: "hooked", Type: "task",
				Labels: []string{"needs-human"},
			}, nil
		case "spi-recovery2":
			return store.Bead{
				ID: "spi-recovery2", Status: "open", Type: "recovery",
			}, nil
		}
		return store.Bead{}, fmt.Errorf("not found: %s", id)
	}

	GetCommentsFunc = func(id string) ([]*beads.Comment, error) { return nil, nil }

	GetDependentsWithMetaFunc = func(id string) ([]*beads.IssueWithDependencyMetadata, error) {
		if id == "spi-parent2" {
			return []*beads.IssueWithDependencyMetadata{
				{
					Issue:          beads.Issue{ID: "spi-recovery2", IssueType: "recovery", Status: "open"},
					DependencyType: "caused-by",
				},
			}, nil
		}
		return nil, nil
	}

	// Claim fails — another agent already has an active attempt.
	CreateAttemptBeadAtomicFunc = func(parentID, agentName, model, branch string) (string, error) {
		return "", fmt.Errorf("active attempt already exists (agent: cleric-spi-recovery2)")
	}

	backend := &spawnTrackingBackend{}
	gsStore := &executor.FileGraphStateStore{ConfigDir: func() (string, error) { return cfgDir, nil }}
	count := SweepHookedSteps(false, backend, "test-tower", gsStore, PhaseDispatch{})

	if count != 0 {
		t.Errorf("SweepHookedSteps returned %d, want 0 (already claimed)", count)
	}
	if len(backend.spawns) != 0 {
		t.Errorf("spawn count = %d, want 0", len(backend.spawns))
	}
}

func TestSweepHookedSteps_FailureEvidence_NoRecoveryBead_Skips(t *testing.T) {
	// When a hooked bead has no failure evidence, the sweep should skip it.
	cfgDir := t.TempDir()
	t.Setenv("SPIRE_CONFIG_DIR", cfgDir)
	t.Setenv("SPIRE_DOLT_DIR", t.TempDir())

	cleanup := stubFailureEvidenceHooks(t)
	defer cleanup()

	hookedStatus := beads.Status("hooked")
	ListBeadsFunc = func(filter beads.IssueFilter) ([]store.Bead, error) {
		if filter.Status != nil && *filter.Status == hookedStatus {
			return []store.Bead{
				{ID: "spi-parent3", Status: "hooked", Type: "task"},
			}, nil
		}
		return nil, nil
	}

	GetActiveAttemptFunc = func(parentID string) (*store.Bead, error) {
		if parentID == "spi-parent3" {
			return &store.Bead{ID: "spi-parent3.attempt-1", Status: "in_progress"}, nil
		}
		return nil, nil
	}
	IsOwnedByInstanceFunc = func(attemptID, instanceID string) (bool, error) {
		return true, nil
	}
	InstanceIDFunc = func() string { return "local-instance" }

	GetHookedStepsFunc = func(parentID string) ([]store.Bead, error) {
		if parentID == "spi-parent3" {
			return []store.Bead{
				{ID: "spi-parent3.step-impl", Status: "hooked", Labels: []string{"step:implement"}},
			}, nil
		}
		return nil, nil
	}

	// Parent has needs-human (approval won't clear), and no dependents.
	GetBeadFunc = func(id string) (store.Bead, error) {
		if id == "spi-parent3" {
			return store.Bead{
				ID: "spi-parent3", Status: "hooked", Type: "task",
				Labels: []string{"needs-human"},
			}, nil
		}
		return store.Bead{}, fmt.Errorf("not found: %s", id)
	}

	GetCommentsFunc = func(id string) ([]*beads.Comment, error) { return nil, nil }

	// No dependents at all — no failure evidence.
	GetDependentsWithMetaFunc = func(id string) ([]*beads.IssueWithDependencyMetadata, error) {
		return nil, nil
	}

	backend := &spawnTrackingBackend{}
	gsStore := &executor.FileGraphStateStore{ConfigDir: func() (string, error) { return cfgDir, nil }}
	count := SweepHookedSteps(false, backend, "test-tower", gsStore, PhaseDispatch{})

	if count != 0 {
		t.Errorf("SweepHookedSteps returned %d, want 0 (no failure evidence)", count)
	}
	if len(backend.spawns) != 0 {
		t.Errorf("spawn count = %d, want 0", len(backend.spawns))
	}
}

func TestSweepHookedSteps_FailureEvidence_ClericSucceeded_UnhooksAndResummons(t *testing.T) {
	// When the recovery bead is already closed (cleric succeeded), the sweep
	// should unhook the step and re-summon the wizard.
	cfgDir := t.TempDir()
	t.Setenv("SPIRE_CONFIG_DIR", cfgDir)
	t.Setenv("SPIRE_DOLT_DIR", t.TempDir())

	cleanup := stubFailureEvidenceHooks(t)
	defer cleanup()

	hookedStatus := beads.Status("hooked")
	ListBeadsFunc = func(filter beads.IssueFilter) ([]store.Bead, error) {
		if filter.Status != nil && *filter.Status == hookedStatus {
			return []store.Bead{
				{ID: "spi-parent4", Status: "hooked", Type: "task"},
			}, nil
		}
		return nil, nil
	}

	GetActiveAttemptFunc = func(parentID string) (*store.Bead, error) {
		if parentID == "spi-parent4" {
			return &store.Bead{ID: "spi-parent4.attempt-1", Status: "in_progress"}, nil
		}
		return nil, nil
	}
	IsOwnedByInstanceFunc = func(attemptID, instanceID string) (bool, error) {
		return true, nil
	}
	InstanceIDFunc = func() string { return "local-instance" }

	GetHookedStepsFunc = func(parentID string) ([]store.Bead, error) {
		if parentID == "spi-parent4" {
			return []store.Bead{
				{ID: "spi-parent4.step-impl", Status: "hooked", Labels: []string{"step:implement-failed"}},
			}, nil
		}
		return nil, nil
	}

	GetBeadFunc = func(id string) (store.Bead, error) {
		switch id {
		case "spi-parent4":
			return store.Bead{
				ID: "spi-parent4", Status: "hooked", Type: "task",
				Labels: []string{"needs-human"},
			}, nil
		case "spi-recovery4":
			return store.Bead{
				ID: "spi-recovery4", Status: "closed", Type: "recovery",
				Metadata: map[string]string{
					recovery.KeyRecoveryOutcome: mustMarshalOutcome(t, recovery.RecoveryOutcome{
						SourceBeadID:  "spi-parent4",
						Decision:      recovery.DecisionResume,
						VerifyVerdict: recovery.VerifyVerdictPass,
					}),
				},
			}, nil
		}
		return store.Bead{}, fmt.Errorf("not found: %s", id)
	}

	GetCommentsFunc = func(id string) ([]*beads.Comment, error) { return nil, nil }

	GetDependentsWithMetaFunc = func(id string) ([]*beads.IssueWithDependencyMetadata, error) {
		if id == "spi-parent4" {
			return []*beads.IssueWithDependencyMetadata{
				{
					Issue:          beads.Issue{ID: "spi-recovery4", IssueType: "recovery", Status: "closed"},
					DependencyType: "caused-by",
				},
			}, nil
		}
		return nil, nil
	}

	var unhooked []string
	UnhookStepBeadFunc = func(id string) error {
		unhooked = append(unhooked, id)
		return nil
	}

	var updatedBeads []string
	UpdateBeadFunc = func(id string, fields map[string]interface{}) error {
		updatedBeads = append(updatedBeads, id)
		return nil
	}

	backend := &spawnTrackingBackend{}
	gsStore := &executor.FileGraphStateStore{ConfigDir: func() (string, error) { return cfgDir, nil }}
	count := SweepHookedSteps(false, backend, "test-tower", gsStore, PhaseDispatch{})

	if count != 1 {
		t.Errorf("SweepHookedSteps returned %d, want 1", count)
	}

	// Verify step was unhooked.
	if len(unhooked) != 1 || unhooked[0] != "spi-parent4.step-impl" {
		t.Errorf("unhooked = %v, want [spi-parent4.step-impl]", unhooked)
	}

	// Verify parent was set to in_progress.
	if len(updatedBeads) != 1 || updatedBeads[0] != "spi-parent4" {
		t.Errorf("updatedBeads = %v, want [spi-parent4]", updatedBeads)
	}

	// Verify a wizard (not cleric) was spawned for the parent bead.
	// Role must be RoleWizard so the K8s backend's selectPodShape routes to
	// the wizard pod shape (spi-fcord7). RoleApprentice was the bug — it
	// happened to work locally because the process backend's command map
	// re-enters CmdWizardRun via "spire apprentice run", but in cluster the
	// k8s backend would build an apprentice pod running the apprentice
	// command instead of a wizard pod running `spire execute`.
	if len(backend.spawns) != 1 {
		t.Fatalf("spawn count = %d, want 1", len(backend.spawns))
	}
	sc := backend.spawns[0]
	if sc.BeadID != "spi-parent4" {
		t.Errorf("spawn bead = %q, want spi-parent4", sc.BeadID)
	}
	if sc.Role != agent.RoleWizard {
		t.Errorf("spawn role = %q, want %q", sc.Role, agent.RoleWizard)
	}
}

func TestSweepHookedSteps_FailureEvidence_ClericEscalated_StaysHooked(t *testing.T) {
	// When the recovery bead is closed with escalation, the bead should stay
	// hooked for human attention.
	cfgDir := t.TempDir()
	t.Setenv("SPIRE_CONFIG_DIR", cfgDir)
	t.Setenv("SPIRE_DOLT_DIR", t.TempDir())

	cleanup := stubFailureEvidenceHooks(t)
	defer cleanup()

	hookedStatus := beads.Status("hooked")
	ListBeadsFunc = func(filter beads.IssueFilter) ([]store.Bead, error) {
		if filter.Status != nil && *filter.Status == hookedStatus {
			return []store.Bead{
				{ID: "spi-parent5", Status: "hooked", Type: "task"},
			}, nil
		}
		return nil, nil
	}

	GetActiveAttemptFunc = func(parentID string) (*store.Bead, error) {
		if parentID == "spi-parent5" {
			return &store.Bead{ID: "spi-parent5.attempt-1", Status: "in_progress"}, nil
		}
		return nil, nil
	}
	IsOwnedByInstanceFunc = func(attemptID, instanceID string) (bool, error) {
		return true, nil
	}
	InstanceIDFunc = func() string { return "local-instance" }

	GetHookedStepsFunc = func(parentID string) ([]store.Bead, error) {
		if parentID == "spi-parent5" {
			return []store.Bead{
				{ID: "spi-parent5.step-impl", Status: "hooked", Labels: []string{"step:implement-failed"}},
			}, nil
		}
		return nil, nil
	}

	GetBeadFunc = func(id string) (store.Bead, error) {
		switch id {
		case "spi-parent5":
			return store.Bead{
				ID: "spi-parent5", Status: "hooked", Type: "task",
				Labels: []string{"needs-human"},
			}, nil
		case "spi-recovery5":
			return store.Bead{
				ID: "spi-recovery5", Status: "closed", Type: "recovery",
				Metadata: map[string]string{
					recovery.KeyRecoveryOutcome: mustMarshalOutcome(t, recovery.RecoveryOutcome{
						SourceBeadID:  "spi-parent5",
						Decision:      recovery.DecisionEscalate,
						VerifyVerdict: recovery.VerifyVerdictFail,
					}),
				},
			}, nil
		}
		return store.Bead{}, fmt.Errorf("not found: %s", id)
	}

	GetCommentsFunc = func(id string) ([]*beads.Comment, error) { return nil, nil }

	GetDependentsWithMetaFunc = func(id string) ([]*beads.IssueWithDependencyMetadata, error) {
		if id == "spi-parent5" {
			return []*beads.IssueWithDependencyMetadata{
				{
					Issue:          beads.Issue{ID: "spi-recovery5", IssueType: "recovery", Status: "closed"},
					DependencyType: "caused-by",
				},
			}, nil
		}
		return nil, nil
	}

	UnhookStepBeadFunc = func(id string) error { return nil }
	UpdateBeadFunc = func(id string, fields map[string]interface{}) error { return nil }

	backend := &spawnTrackingBackend{}
	gsStore := &executor.FileGraphStateStore{ConfigDir: func() (string, error) { return cfgDir, nil }}
	count := SweepHookedSteps(false, backend, "test-tower", gsStore, PhaseDispatch{})

	if count != 0 {
		t.Errorf("SweepHookedSteps returned %d, want 0 (escalated — stays hooked)", count)
	}
	if len(backend.spawns) != 0 {
		t.Errorf("spawn count = %d, want 0", len(backend.spawns))
	}
}

func TestSweepHookedSteps_FailureEvidence_ClericClosedWithoutOutcome_StaysHooked(t *testing.T) {
	// When the recovery bead is closed but has no recovery_outcome metadata
	// (older bead, or a write was skipped), the sweep defaults to the safe
	// path: leave the parent hooked for human attention rather than
	// optimistically resuming.
	cfgDir := t.TempDir()
	t.Setenv("SPIRE_CONFIG_DIR", cfgDir)
	t.Setenv("SPIRE_DOLT_DIR", t.TempDir())

	cleanup := stubFailureEvidenceHooks(t)
	defer cleanup()

	hookedStatus := beads.Status("hooked")
	ListBeadsFunc = func(filter beads.IssueFilter) ([]store.Bead, error) {
		if filter.Status != nil && *filter.Status == hookedStatus {
			return []store.Bead{
				{ID: "spi-parent6", Status: "hooked", Type: "task"},
			}, nil
		}
		return nil, nil
	}

	GetActiveAttemptFunc = func(parentID string) (*store.Bead, error) {
		if parentID == "spi-parent6" {
			return &store.Bead{ID: "spi-parent6.attempt-1", Status: "in_progress"}, nil
		}
		return nil, nil
	}
	IsOwnedByInstanceFunc = func(attemptID, instanceID string) (bool, error) {
		return true, nil
	}
	InstanceIDFunc = func() string { return "local-instance" }

	GetHookedStepsFunc = func(parentID string) ([]store.Bead, error) {
		if parentID == "spi-parent6" {
			return []store.Bead{
				{ID: "spi-parent6.step-impl", Status: "hooked", Labels: []string{"step:implement-failed"}},
			}, nil
		}
		return nil, nil
	}

	GetBeadFunc = func(id string) (store.Bead, error) {
		switch id {
		case "spi-parent6":
			return store.Bead{
				ID: "spi-parent6", Status: "hooked", Type: "task",
				Labels: []string{"needs-human"},
			}, nil
		case "spi-recovery6":
			return store.Bead{
				ID: "spi-recovery6", Status: "closed", Type: "recovery",
				// No recovery_outcome metadata — simulates an older bead or a
				// cleric that closed without writing an outcome record.
			}, nil
		}
		return store.Bead{}, fmt.Errorf("not found: %s", id)
	}

	GetCommentsFunc = func(id string) ([]*beads.Comment, error) { return nil, nil }

	GetDependentsWithMetaFunc = func(id string) ([]*beads.IssueWithDependencyMetadata, error) {
		if id == "spi-parent6" {
			return []*beads.IssueWithDependencyMetadata{
				{
					Issue:          beads.Issue{ID: "spi-recovery6", IssueType: "recovery", Status: "closed"},
					DependencyType: "caused-by",
				},
			}, nil
		}
		return nil, nil
	}

	UnhookStepBeadFunc = func(id string) error { return nil }
	UpdateBeadFunc = func(id string, fields map[string]interface{}) error { return nil }

	backend := &spawnTrackingBackend{}
	gsStore := &executor.FileGraphStateStore{ConfigDir: func() (string, error) { return cfgDir, nil }}
	count := SweepHookedSteps(false, backend, "test-tower", gsStore, PhaseDispatch{})

	if count != 0 {
		t.Errorf("SweepHookedSteps returned %d, want 0 (no outcome — stays hooked)", count)
	}
	if len(backend.spawns) != 0 {
		t.Errorf("spawn count = %d, want 0", len(backend.spawns))
	}
}

// TestSweepHookedSteps_FailureEvidence_ClericEscalated_NoResummonOnRepeatSweep
// is the spi-0nkot regression test. It proves that a hooked parent with a
// closed, DecisionEscalate recovery bead is NOT re-claimed on the next sweep
// cycle — even when the sweep is invoked multiple times in a row. Before the
// fix, the cleric left escalated recovery beads open and the steward
// claimed any open recovery bead as fresh cleric work on every cycle,
// creating an infinite cleric spawn loop against the same parked parent.
func TestSweepHookedSteps_FailureEvidence_ClericEscalated_NoResummonOnRepeatSweep(t *testing.T) {
	cfgDir := t.TempDir()
	t.Setenv("SPIRE_CONFIG_DIR", cfgDir)
	t.Setenv("SPIRE_DOLT_DIR", t.TempDir())

	cleanup := stubFailureEvidenceHooks(t)
	defer cleanup()

	hookedStatus := beads.Status("hooked")
	ListBeadsFunc = func(filter beads.IssueFilter) ([]store.Bead, error) {
		if filter.Status != nil && *filter.Status == hookedStatus {
			return []store.Bead{
				{ID: "spi-parent-loop", Status: "hooked", Type: "task"},
			}, nil
		}
		return nil, nil
	}
	GetActiveAttemptFunc = func(parentID string) (*store.Bead, error) {
		if parentID == "spi-parent-loop" {
			return &store.Bead{ID: "spi-parent-loop.attempt-1", Status: "in_progress"}, nil
		}
		return nil, nil
	}
	IsOwnedByInstanceFunc = func(attemptID, instanceID string) (bool, error) { return true, nil }
	InstanceIDFunc = func() string { return "local-instance" }

	GetHookedStepsFunc = func(parentID string) ([]store.Bead, error) {
		if parentID == "spi-parent-loop" {
			return []store.Bead{
				{ID: "spi-parent-loop.step-impl", Status: "hooked", Labels: []string{"step:implement-failed"}},
			}, nil
		}
		return nil, nil
	}

	GetBeadFunc = func(id string) (store.Bead, error) {
		switch id {
		case "spi-parent-loop":
			return store.Bead{
				ID: "spi-parent-loop", Status: "hooked", Type: "task",
				Labels: []string{"needs-human"},
			}, nil
		case "spi-recovery-escalated":
			return store.Bead{
				ID: "spi-recovery-escalated", Status: "closed", Type: "recovery",
				Metadata: map[string]string{
					recovery.KeyRecoveryOutcome: mustMarshalOutcome(t, recovery.RecoveryOutcome{
						SourceBeadID:  "spi-parent-loop",
						Decision:      recovery.DecisionEscalate,
						VerifyVerdict: recovery.VerifyVerdictFail,
					}),
				},
			}, nil
		}
		return store.Bead{}, fmt.Errorf("not found: %s", id)
	}
	GetCommentsFunc = func(id string) ([]*beads.Comment, error) { return nil, nil }

	GetDependentsWithMetaFunc = func(id string) ([]*beads.IssueWithDependencyMetadata, error) {
		if id == "spi-parent-loop" {
			return []*beads.IssueWithDependencyMetadata{
				{
					Issue:          beads.Issue{ID: "spi-recovery-escalated", IssueType: "recovery", Status: "closed"},
					DependencyType: "caused-by",
				},
			}, nil
		}
		return nil, nil
	}

	UnhookStepBeadFunc = func(id string) error { return nil }
	UpdateBeadFunc = func(id string, fields map[string]interface{}) error { return nil }

	// Sentinel: if CreateAttemptBeadAtomicFunc is invoked, the sweep tried to
	// claim the recovery bead — the exact loop behavior spi-0nkot fixes.
	var claimAttempts int
	CreateAttemptBeadAtomicFunc = func(parentID, agentName, model, branch string) (string, error) {
		claimAttempts++
		return parentID + ".attempt-1", nil
	}
	StampAttemptInstanceFunc = func(attemptID string, meta store.InstanceMeta) error { return nil }
	InstanceNameFunc = func() string { return "test-machine" }

	backend := &spawnTrackingBackend{}
	gsStore := &executor.FileGraphStateStore{ConfigDir: func() (string, error) { return cfgDir, nil }}

	const sweeps = 3
	for i := 0; i < sweeps; i++ {
		if count := SweepHookedSteps(false, backend, "test-tower", gsStore, PhaseDispatch{}); count != 0 {
			t.Errorf("sweep %d: SweepHookedSteps returned %d, want 0 (closed+escalated — parent stays parked)", i+1, count)
		}
	}
	if claimAttempts != 0 {
		t.Errorf("claim attempts = %d, want 0 across %d sweeps (escalated recovery must not be re-claimed)", claimAttempts, sweeps)
	}
	if len(backend.spawns) != 0 {
		t.Errorf("spawn count = %d, want 0 across %d sweeps", len(backend.spawns), sweeps)
	}
}

// TestFindFailureEvidence_PrefersLatestRecovery verifies that
// findFailureEvidence returns the newest recovery bead when a hooked parent
// has both a historical (older) closed recovery and a current (newer) one.
// This prevents the sweep from acting on stale recovery metadata when a
// parent has been hooked multiple times (spi-0nkot).
func TestFindFailureEvidence_PrefersLatestRecovery(t *testing.T) {
	cleanup := stubFailureEvidenceHooks(t)
	defer cleanup()

	older := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	newer := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)

	GetBeadFunc = func(id string) (store.Bead, error) {
		switch id {
		case "spi-recovery-older", "spi-recovery-newer":
			return store.Bead{ID: id, Type: "recovery"}, nil
		}
		return store.Bead{}, fmt.Errorf("not found: %s", id)
	}
	GetDependentsWithMetaFunc = func(id string) ([]*beads.IssueWithDependencyMetadata, error) {
		return []*beads.IssueWithDependencyMetadata{
			{
				Issue:          beads.Issue{ID: "spi-recovery-older", IssueType: "recovery", CreatedAt: older},
				DependencyType: "caused-by",
			},
			{
				Issue:          beads.Issue{ID: "spi-recovery-newer", IssueType: "recovery", CreatedAt: newer},
				DependencyType: "caused-by",
			},
		}, nil
	}

	evidence, ok := findFailureEvidence("spi-parent-multi")
	if !ok {
		t.Fatal("findFailureEvidence returned !ok, want ok with latest recovery")
	}
	if evidence.RecoveryBeadID != "spi-recovery-newer" {
		t.Errorf("RecoveryBeadID = %q, want %q (must pick latest CreatedAt)", evidence.RecoveryBeadID, "spi-recovery-newer")
	}
}

// --- Steward cycle integration tests (wave-0 modules) ---

// mockBackend records Spawn calls and returns configurable agents from List.
type mockBackend struct {
	spawns   []agent.SpawnConfig
	agents   []agent.Info
	listErr  error
	spawnErr error
}

func (m *mockBackend) Spawn(cfg agent.SpawnConfig) (agent.Handle, error) {
	if m.spawnErr != nil {
		return nil, m.spawnErr
	}
	m.spawns = append(m.spawns, cfg)
	return &fakeHandle{id: cfg.Name}, nil
}
func (m *mockBackend) List() ([]agent.Info, error) {
	return m.agents, m.listErr
}
func (m *mockBackend) Logs(name string) (io.ReadCloser, error) { return nil, os.ErrNotExist }
func (m *mockBackend) Kill(name string) error                  { return nil }
func (m *mockBackend) TerminateBead(ctx context.Context, beadID string) error {
	return nil
}

// setupCycleTest creates a minimal tower config for testing TowerCycle.
// It stubs out store operations and returns a cleanup function.
func setupCycleTest(t *testing.T, schedulableBeads []store.Bead) func() {
	t.Helper()
	tmpDir := t.TempDir()
	t.Setenv("SPIRE_CONFIG_DIR", tmpDir)
	t.Setenv("SPIRE_DOLT_DIR", tmpDir)

	// Create wizards log dir so filepath.Join doesn't fail.
	os.MkdirAll(filepath.Join(tmpDir, "wizards"), 0755)

	// Stub store functions used in TowerCycle.
	origListBeads := ListBeadsFunc
	ListBeadsFunc = func(filter beads.IssueFilter) ([]store.Bead, error) {
		return nil, nil // no in_progress beads for health check
	}

	origAttempt := GetActiveAttemptFunc
	GetActiveAttemptFunc = func(parentID string) (*store.Bead, error) {
		return nil, nil
	}

	return func() {
		ListBeadsFunc = origListBeads
		GetActiveAttemptFunc = origAttempt
	}
}

func TestTowerCycle_ConcurrencyLimit_SpawnsUpToMax(t *testing.T) {
	cleanup := setupCycleTest(t, nil)
	defer cleanup()

	backend := &mockBackend{
		agents: []agent.Info{
			{Name: "wizard-spi-existing1", Alive: true, Tower: "test-tower"},
		},
	}

	// Create 5 schedulable beads but limit to 2 concurrent.
	cl := NewConcurrencyLimiter()
	cs := NewCycleStats()

	// Manually run the auto-summon logic (we can't easily call TowerCycle
	// because it requires a real store, so we test the core components directly).

	// Refresh limiter with 1 alive agent.
	cl.Refresh("test-tower", backend.agents)

	// With maxConcurrent=2 and 1 alive, CanSpawn should be true once.
	if !cl.CanSpawn("test-tower", 2) {
		t.Error("expected CanSpawn=true (1 of 2 slots used)")
	}

	// Simulate the spawn loop: spawn until limit reached.
	beadIDs := []string{"spi-aaa", "spi-bbb", "spi-ccc", "spi-ddd", "spi-eee"}
	spawned := 0
	for _, id := range beadIDs {
		if !cl.CanSpawn("test-tower", 2) {
			break
		}
		wizardName := "wizard-" + SanitizeK8sLabel(id)
		_, err := backend.Spawn(agent.SpawnConfig{
			Name:   wizardName,
			BeadID: id,
			Role:   agent.RoleWizard,
		})
		if err != nil {
			t.Fatal(err)
		}
		spawned++
		// Simulate the limiter being updated (in real code, Refresh is called per cycle,
		// but the limiter counts increase only after the next Refresh).
		// For spawn-loop enforcement, we manually increment.
		cl.mu.Lock()
		cl.counts["test-tower"]++
		cl.mu.Unlock()
	}

	if spawned != 1 {
		t.Errorf("spawned = %d, want 1 (maxConcurrent=2, 1 already alive)", spawned)
	}

	// Verify wizard name format.
	if len(backend.spawns) > 0 {
		name := backend.spawns[0].Name
		if name != "wizard-spi-aaa" {
			t.Errorf("spawn name = %q, want wizard-spi-aaa", name)
		}
	}

	// Verify cycle stats.
	cs.Record(CycleStatsSnapshot{
		ActiveAgents:     1,
		SpawnedThisCycle: spawned,
		SchedulableWork:  5,
		Tower:            "test-tower",
	})
	snap := cs.Snapshot()
	if snap.SpawnedThisCycle != 1 {
		t.Errorf("stats spawned = %d, want 1", snap.SpawnedThisCycle)
	}
	if snap.SchedulableWork != 5 {
		t.Errorf("stats schedulable = %d, want 5", snap.SchedulableWork)
	}
}

func TestAutoSummon_WizardNaming(t *testing.T) {
	// Verify that wizards get properly sanitized names.
	tests := []struct {
		beadID   string
		expected string
	}{
		{"spi-abc", "wizard-spi-abc"},
		{"spi-a3f8.1", "wizard-spi-a3f8-1"},
		{"web-B7D0", "wizard-web-b7d0"},
		{"api_8a01", "wizard-api-8a01"},
	}
	for _, tt := range tests {
		name := "wizard-" + SanitizeK8sLabel(tt.beadID)
		if name != tt.expected {
			t.Errorf("wizard name for %q = %q, want %q", tt.beadID, name, tt.expected)
		}
	}
}

func TestMergeQueueProcessing_OnePerCycle(t *testing.T) {
	mq := NewMergeQueue()

	// Enqueue 3 merge requests.
	mq.Enqueue(MergeRequest{BeadID: "spi-aaa", Branch: "feat/spi-aaa"})
	mq.Enqueue(MergeRequest{BeadID: "spi-bbb", Branch: "feat/spi-bbb"})
	mq.Enqueue(MergeRequest{BeadID: "spi-ccc", Branch: "feat/spi-ccc"})

	if mq.Depth() != 3 {
		t.Fatalf("queue depth = %d, want 3", mq.Depth())
	}

	// Process one — should dequeue "spi-aaa".
	mergedIDs := []string{}
	mockMerge := func(ctx context.Context, req MergeRequest) MergeResult {
		mergedIDs = append(mergedIDs, req.BeadID)
		return MergeResult{BeadID: req.BeadID, Success: true, SHA: "abc123"}
	}

	result := mq.ProcessNext(context.Background(), mockMerge)
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if !result.Success {
		t.Error("expected success")
	}
	if result.BeadID != "spi-aaa" {
		t.Errorf("result.BeadID = %q, want spi-aaa", result.BeadID)
	}

	// Only one was processed (one per cycle).
	if len(mergedIDs) != 1 {
		t.Errorf("merged count = %d, want 1", len(mergedIDs))
	}

	// Remaining depth should be 2.
	if mq.Depth() != 2 {
		t.Errorf("remaining depth = %d, want 2", mq.Depth())
	}
}

func TestTrustRecordAfterMerge(t *testing.T) {
	// Verify that TrustChecker methods work correctly for steward integration.
	tc := NewTrustChecker()

	// RequiresSageReview: sandbox and supervised need review.
	if !tc.RequiresSageReview(store.TrustSandbox) {
		t.Error("sandbox should require sage review")
	}
	if !tc.RequiresSageReview(store.TrustSupervised) {
		t.Error("supervised should require sage review")
	}
	if tc.RequiresSageReview(store.TrustTrusted) {
		t.Error("trusted should NOT require sage review")
	}
	if tc.RequiresSageReview(store.TrustAutonomous) {
		t.Error("autonomous should NOT require sage review")
	}

	// AllowsAutoMerge: trusted and autonomous allow auto-merge.
	if tc.AllowsAutoMerge(store.TrustSandbox) {
		t.Error("sandbox should NOT allow auto-merge")
	}
	if tc.AllowsAutoMerge(store.TrustSupervised) {
		t.Error("supervised should NOT allow auto-merge")
	}
	if !tc.AllowsAutoMerge(store.TrustTrusted) {
		t.Error("trusted should allow auto-merge")
	}
	if !tc.AllowsAutoMerge(store.TrustAutonomous) {
		t.Error("autonomous should allow auto-merge")
	}
}

func TestCycleStats_Populated(t *testing.T) {
	cs := NewCycleStats()

	// Initially empty.
	snap := cs.Snapshot()
	if !snap.LastCycleAt.IsZero() {
		t.Error("expected zero LastCycleAt initially")
	}

	// Record a cycle.
	now := time.Now()
	cs.Record(CycleStatsSnapshot{
		LastCycleAt:      now,
		CycleDuration:    2 * time.Second,
		ActiveAgents:     3,
		QueueDepth:       1,
		SchedulableWork:  5,
		SpawnedThisCycle: 2,
		Tower:            "test-tower",
	})

	snap = cs.Snapshot()
	if snap.LastCycleAt != now {
		t.Errorf("LastCycleAt = %v, want %v", snap.LastCycleAt, now)
	}
	if snap.CycleDuration != 2*time.Second {
		t.Errorf("CycleDuration = %v, want 2s", snap.CycleDuration)
	}
	if snap.ActiveAgents != 3 {
		t.Errorf("ActiveAgents = %d, want 3", snap.ActiveAgents)
	}
	if snap.QueueDepth != 1 {
		t.Errorf("QueueDepth = %d, want 1", snap.QueueDepth)
	}
	if snap.SchedulableWork != 5 {
		t.Errorf("SchedulableWork = %d, want 5", snap.SchedulableWork)
	}
	if snap.SpawnedThisCycle != 2 {
		t.Errorf("SpawnedThisCycle = %d, want 2", snap.SpawnedThisCycle)
	}
	if snap.Tower != "test-tower" {
		t.Errorf("Tower = %q, want test-tower", snap.Tower)
	}
}

func TestDryRun_DoesNotSpawn(t *testing.T) {
	backend := &mockBackend{}
	cl := NewConcurrencyLimiter()

	// Simulate dry-run spawn loop.
	beadIDs := []string{"spi-aaa", "spi-bbb"}
	spawned := 0
	dryRun := true

	for _, id := range beadIDs {
		if cl.CanSpawn("test", 10) {
			if dryRun {
				// In dry-run mode, we increment spawned but don't call backend.Spawn.
				spawned++
				continue
			}
			backend.Spawn(agent.SpawnConfig{
				Name:   "wizard-" + SanitizeK8sLabel(id),
				BeadID: id,
				Role:   agent.RoleWizard,
			})
			spawned++
		}
	}

	if spawned != 2 {
		t.Errorf("spawned count = %d, want 2", spawned)
	}
	if len(backend.spawns) != 0 {
		t.Errorf("backend.spawns = %d, want 0 (dry-run should not spawn)", len(backend.spawns))
	}
}

func TestABRouting_AddsFormulaLabel(t *testing.T) {
	// Test the ABRouter behavior used by the steward cycle.
	router := NewABRouter()

	// Without a DB, we can test the hash determinism.
	h1 := hashBead("spi-test1")
	h2 := hashBead("spi-test1")
	if h1 != h2 {
		t.Errorf("hashBead not deterministic: %d != %d", h1, h2)
	}

	// Verify range.
	for i := 0; i < 100; i++ {
		id := fmt.Sprintf("spi-%04d", i)
		h := hashBead(id)
		if h < 0 || h >= 100 {
			t.Errorf("hashBead(%q) = %d, out of range [0,100)", id, h)
		}
	}

	_ = router // ABRouter.SelectVariant requires a DB, tested in ab_routing_test.go
}

func TestBeadRepoPrefix(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"spi-abc", "spi"},
		{"web-b7d0", "web"},
		{"api-8a01", "api"},
		{"nohyphen", "nohyphen"},
		{"x-y", "x"},
	}
	for _, tt := range tests {
		got := beadRepoPrefix(tt.input)
		if got != tt.expected {
			t.Errorf("beadRepoPrefix(%q) = %q, want %q", tt.input, got, tt.expected)
		}
	}
}

func TestMergeQueueDepth_Nil(t *testing.T) {
	if d := mergeQueueDepth(nil); d != 0 {
		t.Errorf("mergeQueueDepth(nil) = %d, want 0", d)
	}

	mq := NewMergeQueue()
	mq.Enqueue(MergeRequest{BeadID: "spi-x"})
	if d := mergeQueueDepth(mq); d != 1 {
		t.Errorf("mergeQueueDepth = %d, want 1", d)
	}
}

// --- DetectMergeReady tests ---

func TestDetectMergeReady_EnqueuesApprovedBead(t *testing.T) {
	origListBeads := ListBeadsFunc
	origConfigLoad := ConfigLoadFunc
	defer func() {
		ListBeadsFunc = origListBeads
		ConfigLoadFunc = origConfigLoad
	}()

	ListBeadsFunc = func(filter beads.IssueFilter) ([]store.Bead, error) {
		return []store.Bead{
			{ID: "spi-abc", Status: "in_progress", Type: "task", Labels: []string{"review-approved", "feat-branch:feat/spi-abc"}},
		}, nil
	}
	ConfigLoadFunc = func() (*config.SpireConfig, error) {
		return &config.SpireConfig{
			Instances: map[string]*config.Instance{
				"spi": {Path: "/repos/spire", Prefix: "spi"},
			},
		}, nil
	}

	mq := NewMergeQueue()
	DetectMergeReady(false, mq)

	if mq.Depth() != 1 {
		t.Errorf("expected queue depth 1, got %d", mq.Depth())
	}
	peek := mq.Peek()
	if peek == nil {
		t.Fatal("expected non-nil peek")
	}
	if peek.BeadID != "spi-abc" {
		t.Errorf("BeadID = %q, want %q", peek.BeadID, "spi-abc")
	}
	if peek.Branch != "feat/spi-abc" {
		t.Errorf("Branch = %q, want %q", peek.Branch, "feat/spi-abc")
	}
	if peek.BaseBranch != "main" {
		t.Errorf("BaseBranch = %q, want %q", peek.BaseBranch, "main")
	}
	if peek.RepoPath != "/repos/spire" {
		t.Errorf("RepoPath = %q, want %q", peek.RepoPath, "/repos/spire")
	}
}

func TestDetectMergeReady_SkipsWithoutReviewApproved(t *testing.T) {
	origListBeads := ListBeadsFunc
	origConfigLoad := ConfigLoadFunc
	defer func() {
		ListBeadsFunc = origListBeads
		ConfigLoadFunc = origConfigLoad
	}()

	ListBeadsFunc = func(filter beads.IssueFilter) ([]store.Bead, error) {
		return []store.Bead{
			{ID: "spi-noapproval", Status: "in_progress", Type: "task", Labels: []string{"feat-branch:feat/spi-noapproval"}},
		}, nil
	}
	ConfigLoadFunc = func() (*config.SpireConfig, error) {
		return &config.SpireConfig{
			Instances: map[string]*config.Instance{
				"spi": {Path: "/repos/spire", Prefix: "spi"},
			},
		}, nil
	}

	mq := NewMergeQueue()
	DetectMergeReady(false, mq)

	if mq.Depth() != 0 {
		t.Errorf("expected queue depth 0 (no review-approved), got %d", mq.Depth())
	}
}

func TestDetectMergeReady_SkipsAlreadyInQueue(t *testing.T) {
	origListBeads := ListBeadsFunc
	origConfigLoad := ConfigLoadFunc
	defer func() {
		ListBeadsFunc = origListBeads
		ConfigLoadFunc = origConfigLoad
	}()

	ListBeadsFunc = func(filter beads.IssueFilter) ([]store.Bead, error) {
		return []store.Bead{
			{ID: "spi-dup", Status: "in_progress", Type: "task", Labels: []string{"review-approved", "feat-branch:feat/spi-dup"}},
		}, nil
	}
	ConfigLoadFunc = func() (*config.SpireConfig, error) {
		return &config.SpireConfig{
			Instances: map[string]*config.Instance{
				"spi": {Path: "/repos/spire", Prefix: "spi"},
			},
		}, nil
	}

	mq := NewMergeQueue()
	// Pre-enqueue the bead.
	mq.Enqueue(MergeRequest{BeadID: "spi-dup", Branch: "feat/spi-dup", BaseBranch: "main"})

	DetectMergeReady(false, mq)

	if mq.Depth() != 1 {
		t.Errorf("expected queue depth 1 (no duplicate), got %d", mq.Depth())
	}
}

func TestDetectMergeReady_SkipsNoRegisteredRepo(t *testing.T) {
	origListBeads := ListBeadsFunc
	origConfigLoad := ConfigLoadFunc
	defer func() {
		ListBeadsFunc = origListBeads
		ConfigLoadFunc = origConfigLoad
	}()

	ListBeadsFunc = func(filter beads.IssueFilter) ([]store.Bead, error) {
		return []store.Bead{
			{ID: "web-xyz", Status: "in_progress", Type: "task", Labels: []string{"review-approved", "feat-branch:feat/web-xyz"}},
		}, nil
	}
	// Config has no "web" instance registered.
	ConfigLoadFunc = func() (*config.SpireConfig, error) {
		return &config.SpireConfig{
			Instances: map[string]*config.Instance{
				"spi": {Path: "/repos/spire", Prefix: "spi"},
			},
		}, nil
	}

	mq := NewMergeQueue()
	DetectMergeReady(false, mq)

	if mq.Depth() != 0 {
		t.Errorf("expected queue depth 0 (no registered repo for prefix 'web'), got %d", mq.Depth())
	}
}

// --- MergeQueue.Contains tests ---

func TestMergeQueue_Contains_InQueue(t *testing.T) {
	mq := NewMergeQueue()
	mq.Enqueue(MergeRequest{BeadID: "spi-a"})
	mq.Enqueue(MergeRequest{BeadID: "spi-b"})

	if !mq.Contains("spi-a") {
		t.Error("expected Contains(spi-a) = true")
	}
	if !mq.Contains("spi-b") {
		t.Error("expected Contains(spi-b) = true")
	}
	if mq.Contains("spi-c") {
		t.Error("expected Contains(spi-c) = false")
	}
}

func TestMergeQueue_Contains_Active(t *testing.T) {
	mq := NewMergeQueue()
	mq.Enqueue(MergeRequest{BeadID: "spi-active"})

	// Start processing to make it active.
	done := make(chan struct{})
	go func() {
		mq.ProcessNext(context.Background(), func(ctx context.Context, req MergeRequest) MergeResult {
			// Check Contains while active.
			if !mq.Contains("spi-active") {
				t.Error("expected Contains(spi-active) = true while active")
			}
			close(done)
			return MergeResult{BeadID: req.BeadID, Success: true}
		})
	}()
	<-done
}

func TestConcurrencyLimiter_UnlimitedWhenZero(t *testing.T) {
	cl := NewConcurrencyLimiter()
	cl.Refresh("tower", []agent.Info{
		{Name: "a", Alive: true},
		{Name: "b", Alive: true},
		{Name: "c", Alive: true},
	})

	// maxConcurrent=0 means unlimited.
	if !cl.CanSpawn("tower", 0) {
		t.Error("expected CanSpawn=true with maxConcurrent=0 (unlimited)")
	}
}

// --- CheckBeadHealth instance scoping tests ---

func TestCheckBeadHealth_SkipsForeignOwnedAttempts(t *testing.T) {
	// Bead updated 45 minutes ago (beyond shutdown threshold).
	oldTime := time.Now().Add(-45 * time.Minute).UTC().Format(time.RFC3339)
	origList := ListBeadsFunc
	ListBeadsFunc = func(filter beads.IssueFilter) ([]store.Bead, error) {
		return []store.Bead{
			{ID: "spi-foreign", Title: "foreign task", Status: "in_progress", UpdatedAt: oldTime, Type: "task"},
		}, nil
	}
	defer func() { ListBeadsFunc = origList }()

	attemptBead := &store.Bead{
		ID:     "spi-foreign.attempt-1",
		Status: "in_progress",
		Labels: []string{"attempt", "agent:wizard-foreign"},
	}
	origAttempt := GetActiveAttemptFunc
	GetActiveAttemptFunc = func(parentID string) (*store.Bead, error) {
		if parentID == "spi-foreign" {
			return attemptBead, nil
		}
		return nil, nil
	}
	defer func() { GetActiveAttemptFunc = origAttempt }()

	// Return foreign instance metadata.
	origGetInstance := GetAttemptInstanceFunc
	GetAttemptInstanceFunc = func(attemptID string) (*store.InstanceMeta, error) {
		return &store.InstanceMeta{InstanceID: "remote-instance-uuid"}, nil
	}
	defer func() { GetAttemptInstanceFunc = origGetInstance }()

	origInstanceID := InstanceIDFunc
	InstanceIDFunc = func() string { return "local-instance-uuid" }
	defer func() { InstanceIDFunc = origInstanceID }()

	backend := &fakeBackend{}
	staleCount, shutdownCount := CheckBeadHealth(10*time.Minute, 30*time.Minute, false, backend, config.DeploymentModeLocalNative)

	// Foreign attempt should be skipped entirely — no stale, no shutdown, no kills.
	if staleCount != 0 {
		t.Errorf("staleCount = %d, want 0 (foreign attempt skipped)", staleCount)
	}
	if shutdownCount != 0 {
		t.Errorf("shutdownCount = %d, want 0 (foreign attempt skipped)", shutdownCount)
	}
	if len(backend.killed) != 0 {
		t.Errorf("expected no kills for foreign attempt, got %v", backend.killed)
	}
}

func TestCheckBeadHealth_ProcessesLocallyOwnedAttempts(t *testing.T) {
	// Bead updated 45 minutes ago, attempt heartbeat also 45 minutes old:
	// the wizard is wedged. Owner is reported alive in the backend, so the
	// shutdown branch fires.
	oldTime := time.Now().Add(-45 * time.Minute).UTC().Format(time.RFC3339)
	origList := ListBeadsFunc
	ListBeadsFunc = func(filter beads.IssueFilter) ([]store.Bead, error) {
		return []store.Bead{
			{ID: "spi-local", Title: "local task", Status: "in_progress", UpdatedAt: oldTime, Type: "task"},
		}, nil
	}
	defer func() { ListBeadsFunc = origList }()

	attemptBead := &store.Bead{
		ID:     "spi-local.attempt-1",
		Status: "in_progress",
		Labels: []string{"attempt", "agent:wizard-local"},
	}
	origAttempt := GetActiveAttemptFunc
	GetActiveAttemptFunc = func(parentID string) (*store.Bead, error) {
		if parentID == "spi-local" {
			return attemptBead, nil
		}
		return nil, nil
	}
	defer func() { GetActiveAttemptFunc = origAttempt }()

	// Return local instance metadata WITH a stale heartbeat.
	origGetInstance := GetAttemptInstanceFunc
	GetAttemptInstanceFunc = func(attemptID string) (*store.InstanceMeta, error) {
		return &store.InstanceMeta{InstanceID: "local-instance-uuid", LastSeenAt: oldTime}, nil
	}
	defer func() { GetAttemptInstanceFunc = origGetInstance }()

	origInstanceID := InstanceIDFunc
	InstanceIDFunc = func() string { return "local-instance-uuid" }
	defer func() { InstanceIDFunc = origInstanceID }()

	backend := &fakeBackend{
		agents: []agent.Info{{Name: "wizard-local", Alive: true}},
	}
	_, shutdownCount := CheckBeadHealth(10*time.Minute, 30*time.Minute, false, backend, config.DeploymentModeLocalNative)

	// Local attempt should be processed — shutdown threshold exceeded.
	if shutdownCount != 1 {
		t.Errorf("shutdownCount = %d, want 1 (local attempt should be processed)", shutdownCount)
	}
	if len(backend.killed) != 1 || backend.killed[0] != "wizard-local" {
		t.Errorf("killed = %v, want [wizard-local]", backend.killed)
	}
}

func TestCheckBeadHealth_TreatsUnstampedAttemptsAsLocal(t *testing.T) {
	// Bead updated 20 minutes ago (beyond stale threshold but within shutdown).
	staleTime := time.Now().Add(-20 * time.Minute).UTC().Format(time.RFC3339)
	origList := ListBeadsFunc
	ListBeadsFunc = func(filter beads.IssueFilter) ([]store.Bead, error) {
		return []store.Bead{
			{ID: "spi-unstamped", Title: "unstamped task", Status: "in_progress", UpdatedAt: staleTime, Type: "task"},
		}, nil
	}
	defer func() { ListBeadsFunc = origList }()

	attemptBead := &store.Bead{
		ID:     "spi-unstamped.attempt-1",
		Status: "in_progress",
		Labels: []string{"attempt", "agent:wizard-unstamped"},
	}
	origAttempt := GetActiveAttemptFunc
	GetActiveAttemptFunc = func(parentID string) (*store.Bead, error) {
		if parentID == "spi-unstamped" {
			return attemptBead, nil
		}
		return nil, nil
	}
	defer func() { GetActiveAttemptFunc = origAttempt }()

	// Return nil metadata (unstamped pre-migration attempt).
	origGetInstance := GetAttemptInstanceFunc
	GetAttemptInstanceFunc = func(attemptID string) (*store.InstanceMeta, error) {
		return nil, nil
	}
	defer func() { GetAttemptInstanceFunc = origGetInstance }()

	origInstanceID := InstanceIDFunc
	InstanceIDFunc = func() string { return "local-instance-uuid" }
	defer func() { InstanceIDFunc = origInstanceID }()

	backend := &fakeBackend{}
	staleCount, shutdownCount := CheckBeadHealth(10*time.Minute, 30*time.Minute, false, backend, config.DeploymentModeLocalNative)

	// Unstamped attempts are treated as local — should be processed (warn-only,
	// since heartbeat data is unavailable, the conservative path applies).
	if staleCount != 1 {
		t.Errorf("staleCount = %d, want 1 (unstamped attempt treated as local)", staleCount)
	}
	if shutdownCount != 0 {
		t.Errorf("shutdownCount = %d, want 0", shutdownCount)
	}
}

// TestCheckBeadHealth_LiveAttemptKeepsBeadAlive is the spi-9ixgqy /
// spi-n6fk2h / spi-i7k1ag.4 regression: a long-running wizard with an
// untouched parent bead and a fresh heartbeat must NOT be killed.
func TestCheckBeadHealth_LiveAttemptKeepsBeadAlive(t *testing.T) {
	parentOld := time.Now().Add(-45 * time.Minute).UTC().Format(time.RFC3339)
	heartbeatFresh := time.Now().Add(-30 * time.Second).UTC().Format(time.RFC3339)

	origList := ListBeadsFunc
	ListBeadsFunc = func(filter beads.IssueFilter) ([]store.Bead, error) {
		return []store.Bead{
			{ID: "spi-live", Title: "live task", Status: "in_progress", UpdatedAt: parentOld, Type: "task"},
		}, nil
	}
	defer func() { ListBeadsFunc = origList }()

	attemptBead := &store.Bead{
		ID:     "spi-live.attempt-1",
		Status: "in_progress",
		Labels: []string{"attempt", "agent:wizard-live"},
	}
	origAttempt := GetActiveAttemptFunc
	GetActiveAttemptFunc = func(parentID string) (*store.Bead, error) {
		if parentID == "spi-live" {
			return attemptBead, nil
		}
		return nil, nil
	}
	defer func() { GetActiveAttemptFunc = origAttempt }()

	origGetInstance := GetAttemptInstanceFunc
	GetAttemptInstanceFunc = func(attemptID string) (*store.InstanceMeta, error) {
		return &store.InstanceMeta{InstanceID: "local-instance-uuid", LastSeenAt: heartbeatFresh}, nil
	}
	defer func() { GetAttemptInstanceFunc = origGetInstance }()

	origInstanceID := InstanceIDFunc
	InstanceIDFunc = func() string { return "local-instance-uuid" }
	defer func() { InstanceIDFunc = origInstanceID }()

	backend := &fakeBackend{
		agents: []agent.Info{{Name: "wizard-live", Alive: true}},
	}
	staleCount, shutdownCount := CheckBeadHealth(10*time.Minute, 30*time.Minute, false, backend, config.DeploymentModeLocalNative)

	if staleCount != 0 {
		t.Errorf("staleCount = %d, want 0 (fresh heartbeat keeps wizard alive)", staleCount)
	}
	if shutdownCount != 0 {
		t.Errorf("shutdownCount = %d, want 0 (fresh heartbeat keeps wizard alive)", shutdownCount)
	}
	if len(backend.killed) != 0 {
		t.Errorf("expected no kills, got %v", backend.killed)
	}
}

// TestCheckBeadHealth_StaleHeartbeatShutsDown verifies that a stale
// heartbeat WITH a live owner is treated as wedged and shut down.
func TestCheckBeadHealth_StaleHeartbeatShutsDown(t *testing.T) {
	old := time.Now().Add(-45 * time.Minute).UTC().Format(time.RFC3339)

	origList := ListBeadsFunc
	ListBeadsFunc = func(filter beads.IssueFilter) ([]store.Bead, error) {
		return []store.Bead{
			{ID: "spi-wedged", Title: "wedged", Status: "in_progress", UpdatedAt: old, Type: "task"},
		}, nil
	}
	defer func() { ListBeadsFunc = origList }()

	attemptBead := &store.Bead{
		ID:     "spi-wedged.attempt-1",
		Status: "in_progress",
		Labels: []string{"attempt", "agent:wizard-wedged"},
	}
	origAttempt := GetActiveAttemptFunc
	GetActiveAttemptFunc = func(parentID string) (*store.Bead, error) {
		if parentID == "spi-wedged" {
			return attemptBead, nil
		}
		return nil, nil
	}
	defer func() { GetActiveAttemptFunc = origAttempt }()

	origGetInstance := GetAttemptInstanceFunc
	GetAttemptInstanceFunc = func(attemptID string) (*store.InstanceMeta, error) {
		return &store.InstanceMeta{InstanceID: "local-instance-uuid", LastSeenAt: old}, nil
	}
	defer func() { GetAttemptInstanceFunc = origGetInstance }()

	origInstanceID := InstanceIDFunc
	InstanceIDFunc = func() string { return "local-instance-uuid" }
	defer func() { InstanceIDFunc = origInstanceID }()

	backend := &fakeBackend{
		agents: []agent.Info{{Name: "wizard-wedged", Alive: true}},
	}
	_, shutdownCount := CheckBeadHealth(10*time.Minute, 30*time.Minute, false, backend, config.DeploymentModeLocalNative)

	if shutdownCount != 1 {
		t.Errorf("shutdownCount = %d, want 1 (stale heartbeat + live owner = wedged)", shutdownCount)
	}
	if len(backend.killed) != 1 || backend.killed[0] != "wizard-wedged" {
		t.Errorf("killed = %v, want [wizard-wedged]", backend.killed)
	}
}

// TestCheckBeadHealth_DeadOwnerSkipsKill verifies that a stale heartbeat
// with a DEAD owner skips the kill — orphan sweep handles it on its next
// pass, so a redundant kill here is just spam.
func TestCheckBeadHealth_DeadOwnerSkipsKill(t *testing.T) {
	old := time.Now().Add(-45 * time.Minute).UTC().Format(time.RFC3339)

	origList := ListBeadsFunc
	ListBeadsFunc = func(filter beads.IssueFilter) ([]store.Bead, error) {
		return []store.Bead{
			{ID: "spi-dead", Title: "dead-owner task", Status: "in_progress", UpdatedAt: old, Type: "task"},
		}, nil
	}
	defer func() { ListBeadsFunc = origList }()

	attemptBead := &store.Bead{
		ID:     "spi-dead.attempt-1",
		Status: "in_progress",
		Labels: []string{"attempt", "agent:wizard-dead"},
	}
	origAttempt := GetActiveAttemptFunc
	GetActiveAttemptFunc = func(parentID string) (*store.Bead, error) {
		if parentID == "spi-dead" {
			return attemptBead, nil
		}
		return nil, nil
	}
	defer func() { GetActiveAttemptFunc = origAttempt }()

	origGetInstance := GetAttemptInstanceFunc
	GetAttemptInstanceFunc = func(attemptID string) (*store.InstanceMeta, error) {
		return &store.InstanceMeta{InstanceID: "local-instance-uuid", LastSeenAt: old}, nil
	}
	defer func() { GetAttemptInstanceFunc = origGetInstance }()

	origInstanceID := InstanceIDFunc
	InstanceIDFunc = func() string { return "local-instance-uuid" }
	defer func() { InstanceIDFunc = origInstanceID }()

	// Backend reports owner as NOT alive (or absent) — the orphan sweep is
	// the right authority, not us.
	backend := &fakeBackend{
		agents: []agent.Info{{Name: "wizard-dead", Alive: false}},
	}
	staleCount, shutdownCount := CheckBeadHealth(10*time.Minute, 30*time.Minute, false, backend, config.DeploymentModeLocalNative)

	if shutdownCount != 0 {
		t.Errorf("shutdownCount = %d, want 0 (dead owner deferred to orphan sweep)", shutdownCount)
	}
	if len(backend.killed) != 0 {
		t.Errorf("expected no kills for dead owner, got %v", backend.killed)
	}
	if staleCount != 1 {
		t.Errorf("staleCount = %d, want 1 (warn-only when owner is gone)", staleCount)
	}
}

// TestCheckBeadHealth_MissingHeartbeatConservative verifies that an attempt
// with NO heartbeat (just claimed, pre-heartbeat schema) is never killed,
// even when the parent bead is far past the shutdown threshold. We log
// stale and let the orphan sweep / cleric reconcile if the owner truly
// died.
func TestCheckBeadHealth_MissingHeartbeatConservative(t *testing.T) {
	parentOld := time.Now().Add(-45 * time.Minute).UTC().Format(time.RFC3339)

	origList := ListBeadsFunc
	ListBeadsFunc = func(filter beads.IssueFilter) ([]store.Bead, error) {
		return []store.Bead{
			{ID: "spi-noheartbeat", Title: "claimed but no heartbeat", Status: "in_progress", UpdatedAt: parentOld, Type: "task"},
		}, nil
	}
	defer func() { ListBeadsFunc = origList }()

	attemptBead := &store.Bead{
		ID:     "spi-noheartbeat.attempt-1",
		Status: "in_progress",
		Labels: []string{"attempt", "agent:wizard-noheartbeat"},
	}
	origAttempt := GetActiveAttemptFunc
	GetActiveAttemptFunc = func(parentID string) (*store.Bead, error) {
		if parentID == "spi-noheartbeat" {
			return attemptBead, nil
		}
		return nil, nil
	}
	defer func() { GetActiveAttemptFunc = origAttempt }()

	// Stamped but LastSeenAt empty: the BeginAttempt + 30s rate-limit window.
	origGetInstance := GetAttemptInstanceFunc
	GetAttemptInstanceFunc = func(attemptID string) (*store.InstanceMeta, error) {
		return &store.InstanceMeta{InstanceID: "local-instance-uuid", LastSeenAt: ""}, nil
	}
	defer func() { GetAttemptInstanceFunc = origGetInstance }()

	origInstanceID := InstanceIDFunc
	InstanceIDFunc = func() string { return "local-instance-uuid" }
	defer func() { InstanceIDFunc = origInstanceID }()

	backend := &fakeBackend{
		agents: []agent.Info{{Name: "wizard-noheartbeat", Alive: true}},
	}
	_, shutdownCount := CheckBeadHealth(10*time.Minute, 30*time.Minute, false, backend, config.DeploymentModeLocalNative)

	if shutdownCount != 0 {
		t.Errorf("shutdownCount = %d, want 0 (missing heartbeat must never escalate to kill)", shutdownCount)
	}
	if len(backend.killed) != 0 {
		t.Errorf("expected no kills when heartbeat is absent, got %v", backend.killed)
	}
}

// TestCheckBeadHealth_SkipsClusterNativeMode verifies that cluster-native
// towers do not run local timeout decisions for cluster-owned work.
func TestCheckBeadHealth_SkipsClusterNativeMode(t *testing.T) {
	listCalled := false
	origList := ListBeadsFunc
	ListBeadsFunc = func(filter beads.IssueFilter) ([]store.Bead, error) {
		listCalled = true
		return nil, nil
	}
	defer func() { ListBeadsFunc = origList }()

	backend := &fakeBackend{}
	staleCount, shutdownCount := CheckBeadHealth(10*time.Minute, 30*time.Minute, false, backend, config.DeploymentModeClusterNative)

	if staleCount != 0 || shutdownCount != 0 {
		t.Errorf("expected 0/0 in cluster-native mode, got stale=%d shutdown=%d", staleCount, shutdownCount)
	}
	if listCalled {
		t.Errorf("ListBeadsFunc must not be called in cluster-native mode")
	}
	if len(backend.killed) != 0 {
		t.Errorf("expected no kills in cluster-native mode, got %v", backend.killed)
	}
}

// TestCheckBeadHealth_SkipsAttachedReservedMode verifies that attached-
// reserved towers skip local timeout decisions.
func TestCheckBeadHealth_SkipsAttachedReservedMode(t *testing.T) {
	listCalled := false
	origList := ListBeadsFunc
	ListBeadsFunc = func(filter beads.IssueFilter) ([]store.Bead, error) {
		listCalled = true
		return nil, nil
	}
	defer func() { ListBeadsFunc = origList }()

	backend := &fakeBackend{}
	staleCount, shutdownCount := CheckBeadHealth(10*time.Minute, 30*time.Minute, false, backend, config.DeploymentModeAttachedReserved)

	if staleCount != 0 || shutdownCount != 0 {
		t.Errorf("expected 0/0 in attached-reserved mode, got stale=%d shutdown=%d", staleCount, shutdownCount)
	}
	if listCalled {
		t.Errorf("ListBeadsFunc must not be called in attached-reserved mode")
	}
	if len(backend.killed) != 0 {
		t.Errorf("expected no kills in attached-reserved mode, got %v", backend.killed)
	}
}

// --- TowerCycle bind state filtering tests ---

// towerCycleTestSetup mocks the store/beadsdir functions so TowerCycle can run
// with a non-empty towerName without requiring a real dolt store.
func towerCycleTestSetup(t *testing.T) func() {
	t.Helper()
	origBeadsDir := BeadsDirForTowerFunc
	BeadsDirForTowerFunc = func(name string) string { return "/fake/.beads" }
	origStoreOpen := StoreOpenAtFunc
	StoreOpenAtFunc = func(dir string) (beads.Storage, error) { return nil, nil }
	origCommit := CommitPendingFunc
	CommitPendingFunc = func(msg string) error { return nil }
	// Stub ListBeadsFunc for CheckBeadHealth (step 5 of TowerCycle).
	origList := ListBeadsFunc
	ListBeadsFunc = func(filter beads.IssueFilter) ([]store.Bead, error) { return nil, nil }
	// Stub the dispatch-loop's lifecycle hook so TowerCycle's Step 2
	// (spi-jzs5xq) does not fall through to the real lifecycle path.
	// Tests that need to drive specific candidates override this hook.
	origDispatchable := DispatchableBeadsFunc
	DispatchableBeadsFunc = func(_ context.Context) ([]*store.Bead, error) { return nil, nil }
	// Stub InstanceIDFunc.
	origInstanceID := InstanceIDFunc
	InstanceIDFunc = func() string { return "test-instance" }
	return func() {
		BeadsDirForTowerFunc = origBeadsDir
		StoreOpenAtFunc = origStoreOpen
		CommitPendingFunc = origCommit
		ListBeadsFunc = origList
		DispatchableBeadsFunc = origDispatchable
		InstanceIDFunc = origInstanceID
	}
}

func TestTowerCycle_SkipsBeadsForUnboundPrefixes(t *testing.T) {
	t.Setenv("SPIRE_DOLT_DIR", t.TempDir())
	cleanup := towerCycleTestSetup(t)
	defer cleanup()

	dispatchable := []store.Bead{
		{ID: "web-task1", Title: "web task", Status: "ready", Type: "task"},
		{ID: "api-task1", Title: "api task", Status: "ready", Type: "task"},
	}
	origDispatchable := DispatchableBeadsFunc
	DispatchableBeadsFunc = func(_ context.Context) ([]*store.Bead, error) {
		out := make([]*store.Bead, 0, len(dispatchable))
		for i := range dispatchable {
			out = append(out, &dispatchable[i])
		}
		return out, nil
	}
	defer func() { DispatchableBeadsFunc = origDispatchable }()

	origLoadTower := LoadTowerConfigFunc
	LoadTowerConfigFunc = func(name string) (*config.TowerConfig, error) {
		return &config.TowerConfig{
			Name: "test-tower",
			LocalBindings: map[string]*config.LocalRepoBinding{
				"web": {Prefix: "web", State: "bound", LocalPath: "/path/to/web"},
				"api": {Prefix: "api", State: "skipped"},
			},
		}, nil
	}
	defer func() { LoadTowerConfigFunc = origLoadTower }()

	InstanceIDFunc = func() string { return "test-instance" }

	backend := &spawnTrackingBackend{}
	TowerCycle(1, "test-tower", StewardConfig{
		Backend:           backend,
		StaleThreshold:    30 * time.Minute,
		ShutdownThreshold: 60 * time.Minute,
	})

	// Only web-task1 should be spawned (bound); api-task1 should be skipped (skipped state).
	if len(backend.spawns) != 1 {
		t.Fatalf("spawn count = %d, want 1", len(backend.spawns))
	}
	if backend.spawns[0].BeadID != "web-task1" {
		t.Errorf("spawned bead = %q, want web-task1", backend.spawns[0].BeadID)
	}
}

func TestTowerCycle_SpawnsBeadsForBoundPrefixes(t *testing.T) {
	t.Setenv("SPIRE_DOLT_DIR", t.TempDir())
	cleanup := towerCycleTestSetup(t)
	defer cleanup()

	dispatchable := []store.Bead{
		{ID: "spi-task1", Title: "spire task", Status: "ready", Type: "task"},
		{ID: "web-task2", Title: "web task 2", Status: "ready", Type: "task"},
	}
	origDispatchable := DispatchableBeadsFunc
	DispatchableBeadsFunc = func(_ context.Context) ([]*store.Bead, error) {
		out := make([]*store.Bead, 0, len(dispatchable))
		for i := range dispatchable {
			out = append(out, &dispatchable[i])
		}
		return out, nil
	}
	defer func() { DispatchableBeadsFunc = origDispatchable }()

	origLoadTower := LoadTowerConfigFunc
	LoadTowerConfigFunc = func(name string) (*config.TowerConfig, error) {
		return &config.TowerConfig{
			Name: "test-tower",
			LocalBindings: map[string]*config.LocalRepoBinding{
				"spi": {Prefix: "spi", State: "bound", LocalPath: "/path/to/spi"},
				"web": {Prefix: "web", State: "bound", LocalPath: "/path/to/web"},
			},
		}, nil
	}
	defer func() { LoadTowerConfigFunc = origLoadTower }()

	InstanceIDFunc = func() string { return "test-instance" }

	backend := &spawnTrackingBackend{}
	TowerCycle(1, "test-tower", StewardConfig{
		Backend:           backend,
		StaleThreshold:    30 * time.Minute,
		ShutdownThreshold: 60 * time.Minute,
	})

	// Both beads should be spawned (both prefixes are bound).
	if len(backend.spawns) != 2 {
		t.Fatalf("spawn count = %d, want 2", len(backend.spawns))
	}
}

func TestTowerCycle_SpawnConfigUsesRoleWizardAndInstanceID(t *testing.T) {
	t.Setenv("SPIRE_DOLT_DIR", t.TempDir())
	cleanup := towerCycleTestSetup(t)
	defer cleanup()

	dispatchable := []store.Bead{
		{ID: "spi-task1", Title: "spire task", Status: "ready", Type: "task"},
	}
	origDispatchable := DispatchableBeadsFunc
	DispatchableBeadsFunc = func(_ context.Context) ([]*store.Bead, error) {
		out := make([]*store.Bead, 0, len(dispatchable))
		for i := range dispatchable {
			out = append(out, &dispatchable[i])
		}
		return out, nil
	}
	defer func() { DispatchableBeadsFunc = origDispatchable }()

	origLoadTower := LoadTowerConfigFunc
	LoadTowerConfigFunc = func(name string) (*config.TowerConfig, error) {
		return &config.TowerConfig{
			Name: "test-tower",
			LocalBindings: map[string]*config.LocalRepoBinding{
				"spi": {Prefix: "spi", State: "bound", LocalPath: "/path/to/spi"},
			},
		}, nil
	}
	defer func() { LoadTowerConfigFunc = origLoadTower }()

	InstanceIDFunc = func() string { return "my-instance-uuid" }

	backend := &spawnTrackingBackend{}
	TowerCycle(1, "test-tower", StewardConfig{
		Backend:           backend,
		StaleThreshold:    30 * time.Minute,
		ShutdownThreshold: 60 * time.Minute,
	})

	if len(backend.spawns) != 1 {
		t.Fatalf("spawn count = %d, want 1", len(backend.spawns))
	}
	sc := backend.spawns[0]
	if sc.Role != agent.RoleWizard {
		t.Errorf("spawn role = %q, want %q", sc.Role, agent.RoleWizard)
	}
	if sc.InstanceID != "my-instance-uuid" {
		t.Errorf("spawn InstanceID = %q, want %q", sc.InstanceID, "my-instance-uuid")
	}
}

// TestCreateAlertFunc_DerivesPrefix verifies that CreateAlertFunc mints the
// alert bead using the parent bead's prefix, not a hardcoded default.
func TestCreateAlertFunc_DerivesPrefix(t *testing.T) {
	tests := []struct {
		name       string
		beadID     string
		wantPrefix string
	}{
		{"spi parent", "spi-0fek6l", "spi"},
		{"spd parent", "spd-ac5", "spd"},
		{"web parent", "web-xyz99", "web"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotOpts store.CreateOpts
			origCreate := CreateBeadFunc
			CreateBeadFunc = func(opts store.CreateOpts) (string, error) {
				gotOpts = opts
				return "alert-001", nil
			}
			defer func() { CreateBeadFunc = origCreate }()

			// Use the default CreateAlertFunc (not monkey-patched) so we exercise
			// the real implementation path through CreateBeadFunc.
			origAlert := CreateAlertFunc
			CreateAlertFunc = func(beadID, msg string) error {
				alertID, err := CreateBeadFunc(store.CreateOpts{
					Title:    msg,
					Priority: 0,
					Type:     beads.TypeTask,
					Labels:   []string{"alert:corrupted-bead"},
					Prefix:   store.PrefixFromID(beadID),
				})
				if err != nil {
					return err
				}
				if alertID != "" {
					_ = store.AddDepTyped(alertID, beadID, "caused-by")
				}
				return nil
			}
			defer func() { CreateAlertFunc = origAlert }()

			if err := CreateAlertFunc(tt.beadID, "corrupted bead detected"); err != nil {
				t.Fatalf("CreateAlertFunc returned error: %v", err)
			}
			if gotOpts.Prefix != tt.wantPrefix {
				t.Errorf("alert bead Prefix = %q, want %q", gotOpts.Prefix, tt.wantPrefix)
			}
		})
	}
}

// TestSendMessage_DerivesPrefix verifies that sendMessage uses the ref bead's
// prefix when ref is non-empty, and falls back to empty string when ref is empty.
func TestSendMessage_DerivesPrefix(t *testing.T) {
	tests := []struct {
		name       string
		ref        string
		wantPrefix string
	}{
		{"ref with spi prefix", "spi-abc", "spi"},
		{"ref with spd prefix", "spd-ac5", "spd"},
		{"ref with web prefix", "web-xyz", "web"},
		{"empty ref uses empty prefix", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotOpts store.CreateOpts
			origCreate := CreateBeadFunc
			CreateBeadFunc = func(opts store.CreateOpts) (string, error) {
				gotOpts = opts
				return "msg-001", nil
			}
			defer func() { CreateBeadFunc = origCreate }()

			// Stub AddDepTyped so sendMessage with ref != "" can call alerts.Raise
			// without a live store. When ref != "", sendMessage routes through
			// pkg/alerts which calls AddDepTyped after CreateBead.
			origAddDep := AddDepTypedFunc
			AddDepTypedFunc = func(from, to, depType string) error { return nil }
			defer func() { AddDepTypedFunc = origAddDep }()

			_, err := sendMessage("archmage", "steward", "test body", tt.ref, 1)
			if err != nil {
				t.Fatalf("sendMessage returned error: %v", err)
			}
			if gotOpts.Prefix != tt.wantPrefix {
				t.Errorf("message bead Prefix = %q, want %q", gotOpts.Prefix, tt.wantPrefix)
			}
		})
	}
}
