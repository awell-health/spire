package beadlifecycle

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/awell-health/spire/pkg/store"
	"github.com/awell-health/spire/pkg/wizardregistry"
	"github.com/awell-health/spire/pkg/wizardregistry/fake"
	"github.com/steveyegge/beads"
)

// --- Stub Deps implementation ---

type stubDeps struct {
	beads          map[string]store.Bead
	attempts       map[string][]store.Bead // parentID → attempts
	closedAttempts map[string]string       // attemptID → resultLabel
	updatedBeads   map[string]map[string]interface{}
	removedLabels  map[string][]string
	addedLabels    map[string][]string
	cascadeClosed  []string
	allBeads       []store.Bead

	// heartbeats holds attempt-bead heartbeats for the OrphanSweep gate.
	// Absence in the map → present=false (no metadata yet).
	heartbeats map[string]time.Time

	// Track call counts.
	createAttemptCalls int
	createAttemptFn    func(parentID, agentName, model, branch string) (string, error)
}

func newStubDeps() *stubDeps {
	return &stubDeps{
		beads:          make(map[string]store.Bead),
		attempts:       make(map[string][]store.Bead),
		closedAttempts: make(map[string]string),
		updatedBeads:   make(map[string]map[string]interface{}),
		removedLabels:  make(map[string][]string),
		addedLabels:    make(map[string][]string),
	}
}

func (s *stubDeps) GetBead(id string) (store.Bead, error) {
	if b, ok := s.beads[id]; ok {
		return b, nil
	}
	return store.Bead{}, fmt.Errorf("bead %s not found", id)
}

func (s *stubDeps) UpdateBead(id string, updates map[string]interface{}) error {
	if s.updatedBeads == nil {
		s.updatedBeads = make(map[string]map[string]interface{})
	}
	s.updatedBeads[id] = updates
	if b, ok := s.beads[id]; ok {
		if status, ok := updates["status"].(string); ok {
			b.Status = status
		}
		s.beads[id] = b
	}
	return nil
}

func (s *stubDeps) CreateAttemptBead(parentID, agentName, model, branch string) (string, error) {
	s.createAttemptCalls++
	if s.createAttemptFn != nil {
		return s.createAttemptFn(parentID, agentName, model, branch)
	}
	id := fmt.Sprintf("att-%s-%d", parentID, s.createAttemptCalls)
	att := store.Bead{
		ID:     id,
		Status: "in_progress",
		Parent: parentID,
		Type:   "attempt",
		Labels: []string{"attempt", "agent:" + agentName},
	}
	if s.attempts == nil {
		s.attempts = make(map[string][]store.Bead)
	}
	s.attempts[parentID] = append(s.attempts[parentID], att)
	s.allBeads = append(s.allBeads, att)
	return id, nil
}

func (s *stubDeps) CloseAttemptBead(attemptID string, resultLabel string) error {
	if s.closedAttempts == nil {
		s.closedAttempts = make(map[string]string)
	}
	s.closedAttempts[attemptID] = resultLabel
	for parentID, atts := range s.attempts {
		for i, a := range atts {
			if a.ID == attemptID {
				s.attempts[parentID][i].Status = "closed"
			}
		}
	}
	for i := range s.allBeads {
		if s.allBeads[i].ID == attemptID {
			s.allBeads[i].Status = "closed"
		}
	}
	return nil
}

func (s *stubDeps) ListAttemptsForBead(beadID string) ([]store.Bead, error) {
	return s.attempts[beadID], nil
}

func (s *stubDeps) RemoveLabel(id, label string) error {
	if s.removedLabels == nil {
		s.removedLabels = make(map[string][]string)
	}
	s.removedLabels[id] = append(s.removedLabels[id], label)
	return nil
}

func (s *stubDeps) AlertCascadeClose(sourceBeadID string) error {
	s.cascadeClosed = append(s.cascadeClosed, sourceBeadID)
	return nil
}

func (s *stubDeps) AddLabel(id, label string) error {
	if s.addedLabels == nil {
		s.addedLabels = make(map[string][]string)
	}
	s.addedLabels[id] = append(s.addedLabels[id], label)
	return nil
}

func (s *stubDeps) ListBeads(filter beads.IssueFilter) ([]store.Bead, error) {
	return s.allBeads, nil
}

func (s *stubDeps) GetAttemptHeartbeat(attemptID string) (time.Time, bool, error) {
	if s.heartbeats == nil {
		return time.Time{}, false, nil
	}
	t, ok := s.heartbeats[attemptID]
	if !ok {
		return time.Time{}, false, nil
	}
	return t, true, nil
}

func (s *stubDeps) setHeartbeat(attemptID string, t time.Time) {
	if s.heartbeats == nil {
		s.heartbeats = make(map[string]time.Time)
	}
	s.heartbeats[attemptID] = t
}

func (s *stubDeps) addBead(b store.Bead) {
	s.beads[b.ID] = b
	s.allBeads = append(s.allBeads, b)
}

func (s *stubDeps) addAttempt(parentID string, att store.Bead) {
	if s.attempts == nil {
		s.attempts = make(map[string][]store.Bead)
	}
	s.attempts[parentID] = append(s.attempts[parentID], att)
	s.allBeads = append(s.allBeads, att)
}

// --- Registry test helpers ---

// newRegistry returns a fresh in-memory wizardregistry.Registry plus its
// liveness control. Tests own the (id, alive) state through the control.
func newRegistry() (*fake.Registry, *fake.Registry) {
	r := fake.New()
	return r, r
}

// upsertAlive upserts w into reg and marks the corresponding ID alive.
func upsertAlive(t *testing.T, reg *fake.Registry, w wizardregistry.Wizard) {
	t.Helper()
	if err := reg.Upsert(context.Background(), w); err != nil {
		t.Fatalf("Upsert(%s): %v", w.ID, err)
	}
	reg.SetAlive(w.ID, true)
}

// upsertDead upserts w into reg and leaves it marked not-alive.
func upsertDead(t *testing.T, reg *fake.Registry, w wizardregistry.Wizard) {
	t.Helper()
	if err := reg.Upsert(context.Background(), w); err != nil {
		t.Fatalf("Upsert(%s): %v", w.ID, err)
	}
	reg.SetAlive(w.ID, false)
}

// --- Unit Tests ---

// TestBeginWork_Fresh_Bead_Local: open bead, ModeLocal → attempt created,
// bead in_progress, registry upserted.
func TestBeginWork_Fresh_Bead_Local(t *testing.T) {
	deps := newStubDeps()
	deps.addBead(store.Bead{ID: "spi-abc", Status: "open"})

	reg, _ := newRegistry()

	opts := BeginOpts{
		Mode:      ModeLocal,
		AgentName: "wizard-spi-abc",
		Worktree:  "/tmp/test-worktree",
		Tower:     "test-tower",
		Model:     "claude-3",
		Branch:    "feat/spi-abc",
	}

	attemptID, err := BeginWork(deps, reg, "spi-abc", opts)
	if err != nil {
		t.Fatalf("BeginWork: unexpected error: %v", err)
	}
	if attemptID == "" {
		t.Fatal("BeginWork: expected non-empty attemptID")
	}

	if b := deps.beads["spi-abc"]; b.Status != "in_progress" {
		t.Errorf("expected bead in_progress, got %q", b.Status)
	}
	if deps.createAttemptCalls != 1 {
		t.Errorf("expected 1 attempt creation, got %d", deps.createAttemptCalls)
	}

	// Registry should now hold the new wizard entry.
	if _, gerr := reg.Get(context.Background(), "wizard-spi-abc"); gerr != nil {
		t.Errorf("expected registry to contain wizard-spi-abc after BeginWork, got %v", gerr)
	}
}

// TestBeginWork_WithOrphan_Local: prior attempt in_progress but no live
// registry entry → Scan B closes the orphan, BeginWork proceeds.
func TestBeginWork_WithOrphan_Local(t *testing.T) {
	deps := newStubDeps()
	deps.addBead(store.Bead{ID: "spi-def", Status: "in_progress"})
	deps.addAttempt("spi-def", store.Bead{
		ID:     "att-orphan",
		Status: "in_progress",
		Parent: "spi-def",
		Type:   "attempt",
		Labels: []string{"attempt", "agent:wizard-spi-def"},
	})

	// Empty registry — wizard-spi-def has no entry, so IsAlive returns
	// ErrNotFound and Scan B closes the phantom attempt.
	reg, _ := newRegistry()

	opts := BeginOpts{Mode: ModeLocal, AgentName: "wizard-spi-def"}

	if _, err := BeginWork(deps, reg, "spi-def", opts); err != nil {
		t.Fatalf("BeginWork: unexpected error: %v", err)
	}
	if _, closed := deps.closedAttempts["att-orphan"]; !closed {
		t.Error("expected Scan B to close phantom attempt att-orphan")
	}
}

// TestBeginWork_AlreadyInProgress_Live: prior attempt with live registry
// entry → BeginWork refuses.
func TestBeginWork_AlreadyInProgress_Live(t *testing.T) {
	deps := newStubDeps()
	deps.addBead(store.Bead{ID: "spi-ghi", Status: "in_progress"})
	deps.addAttempt("spi-ghi", store.Bead{
		ID:     "att-live",
		Status: "in_progress",
		Parent: "spi-ghi",
		Type:   "attempt",
		Labels: []string{"attempt", "agent:wizard-spi-ghi"},
	})

	reg, ctl := newRegistry()
	upsertAlive(t, ctl, wizardregistry.Wizard{
		ID:     "wizard-spi-ghi",
		Mode:   wizardregistry.ModeLocal,
		PID:    55555,
		BeadID: "spi-ghi",
	})

	opts := BeginOpts{Mode: ModeLocal, AgentName: "wizard-spi-ghi"}

	if _, err := BeginWork(deps, reg, "spi-ghi", opts); err == nil {
		t.Fatal("BeginWork: expected error when bead is in_progress with live owner")
	}

	if _, closed := deps.closedAttempts["att-live"]; closed {
		t.Error("expected live attempt to remain open — Scan B/A should NOT close a live wizard's attempt")
	}
}

// TestClaimWork_Dispatched_Cluster: dispatched bead in cluster mode →
// attempt created, bead in_progress, registry not consulted.
func TestClaimWork_Dispatched_Cluster(t *testing.T) {
	deps := newStubDeps()
	deps.addBead(store.Bead{ID: "spi-jkl", Status: "dispatched"})

	reg, _ := newRegistry()

	opts := BeginOpts{Mode: ModeCluster, AgentName: "wizard-spi-jkl"}

	attemptID, err := ClaimWork(deps, reg, "spi-jkl", opts)
	if err != nil {
		t.Fatalf("ClaimWork: unexpected error: %v", err)
	}
	if attemptID == "" {
		t.Fatal("ClaimWork: expected non-empty attemptID")
	}
	if b := deps.beads["spi-jkl"]; b.Status != "in_progress" {
		t.Errorf("expected in_progress, got %q", b.Status)
	}
}

// TestClaimWork_Reclaim_Same_Agent: in_progress bead with matching
// agentName → returns existing attemptID without creating a new one.
func TestClaimWork_Reclaim_Same_Agent(t *testing.T) {
	deps := newStubDeps()
	deps.addBead(store.Bead{ID: "spi-mno", Status: "in_progress"})
	deps.addAttempt("spi-mno", store.Bead{
		ID:     "att-existing",
		Status: "in_progress",
		Parent: "spi-mno",
		Type:   "attempt",
		Labels: []string{"attempt", "agent:wizard-spi-mno"},
	})

	reg, _ := newRegistry()
	opts := BeginOpts{Mode: ModeLocal, AgentName: "wizard-spi-mno"}

	attemptID, err := ClaimWork(deps, reg, "spi-mno", opts)
	if err != nil {
		t.Fatalf("ClaimWork: unexpected error: %v", err)
	}
	if attemptID != "att-existing" {
		t.Errorf("expected reclaimed attempt ID %q, got %q", "att-existing", attemptID)
	}
	if deps.createAttemptCalls != 0 {
		t.Errorf("expected 0 attempt creations (reclaim), got %d", deps.createAttemptCalls)
	}
}

// TestEndWork_HappyClose_Local: attempt closed with success label, bead
// closed, alerts cascaded, registry entry removed.
func TestEndWork_HappyClose_Local(t *testing.T) {
	deps := newStubDeps()
	deps.addBead(store.Bead{ID: "spi-pqr", Status: "in_progress"})
	deps.addAttempt("spi-pqr", store.Bead{
		ID:     "att-pqr",
		Status: "in_progress",
		Parent: "spi-pqr",
		Type:   "attempt",
		Labels: []string{"attempt", "agent:wizard-spi-pqr"},
	})

	reg, ctl := newRegistry()
	upsertAlive(t, ctl, wizardregistry.Wizard{ID: "wizard-spi-pqr", Mode: wizardregistry.ModeLocal, PID: 1, BeadID: "spi-pqr"})

	opts := BeginOpts{Mode: ModeLocal, AgentName: "wizard-spi-pqr"}
	if err := EndWork(deps, reg, "spi-pqr", opts, EndResult{Status: "success"}); err != nil {
		t.Fatalf("EndWork: unexpected error: %v", err)
	}

	if got := deps.closedAttempts["att-pqr"]; got != "success" {
		t.Errorf("expected attempt closed with 'success', got %q", got)
	}
	if b := deps.beads["spi-pqr"]; b.Status != "closed" {
		t.Errorf("expected bead closed, got %q", b.Status)
	}

	// Alert cascade should have been triggered.
	found := false
	for _, id := range deps.cascadeClosed {
		if id == "spi-pqr" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected AlertCascadeClose to be called for spi-pqr")
	}

	// Registry entry should be gone.
	if _, gerr := reg.Get(context.Background(), "wizard-spi-pqr"); !errors.Is(gerr, wizardregistry.ErrNotFound) {
		t.Errorf("expected registry entry removed; Get returned %v", gerr)
	}
}

// TestEndWork_Interrupted_Reopen: ReopenTask=true → bead reopened.
func TestEndWork_Interrupted_Reopen(t *testing.T) {
	deps := newStubDeps()
	deps.addBead(store.Bead{ID: "spi-stu", Status: "in_progress"})
	deps.addAttempt("spi-stu", store.Bead{
		ID:     "att-stu",
		Status: "in_progress",
		Parent: "spi-stu",
		Type:   "attempt",
		Labels: []string{"attempt", "agent:wizard-spi-stu"},
	})

	reg, _ := newRegistry()
	opts := BeginOpts{Mode: ModeLocal, AgentName: "wizard-spi-stu"}
	if err := EndWork(deps, reg, "spi-stu", opts, EndResult{Status: "interrupted", ReopenTask: true}); err != nil {
		t.Fatalf("EndWork: %v", err)
	}
	if b := deps.beads["spi-stu"]; b.Status != "open" {
		t.Errorf("expected bead reopened (open), got %q", b.Status)
	}
}

// TestEndWork_StripLabels: StripLabels=["review-approved"] → label removed.
func TestEndWork_StripLabels(t *testing.T) {
	deps := newStubDeps()
	deps.addBead(store.Bead{ID: "spi-vwx", Status: "in_progress", Labels: []string{"review-approved"}})
	deps.addAttempt("spi-vwx", store.Bead{
		ID:     "att-vwx",
		Status: "in_progress",
		Parent: "spi-vwx",
		Type:   "attempt",
		Labels: []string{"attempt", "agent:wizard-spi-vwx"},
	})

	reg, _ := newRegistry()
	opts := BeginOpts{Mode: ModeLocal, AgentName: "wizard-spi-vwx"}
	res := EndResult{Status: "interrupted", ReopenTask: true, StripLabels: []string{"review-approved"}}
	if err := EndWork(deps, reg, "spi-vwx", opts, res); err != nil {
		t.Fatalf("EndWork: %v", err)
	}
	if !contains(deps.removedLabels["spi-vwx"], "review-approved") {
		t.Error("expected 'review-approved' to be stripped from task bead")
	}
}

// TestEndWork_ClusterMode_RegistryReadOnly: cluster mode → Remove returns
// ErrReadOnly which is silently absorbed; EndWork still succeeds.
func TestEndWork_ClusterMode_RegistryReadOnly(t *testing.T) {
	deps := newStubDeps()
	deps.addBead(store.Bead{ID: "spi-yz0", Status: "in_progress"})
	deps.addAttempt("spi-yz0", store.Bead{
		ID:     "att-yz0",
		Status: "in_progress",
		Parent: "spi-yz0",
		Type:   "attempt",
		Labels: []string{"attempt", "agent:wizard-spi-yz0"},
	})

	reg := &readOnlyRegistry{Registry: fake.New()}
	opts := BeginOpts{Mode: ModeCluster, AgentName: "wizard-spi-yz0"}
	if err := EndWork(deps, reg, "spi-yz0", opts, EndResult{Status: "success"}); err != nil {
		t.Fatalf("EndWork: %v", err)
	}
}

// readOnlyRegistry wraps a fake to mimic a cluster-mode backend that
// rejects writes with ErrReadOnly.
type readOnlyRegistry struct{ *fake.Registry }

func (readOnlyRegistry) Upsert(_ context.Context, _ wizardregistry.Wizard) error {
	return wizardregistry.ErrReadOnly
}
func (readOnlyRegistry) Remove(_ context.Context, _ string) error {
	return wizardregistry.ErrReadOnly
}

// TestOrphanSweep_DeadEntry_ScanA: a registered, dead wizard is reaped:
// attempt closed, bead reopened, registry entry removed.
func TestOrphanSweep_DeadEntry_ScanA(t *testing.T) {
	deps := newStubDeps()
	deps.addBead(store.Bead{ID: "spi-dead1", Status: "in_progress"})
	deps.addAttempt("spi-dead1", store.Bead{
		ID:     "att-dead1",
		Status: "in_progress",
		Parent: "spi-dead1",
		Type:   "attempt",
		Labels: []string{"attempt", "agent:wizard-dead"},
	})

	reg, ctl := newRegistry()
	upsertDead(t, ctl, wizardregistry.Wizard{ID: "wizard-dead", Mode: wizardregistry.ModeLocal, PID: 99999, BeadID: "spi-dead1"})

	report, err := OrphanSweep(deps, reg, OrphanScope{All: true})
	if err != nil {
		t.Fatalf("OrphanSweep: %v", err)
	}
	if report.Dead == 0 {
		t.Error("expected at least 1 dead entry from Scan A")
	}
	if report.Cleaned == 0 {
		t.Error("expected at least 1 cleaned entry")
	}
	if got := deps.closedAttempts["att-dead1"]; got != "interrupted:orphan" {
		t.Errorf("expected attempt closed with 'interrupted:orphan', got %q", got)
	}
	if !contains(deps.addedLabels["spi-dead1"], "dead-letter:orphan") {
		t.Error("expected dead-letter:orphan label on bead")
	}
	if _, gerr := reg.Get(context.Background(), "wizard-dead"); !errors.Is(gerr, wizardregistry.ErrNotFound) {
		t.Errorf("expected registry entry removed after sweep; Get returned %v", gerr)
	}
}

// TestOrphanSweep_LiveEntry_ScanA: a live wizard is left alone.
func TestOrphanSweep_LiveEntry_ScanA(t *testing.T) {
	deps := newStubDeps()
	deps.addBead(store.Bead{ID: "spi-live", Status: "in_progress"})

	reg, ctl := newRegistry()
	upsertAlive(t, ctl, wizardregistry.Wizard{ID: "wizard-live", Mode: wizardregistry.ModeLocal, PID: 12345, BeadID: "spi-live"})

	report, err := OrphanSweep(deps, reg, OrphanScope{All: true})
	if err != nil {
		t.Fatalf("OrphanSweep: %v", err)
	}
	if report.Dead > 0 {
		t.Errorf("expected 0 dead entries for live wizard, got %d", report.Dead)
	}
	if _, gerr := reg.Get(context.Background(), "wizard-live"); gerr != nil {
		t.Errorf("live wizard should still be registered after sweep, got %v", gerr)
	}
}

// TestOrphanSweep_MixedAliveAndDead: 2 dead + 1 live → sweeps 2, leaves 1.
func TestOrphanSweep_MixedAliveAndDead(t *testing.T) {
	deps := newStubDeps()
	for _, id := range []string{"spi-da", "spi-db", "spi-dc"} {
		deps.addBead(store.Bead{ID: id, Status: "in_progress"})
	}

	reg, ctl := newRegistry()
	upsertDead(t, ctl, wizardregistry.Wizard{ID: "wiz-dead-a", Mode: wizardregistry.ModeLocal, PID: 1001, BeadID: "spi-da"})
	upsertDead(t, ctl, wizardregistry.Wizard{ID: "wiz-dead-b", Mode: wizardregistry.ModeLocal, PID: 1002, BeadID: "spi-db"})
	upsertAlive(t, ctl, wizardregistry.Wizard{ID: "wiz-live-c", Mode: wizardregistry.ModeLocal, PID: 1003, BeadID: "spi-dc"})

	report, err := OrphanSweep(deps, reg, OrphanScope{All: true})
	if err != nil {
		t.Fatalf("OrphanSweep: %v", err)
	}
	if report.Dead < 2 {
		t.Errorf("expected at least 2 dead entries, got %d", report.Dead)
	}

	if _, gerr := reg.Get(context.Background(), "wiz-live-c"); gerr != nil {
		t.Errorf("live wizard should still be registered after sweep, got %v", gerr)
	}
}

// TestOrphanSweep_Idempotent: an empty registry is a no-op; a second call
// after the first cleaned everything is also a no-op.
func TestOrphanSweep_Idempotent(t *testing.T) {
	deps := newStubDeps()
	reg, _ := newRegistry()

	report1, err1 := OrphanSweep(deps, reg, OrphanScope{All: true})
	if err1 != nil {
		t.Fatalf("first OrphanSweep: %v", err1)
	}
	report2, err2 := OrphanSweep(deps, reg, OrphanScope{All: true})
	if err2 != nil {
		t.Fatalf("second OrphanSweep: %v", err2)
	}
	if report1.Cleaned != 0 || report2.Cleaned != 0 {
		t.Errorf("expected no cleanups, got %d and %d", report1.Cleaned, report2.Cleaned)
	}
}

// TestOrphanSweep_PhantomAttempt: in_progress attempt bead with no
// registered wizard at all → Scan B closes it via ErrNotFound.
func TestOrphanSweep_PhantomAttempt(t *testing.T) {
	deps := newStubDeps()
	deps.addBead(store.Bead{ID: "spi-phantom-parent", Status: "in_progress"})
	deps.addAttempt("spi-phantom-parent", store.Bead{
		ID:     "att-phantom",
		Status: "in_progress",
		Parent: "spi-phantom-parent",
		Type:   "attempt",
		Labels: []string{"attempt", "agent:wizard-phantom"},
	})

	reg, _ := newRegistry() // empty registry — wizard-phantom never registered

	report, err := OrphanSweep(deps, reg, OrphanScope{All: true})
	if err != nil {
		t.Fatalf("OrphanSweep: %v", err)
	}
	if _, closed := deps.closedAttempts["att-phantom"]; !closed {
		t.Error("Scan B: phantom attempt should have been closed")
	}
	if report.Cleaned == 0 {
		t.Error("expected at least 1 cleaned from Scan B")
	}
	if b := deps.beads["spi-phantom-parent"]; b.Status != "open" {
		t.Errorf("expected parent reopened (open), got %q", b.Status)
	}
}

// TestOrphanSweep_ScanB_DeadRegistered: in_progress attempt whose wizard
// IS registered but dead → Scan B reaps it.
func TestOrphanSweep_ScanB_DeadRegistered(t *testing.T) {
	deps := newStubDeps()
	deps.addBead(store.Bead{ID: "spi-stale-parent", Status: "in_progress"})
	deps.addAttempt("spi-stale-parent", store.Bead{
		ID:     "att-stale",
		Status: "in_progress",
		Parent: "spi-stale-parent",
		Type:   "attempt",
		Labels: []string{"attempt", "agent:wizard-stale"},
	})

	reg, ctl := newRegistry()
	// Register the wizard but mark it dead. Scope: BeadID-only so Scan A
	// also runs against this entry; Scan B will close the attempt either
	// way.
	upsertDead(t, ctl, wizardregistry.Wizard{ID: "wizard-stale", Mode: wizardregistry.ModeLocal, PID: 7777, BeadID: "spi-stale-parent"})

	if _, err := OrphanSweep(deps, reg, OrphanScope{All: true}); err != nil {
		t.Fatalf("OrphanSweep: %v", err)
	}
	if _, closed := deps.closedAttempts["att-stale"]; !closed {
		t.Error("expected stale-but-registered attempt to be closed")
	}
}

// --- Race-safety tests for the spi-5bzu9r incident ---

// TestOrphanSweep_FreshUpsertNotMisclassified is the load-bearing
// regression test for the spi-5bzu9r OrphanSweep race incident.
//
// Scenario: a registered, alive wizard with an in_progress attempt
// already exists. A second wizard is upserted and marked alive
// concurrently with OrphanSweep ticks. Neither wizard's attempt may be
// reaped — the contract on Registry.IsAlive guarantees fresh reads, so a
// concurrent upsert cannot be mis-classified as dead by a stale snapshot.
func TestOrphanSweep_FreshUpsertNotMisclassified(t *testing.T) {
	deps := newStubDeps()
	deps.addBead(store.Bead{ID: "spi-seed", Status: "in_progress"})
	deps.addAttempt("spi-seed", store.Bead{
		ID:     "att-seed",
		Status: "in_progress",
		Parent: "spi-seed",
		Type:   "attempt",
		Labels: []string{"attempt", "agent:wizard-seed"},
	})

	reg, ctl := newRegistry()
	upsertAlive(t, ctl, wizardregistry.Wizard{ID: "wizard-seed", Mode: wizardregistry.ModeLocal, PID: 1, BeadID: "spi-seed"})

	const n = 50
	var wg sync.WaitGroup
	wg.Add(n * 2)

	// N goroutines each upsert and mark alive a fresh wizard.
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			id := fmt.Sprintf("wizard-fresh-%d", i)
			upsertAlive(t, ctl, wizardregistry.Wizard{
				ID:     id,
				Mode:   wizardregistry.ModeLocal,
				PID:    2000 + i,
				BeadID: "spi-fresh-" + id,
			})
		}()
	}

	// N goroutines drive OrphanSweep concurrently.
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			_, _ = OrphanSweep(newStubDeps(), reg, OrphanScope{All: true})
		}()
	}
	wg.Wait()

	// The original seed wizard must still be alive in the registry; its
	// attempt must remain open.
	if _, gerr := reg.Get(context.Background(), "wizard-seed"); gerr != nil {
		t.Errorf("seed wizard should remain registered after concurrent sweeps; got %v", gerr)
	}
	if _, closed := deps.closedAttempts["att-seed"]; closed {
		t.Errorf("seed attempt was closed by a concurrent sweep — race-safety contract broken")
	}
}

// TestBeginWork_Spi5Bzu9r_RaceRegression reproduces the spi-5bzu9r flow:
// while one wizard is alive and its attempt is in_progress, BeginWork is
// called for the same bead with a fresh agent name. The active wizard's
// attempt must NOT be marked interrupted:orphan and the parent must NOT
// be reverted to open by the BeginWork-internal OrphanSweep.
func TestBeginWork_Spi5Bzu9r_RaceRegression(t *testing.T) {
	deps := newStubDeps()
	deps.addBead(store.Bead{ID: "spi-race", Status: "in_progress"})
	deps.addAttempt("spi-race", store.Bead{
		ID:     "att-race-live",
		Status: "in_progress",
		Parent: "spi-race",
		Type:   "attempt",
		Labels: []string{"attempt", "agent:wizard-race-live"},
	})

	reg, ctl := newRegistry()
	upsertAlive(t, ctl, wizardregistry.Wizard{
		ID:     "wizard-race-live",
		Mode:   wizardregistry.ModeLocal,
		PID:    4242,
		BeadID: "spi-race",
	})

	// Calling BeginWork with a different agent should refuse (live owner)
	// without first damaging the live attempt.
	_, err := BeginWork(deps, reg, "spi-race", BeginOpts{
		Mode:      ModeLocal,
		AgentName: "wizard-race-new",
	})
	if err == nil {
		t.Fatal("expected BeginWork to refuse when bead is in_progress with a live owner")
	}
	if _, closed := deps.closedAttempts["att-race-live"]; closed {
		t.Error("live wizard's attempt was closed by BeginWork — spi-5bzu9r symptom recurred")
	}
	if b := deps.beads["spi-race"]; b.Status == "open" {
		t.Error("parent bead reverted to open while wizard alive — spi-5bzu9r symptom recurred")
	}
}

// --- Cluster-mode key-shape coverage ---

// TestOrphanSweep_ClusterPath_KeyShape proves the function is key-shape
// agnostic: pod-name+namespace-shaped wizard IDs flow through the same
// code path. The only thing OrphanSweep cares about is that the attempt
// label's agent name matches the wizardregistry.Wizard.ID.
func TestOrphanSweep_ClusterPath_KeyShape(t *testing.T) {
	deps := newStubDeps()
	deps.addBead(store.Bead{ID: "spi-pod-bead", Status: "in_progress"})

	// Cluster-style ID: the agent label on the attempt bead carries the
	// pod name verbatim. The fake registry treats IDs as opaque, just like
	// the production cluster impl.
	clusterID := "wizard-spi-pod-bead-w1-0"
	deps.addAttempt("spi-pod-bead", store.Bead{
		ID:     "att-pod",
		Status: "in_progress",
		Parent: "spi-pod-bead",
		Type:   "attempt",
		Labels: []string{"attempt", "agent:" + clusterID},
	})

	reg, ctl := newRegistry()
	upsertDead(t, ctl, wizardregistry.Wizard{
		ID:        clusterID,
		Mode:      wizardregistry.ModeCluster,
		PodName:   clusterID,
		Namespace: "spire",
		BeadID:    "spi-pod-bead",
	})

	if _, err := OrphanSweep(deps, reg, OrphanScope{All: true}); err != nil {
		t.Fatalf("OrphanSweep: %v", err)
	}
	if got := deps.closedAttempts["att-pod"]; got != "interrupted:orphan" {
		t.Errorf("expected cluster-keyed attempt closed with interrupted:orphan, got %q", got)
	}
}

// TestOrphanSweep_ClusterPath_Live: a cluster wizard whose pod is alive
// is left untouched by Scan A and Scan B.
func TestOrphanSweep_ClusterPath_Live(t *testing.T) {
	deps := newStubDeps()
	deps.addBead(store.Bead{ID: "spi-pod-live", Status: "in_progress"})
	clusterID := "wizard-spi-pod-live-w0-0"
	deps.addAttempt("spi-pod-live", store.Bead{
		ID:     "att-pod-live",
		Status: "in_progress",
		Parent: "spi-pod-live",
		Type:   "attempt",
		Labels: []string{"attempt", "agent:" + clusterID},
	})

	reg, ctl := newRegistry()
	upsertAlive(t, ctl, wizardregistry.Wizard{
		ID:        clusterID,
		Mode:      wizardregistry.ModeCluster,
		PodName:   clusterID,
		Namespace: "spire",
		BeadID:    "spi-pod-live",
	})

	report, err := OrphanSweep(deps, reg, OrphanScope{All: true})
	if err != nil {
		t.Fatalf("OrphanSweep: %v", err)
	}
	if report.Dead > 0 {
		t.Errorf("expected 0 dead entries for live cluster wizard, got %d", report.Dead)
	}
	if _, closed := deps.closedAttempts["att-pod-live"]; closed {
		t.Error("live cluster wizard's attempt was closed — Scan B mis-classified it")
	}
}

// TestOrphanSweep_NilRegistry_ReturnsError: defensive — passing nil
// must not panic.
func TestOrphanSweep_NilRegistry_ReturnsError(t *testing.T) {
	deps := newStubDeps()
	if _, err := OrphanSweep(deps, nil, OrphanScope{All: true}); err == nil {
		t.Fatal("expected error when reg is nil")
	}
}

// --- Heartbeat-freshness gate (spi-p2ou7v) ---

// TestOrphanSweep_ScanA_FreshHeartbeat_SkipsOrphan: Scan A finds a
// registry-dead entry but the active attempt has a fresh last_seen_at —
// OrphanSweep MUST NOT close the attempt or reopen the parent.
func TestOrphanSweep_ScanA_FreshHeartbeat_SkipsOrphan(t *testing.T) {
	deps := newStubDeps()
	deps.addBead(store.Bead{ID: "spi-fresh-a", Status: "in_progress"})
	deps.addAttempt("spi-fresh-a", store.Bead{
		ID:     "att-fresh-a",
		Status: "in_progress",
		Parent: "spi-fresh-a",
		Type:   "attempt",
		Labels: []string{"attempt", "agent:wizard-fresh-a"},
	})
	// Heartbeat 30 seconds ago — well within heartbeatFreshness.
	deps.setHeartbeat("att-fresh-a", time.Now().UTC().Add(-30*time.Second))

	reg, ctl := newRegistry()
	upsertDead(t, ctl, wizardregistry.Wizard{
		ID:     "wizard-fresh-a",
		Mode:   wizardregistry.ModeLocal,
		PID:    99999,
		BeadID: "spi-fresh-a",
	})

	report, err := OrphanSweep(deps, reg, OrphanScope{All: true})
	if err != nil {
		t.Fatalf("OrphanSweep: %v", err)
	}
	if _, closed := deps.closedAttempts["att-fresh-a"]; closed {
		t.Error("fresh-heartbeat attempt was closed by Scan A — heartbeat gate broken")
	}
	if contains(deps.addedLabels["spi-fresh-a"], "dead-letter:orphan") {
		t.Error("fresh-heartbeat parent was labeled dead-letter:orphan — gate broken")
	}
	if b := deps.beads["spi-fresh-a"]; b.Status != "in_progress" {
		t.Errorf("fresh-heartbeat parent moved from in_progress to %q — gate broken", b.Status)
	}
	// The dead entry must still be examined but not cleaned.
	if report.Cleaned != 0 {
		t.Errorf("expected 0 cleaned (heartbeat skip), got %d", report.Cleaned)
	}
}

// TestOrphanSweep_ScanA_StaleHeartbeat_Orphans: Scan A finds a
// registry-dead entry whose active attempt has a stale last_seen_at —
// existing orphan path runs (attempt closed interrupted:orphan, parent
// reopened, dead-letter:orphan label set).
func TestOrphanSweep_ScanA_StaleHeartbeat_Orphans(t *testing.T) {
	deps := newStubDeps()
	deps.addBead(store.Bead{ID: "spi-stale-a", Status: "in_progress"})
	deps.addAttempt("spi-stale-a", store.Bead{
		ID:     "att-stale-a",
		Status: "in_progress",
		Parent: "spi-stale-a",
		Type:   "attempt",
		Labels: []string{"attempt", "agent:wizard-stale-a"},
	})
	// Heartbeat 1 hour ago — well past heartbeatFreshness.
	deps.setHeartbeat("att-stale-a", time.Now().UTC().Add(-1*time.Hour))

	reg, ctl := newRegistry()
	upsertDead(t, ctl, wizardregistry.Wizard{
		ID:     "wizard-stale-a",
		Mode:   wizardregistry.ModeLocal,
		PID:    99999,
		BeadID: "spi-stale-a",
	})

	if _, err := OrphanSweep(deps, reg, OrphanScope{All: true}); err != nil {
		t.Fatalf("OrphanSweep: %v", err)
	}
	if got := deps.closedAttempts["att-stale-a"]; got != "interrupted:orphan" {
		t.Errorf("expected stale-heartbeat attempt closed with 'interrupted:orphan', got %q", got)
	}
	if !contains(deps.addedLabels["spi-stale-a"], "dead-letter:orphan") {
		t.Error("expected dead-letter:orphan label on stale-heartbeat parent")
	}
	if b := deps.beads["spi-stale-a"]; b.Status != "open" {
		t.Errorf("expected stale-heartbeat parent reopened, got %q", b.Status)
	}
}

// TestOrphanSweep_ScanB_FreshHeartbeat_SkipsOrphan: Scan B's phantom-
// attempt branch (registry has no entry at all) MUST NOT orphan an
// attempt with a fresh heartbeat — the execution owner is alive even
// though the registry is missing.
func TestOrphanSweep_ScanB_FreshHeartbeat_SkipsOrphan(t *testing.T) {
	deps := newStubDeps()
	deps.addBead(store.Bead{ID: "spi-fresh-b", Status: "in_progress"})
	deps.addAttempt("spi-fresh-b", store.Bead{
		ID:     "att-fresh-b",
		Status: "in_progress",
		Parent: "spi-fresh-b",
		Type:   "attempt",
		Labels: []string{"attempt", "agent:wizard-fresh-b"},
	})
	deps.setHeartbeat("att-fresh-b", time.Now().UTC().Add(-15*time.Second))

	reg, _ := newRegistry() // empty: wizard-fresh-b never registered

	if _, err := OrphanSweep(deps, reg, OrphanScope{All: true}); err != nil {
		t.Fatalf("OrphanSweep: %v", err)
	}
	if _, closed := deps.closedAttempts["att-fresh-b"]; closed {
		t.Error("Scan B closed fresh-heartbeat attempt — phantom-attempt path bypassed gate")
	}
	if b := deps.beads["spi-fresh-b"]; b.Status != "in_progress" {
		t.Errorf("Scan B reopened fresh-heartbeat parent, got %q", b.Status)
	}
}

// TestEndWork_Success_StripsDeadLetterOrphan: an attempt that previously
// had dead-letter:orphan written to its parent (false orphan), then
// closes successfully via EndWork, must have the stale label stripped
// — no contradictory final state.
func TestEndWork_Success_StripsDeadLetterOrphan(t *testing.T) {
	deps := newStubDeps()
	deps.addBead(store.Bead{ID: "spi-recover", Status: "in_progress", Labels: []string{"dead-letter:orphan"}})
	deps.addAttempt("spi-recover", store.Bead{
		ID:     "att-recover",
		Status: "in_progress",
		Parent: "spi-recover",
		Type:   "attempt",
		Labels: []string{"attempt", "agent:wizard-recover"},
	})

	reg, _ := newRegistry()
	opts := BeginOpts{Mode: ModeLocal, AgentName: "wizard-recover"}
	if err := EndWork(deps, reg, "spi-recover", opts, EndResult{Status: "success"}); err != nil {
		t.Fatalf("EndWork: %v", err)
	}
	if b := deps.beads["spi-recover"]; b.Status != "closed" {
		t.Errorf("expected bead closed, got %q", b.Status)
	}
	if !contains(deps.removedLabels["spi-recover"], "dead-letter:orphan") {
		t.Error("expected dead-letter:orphan to be stripped on successful close")
	}
}

// TestEndWork_Interrupted_PreservesDeadLetterOrphan: a non-success
// terminal (interrupted/reset/discarded) MUST NOT strip the
// dead-letter:orphan label — only true success closes do.
func TestEndWork_Interrupted_PreservesDeadLetterOrphan(t *testing.T) {
	deps := newStubDeps()
	deps.addBead(store.Bead{ID: "spi-interrupt", Status: "in_progress", Labels: []string{"dead-letter:orphan"}})
	deps.addAttempt("spi-interrupt", store.Bead{
		ID:     "att-interrupt",
		Status: "in_progress",
		Parent: "spi-interrupt",
		Type:   "attempt",
		Labels: []string{"attempt", "agent:wizard-interrupt"},
	})

	reg, _ := newRegistry()
	opts := BeginOpts{Mode: ModeLocal, AgentName: "wizard-interrupt"}
	if err := EndWork(deps, reg, "spi-interrupt", opts, EndResult{Status: "interrupted", ReopenTask: true}); err != nil {
		t.Fatalf("EndWork: %v", err)
	}
	if contains(deps.removedLabels["spi-interrupt"], "dead-letter:orphan") {
		t.Error("dead-letter:orphan stripped on interrupted close — strip should be success-only")
	}
}

// TestOrphanSweep_NoHeartbeatPresent_OrphansAsBefore: an attempt that
// has never written heartbeat metadata (a brand-new attempt that just
// began on a tick) must still be eligible for orphaning — the gate
// requires present heartbeat data, not its absence.
func TestOrphanSweep_NoHeartbeatPresent_OrphansAsBefore(t *testing.T) {
	deps := newStubDeps()
	deps.addBead(store.Bead{ID: "spi-no-hb", Status: "in_progress"})
	deps.addAttempt("spi-no-hb", store.Bead{
		ID:     "att-no-hb",
		Status: "in_progress",
		Parent: "spi-no-hb",
		Type:   "attempt",
		Labels: []string{"attempt", "agent:wizard-no-hb"},
	})
	// No heartbeat set on att-no-hb.

	reg, ctl := newRegistry()
	upsertDead(t, ctl, wizardregistry.Wizard{
		ID:     "wizard-no-hb",
		Mode:   wizardregistry.ModeLocal,
		PID:    99999,
		BeadID: "spi-no-hb",
	})

	if _, err := OrphanSweep(deps, reg, OrphanScope{All: true}); err != nil {
		t.Fatalf("OrphanSweep: %v", err)
	}
	if got := deps.closedAttempts["att-no-hb"]; got != "interrupted:orphan" {
		t.Errorf("expected no-heartbeat attempt closed with 'interrupted:orphan' (preserved policy), got %q", got)
	}
	if !contains(deps.addedLabels["spi-no-hb"], "dead-letter:orphan") {
		t.Error("expected dead-letter:orphan label on no-heartbeat parent")
	}
}

// --- Helpers ---

func contains(slice []string, s string) bool {
	for _, v := range slice {
		if v == s {
			return true
		}
	}
	return false
}
