package store

import (
	"context"
	"testing"
	"time"

	"github.com/steveyegge/beads"
)

// mockUpdateStorage captures update calls so tests can verify status-
// transition stamping paths without a real DB. It also satisfies CloseIssue
// and CreateIssue so the other mutation paths can be exercised.
type mockUpdateStorage struct {
	beads.Storage
	updateCalls []map[string]interface{}
	closeCalls  []string
	createCalls []*beads.Issue
}

func (m *mockUpdateStorage) UpdateIssue(_ context.Context, _ string, updates map[string]interface{}, _ string) error {
	cp := make(map[string]interface{}, len(updates))
	for k, v := range updates {
		cp[k] = v
	}
	m.updateCalls = append(m.updateCalls, cp)
	return nil
}

func (m *mockUpdateStorage) CloseIssue(_ context.Context, id, _, _, _ string) error {
	m.closeCalls = append(m.closeCalls, id)
	return nil
}

func (m *mockUpdateStorage) CreateIssue(_ context.Context, issue *beads.Issue, _ string) error {
	if issue.ID == "" {
		issue.ID = "mock-" + string(issue.IssueType)
	}
	m.createCalls = append(m.createCalls, issue)
	return nil
}

func (m *mockUpdateStorage) GetIssue(_ context.Context, id string) (*beads.Issue, error) {
	return &beads.Issue{ID: id, Dependencies: []*beads.Dependency{}}, nil
}

func (m *mockUpdateStorage) AddDependency(_ context.Context, _ *beads.Dependency, _ string) error {
	return nil
}

func (m *mockUpdateStorage) Close() error { return nil }

// TestUpdateBead_StampingIsBestEffort verifies that UpdateBead calls the
// stamper without error when the store has no DB accessor (the mock path).
// The real stamping SQL is exercised in integration — here we just verify
// the no-DB path doesn't break the update.
func TestUpdateBead_StampingIsBestEffort(t *testing.T) {
	mock := &mockUpdateStorage{}
	setTestStore(t, mock)

	cases := []struct {
		name   string
		status string
	}{
		{"to_ready", "ready"},
		{"to_in_progress", "in_progress"},
		{"to_closed", "closed"},
		{"to_blocked_noop", "blocked"}, // non-transition status — no stamp
		{"unknown_noop", "gibberish"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := UpdateBead("spi-test", map[string]interface{}{"status": tc.status})
			if err != nil {
				t.Fatalf("UpdateBead(%q) err = %v", tc.status, err)
			}
		})
	}

	if len(mock.updateCalls) != len(cases) {
		t.Errorf("expected %d UpdateIssue calls, got %d", len(cases), len(mock.updateCalls))
	}
}

// TestCloseBead_StampsClosed verifies CloseBead triggers a closed stamp via
// the best-effort path. With the mock storage, the stamp is a no-op (no DB),
// so this test exists to lock in that the call sequence works.
func TestCloseBead_BestEffort(t *testing.T) {
	mock := &mockUpdateStorage{}
	setTestStore(t, mock)

	if err := CloseBead("spi-close"); err != nil {
		t.Fatalf("CloseBead err = %v", err)
	}
	if len(mock.closeCalls) != 1 || mock.closeCalls[0] != "spi-close" {
		t.Errorf("expected CloseIssue call for spi-close, got %v", mock.closeCalls)
	}
}

// TestCreateBead_StampsFiledBestEffort verifies CreateBead's stamp call does
// not propagate errors through to the caller. The real filing timestamp
// semantics live in integration; here we confirm the no-DB path is quiet.
func TestCreateBead_BestEffort(t *testing.T) {
	mock := &mockUpdateStorage{}
	setTestStore(t, mock)

	id, err := CreateBead(CreateOpts{
		Title:    "test",
		Priority: 1,
		Type:     beads.TypeTask,
	})
	if err != nil {
		t.Fatalf("CreateBead err = %v", err)
	}
	if id == "" {
		t.Error("expected non-empty ID")
	}
}

// TestStampHelpers_NoDBAreNoOps verifies the four StampX helpers return nil
// when the active store doesn't expose a DB. This is the path tests hit and
// must not fail — production wires a Dolt-backed storage that implements DB().
func TestStampHelpers_NoDBAreNoOps(t *testing.T) {
	mock := &mockUpdateStorage{}
	setTestStore(t, mock)

	now := time.Now().UTC()
	if err := StampFiled("b1", "task", now); err != nil {
		t.Errorf("StampFiled: %v", err)
	}
	if err := StampReady("b1", now); err != nil {
		t.Errorf("StampReady: %v", err)
	}
	if err := StampStarted("b1", now); err != nil {
		t.Errorf("StampStarted: %v", err)
	}
	if err := StampClosed("b1", now); err != nil {
		t.Errorf("StampClosed: %v", err)
	}
}

// TestStampStatusTransitionBestEffort_KnownStatuses ensures the dispatch
// vocabulary (ready / in_progress / closed) is covered and that unknown
// statuses are silently dropped.
func TestStampStatusTransitionBestEffort_KnownStatuses(t *testing.T) {
	mock := &mockUpdateStorage{}
	setTestStore(t, mock)

	// Known statuses: should not panic and should not error.
	for _, s := range []string{"ready", "in_progress", "closed", "blocked", ""} {
		stampStatusTransitionBestEffort("spi-x", s)
	}
	// Empty bead ID is a no-op guard.
	stampStatusTransitionBestEffort("", "ready")
}
