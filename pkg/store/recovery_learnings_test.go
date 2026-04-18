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
			row.MechanicalRecipe,
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
		"expected_outcome", "mechanical_recipe", "demoted_at",
	}).
		AddRow("learn-001", "rec-001", "spi-abc", "implement-failed", "implement-failed:implement",
			"resummon", "clean", "Fixed via resummon.", 1, "2026-04-05 18:00:00",
			"Should pass build after resummon.", nil, nil).
		AddRow("learn-002", "rec-002", "spi-abc", "implement-failed", nil,
			"reset", "relapsed", nil, 1, "2026-04-05 16:00:00",
			nil, nil, nil)

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

// ---------------------------------------------------------------------------
// GetLearningStats tests
// ---------------------------------------------------------------------------

func TestGetLearningStats_AggregatesOutcomes(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Simulate grouped outcome counts for failure class "step-failure".
	rows := sqlmock.NewRows([]string{"resolution_kind", "outcome", "count"}).
		AddRow("resummon", "clean", 5).
		AddRow("resummon", "dirty", 2).
		AddRow("resummon", "relapsed", 1).
		AddRow("reset", "clean", 3).
		AddRow("reset", "dirty", 1)

	mock.ExpectQuery(`SELECT resolution_kind, outcome, COUNT\(\*\) FROM recovery_learnings`).
		WithArgs("step-failure").
		WillReturnRows(rows)

	// Prediction accuracy: 4 matched out of 6 with expected_outcome.
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM recovery_learnings WHERE failure_class = \? AND expected_outcome != '' AND outcome = expected_outcome`).
		WithArgs("step-failure").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(4))

	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM recovery_learnings WHERE failure_class = \? AND expected_outcome != ''`).
		WithArgs("step-failure").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(6))

	stats, err := GetLearningStats(context.Background(), db, "step-failure")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if stats.FailureClass != "step-failure" {
		t.Errorf("FailureClass = %q, want %q", stats.FailureClass, "step-failure")
	}
	if stats.TotalRecoveries != 12 {
		t.Errorf("TotalRecoveries = %d, want 12", stats.TotalRecoveries)
	}

	// Find resummon and reset stats.
	actionMap := make(map[string]ActionOutcomeStat)
	for _, as := range stats.ActionStats {
		actionMap[as.ResolutionKind] = as
	}

	resummon, ok := actionMap["resummon"]
	if !ok {
		t.Fatal("missing resummon in ActionStats")
	}
	if resummon.Total != 8 {
		t.Errorf("resummon.Total = %d, want 8", resummon.Total)
	}
	if resummon.CleanCount != 5 {
		t.Errorf("resummon.CleanCount = %d, want 5", resummon.CleanCount)
	}
	if resummon.DirtyCount != 2 {
		t.Errorf("resummon.DirtyCount = %d, want 2", resummon.DirtyCount)
	}
	if resummon.RelapsedCount != 1 {
		t.Errorf("resummon.RelapsedCount = %d, want 1", resummon.RelapsedCount)
	}
	// SuccessRate = 5/8 = 0.625
	if resummon.SuccessRate < 0.624 || resummon.SuccessRate > 0.626 {
		t.Errorf("resummon.SuccessRate = %f, want ~0.625", resummon.SuccessRate)
	}

	reset, ok := actionMap["reset"]
	if !ok {
		t.Fatal("missing reset in ActionStats")
	}
	if reset.Total != 4 {
		t.Errorf("reset.Total = %d, want 4", reset.Total)
	}
	if reset.CleanCount != 3 {
		t.Errorf("reset.CleanCount = %d, want 3", reset.CleanCount)
	}
	// SuccessRate = 3/4 = 0.75
	if reset.SuccessRate < 0.749 || reset.SuccessRate > 0.751 {
		t.Errorf("reset.SuccessRate = %f, want 0.75", reset.SuccessRate)
	}

	// Prediction accuracy = 4/6 ≈ 0.6667
	if stats.PredictionAccuracy < 0.666 || stats.PredictionAccuracy > 0.667 {
		t.Errorf("PredictionAccuracy = %f, want ~0.6667", stats.PredictionAccuracy)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestGetLearningStats_NoRows(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	rows := sqlmock.NewRows([]string{"resolution_kind", "outcome", "count"})
	mock.ExpectQuery(`SELECT resolution_kind, outcome, COUNT\(\*\) FROM recovery_learnings`).
		WithArgs("unknown-failure").
		WillReturnRows(rows)

	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM recovery_learnings WHERE failure_class = \? AND expected_outcome != '' AND outcome = expected_outcome`).
		WithArgs("unknown-failure").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))

	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM recovery_learnings WHERE failure_class = \? AND expected_outcome != ''`).
		WithArgs("unknown-failure").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))

	stats, err := GetLearningStats(context.Background(), db, "unknown-failure")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if stats.TotalRecoveries != 0 {
		t.Errorf("TotalRecoveries = %d, want 0", stats.TotalRecoveries)
	}
	if len(stats.ActionStats) != 0 {
		t.Errorf("ActionStats length = %d, want 0", len(stats.ActionStats))
	}
	if stats.PredictionAccuracy != 0 {
		t.Errorf("PredictionAccuracy = %f, want 0", stats.PredictionAccuracy)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestGetLearningStats_PredictionAccuracyNoPredictions(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	rows := sqlmock.NewRows([]string{"resolution_kind", "outcome", "count"}).
		AddRow("resummon", "clean", 3)

	mock.ExpectQuery(`SELECT resolution_kind, outcome, COUNT\(\*\) FROM recovery_learnings`).
		WithArgs("step-failure").
		WillReturnRows(rows)

	// No rows had expected_outcome set.
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM recovery_learnings WHERE failure_class = \? AND expected_outcome != '' AND outcome = expected_outcome`).
		WithArgs("step-failure").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))

	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM recovery_learnings WHERE failure_class = \? AND expected_outcome != ''`).
		WithArgs("step-failure").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))

	stats, err := GetLearningStats(context.Background(), db, "step-failure")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if stats.TotalRecoveries != 3 {
		t.Errorf("TotalRecoveries = %d, want 3", stats.TotalRecoveries)
	}
	if stats.PredictionAccuracy != 0 {
		t.Errorf("PredictionAccuracy = %f, want 0 (no predictions made)", stats.PredictionAccuracy)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// ---------------------------------------------------------------------------
// GetPromotionSnapshot tests (per spi-4o4bi review round 1)
// ---------------------------------------------------------------------------

func TestGetPromotionSnapshot_EmptySignatureReturnsZero(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	snap, err := GetPromotionSnapshot(context.Background(), db, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if snap == nil || snap.CleanCount != 0 || snap.LatestRecipe != "" || snap.FailureSig != "" {
		t.Errorf("GetPromotionSnapshot(\"\") = %+v, want zero-value snapshot", snap)
	}
}

func TestGetPromotionSnapshot_CountsConsecutiveCleanWithRecipe(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	rows := sqlmock.NewRows([]string{"outcome", "mechanical_recipe", "demoted_at"}).
		AddRow("clean", `{"kind":"builtin","action":"rebase-onto-base"}`, nil).
		AddRow("clean", `{"kind":"builtin","action":"rebase-onto-base"}`, nil).
		AddRow("clean", `{"kind":"builtin","action":"rebase-onto-base"}`, nil)

	mock.ExpectQuery(`SELECT outcome, mechanical_recipe, demoted_at FROM recovery_learnings WHERE failure_sig = \?`).
		WithArgs("step-failure:merge").
		WillReturnRows(rows)

	snap, err := GetPromotionSnapshot(context.Background(), db, "step-failure:merge")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if snap.CleanCount != 3 {
		t.Errorf("CleanCount = %d, want 3", snap.CleanCount)
	}
	if snap.LatestRecipe != `{"kind":"builtin","action":"rebase-onto-base"}` {
		t.Errorf("LatestRecipe = %q, want builtin rebase recipe", snap.LatestRecipe)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestGetPromotionSnapshot_StopsAtFirstNonCleanRow(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Newest-first: 2 clean, then dirty → count stops at 2.
	rows := sqlmock.NewRows([]string{"outcome", "mechanical_recipe", "demoted_at"}).
		AddRow("clean", `{"kind":"builtin","action":"rebuild"}`, nil).
		AddRow("clean", `{"kind":"builtin","action":"rebuild"}`, nil).
		AddRow("dirty", `{"kind":"builtin","action":"rebuild"}`, nil).
		AddRow("clean", `{"kind":"builtin","action":"rebuild"}`, nil)

	mock.ExpectQuery(`SELECT outcome, mechanical_recipe, demoted_at FROM recovery_learnings WHERE failure_sig = \?`).
		WithArgs("sig1").
		WillReturnRows(rows)

	snap, err := GetPromotionSnapshot(context.Background(), db, "sig1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if snap.CleanCount != 2 {
		t.Errorf("CleanCount = %d, want 2 (walk stops at dirty)", snap.CleanCount)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestGetPromotionSnapshot_StopsAtDemotedRow(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// First clean, then a clean-but-demoted → count stops at 1.
	rows := sqlmock.NewRows([]string{"outcome", "mechanical_recipe", "demoted_at"}).
		AddRow("clean", `{"kind":"builtin","action":"rebuild"}`, nil).
		AddRow("clean", `{"kind":"builtin","action":"rebuild"}`, "2026-04-18 12:00:00").
		AddRow("clean", `{"kind":"builtin","action":"rebuild"}`, nil)

	mock.ExpectQuery(`SELECT outcome, mechanical_recipe, demoted_at FROM recovery_learnings WHERE failure_sig = \?`).
		WithArgs("sig2").
		WillReturnRows(rows)

	snap, err := GetPromotionSnapshot(context.Background(), db, "sig2")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if snap.CleanCount != 1 {
		t.Errorf("CleanCount = %d, want 1 (walk stops at demoted)", snap.CleanCount)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestGetPromotionSnapshot_StopsAtRowWithoutRecipe(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Clean row without a recipe breaks the chain — signatures without
	// codified resolutions must never promote.
	rows := sqlmock.NewRows([]string{"outcome", "mechanical_recipe", "demoted_at"}).
		AddRow("clean", `{"kind":"builtin","action":"rebuild"}`, nil).
		AddRow("clean", nil, nil).
		AddRow("clean", `{"kind":"builtin","action":"rebuild"}`, nil)

	mock.ExpectQuery(`SELECT outcome, mechanical_recipe, demoted_at FROM recovery_learnings WHERE failure_sig = \?`).
		WithArgs("sig3").
		WillReturnRows(rows)

	snap, err := GetPromotionSnapshot(context.Background(), db, "sig3")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if snap.CleanCount != 1 {
		t.Errorf("CleanCount = %d, want 1 (walk stops at empty recipe)", snap.CleanCount)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestGetPromotionSnapshot_EmptyStringRecipeAlsoBreaksChain(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Empty string recipe (not NULL) also breaks the chain.
	rows := sqlmock.NewRows([]string{"outcome", "mechanical_recipe", "demoted_at"}).
		AddRow("clean", "", nil).
		AddRow("clean", `{"kind":"builtin","action":"rebuild"}`, nil)

	mock.ExpectQuery(`SELECT outcome, mechanical_recipe, demoted_at FROM recovery_learnings WHERE failure_sig = \?`).
		WithArgs("sig4").
		WillReturnRows(rows)

	snap, err := GetPromotionSnapshot(context.Background(), db, "sig4")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if snap.CleanCount != 0 {
		t.Errorf("CleanCount = %d, want 0 (first row has empty recipe)", snap.CleanCount)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestGetPromotionSnapshot_NoRowsReturnsZero(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	rows := sqlmock.NewRows([]string{"outcome", "mechanical_recipe", "demoted_at"})
	mock.ExpectQuery(`SELECT outcome, mechanical_recipe, demoted_at FROM recovery_learnings WHERE failure_sig = \?`).
		WithArgs("unknown-sig").
		WillReturnRows(rows)

	snap, err := GetPromotionSnapshot(context.Background(), db, "unknown-sig")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if snap.CleanCount != 0 || snap.LatestRecipe != "" {
		t.Errorf("snap = %+v, want zero-count empty-recipe", snap)
	}
	if snap.FailureSig != "unknown-sig" {
		t.Errorf("FailureSig = %q, want unknown-sig", snap.FailureSig)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// ---------------------------------------------------------------------------
// DemotePromotedRows tests
// ---------------------------------------------------------------------------

func TestDemotePromotedRows_EmptySignatureIsNoop(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// No ExpectExec — empty sig must short-circuit without touching the DB.
	if err := DemotePromotedRows(context.Background(), db, ""); err != nil {
		t.Fatalf("DemotePromotedRows(\"\") err = %v, want nil", err)
	}
}

func TestDemotePromotedRows_UpdatesMatchingRows(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectExec(`UPDATE recovery_learnings SET demoted_at = \? WHERE failure_sig = \? AND outcome = 'clean' AND demoted_at IS NULL AND mechanical_recipe IS NOT NULL AND mechanical_recipe != ''`).
		WithArgs(sqlmock.AnyArg(), "step-failure:merge").
		WillReturnResult(sqlmock.NewResult(0, 3))

	err = DemotePromotedRows(context.Background(), db, "step-failure:merge")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestDemotePromotedRows_NoMatchingRowsStillSucceeds(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectExec(`UPDATE recovery_learnings SET demoted_at = \? WHERE failure_sig = \? AND outcome = 'clean' AND demoted_at IS NULL AND mechanical_recipe IS NOT NULL AND mechanical_recipe != ''`).
		WithArgs(sqlmock.AnyArg(), "nonexistent").
		WillReturnResult(sqlmock.NewResult(0, 0))

	err = DemotePromotedRows(context.Background(), db, "nonexistent")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
