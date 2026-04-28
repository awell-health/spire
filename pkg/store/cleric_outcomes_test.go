package store

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestEnsureClericOutcomesTable(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectExec(`CREATE TABLE IF NOT EXISTS cleric_outcomes`).
		WillReturnResult(sqlmock.NewResult(0, 0))

	if err := EnsureClericOutcomesTable(db); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRecordClericOutcome_PendingApprove(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)
	o := ClericOutcome{
		ID:             "co-test01",
		RecoveryBeadID: "spi-rec1",
		SourceBeadID:   "spi-src1",
		FailureClass:   "step-failure:implement",
		Action:         "resummon",
		Gate:           "approve",
		TargetStep:     "implement",
		Finalized:      false,
		CreatedAt:      now,
	}
	mock.ExpectExec(`INSERT INTO cleric_outcomes`).
		WithArgs(
			o.ID, o.RecoveryBeadID, o.SourceBeadID, o.FailureClass, o.Action,
			o.Gate, o.TargetStep, nil, false,
			now.UTC().Format("2006-01-02 15:04:05"), nil,
		).
		WillReturnResult(sqlmock.NewResult(1, 1))

	if err := RecordClericOutcome(context.Background(), db, o); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRecordClericOutcome_FinalReject(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)
	o := ClericOutcome{
		ID:             "co-rj1",
		RecoveryBeadID: "spi-rec1",
		SourceBeadID:   "spi-src1",
		FailureClass:   "step-failure:implement",
		Action:         "resummon",
		Gate:           "reject",
		Finalized:      true,
		CreatedAt:      now,
	}
	mock.ExpectExec(`INSERT INTO cleric_outcomes`).
		WithArgs(
			o.ID, o.RecoveryBeadID, o.SourceBeadID, o.FailureClass, o.Action,
			o.Gate, nil, nil, true,
			now.UTC().Format("2006-01-02 15:04:05"),
			now.UTC().Format("2006-01-02 15:04:05"),
		).
		WillReturnResult(sqlmock.NewResult(1, 1))

	if err := RecordClericOutcome(context.Background(), db, o); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestFinalizeClericOutcome_Success(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	finalizedAt := time.Date(2026, 4, 28, 13, 0, 0, 0, time.UTC)
	mock.ExpectExec(`UPDATE cleric_outcomes\s+SET wizard_post_action_success = \?, finalized = TRUE, finalized_at = \?\s+WHERE id = \?`).
		WithArgs(1, finalizedAt.UTC().Format("2006-01-02 15:04:05"), "co-test01").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := FinalizeClericOutcome(context.Background(), db, "co-test01", true, finalizedAt); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestLastNFinalizedClericOutcomes_FiltersPending(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	rows := sqlmock.NewRows([]string{
		"id", "recovery_bead_id", "source_bead_id", "failure_class", "action",
		"gate", "target_step", "wizard_post_action_success", "finalized",
		"created_at", "finalized_at",
	}).
		AddRow("co-1", "spi-r1", "spi-s1", "step-failure:implement", "resummon",
			"approve", nil, 1, true, "2026-04-28 11:00:00", "2026-04-28 12:00:00").
		AddRow("co-2", "spi-r2", "spi-s2", "step-failure:implement", "resummon",
			"approve", nil, 1, true, "2026-04-28 10:00:00", "2026-04-28 11:00:00").
		AddRow("co-3", "spi-r3", "spi-s3", "step-failure:implement", "resummon",
			"approve", nil, 1, true, "2026-04-28 09:00:00", "2026-04-28 10:00:00")

	mock.ExpectQuery(`SELECT .+ FROM cleric_outcomes\s+WHERE failure_class = \? AND action = \? AND finalized = TRUE\s+ORDER BY finalized_at DESC, id DESC\s+LIMIT \?`).
		WithArgs("step-failure:implement", "resummon", 3).
		WillReturnRows(rows)

	results, err := LastNFinalizedClericOutcomes(context.Background(), db, "step-failure:implement", "resummon", 3)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("expected 3 outcomes, got %d", len(results))
	}
	if results[0].ID != "co-1" {
		t.Errorf("results[0].ID = %q, want co-1", results[0].ID)
	}
	if !results[0].WizardPostActionSuccess.Valid || !results[0].WizardPostActionSuccess.Bool {
		t.Errorf("results[0].Success = %+v, want true", results[0].WizardPostActionSuccess)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestPendingClericOutcomesForSourceBead(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	rows := sqlmock.NewRows([]string{
		"id", "recovery_bead_id", "source_bead_id", "failure_class", "action",
		"gate", "target_step", "wizard_post_action_success", "finalized",
		"created_at", "finalized_at",
	}).
		AddRow("co-pending", "spi-r1", "spi-s1", "step-failure:implement",
			"resummon", "approve", "implement", nil, false,
			"2026-04-28 11:00:00", nil)

	mock.ExpectQuery(`SELECT .+ FROM cleric_outcomes\s+WHERE source_bead_id = \? AND finalized = FALSE`).
		WithArgs("spi-s1").
		WillReturnRows(rows)

	results, err := PendingClericOutcomesForSourceBead(context.Background(), db, "spi-s1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 pending outcome, got %d", len(results))
	}
	if results[0].TargetStep != "implement" {
		t.Errorf("TargetStep = %q, want implement", results[0].TargetStep)
	}
	if results[0].WizardPostActionSuccess.Valid {
		t.Errorf("WizardPostActionSuccess.Valid = true, want false (NULL)")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestListDemotedClericPairs_Threshold3(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Two distinct pairs in the table.
	mock.ExpectQuery(`SELECT DISTINCT failure_class, action FROM cleric_outcomes WHERE finalized = TRUE`).
		WillReturnRows(sqlmock.NewRows([]string{"failure_class", "action"}).
			AddRow("step-failure:implement", "resummon").
			AddRow("step-failure:review", "reset --hard"))

	// Pair 1: 3 rejects -> demoted.
	mock.ExpectQuery(`SELECT .+ FROM cleric_outcomes\s+WHERE failure_class = \? AND action = \?`).
		WithArgs("step-failure:implement", "resummon", 3).
		WillReturnRows(buildOutcomeRows([]testRow{
			{id: "co-1", gate: "reject", finalized: true, finalizedAt: "2026-04-28 12:00:00"},
			{id: "co-2", gate: "reject", finalized: true, finalizedAt: "2026-04-28 11:00:00"},
			{id: "co-3", gate: "reject", finalized: true, finalizedAt: "2026-04-28 10:00:00"},
		}))

	// Pair 2: 2 rejects + 1 approve -> not demoted.
	mock.ExpectQuery(`SELECT .+ FROM cleric_outcomes\s+WHERE failure_class = \? AND action = \?`).
		WithArgs("step-failure:review", "reset --hard", 3).
		WillReturnRows(buildOutcomeRows([]testRow{
			{id: "co-4", gate: "reject", finalized: true, finalizedAt: "2026-04-28 12:00:00"},
			{id: "co-5", gate: "reject", finalized: true, finalizedAt: "2026-04-28 11:00:00"},
			{id: "co-6", gate: "approve", finalized: true, finalizedAt: "2026-04-28 10:00:00"},
		}))

	demoted, err := ListDemotedClericPairs(context.Background(), db, 3)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(demoted) != 1 {
		t.Fatalf("expected 1 demoted pair, got %d", len(demoted))
	}
	if demoted[0].FailureClass != "step-failure:implement" || demoted[0].Action != "resummon" {
		t.Errorf("demoted = %+v, want (step-failure:implement, resummon)", demoted[0])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// testRow / buildOutcomeRows are local helpers for ListDemoted tests.
type testRow struct {
	id          string
	gate        string
	finalized   bool
	finalizedAt string
}

func buildOutcomeRows(rs []testRow) *sqlmock.Rows {
	rows := sqlmock.NewRows([]string{
		"id", "recovery_bead_id", "source_bead_id", "failure_class", "action",
		"gate", "target_step", "wizard_post_action_success", "finalized",
		"created_at", "finalized_at",
	})
	for _, r := range rs {
		var success interface{} = nil
		if r.gate == "approve" {
			success = 1
		}
		var finalizedAt interface{} = nil
		if r.finalizedAt != "" {
			finalizedAt = r.finalizedAt
		}
		rows.AddRow(r.id, "spi-r", "spi-s", "fc", "act",
			r.gate, nil, success, r.finalized,
			"2026-04-28 09:00:00", finalizedAt)
	}
	return rows
}

// Sanity: scanClericOutcomes correctly hydrates nullable fields.
func TestScanClericOutcomes_NullableFields(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	rows := sqlmock.NewRows([]string{
		"id", "recovery_bead_id", "source_bead_id", "failure_class", "action",
		"gate", "target_step", "wizard_post_action_success", "finalized",
		"created_at", "finalized_at",
	}).
		AddRow("co-x", "spi-r", "spi-s", "fc", "act",
			"takeover", nil, nil, true,
			"2026-04-28 09:00:00", "2026-04-28 09:00:00")

	mock.ExpectQuery(`SELECT .+ FROM cleric_outcomes`).
		WithArgs("spi-s").
		WillReturnRows(rows)

	got, err := PendingClericOutcomesForSourceBead(context.Background(), db, "spi-s")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	// Exactly one row but it's actually finalized=true; mock returned it
	// because we forced the row regardless of WHERE — the assertion is on
	// the scanner's nullable-field handling.
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	if got[0].TargetStep != "" {
		t.Errorf("TargetStep = %q, want empty (NULL)", got[0].TargetStep)
	}
	if got[0].WizardPostActionSuccess.Valid {
		t.Errorf("WizardPostActionSuccess.Valid = true, want false")
	}
	if !got[0].FinalizedAt.Valid {
		t.Errorf("FinalizedAt.Valid = false, want true")
	}
}

// Compile-time guard: ensure ClericOutcome's nullable success field is the
// shape we expect (NullBool from database/sql), so callers can rely on .Valid.
var _ sql.NullBool = ClericOutcome{}.WizardPostActionSuccess
