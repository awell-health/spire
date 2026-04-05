package store

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestUpdateLearningOutcome_Success(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectExec(`UPDATE recovery_learnings SET outcome = \? WHERE recovery_bead = \?`).
		WithArgs("relapsed", "rec-001").
		WillReturnResult(sqlmock.NewResult(0, 1))

	err = UpdateLearningOutcome(context.Background(), db, "rec-001", "relapsed")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestUpdateLearningOutcome_NoRows(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectExec(`UPDATE recovery_learnings SET outcome = \? WHERE recovery_bead = \?`).
		WithArgs("relapsed", "nonexistent").
		WillReturnResult(sqlmock.NewResult(0, 0))

	// Should succeed even if no rows matched — idempotent operation.
	err = UpdateLearningOutcome(context.Background(), db, "nonexistent", "relapsed")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestWriteRecoveryLearning_Success(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := time.Date(2026, 4, 5, 18, 0, 0, 0, time.UTC)
	row := RecoveryLearningRow{
		ID:              "learn-001",
		RecoveryBead:    "rec-001",
		SourceBead:      "spi-abc",
		FailureClass:    "implement-failed",
		FailureSig:      "implement-failed:implement",
		ResolutionKind:  "resummon",
		Outcome:         "clean",
		LearningSummary: "Resummon resolved the infra issue.",
		Reusable:        true,
		ResolvedAt:      now,
		ExpectedOutcome: "Wizard should pass verify-build after resummon.",
	}

	mock.ExpectExec(`INSERT INTO recovery_learnings`).
		WithArgs(
			row.ID, row.RecoveryBead, row.SourceBead, row.FailureClass,
			row.FailureSig, row.ResolutionKind, row.Outcome, row.LearningSummary,
			1, // reusable=true → 1
			now.UTC().Format("2006-01-02 15:04:05"),
			row.ExpectedOutcome,
		).
		WillReturnResult(sqlmock.NewResult(1, 1))

	err = WriteRecoveryLearning(context.Background(), db, row)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestGetBeadLearnings_Success(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	rows := sqlmock.NewRows([]string{
		"id", "recovery_bead", "source_bead", "failure_class", "failure_sig",
		"resolution_kind", "outcome", "learning_summary", "reusable", "resolved_at",
		"expected_outcome",
	}).
		AddRow("learn-001", "rec-001", "spi-abc", "implement-failed", "implement-failed:implement",
			"resummon", "clean", "Fixed via resummon.", 1, "2026-04-05 18:00:00",
			"Should pass build after resummon.").
		AddRow("learn-002", "rec-002", "spi-abc", "implement-failed", nil,
			"reset", "relapsed", nil, 1, "2026-04-05 16:00:00",
			nil)

	mock.ExpectQuery(`SELECT .+ FROM recovery_learnings WHERE source_bead = \? AND failure_class = \?`).
		WithArgs("spi-abc", "implement-failed").
		WillReturnRows(rows)

	results, err := GetBeadLearnings(context.Background(), db, "spi-abc", "implement-failed")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 learnings, got %d", len(results))
	}
	if results[0].Outcome != "clean" {
		t.Errorf("first row outcome = %q, want clean", results[0].Outcome)
	}
	if results[0].ExpectedOutcome != "Should pass build after resummon." {
		t.Errorf("first row expected_outcome = %q", results[0].ExpectedOutcome)
	}
	if results[1].Outcome != "relapsed" {
		t.Errorf("second row outcome = %q, want relapsed", results[1].Outcome)
	}
	if results[1].ExpectedOutcome != "" {
		t.Errorf("second row expected_outcome should be empty, got %q", results[1].ExpectedOutcome)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
