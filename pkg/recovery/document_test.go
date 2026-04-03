package recovery

import (
	"testing"
	"time"
)

// mockRecoveryDeps implements RecoveryDeps for testing.
type mockRecoveryDeps struct {
	updatedMeta map[string]interface{}
	comments    []string
	closedBeads []string
}

func (m *mockRecoveryDeps) GetBead(id string) (DepBead, error) {
	return DepBead{ID: id, Status: "in_progress"}, nil
}

func (m *mockRecoveryDeps) GetDependentsWithMeta(id string) ([]DepDependent, error) {
	return nil, nil
}

func (m *mockRecoveryDeps) UpdateBead(id string, meta map[string]interface{}) error {
	m.updatedMeta = meta
	return nil
}

func (m *mockRecoveryDeps) AddComment(id, text string) error {
	m.comments = append(m.comments, text)
	return nil
}

func (m *mockRecoveryDeps) CloseBead(id string) error {
	m.closedBeads = append(m.closedBeads, id)
	return nil
}

func TestDocumentLearning_WritesLearningSummary(t *testing.T) {
	deps := &mockRecoveryDeps{}
	learning := RecoveryLearning{
		ResolutionKind:     ResolutionResummon,
		VerificationStatus: VerifyHealthy,
		LearningKey:        "implement-merge-conflict",
		Reusable:           true,
		ResolvedAt:         time.Date(2026, 4, 3, 20, 0, 0, 0, time.UTC),
		Narrative:          "Resolved by rebasing onto updated main",
		LearningSummary:    "Merge conflict in implement phase resolved via rebase",
	}

	err := DocumentLearning(deps, "spi-rec-1", learning)
	if err != nil {
		t.Fatalf("DocumentLearning returned error: %v", err)
	}

	// Verify learning_summary was written to metadata.
	if deps.updatedMeta == nil {
		t.Fatal("expected metadata update, got nil")
	}
	got, ok := deps.updatedMeta[KeyLearningSummary]
	if !ok {
		t.Fatal("learning_summary key not found in metadata update")
	}
	if got != "Merge conflict in implement phase resolved via rebase" {
		t.Errorf("learning_summary = %q, want %q", got, "Merge conflict in implement phase resolved via rebase")
	}
}

func TestDocumentLearning_EmptyLearningSummary(t *testing.T) {
	deps := &mockRecoveryDeps{}
	learning := RecoveryLearning{
		ResolutionKind:     ResolutionManual,
		VerificationStatus: VerifyUnknown,
		LearningKey:        "manual-fix",
		Reusable:           false,
		ResolvedAt:         time.Date(2026, 4, 3, 21, 0, 0, 0, time.UTC),
		Narrative:          "Fixed manually",
		LearningSummary:    "",
	}

	err := DocumentLearning(deps, "spi-rec-2", learning)
	if err != nil {
		t.Fatalf("DocumentLearning returned error: %v", err)
	}

	// Even empty LearningSummary should be present in metadata (queryable).
	got, ok := deps.updatedMeta[KeyLearningSummary]
	if !ok {
		t.Fatal("learning_summary key should be present even when empty")
	}
	if got != "" {
		t.Errorf("learning_summary = %q, want empty", got)
	}
}

func TestDocumentLearning_AllFieldsWritten(t *testing.T) {
	deps := &mockRecoveryDeps{}
	resolvedAt := time.Date(2026, 4, 3, 20, 30, 0, 0, time.UTC)
	learning := RecoveryLearning{
		ResolutionKind:     ResolutionRebase,
		VerificationStatus: VerifyHealthy,
		LearningKey:        "rebase-fix",
		Reusable:           true,
		ResolvedAt:         resolvedAt,
		Narrative:          "Applied rebase strategy",
		LearningSummary:    "Rebase resolved diverged branches",
	}

	err := DocumentLearning(deps, "spi-rec-3", learning)
	if err != nil {
		t.Fatalf("DocumentLearning returned error: %v", err)
	}

	expectedKeys := []struct {
		key  string
		want string
	}{
		{KeyResolutionKind, "rebase"},
		{KeyVerificationStatus, "healthy"},
		{KeyLearningKey, "rebase-fix"},
		{KeyReusable, "true"},
		{KeyResolvedAt, "2026-04-03T20:30:00Z"},
		{KeyLearningSummary, "Rebase resolved diverged branches"},
	}

	for _, exp := range expectedKeys {
		got, ok := deps.updatedMeta[exp.key]
		if !ok {
			t.Errorf("key %q not found in metadata", exp.key)
			continue
		}
		if got != exp.want {
			t.Errorf("%s = %q, want %q", exp.key, got, exp.want)
		}
	}
}

func TestDocumentLearning_AddsComment(t *testing.T) {
	deps := &mockRecoveryDeps{}
	learning := RecoveryLearning{
		ResolutionKind:     ResolutionResummon,
		VerificationStatus: VerifyHealthy,
		LearningKey:        "test-key",
		Reusable:           false,
		ResolvedAt:         time.Date(2026, 4, 3, 22, 0, 0, 0, time.UTC),
		Narrative:          "Test narrative",
		LearningSummary:    "Test summary",
	}

	err := DocumentLearning(deps, "spi-rec-4", learning)
	if err != nil {
		t.Fatalf("DocumentLearning returned error: %v", err)
	}

	if len(deps.comments) != 1 {
		t.Fatalf("expected 1 comment, got %d", len(deps.comments))
	}
}
