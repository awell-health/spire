package alerts

import (
	"errors"
	"strings"
	"testing"

	"github.com/awell-health/spire/pkg/store"
)

// fakeOps is an in-memory BeadOps for testing without a live store.
type fakeOps struct {
	created  []store.CreateOpts
	deps     [][3]string // [from, to, depType]
	createID string      // ID to return from CreateBead; defaults to prefix+"test-id"
	createErr error
	depErr    error
}

func (f *fakeOps) CreateBead(opts store.CreateOpts) (string, error) {
	f.created = append(f.created, opts)
	if f.createErr != nil {
		return "", f.createErr
	}
	id := f.createID
	if id == "" {
		id = opts.Prefix + "-test-id"
	}
	return id, nil
}

func (f *fakeOps) AddDepTyped(from, to, depType string) error {
	f.deps = append(f.deps, [3]string{from, to, depType})
	return f.depErr
}

// --- Tests ---

func TestRaise_DerivesPrefix_Spd(t *testing.T) {
	ops := &fakeOps{}
	id, err := Raise(ops, "spd-ac5", ClassAlert, "test alert", WithSubclass("merge-failure"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasPrefix(id, "spd-") {
		t.Errorf("alertID should have prefix spd-, got %q", id)
	}
	if len(ops.created) != 1 || ops.created[0].Prefix != "spd" {
		t.Errorf("CreateBead should be called with Prefix=spd, got %v", ops.created)
	}
}

func TestRaise_DerivesPrefix_Spi(t *testing.T) {
	ops := &fakeOps{}
	id, err := Raise(ops, "spi-0fek6l", ClassAlert, "test alert", WithSubclass("build-failure"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasPrefix(id, "spi-") {
		t.Errorf("alertID should have prefix spi-, got %q", id)
	}
	if ops.created[0].Prefix != "spi" {
		t.Errorf("Prefix should be spi, got %q", ops.created[0].Prefix)
	}
}

func TestRaise_InvalidSource_Empty(t *testing.T) {
	ops := &fakeOps{}
	_, err := Raise(ops, "", ClassAlert, "test")
	if err == nil {
		t.Fatal("expected error for empty sourceBeadID")
	}
	if len(ops.created) != 0 {
		t.Error("no bead should be created on empty sourceBeadID")
	}
}

func TestRaise_InvalidSource_Malformed(t *testing.T) {
	ops := &fakeOps{}
	// No hyphen → PrefixFromID returns ""
	_, err := Raise(ops, "nohyphen", ClassAlert, "test")
	if err == nil {
		t.Fatal("expected error for malformed sourceBeadID (no prefix)")
	}
	if len(ops.created) != 0 {
		t.Error("no bead should be created when prefix cannot be derived")
	}
}

func TestRaise_ArchmageMsg_Labels(t *testing.T) {
	ops := &fakeOps{}
	_, err := Raise(ops, "spi-abc", ClassArchmageMsg, "help needed", WithFrom("wizard-42"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(ops.created) != 1 {
		t.Fatalf("expected 1 CreateBead call, got %d", len(ops.created))
	}
	got := ops.created[0].Labels
	wantLabels := []string{"msg", "to:archmage", "from:wizard-42"}
	for _, want := range wantLabels {
		found := false
		for _, g := range got {
			if g == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("missing label %q in %v", want, got)
		}
	}
	// Must NOT have alert:* label
	for _, g := range got {
		if strings.HasPrefix(g, "alert:") {
			t.Errorf("archmage msg must not have alert: label, got %q", g)
		}
	}
}

func TestRaise_Alert_Labels(t *testing.T) {
	ops := &fakeOps{}
	_, err := Raise(ops, "spi-abc", ClassAlert, "title", WithSubclass("merge-failure"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := ops.created[0].Labels
	found := false
	for _, g := range got {
		if g == "alert:merge-failure" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected label alert:merge-failure in %v", got)
	}
}

func TestRaise_AddsCausedByDep_ForAlert(t *testing.T) {
	ops := &fakeOps{createID: "spi-alert-new"}
	_, err := Raise(ops, "spi-parent-123", ClassAlert, "test", WithSubclass("dispatch-failure"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(ops.deps) != 1 {
		t.Fatalf("expected 1 dep call, got %d", len(ops.deps))
	}
	dep := ops.deps[0]
	if dep[0] != "spi-alert-new" {
		t.Errorf("dep from should be alert ID, got %q", dep[0])
	}
	if dep[1] != "spi-parent-123" {
		t.Errorf("dep to should be sourceBeadID, got %q", dep[1])
	}
	if dep[2] != "caused-by" {
		t.Errorf("dep type should be caused-by, got %q", dep[2])
	}
}

func TestRaise_AddsRelatedDep_ForArchmageMsg(t *testing.T) {
	ops := &fakeOps{createID: "spi-msg-new"}
	_, err := Raise(ops, "spi-parent-123", ClassArchmageMsg, "help", WithFrom("wiz"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(ops.deps) != 1 {
		t.Fatalf("expected 1 dep call, got %d", len(ops.deps))
	}
	dep := ops.deps[0]
	if dep[2] != "related" {
		t.Errorf("archmage msg dep type should be related, got %q", dep[2])
	}
}

func TestRaise_ErrorsPropagate_CreateBeadFailure(t *testing.T) {
	ops := &fakeOps{createErr: errors.New("store unavailable")}
	id, err := Raise(ops, "spi-abc", ClassAlert, "test")
	if err == nil {
		t.Fatal("expected error from CreateBead failure")
	}
	if id != "" {
		t.Errorf("expected empty ID on CreateBead failure, got %q", id)
	}
	if len(ops.deps) != 0 {
		t.Error("AddDepTyped must not be called when CreateBead fails")
	}
}

func TestRaise_ErrorsPropagate_AddDepTypedFailure(t *testing.T) {
	ops := &fakeOps{
		createID: "spi-alert-x",
		depErr:   errors.New("dep store error"),
	}
	id, err := Raise(ops, "spi-abc", ClassAlert, "test")
	if err == nil {
		t.Fatal("expected error from AddDepTyped failure")
	}
	// Bead was created — ID must be returned alongside the error.
	if id != "spi-alert-x" {
		t.Errorf("on AddDepTyped failure, alertID should be returned, got %q", id)
	}
}

func TestRaise_WithPriority_Override(t *testing.T) {
	ops := &fakeOps{}
	_, err := Raise(ops, "spi-abc", ClassAlert, "test", WithPriority(2))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ops.created[0].Priority != 2 {
		t.Errorf("priority should be 2, got %d", ops.created[0].Priority)
	}
}

func TestRaise_WithExtraLabels(t *testing.T) {
	ops := &fakeOps{}
	_, err := Raise(ops, "spi-abc", ClassArchmageMsg, "msg", WithFrom("wiz"), WithExtraLabels("custom-label", "another"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := ops.created[0].Labels
	for _, want := range []string{"custom-label", "another"} {
		found := false
		for _, g := range got {
			if g == want {
				found = true
			}
		}
		if !found {
			t.Errorf("missing extra label %q in %v", want, got)
		}
	}
}

func TestRaise_DefaultPriority_Alert(t *testing.T) {
	ops := &fakeOps{}
	_, err := Raise(ops, "spi-abc", ClassAlert, "test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ops.created[0].Priority != 0 {
		t.Errorf("ClassAlert default priority should be 0 (P0), got %d", ops.created[0].Priority)
	}
}

func TestRaise_DefaultPriority_ArchmageMsg(t *testing.T) {
	ops := &fakeOps{}
	_, err := Raise(ops, "spi-abc", ClassArchmageMsg, "test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ops.created[0].Priority != 1 {
		t.Errorf("ClassArchmageMsg default priority should be 1 (P1), got %d", ops.created[0].Priority)
	}
}

func TestRaise_TitleTruncation(t *testing.T) {
	ops := &fakeOps{}
	longTitle := strings.Repeat("x", 300)
	_, err := Raise(ops, "spi-abc", ClassAlert, longTitle)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(ops.created[0].Title) > 200 {
		t.Errorf("title should be truncated to 200 chars, got %d", len(ops.created[0].Title))
	}
}
