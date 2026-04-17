package store

import (
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestEnsureRecoveryAttemptsTable(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectExec(`CREATE TABLE IF NOT EXISTS recovery_attempts`).
		WillReturnResult(sqlmock.NewResult(0, 0))

	if err := EnsureRecoveryAttemptsTable(db); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRecordRecoveryAttempt(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := time.Date(2026, 4, 14, 12, 0, 0, 0, time.UTC)
	attempt := RecoveryAttempt{
		ID:             "ra-test0001",
		RecoveryBeadID: "spi-rec01",
		TargetBeadID:   "spi-abc",
		Action:         "resummon",
		Params:         `{"step":"implement"}`,
		Outcome:        "in_progress",
		Error:          "",
		AttemptNumber:  1,
		CreatedAt:      now,
	}

	mock.ExpectExec(`INSERT INTO recovery_attempts`).
		WithArgs(
			attempt.ID, attempt.RecoveryBeadID, attempt.TargetBeadID,
			attempt.Action, attempt.Params, attempt.Outcome, attempt.Error,
			attempt.AttemptNumber, now.UTC().Format("2006-01-02 15:04:05"),
		).
		WillReturnResult(sqlmock.NewResult(1, 1))

	if err := RecordRecoveryAttempt(db, attempt); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRecordRecoveryAttempt_AutoSetsIDAndCreatedAt(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	attempt := RecoveryAttempt{
		RecoveryBeadID: "spi-rec01",
		TargetBeadID:   "spi-abc",
		Action:         "rebase-onto-base",
		Outcome:        "in_progress",
		AttemptNumber:  1,
	}

	mock.ExpectExec(`INSERT INTO recovery_attempts`).
		WithArgs(
			sqlmock.AnyArg(), // auto-generated ID
			attempt.RecoveryBeadID, attempt.TargetBeadID,
			attempt.Action, attempt.Params, attempt.Outcome, attempt.Error,
			attempt.AttemptNumber,
			sqlmock.AnyArg(), // auto-set CreatedAt
		).
		WillReturnResult(sqlmock.NewResult(1, 1))

	if err := RecordRecoveryAttempt(db, attempt); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestUpdateAttemptOutcome(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectExec(`UPDATE recovery_attempts SET outcome = \?, error_text = \? WHERE id = \?`).
		WithArgs("success", "", "ra-test0001").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := UpdateAttemptOutcome(db, "ra-test0001", "success", ""); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestUpdateAttemptOutcome_WithError(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectExec(`UPDATE recovery_attempts SET outcome = \?, error_text = \? WHERE id = \?`).
		WithArgs("failure", "rebase conflict in main.go", "ra-test0002").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := UpdateAttemptOutcome(db, "ra-test0002", "failure", "rebase conflict in main.go"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestListRecoveryAttempts(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	rows := sqlmock.NewRows([]string{
		"id", "recovery_bead_id", "target_bead_id", "action", "params",
		"outcome", "error_text", "attempt_number", "created_at",
	}).
		AddRow("ra-001", "spi-rec01", "spi-abc", "resummon", nil,
			"failure", "build failed", 1, "2026-04-14 10:00:00").
		AddRow("ra-002", "spi-rec01", "spi-abc", "rebase-onto-base", `{"branch":"main"}`,
			"success", nil, 2, "2026-04-14 11:00:00")

	mock.ExpectQuery(`SELECT .+ FROM recovery_attempts WHERE recovery_bead_id = \? ORDER BY attempt_number ASC`).
		WithArgs("spi-rec01").
		WillReturnRows(rows)

	results, err := ListRecoveryAttempts(db, "spi-rec01")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 attempts, got %d", len(results))
	}
	if results[0].ID != "ra-001" {
		t.Errorf("first attempt ID = %q, want ra-001", results[0].ID)
	}
	if results[0].Action != "resummon" {
		t.Errorf("first attempt action = %q, want resummon", results[0].Action)
	}
	if results[0].Outcome != "failure" {
		t.Errorf("first attempt outcome = %q, want failure", results[0].Outcome)
	}
	if results[0].Error != "build failed" {
		t.Errorf("first attempt error = %q, want 'build failed'", results[0].Error)
	}
	if results[0].Params != "" {
		t.Errorf("first attempt params = %q, want empty", results[0].Params)
	}
	if results[1].ID != "ra-002" {
		t.Errorf("second attempt ID = %q, want ra-002", results[1].ID)
	}
	if results[1].Params != `{"branch":"main"}` {
		t.Errorf("second attempt params = %q", results[1].Params)
	}
	if results[1].Outcome != "success" {
		t.Errorf("second attempt outcome = %q, want success", results[1].Outcome)
	}
	if results[1].Error != "" {
		t.Errorf("second attempt error = %q, want empty", results[1].Error)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestListRecoveryAttempts_Empty(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	rows := sqlmock.NewRows([]string{
		"id", "recovery_bead_id", "target_bead_id", "action", "params",
		"outcome", "error_text", "attempt_number", "created_at",
	})

	mock.ExpectQuery(`SELECT .+ FROM recovery_attempts WHERE recovery_bead_id = \?`).
		WithArgs("spi-nonexistent").
		WillReturnRows(rows)

	results, err := ListRecoveryAttempts(db, "spi-nonexistent")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("expected 0 attempts, got %d", len(results))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestGetLatestAttempt(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	rows := sqlmock.NewRows([]string{
		"id", "recovery_bead_id", "target_bead_id", "action", "params",
		"outcome", "error_text", "attempt_number", "created_at",
	}).AddRow("ra-003", "spi-rec01", "spi-abc", "rebuild", nil,
		"success", nil, 3, "2026-04-14 12:00:00")

	mock.ExpectQuery(`SELECT .+ FROM recovery_attempts WHERE recovery_bead_id = \? ORDER BY attempt_number DESC LIMIT 1`).
		WithArgs("spi-rec01").
		WillReturnRows(rows)

	result, err := GetLatestAttempt(db, "spi-rec01")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if result.ID != "ra-003" {
		t.Errorf("ID = %q, want ra-003", result.ID)
	}
	if result.AttemptNumber != 3 {
		t.Errorf("AttemptNumber = %d, want 3", result.AttemptNumber)
	}
	if result.Outcome != "success" {
		t.Errorf("Outcome = %q, want success", result.Outcome)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestGetLatestAttempt_None(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	rows := sqlmock.NewRows([]string{
		"id", "recovery_bead_id", "target_bead_id", "action", "params",
		"outcome", "error_text", "attempt_number", "created_at",
	})

	mock.ExpectQuery(`SELECT .+ FROM recovery_attempts WHERE recovery_bead_id = \?`).
		WithArgs("spi-nonexistent").
		WillReturnRows(rows)

	result, err := GetLatestAttempt(db, "spi-nonexistent")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != nil {
		t.Errorf("expected nil result, got %+v", result)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestCountAttemptsByAction(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM recovery_attempts WHERE recovery_bead_id = \? AND action = \?`).
		WithArgs("spi-rec01", "resummon").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(3))

	count, err := CountAttemptsByAction(db, "spi-rec01", "resummon")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 3 {
		t.Errorf("count = %d, want 3", count)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestCountAttemptsByAction_Zero(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM recovery_attempts WHERE recovery_bead_id = \? AND action = \?`).
		WithArgs("spi-rec01", "escalate").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))

	count, err := CountAttemptsByAction(db, "spi-rec01", "escalate")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 0 {
		t.Errorf("count = %d, want 0", count)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestGenerateAttemptID(t *testing.T) {
	id := generateAttemptID()
	if len(id) != 11 { // "ra-" + 8 hex chars
		t.Errorf("ID length = %d, want 11, got %q", len(id), id)
	}
	if id[:3] != "ra-" {
		t.Errorf("ID prefix = %q, want 'ra-'", id[:3])
	}
	// Ensure uniqueness (probabilistic but effectively guaranteed).
	id2 := generateAttemptID()
	if id == id2 {
		t.Errorf("two generated IDs are equal: %s", id)
	}
}

func TestRecoveryAttemptTimeParsing(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	rows := sqlmock.NewRows([]string{
		"id", "recovery_bead_id", "target_bead_id", "action", "params",
		"outcome", "error_text", "attempt_number", "created_at",
	}).AddRow("ra-time", "spi-rec01", "spi-abc", "rebuild", nil,
		"success", nil, 1, "2026-04-14 15:30:45")

	mock.ExpectQuery(`SELECT .+ FROM recovery_attempts WHERE recovery_bead_id = \?`).
		WithArgs("spi-rec01").
		WillReturnRows(rows)

	result, err := GetLatestAttempt(db, "spi-rec01")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}

	expected := time.Date(2026, 4, 14, 15, 30, 45, 0, time.UTC)
	if !result.CreatedAt.Equal(expected) {
		t.Errorf("CreatedAt = %v, want %v", result.CreatedAt, expected)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
