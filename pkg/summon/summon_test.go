package summon

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/awell-health/spire/pkg/agent"
	"github.com/awell-health/spire/pkg/store"
)

// fakeHandle satisfies agent.Handle for SpawnFunc stubs. A real process
// never runs; Identifier() returns a synthetic PID string that becomes the
// registry entry's PID field after Run/SpawnWizard parse it with Atoi.
type fakeHandle struct {
	name string
	id   string
}

func (f *fakeHandle) Wait() error            { return nil }
func (f *fakeHandle) Signal(os.Signal) error { return nil }
func (f *fakeHandle) Alive() bool            { return true }
func (f *fakeHandle) Name() string           { return f.name }
func (f *fakeHandle) Identifier() string     { return f.id }

// stubCalls records what the seam stubs observed during a single test.
type stubCalls struct {
	statusUpdates []statusUpdate
	labelsAdded   []labelOp
	labelsRemoved []labelOp
	comments      []commentCall
	spawns        []agent.SpawnConfig
}

type statusUpdate struct{ id, status string }
type labelOp struct{ id, label string }
type commentCall struct{ id, text string }

// stubOpts configures installStubs.
type stubOpts struct {
	bead       store.Bead // returned by GetBeadFunc (ID gets set from request arg)
	getErr     error      // returned by GetBeadFunc
	updateErr  error      // returned by UpdateBeadFunc
	commentErr error      // returned by AddCommentFunc
	spawnErr   error      // returned by SpawnFunc
}

// installStubs swaps package-level seams with test doubles and isolates the
// wizard registry to a per-test tempdir via SPIRE_CONFIG_DIR.
func installStubs(t *testing.T, opts stubOpts) *stubCalls {
	t.Helper()
	// Isolate the registry file + any config.Dir() writes.
	t.Setenv("SPIRE_CONFIG_DIR", t.TempDir())
	t.Setenv("SPIRE_TOWER", "")

	prevGet := GetBeadFunc
	prevUpdate := UpdateBeadFunc
	prevAddLabel := AddLabelFunc
	prevRemoveLabel := RemoveLabelFunc
	prevComment := AddCommentFunc
	prevSpawn := SpawnFunc

	calls := &stubCalls{}
	GetBeadFunc = func(id string) (store.Bead, error) {
		if opts.getErr != nil {
			return store.Bead{}, opts.getErr
		}
		b := opts.bead
		b.ID = id
		return b, nil
	}
	UpdateBeadFunc = func(id string, updates map[string]interface{}) error {
		if opts.updateErr != nil {
			return opts.updateErr
		}
		if s, ok := updates["status"].(string); ok {
			calls.statusUpdates = append(calls.statusUpdates, statusUpdate{id: id, status: s})
		}
		return nil
	}
	AddLabelFunc = func(id, label string) error {
		calls.labelsAdded = append(calls.labelsAdded, labelOp{id: id, label: label})
		return nil
	}
	RemoveLabelFunc = func(id, label string) error {
		calls.labelsRemoved = append(calls.labelsRemoved, labelOp{id: id, label: label})
		return nil
	}
	AddCommentFunc = func(id, text string) (string, error) {
		if opts.commentErr != nil {
			return "", opts.commentErr
		}
		calls.comments = append(calls.comments, commentCall{id: id, text: text})
		return "comment-1", nil
	}
	SpawnFunc = func(_ agent.Backend, cfg agent.SpawnConfig) (agent.Handle, error) {
		if opts.spawnErr != nil {
			return nil, opts.spawnErr
		}
		calls.spawns = append(calls.spawns, cfg)
		return &fakeHandle{name: cfg.Name, id: "42"}, nil
	}

	t.Cleanup(func() {
		GetBeadFunc = prevGet
		UpdateBeadFunc = prevUpdate
		AddLabelFunc = prevAddLabel
		RemoveLabelFunc = prevRemoveLabel
		AddCommentFunc = prevComment
		SpawnFunc = prevSpawn
	})
	return calls
}

// --- ValidateDispatch ---

func TestValidateDispatch_AcceptsKnownModes(t *testing.T) {
	for _, mode := range []string{"", "sequential", "wave", "direct"} {
		if err := ValidateDispatch(mode); err != nil {
			t.Errorf("ValidateDispatch(%q) = %v, want nil", mode, err)
		}
	}
}

func TestValidateDispatch_RejectsUnknown(t *testing.T) {
	for _, mode := range []string{"bogus", "parallel", "serial", " sequential"} {
		err := ValidateDispatch(mode)
		if err == nil {
			t.Errorf("ValidateDispatch(%q) = nil, want error", mode)
			continue
		}
		if !strings.Contains(err.Error(), "invalid dispatch mode") {
			t.Errorf("ValidateDispatch(%q) error = %q, want to mention \"invalid dispatch mode\"", mode, err)
		}
	}
}

// --- Run status gating ---

func TestRun_RejectsClosed(t *testing.T) {
	for _, status := range []string{"closed", "done"} {
		t.Run(status, func(t *testing.T) {
			calls := installStubs(t, stubOpts{bead: store.Bead{Status: status, Type: "task"}})
			_, err := Run("spi-abc", "")
			if err == nil || !strings.Contains(err.Error(), "is closed") {
				t.Fatalf("err = %v, want to mention \"is closed\"", err)
			}
			if len(calls.statusUpdates) != 0 {
				t.Fatalf("unexpected status updates: %+v", calls.statusUpdates)
			}
			if len(calls.spawns) != 0 {
				t.Fatalf("unexpected spawns: %+v", calls.spawns)
			}
		})
	}
}

func TestRun_RejectsDeferred(t *testing.T) {
	calls := installStubs(t, stubOpts{bead: store.Bead{Status: "deferred", Type: "task"}})
	_, err := Run("spi-abc", "")
	if err == nil || !strings.Contains(err.Error(), "deferred") {
		t.Fatalf("err = %v, want to mention \"deferred\"", err)
	}
	if len(calls.statusUpdates) != 0 {
		t.Fatalf("unexpected status updates: %+v", calls.statusUpdates)
	}
}

func TestRun_RejectsDesignType(t *testing.T) {
	calls := installStubs(t, stubOpts{bead: store.Bead{Status: "open", Type: "design"}})
	_, err := Run("spi-abc", "")
	if err == nil || !strings.Contains(err.Error(), "design bead") {
		t.Fatalf("err = %v, want to mention \"design bead\"", err)
	}
	if len(calls.statusUpdates) != 0 {
		t.Fatalf("unexpected status updates: %+v", calls.statusUpdates)
	}
}

func TestRun_RejectsInvalidDispatch(t *testing.T) {
	installStubs(t, stubOpts{bead: store.Bead{Status: "open", Type: "task"}})
	_, err := Run("spi-abc", "bogus")
	if err == nil || !strings.Contains(err.Error(), "invalid dispatch mode") {
		t.Fatalf("err = %v, want invalid-dispatch error", err)
	}
}

func TestRun_BeadNotFound(t *testing.T) {
	installStubs(t, stubOpts{getErr: errors.New("bead not found")})
	_, err := Run("spi-missing", "")
	if err == nil || !strings.Contains(err.Error(), "bead not found") {
		t.Fatalf("err = %v, want underlying \"bead not found\"", err)
	}
}

func TestRun_OpenTransitionsToInProgressAndSpawns(t *testing.T) {
	for _, status := range []string{"open", "ready", "hooked"} {
		t.Run(status, func(t *testing.T) {
			calls := installStubs(t, stubOpts{bead: store.Bead{Status: status, Type: "task"}})
			res, err := Run("spi-abc", "")
			if err != nil {
				t.Fatalf("Run err = %v, want nil", err)
			}
			// status transition to in_progress must happen first
			if len(calls.statusUpdates) != 1 ||
				calls.statusUpdates[0].id != "spi-abc" ||
				calls.statusUpdates[0].status != "in_progress" {
				t.Fatalf("statusUpdates = %+v, want [{spi-abc in_progress}]", calls.statusUpdates)
			}
			// spawn must have happened with the derived wizard name
			if len(calls.spawns) != 1 {
				t.Fatalf("spawns = %d, want 1", len(calls.spawns))
			}
			if calls.spawns[0].Name != "wizard-spi-abc" || calls.spawns[0].BeadID != "spi-abc" {
				t.Fatalf("spawn cfg = %+v, want name=wizard-spi-abc bead=spi-abc", calls.spawns[0])
			}
			if res.WizardName != "wizard-spi-abc" {
				t.Fatalf("WizardName = %q, want wizard-spi-abc", res.WizardName)
			}
			if res.CommentID != "comment-1" {
				t.Fatalf("CommentID = %q, want comment-1", res.CommentID)
			}
		})
	}
}

// --- SpawnWizard directly ---

func TestSpawnWizard_PersistsDispatchLabel(t *testing.T) {
	calls := installStubs(t, stubOpts{})
	bead := store.Bead{ID: "spi-abc", Status: "in_progress", Type: "task"}
	res, err := SpawnWizard(bead, "wave")
	if err != nil {
		t.Fatalf("SpawnWizard err = %v", err)
	}
	if res.WizardName != "wizard-spi-abc" {
		t.Fatalf("WizardName = %q, want wizard-spi-abc", res.WizardName)
	}
	// No existing dispatch labels → no removals.
	if len(calls.labelsRemoved) != 0 {
		t.Fatalf("labelsRemoved = %+v, want []", calls.labelsRemoved)
	}
	// One add for dispatch:wave.
	if len(calls.labelsAdded) != 1 ||
		calls.labelsAdded[0].id != "spi-abc" ||
		calls.labelsAdded[0].label != "dispatch:wave" {
		t.Fatalf("labelsAdded = %+v, want [{spi-abc dispatch:wave}]", calls.labelsAdded)
	}
}

func TestSpawnWizard_ReplacesExistingDispatchLabel(t *testing.T) {
	calls := installStubs(t, stubOpts{})
	bead := store.Bead{
		ID:     "spi-abc",
		Status: "in_progress",
		Type:   "task",
		Labels: []string{"dispatch:direct", "unrelated", "dispatch:legacy"},
	}
	if _, err := SpawnWizard(bead, "wave"); err != nil {
		t.Fatalf("SpawnWizard err = %v", err)
	}
	// Both dispatch:* labels must be removed before the new one is added.
	if len(calls.labelsRemoved) != 2 {
		t.Fatalf("labelsRemoved = %+v, want 2 removals", calls.labelsRemoved)
	}
	for _, r := range calls.labelsRemoved {
		if !strings.HasPrefix(r.label, "dispatch:") {
			t.Fatalf("unexpected removed label %q", r.label)
		}
	}
	if len(calls.labelsAdded) != 1 || calls.labelsAdded[0].label != "dispatch:wave" {
		t.Fatalf("labelsAdded = %+v, want [{spi-abc dispatch:wave}]", calls.labelsAdded)
	}
}

func TestSpawnWizard_EmptyDispatchSkipsLabelOps(t *testing.T) {
	calls := installStubs(t, stubOpts{})
	bead := store.Bead{
		ID:     "spi-abc",
		Status: "in_progress",
		Type:   "task",
		Labels: []string{"dispatch:direct"},
	}
	if _, err := SpawnWizard(bead, ""); err != nil {
		t.Fatalf("SpawnWizard err = %v", err)
	}
	if len(calls.labelsAdded) != 0 || len(calls.labelsRemoved) != 0 {
		t.Fatalf("dispatch label ops ran despite empty dispatch: added=%+v removed=%+v",
			calls.labelsAdded, calls.labelsRemoved)
	}
}

func TestSpawnWizard_RecordsAuditComment(t *testing.T) {
	calls := installStubs(t, stubOpts{})
	bead := store.Bead{ID: "spi-abc", Status: "in_progress", Type: "task"}
	res, err := SpawnWizard(bead, "")
	if err != nil {
		t.Fatalf("SpawnWizard err = %v", err)
	}
	if len(calls.comments) != 1 {
		t.Fatalf("comments = %d, want 1", len(calls.comments))
	}
	if calls.comments[0].id != "spi-abc" {
		t.Fatalf("comment id = %q, want spi-abc", calls.comments[0].id)
	}
	if !strings.Contains(calls.comments[0].text, "wizard-spi-abc") {
		t.Fatalf("comment text = %q, want to mention \"wizard-spi-abc\"", calls.comments[0].text)
	}
	if res.CommentID != "comment-1" {
		t.Fatalf("CommentID = %q, want comment-1", res.CommentID)
	}
}

func TestSpawnWizard_DuplicateReturnsErrAlreadyRunning(t *testing.T) {
	// Seam stubs also set SPIRE_CONFIG_DIR to a tempdir for us.
	calls := installStubs(t, stubOpts{})
	// Pre-populate the registry with a live wizard pointing at this bead.
	// PID=os.Getpid() is guaranteed alive for the duration of this test.
	if err := agent.RegistryAdd(agent.Entry{
		Name:      "wizard-spi-abc",
		PID:       os.Getpid(),
		BeadID:    "spi-abc",
		StartedAt: "2026-04-24T00:00:00Z",
	}); err != nil {
		t.Fatalf("seed registry: %v", err)
	}

	bead := store.Bead{ID: "spi-abc", Status: "in_progress", Type: "task"}
	res, err := SpawnWizard(bead, "")
	if err == nil {
		t.Fatal("SpawnWizard err = nil, want ErrAlreadyRunning")
	}
	if !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("err = %v, want wrapped ErrAlreadyRunning", err)
	}
	// Result.WizardName is reported for caller use; Identifier path stays clean.
	if res.WizardName != "wizard-spi-abc" {
		t.Fatalf("WizardName = %q, want wizard-spi-abc", res.WizardName)
	}
	// No spawn, no audit comment — the duplicate short-circuit must run
	// before the spawn/audit side-effects.
	if len(calls.spawns) != 0 {
		t.Fatalf("spawns = %+v, want none on duplicate", calls.spawns)
	}
	if len(calls.comments) != 0 {
		t.Fatalf("comments = %+v, want none on duplicate", calls.comments)
	}
}

func TestSpawnWizard_SpawnErrorPropagates(t *testing.T) {
	installStubs(t, stubOpts{spawnErr: errors.New("exec: no such file")})
	bead := store.Bead{ID: "spi-abc", Status: "in_progress", Type: "task"}
	_, err := SpawnWizard(bead, "")
	if err == nil {
		t.Fatal("SpawnWizard err = nil, want spawn error")
	}
	if !strings.Contains(err.Error(), "spawn wizard-spi-abc") {
		t.Fatalf("err = %v, want to wrap \"spawn wizard-spi-abc\"", err)
	}
}
