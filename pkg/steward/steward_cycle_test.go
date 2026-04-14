package steward

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/awell-health/spire/pkg/agent"
	"github.com/awell-health/spire/pkg/config"
	"github.com/awell-health/spire/pkg/store"
	"github.com/steveyegge/beads"
)

// --- End-to-end TowerCycle tests using function var mocks ---

// cycleBackend records Spawn calls and returns configurable agents from List.
type cycleBackend struct {
	spawns   []agent.SpawnConfig
	agents   []agent.Info
	listErr  error
	spawnErr error
}

func (b *cycleBackend) Spawn(cfg agent.SpawnConfig) (agent.Handle, error) {
	if b.spawnErr != nil {
		return nil, b.spawnErr
	}
	b.spawns = append(b.spawns, cfg)
	return &fakeHandle{id: cfg.Name}, nil
}
func (b *cycleBackend) List() ([]agent.Info, error)              { return b.agents, b.listErr }
func (b *cycleBackend) Logs(name string) (io.ReadCloser, error)  { return nil, os.ErrNotExist }
func (b *cycleBackend) Kill(name string) error                   { return nil }

// saveCycleFuncVars saves all test-replaceable function vars and restores them on cleanup.
func saveCycleFuncVars(t *testing.T) {
	t.Helper()
	origCommitPending := CommitPendingFunc
	origGetSchedulable := GetSchedulableWorkFunc
	origLoadTowerConfig := LoadTowerConfigFunc
	origGetDB := GetDBForRoutingFunc
	origAddLabel := AddLabelFunc
	origListBeads := ListBeadsFunc
	origExecuteMerge := ExecuteMergeFunc
	origGetActiveAttempt := GetActiveAttemptFunc
	origRaiseAlert := RaiseCorruptedBeadAlertFunc
	origGetChildren := GetChildrenFunc
	origGetBead := GetBeadFunc
	origGetComments := GetCommentsFunc
	origRemoveLabel := RemoveLabelFunc
	origSendMessage := SendMessageFunc
	origConfigLoad := ConfigLoadFunc

	t.Cleanup(func() {
		CommitPendingFunc = origCommitPending
		GetSchedulableWorkFunc = origGetSchedulable
		LoadTowerConfigFunc = origLoadTowerConfig
		GetDBForRoutingFunc = origGetDB
		AddLabelFunc = origAddLabel
		ListBeadsFunc = origListBeads
		ExecuteMergeFunc = origExecuteMerge
		GetActiveAttemptFunc = origGetActiveAttempt
		RaiseCorruptedBeadAlertFunc = origRaiseAlert
		GetChildrenFunc = origGetChildren
		GetBeadFunc = origGetBead
		GetCommentsFunc = origGetComments
		RemoveLabelFunc = origRemoveLabel
		SendMessageFunc = origSendMessage
		ConfigLoadFunc = origConfigLoad
	})
}

// setupCycleFuncMocks installs mocks for TowerCycle to run without a real store.
func setupCycleFuncMocks(t *testing.T, schedulable []store.Bead, maxConcurrent int) {
	t.Helper()
	saveCycleFuncVars(t)

	CommitPendingFunc = func(msg string) error { return nil }
	GetSchedulableWorkFunc = func(filter beads.WorkFilter) (*store.ScheduleResult, error) {
		return &store.ScheduleResult{Schedulable: schedulable}, nil
	}
	LoadTowerConfigFunc = func(name string) (*config.TowerConfig, error) {
		return &config.TowerConfig{MaxConcurrent: maxConcurrent}, nil
	}
	GetDBForRoutingFunc = func(dbName string) *sql.DB { return nil }
	AddLabelFunc = func(id, label string) error { return nil }
	ListBeadsFunc = func(filter beads.IssueFilter) ([]store.Bead, error) { return nil, nil }
	GetActiveAttemptFunc = func(parentID string) (*store.Bead, error) { return nil, nil }
	RaiseCorruptedBeadAlertFunc = func(beadID string, err error) {}
	GetChildrenFunc = func(parentID string) ([]store.Bead, error) { return nil, nil }
	GetBeadFunc = func(id string) (store.Bead, error) { return store.Bead{}, fmt.Errorf("not found") }
	GetCommentsFunc = func(id string) ([]*beads.Comment, error) { return nil, nil }
	RemoveLabelFunc = func(id, label string) error { return nil }
	SendMessageFunc = func(to, from, body, ref string, priority int) (string, error) { return "", nil }
	ExecuteMergeFunc = func(ctx context.Context, req MergeRequest) MergeResult {
		return MergeResult{BeadID: req.BeadID, Success: true, SHA: "abc123"}
	}
	ConfigLoadFunc = func() (*config.SpireConfig, error) {
		return &config.SpireConfig{Instances: make(map[string]*config.Instance)}, nil
	}
}

func testBeads(ids ...string) []store.Bead {
	var out []store.Bead
	for _, id := range ids {
		out = append(out, store.Bead{ID: id, Title: "task " + id, Status: "open", Type: "task"})
	}
	return out
}

// TestE2E_CycleRespectsMaxConcurrent verifies that TowerCycle spawns at most
// MaxConcurrent wizards even when more work is available.
func TestE2E_CycleRespectsMaxConcurrent(t *testing.T) {
	backend := &cycleBackend{}
	setupCycleFuncMocks(t, testBeads("spi-a", "spi-b", "spi-c", "spi-d", "spi-e"), 2)

	cfg := StewardConfig{
		Backend:            backend,
		ConcurrencyLimiter: NewConcurrencyLimiter(),
		StaleThreshold:     10 * time.Minute,
		ShutdownThreshold:  15 * time.Minute,
	}

	TowerCycle(1, "", cfg)

	if len(backend.spawns) != 2 {
		t.Errorf("expected 2 spawns (max_concurrent=2), got %d", len(backend.spawns))
	}
}

// TestE2E_WizardNaming verifies that spawned wizards get sanitized names.
func TestE2E_WizardNaming(t *testing.T) {
	backend := &cycleBackend{}
	setupCycleFuncMocks(t, testBeads("spi-abc.1"), 0)

	cfg := StewardConfig{
		Backend:           backend,
		StaleThreshold:    10 * time.Minute,
		ShutdownThreshold: 15 * time.Minute,
	}

	TowerCycle(1, "", cfg)

	if len(backend.spawns) != 1 {
		t.Fatalf("expected 1 spawn, got %d", len(backend.spawns))
	}
	got := backend.spawns[0].Name
	want := "wizard-spi-abc-1"
	if got != want {
		t.Errorf("wizard name = %q, want %q (dot sanitized to hyphen)", got, want)
	}
	if backend.spawns[0].Role != agent.RoleExecutor {
		t.Errorf("spawn role = %q, want %q", backend.spawns[0].Role, agent.RoleExecutor)
	}
	if backend.spawns[0].BeadID != "spi-abc.1" {
		t.Errorf("spawn beadID = %q, want %q", backend.spawns[0].BeadID, "spi-abc.1")
	}
}

// TestE2E_MergeQueueProcessesOne verifies merge queue processes exactly one
// merge per cycle (serialization guarantee).
func TestE2E_MergeQueueProcessesOne(t *testing.T) {
	backend := &cycleBackend{}
	setupCycleFuncMocks(t, nil, 0)

	processCount := 0
	ExecuteMergeFunc = func(ctx context.Context, req MergeRequest) MergeResult {
		processCount++
		return MergeResult{BeadID: req.BeadID, Success: true, SHA: "sha-" + req.BeadID}
	}

	mq := NewMergeQueue()
	mq.Enqueue(MergeRequest{BeadID: "spi-1", Branch: "feat/spi-1", BaseBranch: "main"})
	mq.Enqueue(MergeRequest{BeadID: "spi-2", Branch: "feat/spi-2", BaseBranch: "main"})
	mq.Enqueue(MergeRequest{BeadID: "spi-3", Branch: "feat/spi-3", BaseBranch: "main"})

	cfg := StewardConfig{
		Backend:           backend,
		MergeQueue:        mq,
		StaleThreshold:    10 * time.Minute,
		ShutdownThreshold: 15 * time.Minute,
	}

	TowerCycle(1, "", cfg)

	if processCount != 1 {
		t.Errorf("expected merge queue to process exactly 1, got %d", processCount)
	}
	if mq.Depth() != 2 {
		t.Errorf("expected 2 remaining in queue, got %d", mq.Depth())
	}
}

// TestE2E_CycleStatsPopulated verifies cycle stats are recorded at end of TowerCycle.
func TestE2E_CycleStatsPopulated(t *testing.T) {
	backend := &cycleBackend{
		agents: []agent.Info{
			{Name: "wizard-existing", Alive: true},
		},
	}
	// 1 alive agent + max_concurrent=3 → 2 slots available → spawn 2 of 3.
	setupCycleFuncMocks(t, testBeads("spi-x", "spi-y", "spi-z"), 3)

	cs := NewCycleStats()
	cfg := StewardConfig{
		Backend:            backend,
		ConcurrencyLimiter: NewConcurrencyLimiter(),
		CycleStats:         cs,
		StaleThreshold:     10 * time.Minute,
		ShutdownThreshold:  15 * time.Minute,
	}

	TowerCycle(1, "", cfg)

	snap := cs.Snapshot()
	if snap.SchedulableWork != 3 {
		t.Errorf("SchedulableWork = %d, want 3", snap.SchedulableWork)
	}
	if snap.SpawnedThisCycle != 2 {
		t.Errorf("SpawnedThisCycle = %d, want 2 (max_concurrent=3, 1 alive = 2 slots)", snap.SpawnedThisCycle)
	}
	if snap.ActiveAgents != 1 {
		t.Errorf("ActiveAgents = %d, want 1 (one alive agent in mock)", snap.ActiveAgents)
	}
	if snap.CycleDuration <= 0 {
		t.Errorf("CycleDuration = %s, want > 0", snap.CycleDuration)
	}
	if snap.LastCycleAt.IsZero() {
		t.Error("LastCycleAt is zero, want non-zero")
	}
	if snap.Tower != "" {
		t.Errorf("Tower = %q, want empty (towerName=\"\")", snap.Tower)
	}
}

// TestE2E_DryRunNoSpawn verifies DryRun mode logs but does not spawn.
func TestE2E_DryRunNoSpawn(t *testing.T) {
	backend := &cycleBackend{}
	setupCycleFuncMocks(t, testBeads("spi-1", "spi-2", "spi-3"), 0)

	cfg := StewardConfig{
		DryRun:            true,
		Backend:           backend,
		StaleThreshold:    10 * time.Minute,
		ShutdownThreshold: 15 * time.Minute,
	}

	TowerCycle(1, "", cfg)

	if len(backend.spawns) != 0 {
		t.Errorf("expected 0 spawns in dry-run mode, got %d", len(backend.spawns))
	}
}

// TestE2E_NilModulesGracefulDegradation verifies TowerCycle works when all
// wave-0 modules are nil.
func TestE2E_NilModulesGracefulDegradation(t *testing.T) {
	backend := &cycleBackend{}
	setupCycleFuncMocks(t, testBeads("spi-ok"), 0)

	cfg := StewardConfig{
		Backend:           backend,
		StaleThreshold:    10 * time.Minute,
		ShutdownThreshold: 15 * time.Minute,
	}

	TowerCycle(1, "", cfg)

	// With no concurrency limiter and maxConcurrent=0 (unlimited), all beads spawn.
	if len(backend.spawns) != 1 {
		t.Errorf("expected 1 spawn with nil modules, got %d", len(backend.spawns))
	}
}

// TestE2E_SpawnConfigFields verifies correct SpawnConfig fields.
func TestE2E_SpawnConfigFields(t *testing.T) {
	backend := &cycleBackend{}
	setupCycleFuncMocks(t, testBeads("spi-test"), 0)

	cfg := StewardConfig{
		Backend:           backend,
		StaleThreshold:    10 * time.Minute,
		ShutdownThreshold: 15 * time.Minute,
	}

	TowerCycle(1, "", cfg)

	if len(backend.spawns) != 1 {
		t.Fatalf("expected 1 spawn, got %d", len(backend.spawns))
	}
	sc := backend.spawns[0]
	if sc.BeadID != "spi-test" {
		t.Errorf("SpawnConfig.BeadID = %q, want %q", sc.BeadID, "spi-test")
	}
	if sc.Role != agent.RoleExecutor {
		t.Errorf("SpawnConfig.Role = %q, want %q", sc.Role, agent.RoleExecutor)
	}
	if !strings.HasPrefix(sc.Name, "wizard-") {
		t.Errorf("SpawnConfig.Name = %q, want prefix 'wizard-'", sc.Name)
	}
	if sc.LogPath == "" {
		t.Error("SpawnConfig.LogPath is empty, expected non-empty")
	}
}

// TestE2E_ABRoutingSkippedWithoutDB verifies AB routing is gracefully skipped
// when no DB connection is available.
func TestE2E_ABRoutingSkippedWithoutDB(t *testing.T) {
	backend := &cycleBackend{}
	setupCycleFuncMocks(t, testBeads("spi-route"), 0)

	var labelCalls []string
	AddLabelFunc = func(id, label string) error {
		labelCalls = append(labelCalls, id+":"+label)
		return nil
	}
	GetDBForRoutingFunc = func(dbName string) *sql.DB { return nil }

	cfg := StewardConfig{
		Backend:           backend,
		ABRouter:          NewABRouter(),
		StaleThreshold:    10 * time.Minute,
		ShutdownThreshold: 15 * time.Minute,
	}

	TowerCycle(1, "", cfg)

	// With nil DB, AB routing is skipped — no labels should be added.
	if len(labelCalls) != 0 {
		t.Errorf("expected no label calls with nil DB, got %d: %v", len(labelCalls), labelCalls)
	}
	// But the bead should still be spawned.
	if len(backend.spawns) != 1 {
		t.Errorf("expected 1 spawn, got %d", len(backend.spawns))
	}
}

// TestE2E_MergeFailureProcessed verifies merge failures are handled without panic.
func TestE2E_MergeFailureProcessed(t *testing.T) {
	backend := &cycleBackend{}
	setupCycleFuncMocks(t, nil, 0)

	ExecuteMergeFunc = func(ctx context.Context, req MergeRequest) MergeResult {
		return MergeResult{BeadID: req.BeadID, Success: false, Error: fmt.Errorf("rebase conflict")}
	}

	mq := NewMergeQueue()
	mq.Enqueue(MergeRequest{BeadID: "spi-fail", Branch: "feat/spi-fail", BaseBranch: "main"})

	cfg := StewardConfig{
		Backend:           backend,
		MergeQueue:        mq,
		StaleThreshold:    10 * time.Minute,
		ShutdownThreshold: 15 * time.Minute,
	}

	TowerCycle(1, "", cfg)

	if mq.Depth() != 0 {
		t.Errorf("expected queue empty after processing (even on failure), got depth %d", mq.Depth())
	}
}

// TestE2E_DetectMergeReady_EnqueueAndProcess verifies that a full cycle with a
// review-approved bead results in DetectMergeReady enqueueing and ProcessNext
// calling ExecuteMergeFunc.
func TestE2E_DetectMergeReady_EnqueueAndProcess(t *testing.T) {
	backend := &cycleBackend{}
	setupCycleFuncMocks(t, nil, 0) // no schedulable work

	// Override ListBeadsFunc to return a review-approved bead for DetectMergeReady.
	ListBeadsFunc = func(filter beads.IssueFilter) ([]store.Bead, error) {
		return []store.Bead{
			{
				ID:     "spi-merge1",
				Status: "in_progress",
				Type:   "task",
				Labels: []string{"review-approved", "feat-branch:feat/spi-merge1"},
			},
		}, nil
	}

	// Override ConfigLoadFunc to return a config with the repo registered.
	ConfigLoadFunc = func() (*config.SpireConfig, error) {
		return &config.SpireConfig{
			Instances: map[string]*config.Instance{
				"spi": {Path: "/repos/spire", Prefix: "spi"},
			},
		}, nil
	}

	var mergedBeadIDs []string
	ExecuteMergeFunc = func(ctx context.Context, req MergeRequest) MergeResult {
		mergedBeadIDs = append(mergedBeadIDs, req.BeadID)
		return MergeResult{BeadID: req.BeadID, Success: true, SHA: "merged-sha"}
	}

	mq := NewMergeQueue()
	cfg := StewardConfig{
		Backend:           backend,
		MergeQueue:        mq,
		StaleThreshold:    10 * time.Minute,
		ShutdownThreshold: 15 * time.Minute,
	}

	TowerCycle(1, "", cfg)

	// DetectMergeReady should have enqueued, ProcessNext should have processed.
	if len(mergedBeadIDs) != 1 {
		t.Fatalf("expected 1 merge execution, got %d", len(mergedBeadIDs))
	}
	if mergedBeadIDs[0] != "spi-merge1" {
		t.Errorf("merged bead = %q, want %q", mergedBeadIDs[0], "spi-merge1")
	}
	// Queue should be empty after processing.
	if mq.Depth() != 0 {
		t.Errorf("expected queue depth 0 after processing, got %d", mq.Depth())
	}
}

// TestE2E_SpawnError verifies that a spawn failure for one bead doesn't block others.
func TestE2E_SpawnError(t *testing.T) {
	backend := &cycleBackend{
		spawnErr: fmt.Errorf("k8s quota exceeded"),
	}
	setupCycleFuncMocks(t, testBeads("spi-a", "spi-b"), 0)

	cfg := StewardConfig{
		Backend:           backend,
		StaleThreshold:    10 * time.Minute,
		ShutdownThreshold: 15 * time.Minute,
	}

	TowerCycle(1, "", cfg)

	// Spawns should be empty because all spawns fail.
	if len(backend.spawns) != 0 {
		t.Errorf("expected 0 spawns (all fail), got %d", len(backend.spawns))
	}
}

// TestE2E_EmptySchedulable verifies TowerCycle completes with no work.
func TestE2E_EmptySchedulable(t *testing.T) {
	backend := &cycleBackend{}
	setupCycleFuncMocks(t, nil, 0)

	cs := NewCycleStats()
	cfg := StewardConfig{
		Backend:           backend,
		CycleStats:        cs,
		StaleThreshold:    10 * time.Minute,
		ShutdownThreshold: 15 * time.Minute,
	}

	TowerCycle(1, "", cfg)

	if len(backend.spawns) != 0 {
		t.Errorf("expected 0 spawns with no schedulable work, got %d", len(backend.spawns))
	}
	snap := cs.Snapshot()
	if snap.SchedulableWork != 0 {
		t.Errorf("SchedulableWork = %d, want 0", snap.SchedulableWork)
	}
	if snap.SpawnedThisCycle != 0 {
		t.Errorf("SpawnedThisCycle = %d, want 0", snap.SpawnedThisCycle)
	}
}
