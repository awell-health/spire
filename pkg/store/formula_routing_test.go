package store

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestEnsureFormulaExperimentsTable(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectExec(`CREATE TABLE IF NOT EXISTS formula_experiments`).
		WillReturnResult(sqlmock.NewResult(0, 0))

	if err := EnsureFormulaExperimentsTable(db); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestCreateExperiment(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := time.Date(2026, 4, 13, 12, 0, 0, 0, time.UTC)
	exp := FormulaExperiment{
		ID:           "exp-001",
		Tower:        "my-tower",
		FormulaBase:  "task-default",
		VariantA:     "task-default",
		VariantB:     "task-default-v2",
		TrafficSplit: 0.2,
		Status:       "active",
		Winner:       "",
		CreatedAt:    now,
		UpdatedAt:    now,
	}

	mock.ExpectExec(`INSERT INTO formula_experiments`).
		WithArgs(exp.ID, exp.Tower, exp.FormulaBase, exp.VariantA, exp.VariantB,
			exp.TrafficSplit, exp.Status, exp.Winner, exp.CreatedAt, exp.UpdatedAt).
		WillReturnResult(sqlmock.NewResult(1, 1))

	if err := CreateExperiment(context.Background(), db, exp); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestGetActiveExperiment_Found(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := time.Date(2026, 4, 13, 12, 0, 0, 0, time.UTC)
	rows := sqlmock.NewRows([]string{
		"id", "tower", "formula_base", "variant_a", "variant_b",
		"traffic_split", "status", "winner", "created_at", "updated_at",
	}).AddRow("exp-001", "my-tower", "task-default", "task-default", "task-default-v2",
		0.2, "active", "", now, now)

	mock.ExpectQuery(`SELECT .+ FROM formula_experiments WHERE tower = \? AND formula_base = \? AND status = 'active'`).
		WithArgs("my-tower", "task-default").
		WillReturnRows(rows)

	exp, err := GetActiveExperiment(context.Background(), db, "my-tower", "task-default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if exp == nil {
		t.Fatal("expected experiment, got nil")
	}
	if exp.ID != "exp-001" {
		t.Errorf("ID = %q, want exp-001", exp.ID)
	}
	if exp.VariantB != "task-default-v2" {
		t.Errorf("VariantB = %q, want task-default-v2", exp.VariantB)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestGetActiveExperiment_NotFound(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery(`SELECT .+ FROM formula_experiments`).
		WithArgs("my-tower", "task-default").
		WillReturnRows(sqlmock.NewRows(nil))

	exp, err := GetActiveExperiment(context.Background(), db, "my-tower", "task-default")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if exp != nil {
		t.Fatalf("expected nil, got %+v", exp)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestConcludeExperiment(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectExec(`UPDATE formula_experiments SET status = 'concluded', winner = \?, updated_at = NOW\(\) WHERE id = \?`).
		WithArgs("task-default-v2", "exp-001").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := ConcludeExperiment(context.Background(), db, "exp-001", "task-default-v2"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestPauseExperiment(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectExec(`UPDATE formula_experiments SET status = 'paused', updated_at = NOW\(\) WHERE id = \?`).
		WithArgs("exp-001").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := PauseExperiment(context.Background(), db, "exp-001"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestListExperiments(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := time.Date(2026, 4, 13, 12, 0, 0, 0, time.UTC)
	rows := sqlmock.NewRows([]string{
		"id", "tower", "formula_base", "variant_a", "variant_b",
		"traffic_split", "status", "winner", "created_at", "updated_at",
	}).
		AddRow("exp-002", "my-tower", "task-default", "task-default", "task-default-v3",
			0.5, "active", "", now, now).
		AddRow("exp-001", "my-tower", "task-default", "task-default", "task-default-v2",
			0.2, "concluded", "task-default-v2", now, now)

	mock.ExpectQuery(`SELECT .+ FROM formula_experiments WHERE tower = \?`).
		WithArgs("my-tower").
		WillReturnRows(rows)

	exps, err := ListExperiments(context.Background(), db, "my-tower")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(exps) != 2 {
		t.Fatalf("expected 2 experiments, got %d", len(exps))
	}
	if exps[0].ID != "exp-002" {
		t.Errorf("first experiment ID = %q, want exp-002", exps[0].ID)
	}
	if exps[1].Winner != "task-default-v2" {
		t.Errorf("second experiment winner = %q, want task-default-v2", exps[1].Winner)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestCompareVariants(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	exp := FormulaExperiment{
		ID:       "exp-001",
		Tower:    "my-tower",
		VariantA: "task-default",
		VariantB: "task-default-v2",
	}

	rows := sqlmock.NewRows([]string{
		"formula_name", "run_count", "success_count", "avg_cost", "avg_duration",
	}).
		AddRow("task-default", 25, 20, 0.15, 120.5).
		AddRow("task-default-v2", 22, 19, 0.12, 95.3)

	mock.ExpectQuery(`SELECT formula_name,.+FROM agent_runs WHERE formula_name IN \(\?, \?\) AND tower = \?`).
		WithArgs("task-default", "task-default-v2", "my-tower").
		WillReturnRows(rows)

	cmp, err := CompareVariants(context.Background(), db, exp)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cmp.VariantAStats.RunCount != 25 {
		t.Errorf("variant A run count = %d, want 25", cmp.VariantAStats.RunCount)
	}
	if cmp.VariantAStats.SuccessRate != 0.8 {
		t.Errorf("variant A success rate = %f, want 0.8", cmp.VariantAStats.SuccessRate)
	}
	if cmp.VariantBStats.RunCount != 22 {
		t.Errorf("variant B run count = %d, want 22", cmp.VariantBStats.RunCount)
	}
	if cmp.VariantBStats.AvgCostUSD != 0.12 {
		t.Errorf("variant B avg cost = %f, want 0.12", cmp.VariantBStats.AvgCostUSD)
	}
	if !cmp.SignificantDiff {
		t.Error("expected SignificantDiff=true (both variants >= 20 runs)")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestCompareVariants_InsufficientData(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	exp := FormulaExperiment{
		ID:       "exp-001",
		Tower:    "my-tower",
		VariantA: "task-default",
		VariantB: "task-default-v2",
	}

	rows := sqlmock.NewRows([]string{
		"formula_name", "run_count", "success_count", "avg_cost", "avg_duration",
	}).
		AddRow("task-default", 5, 3, 0.15, 120.5)
	// variant B has no runs at all

	mock.ExpectQuery(`SELECT formula_name,.+FROM agent_runs`).
		WithArgs("task-default", "task-default-v2", "my-tower").
		WillReturnRows(rows)

	cmp, err := CompareVariants(context.Background(), db, exp)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cmp.VariantBStats.RunCount != 0 {
		t.Errorf("variant B run count = %d, want 0", cmp.VariantBStats.RunCount)
	}
	if cmp.SignificantDiff {
		t.Error("expected SignificantDiff=false (insufficient data)")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestCompareVariants_NoRuns(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	exp := FormulaExperiment{
		ID:       "exp-001",
		Tower:    "my-tower",
		VariantA: "task-default",
		VariantB: "task-default-v2",
	}

	rows := sqlmock.NewRows([]string{
		"formula_name", "run_count", "success_count", "avg_cost", "avg_duration",
	})

	mock.ExpectQuery(`SELECT formula_name,.+FROM agent_runs`).
		WithArgs("task-default", "task-default-v2", "my-tower").
		WillReturnRows(rows)

	cmp, err := CompareVariants(context.Background(), db, exp)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cmp.VariantAStats.RunCount != 0 || cmp.VariantBStats.RunCount != 0 {
		t.Error("expected zero runs for both variants")
	}
	if cmp.SignificantDiff {
		t.Error("expected SignificantDiff=false with no data")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
