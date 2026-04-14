package store

import (
	"context"
	"fmt"
	"testing"

	"github.com/steveyegge/beads"
)

// schedMockStorage embeds beads.Storage and overrides GetReadyWork
// to return a controlled set of issues for schedule tests.
type schedMockStorage struct {
	beads.Storage
	readyIssues []*beads.Issue
}

func (m *schedMockStorage) GetReadyWork(_ context.Context, _ beads.WorkFilter) ([]*beads.Issue, error) {
	return m.readyIssues, nil
}

func (m *schedMockStorage) SearchIssues(_ context.Context, _ string, filter beads.IssueFilter) ([]*beads.Issue, error) {
	// Used by GetChildren inside GetActiveAttempt — return nothing by default.
	return nil, nil
}

func (m *schedMockStorage) Close() error { return nil }

func TestGetSchedulableWork_MsgLabelExcluded(t *testing.T) {
	mock := &schedMockStorage{
		readyIssues: []*beads.Issue{
			{ID: "spi-clean", Title: "Clean task", Status: "ready", IssueType: beads.TypeTask},
			{ID: "spi-msg", Title: "A message", Status: beads.StatusOpen, IssueType: "message", Labels: []string{"msg"}},
		},
	}
	setTestStore(t, mock)

	origAttempt := GetActiveAttemptFunc
	GetActiveAttemptFunc = func(parentID string) (*Bead, error) { return nil, nil }
	defer func() { GetActiveAttemptFunc = origAttempt }()

	result, err := GetSchedulableWork(beads.WorkFilter{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Schedulable) != 1 {
		t.Fatalf("expected 1 schedulable, got %d", len(result.Schedulable))
	}
	if result.Schedulable[0].ID != "spi-clean" {
		t.Errorf("expected spi-clean, got %s", result.Schedulable[0].ID)
	}
}

func TestGetSchedulableWork_MsgPrefixLabelExcluded(t *testing.T) {
	mock := &schedMockStorage{
		readyIssues: []*beads.Issue{
			{ID: "spi-clean", Title: "Clean task", Status: "ready", IssueType: beads.TypeTask},
			{ID: "spi-msgpfx", Title: "Msg with prefix", Status: beads.StatusOpen, IssueType: "message", Labels: []string{"msg:routing"}},
		},
	}
	setTestStore(t, mock)

	origAttempt := GetActiveAttemptFunc
	GetActiveAttemptFunc = func(parentID string) (*Bead, error) { return nil, nil }
	defer func() { GetActiveAttemptFunc = origAttempt }()

	result, err := GetSchedulableWork(beads.WorkFilter{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Schedulable) != 1 {
		t.Fatalf("expected 1 schedulable, got %d", len(result.Schedulable))
	}
	if result.Schedulable[0].ID != "spi-clean" {
		t.Errorf("expected spi-clean, got %s", result.Schedulable[0].ID)
	}
}

func TestGetSchedulableWork_TemplateLabelExcluded(t *testing.T) {
	mock := &schedMockStorage{
		readyIssues: []*beads.Issue{
			{ID: "spi-clean", Title: "Clean task", Status: "ready", IssueType: beads.TypeTask},
			{ID: "spi-tmpl", Title: "Template bead", Status: "ready", IssueType: beads.TypeTask, Labels: []string{"template"}},
		},
	}
	setTestStore(t, mock)

	origAttempt := GetActiveAttemptFunc
	GetActiveAttemptFunc = func(parentID string) (*Bead, error) { return nil, nil }
	defer func() { GetActiveAttemptFunc = origAttempt }()

	result, err := GetSchedulableWork(beads.WorkFilter{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Schedulable) != 1 {
		t.Fatalf("expected 1 schedulable, got %d", len(result.Schedulable))
	}
	if result.Schedulable[0].ID != "spi-clean" {
		t.Errorf("expected spi-clean, got %s", result.Schedulable[0].ID)
	}
}

func TestGetSchedulableWork_ActiveAttemptExcluded(t *testing.T) {
	mock := &schedMockStorage{
		readyIssues: []*beads.Issue{
			{ID: "spi-clean", Title: "Clean task", Status: "ready", IssueType: beads.TypeTask},
			{ID: "spi-owned", Title: "Owned bead", Status: "ready", IssueType: beads.TypeTask},
		},
	}
	setTestStore(t, mock)

	origAttempt := GetActiveAttemptFunc
	GetActiveAttemptFunc = func(parentID string) (*Bead, error) {
		if parentID == "spi-owned" {
			return &Bead{ID: "spi-owned.attempt-1", Status: "in_progress", Labels: []string{"attempt"}}, nil
		}
		return nil, nil
	}
	defer func() { GetActiveAttemptFunc = origAttempt }()

	result, err := GetSchedulableWork(beads.WorkFilter{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Schedulable) != 1 {
		t.Fatalf("expected 1 schedulable, got %d", len(result.Schedulable))
	}
	if result.Schedulable[0].ID != "spi-clean" {
		t.Errorf("expected spi-clean, got %s", result.Schedulable[0].ID)
	}
}

func TestGetSchedulableWork_MultipleAttemptsQuarantined(t *testing.T) {
	mock := &schedMockStorage{
		readyIssues: []*beads.Issue{
			{ID: "spi-clean", Title: "Clean task", Status: "ready", IssueType: beads.TypeTask},
			{ID: "spi-broken", Title: "Broken bead", Status: "ready", IssueType: beads.TypeTask},
		},
	}
	setTestStore(t, mock)

	origAttempt := GetActiveAttemptFunc
	GetActiveAttemptFunc = func(parentID string) (*Bead, error) {
		if parentID == "spi-broken" {
			return nil, fmt.Errorf("invariant violation: 2 open attempt beads for spi-broken")
		}
		return nil, nil
	}
	defer func() { GetActiveAttemptFunc = origAttempt }()

	result, err := GetSchedulableWork(beads.WorkFilter{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Schedulable) != 1 {
		t.Fatalf("expected 1 schedulable, got %d", len(result.Schedulable))
	}
	if result.Schedulable[0].ID != "spi-clean" {
		t.Errorf("expected spi-clean, got %s", result.Schedulable[0].ID)
	}
	if len(result.Quarantined) != 1 {
		t.Fatalf("expected 1 quarantined, got %d", len(result.Quarantined))
	}
	if result.Quarantined[0].ID != "spi-broken" {
		t.Errorf("expected quarantined spi-broken, got %s", result.Quarantined[0].ID)
	}
	if result.Quarantined[0].Error == nil {
		t.Error("expected quarantined error to be non-nil")
	}
}

func TestGetSchedulableWork_CleanBeadPassesThrough(t *testing.T) {
	mock := &schedMockStorage{
		readyIssues: []*beads.Issue{
			{ID: "spi-task1", Title: "Task 1", Status: "ready", IssueType: beads.TypeTask, Priority: 1},
			{ID: "spi-task2", Title: "Task 2", Status: "ready", IssueType: beads.TypeFeature, Priority: 2},
		},
	}
	setTestStore(t, mock)

	origAttempt := GetActiveAttemptFunc
	GetActiveAttemptFunc = func(parentID string) (*Bead, error) { return nil, nil }
	defer func() { GetActiveAttemptFunc = origAttempt }()

	result, err := GetSchedulableWork(beads.WorkFilter{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Schedulable) != 2 {
		t.Fatalf("expected 2 schedulable, got %d", len(result.Schedulable))
	}
	if len(result.Quarantined) != 0 {
		t.Errorf("expected 0 quarantined, got %d", len(result.Quarantined))
	}
}
