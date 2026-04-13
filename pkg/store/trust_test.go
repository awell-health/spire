package store

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestEnsureTrustTable(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectExec(`CREATE TABLE IF NOT EXISTS trust_levels`).
		WillReturnResult(sqlmock.NewResult(0, 0))

	if err := EnsureTrustTable(db); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestGetTrustRecord_Missing(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery(`SELECT .+ FROM trust_levels WHERE tower = \? AND repo_prefix = \?`).
		WithArgs("my-tower", "spi").
		WillReturnRows(sqlmock.NewRows([]string{
			"level", "consecutive_clean", "total_merges", "total_reverts", "last_change_at", "updated_at",
		}))

	rec, err := GetTrustRecord(context.Background(), db, "my-tower", "spi")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.Level != TrustSandbox {
		t.Errorf("expected TrustSandbox (0), got %d", rec.Level)
	}
	if rec.RepoPrefix != "spi" {
		t.Errorf("expected prefix spi, got %s", rec.RepoPrefix)
	}
	if rec.Tower != "my-tower" {
		t.Errorf("expected tower my-tower, got %s", rec.Tower)
	}
	if rec.ConsecutiveClean != 0 {
		t.Errorf("expected 0 consecutive_clean, got %d", rec.ConsecutiveClean)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestGetTrustRecord_Exists(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	rows := sqlmock.NewRows([]string{
		"level", "consecutive_clean", "total_merges", "total_reverts", "last_change_at", "updated_at",
	}).AddRow(2, 5, 15, 1, "2026-04-10 12:00:00", "2026-04-13 08:00:00")

	mock.ExpectQuery(`SELECT .+ FROM trust_levels WHERE tower = \? AND repo_prefix = \?`).
		WithArgs("my-tower", "web").
		WillReturnRows(rows)

	rec, err := GetTrustRecord(context.Background(), db, "my-tower", "web")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.Level != TrustTrusted {
		t.Errorf("expected TrustTrusted (2), got %d", rec.Level)
	}
	if rec.ConsecutiveClean != 5 {
		t.Errorf("expected 5 consecutive_clean, got %d", rec.ConsecutiveClean)
	}
	if rec.TotalMerges != 15 {
		t.Errorf("expected 15 total_merges, got %d", rec.TotalMerges)
	}
	if rec.TotalReverts != 1 {
		t.Errorf("expected 1 total_reverts, got %d", rec.TotalReverts)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestUpsertTrustRecord(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectExec(`INSERT INTO trust_levels`).
		WithArgs("spi", "my-tower", TrustSupervised, 3, 13, 2,
			sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))

	rec := TrustRecord{
		RepoPrefix:       "spi",
		Tower:            "my-tower",
		Level:            TrustSupervised,
		ConsecutiveClean: 3,
		TotalMerges:      13,
		TotalReverts:     2,
	}
	if err := UpsertTrustRecord(context.Background(), db, rec); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestListTrustRecords(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	rows := sqlmock.NewRows([]string{
		"repo_prefix", "tower", "level", "consecutive_clean", "total_merges", "total_reverts", "last_change_at", "updated_at",
	}).
		AddRow("api", "my-tower", 1, 3, 13, 2, "2026-04-10 12:00:00", "2026-04-13 08:00:00").
		AddRow("spi", "my-tower", 2, 7, 27, 0, nil, "2026-04-13 09:00:00").
		AddRow("web", "my-tower", 0, 0, 0, 0, nil, nil)

	mock.ExpectQuery(`SELECT .+ FROM trust_levels WHERE tower = \?`).
		WithArgs("my-tower").
		WillReturnRows(rows)

	records, err := ListTrustRecords(context.Background(), db, "my-tower")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(records) != 3 {
		t.Fatalf("expected 3 records, got %d", len(records))
	}
	if records[0].RepoPrefix != "api" || records[0].Level != TrustSupervised {
		t.Errorf("first record: prefix=%s level=%d", records[0].RepoPrefix, records[0].Level)
	}
	if records[1].RepoPrefix != "spi" || records[1].Level != TrustTrusted {
		t.Errorf("second record: prefix=%s level=%d", records[1].RepoPrefix, records[1].Level)
	}
	if records[2].RepoPrefix != "web" || records[2].Level != TrustSandbox {
		t.Errorf("third record: prefix=%s level=%d", records[2].RepoPrefix, records[2].Level)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRecordMergeOutcome_Clean(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// GetTrustRecord returns existing record with 4 consecutive clean.
	mock.ExpectQuery(`SELECT .+ FROM trust_levels WHERE tower = \? AND repo_prefix = \?`).
		WithArgs("my-tower", "spi").
		WillReturnRows(sqlmock.NewRows([]string{
			"level", "consecutive_clean", "total_merges", "total_reverts", "last_change_at", "updated_at",
		}).AddRow(1, 4, 10, 1, "2026-04-10 12:00:00", "2026-04-13 08:00:00"))

	// UpsertTrustRecord after increment.
	mock.ExpectExec(`INSERT INTO trust_levels`).
		WithArgs("spi", "my-tower", TrustSupervised, 5, 11, 1,
			sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))

	rec, err := RecordMergeOutcome(context.Background(), db, "my-tower", "spi", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.ConsecutiveClean != 5 {
		t.Errorf("expected 5 consecutive_clean, got %d", rec.ConsecutiveClean)
	}
	if rec.TotalMerges != 11 {
		t.Errorf("expected 11 total_merges, got %d", rec.TotalMerges)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRecordMergeOutcome_Failure(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// GetTrustRecord returns existing record with 7 consecutive clean.
	mock.ExpectQuery(`SELECT .+ FROM trust_levels WHERE tower = \? AND repo_prefix = \?`).
		WithArgs("my-tower", "spi").
		WillReturnRows(sqlmock.NewRows([]string{
			"level", "consecutive_clean", "total_merges", "total_reverts", "last_change_at", "updated_at",
		}).AddRow(2, 7, 20, 0, "2026-04-10 12:00:00", "2026-04-13 08:00:00"))

	// UpsertTrustRecord after reset.
	mock.ExpectExec(`INSERT INTO trust_levels`).
		WithArgs("spi", "my-tower", TrustTrusted, 0, 20, 1,
			sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))

	rec, err := RecordMergeOutcome(context.Background(), db, "my-tower", "spi", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.ConsecutiveClean != 0 {
		t.Errorf("expected 0 consecutive_clean, got %d", rec.ConsecutiveClean)
	}
	if rec.TotalReverts != 1 {
		t.Errorf("expected 1 total_reverts, got %d", rec.TotalReverts)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestTrustLevelName(t *testing.T) {
	tests := []struct {
		level TrustLevel
		want  string
	}{
		{TrustSandbox, "sandbox"},
		{TrustSupervised, "supervised"},
		{TrustTrusted, "trusted"},
		{TrustAutonomous, "autonomous"},
		{TrustLevel(99), "unknown(99)"},
	}
	for _, tt := range tests {
		got := TrustLevelName(tt.level)
		if got != tt.want {
			t.Errorf("TrustLevelName(%d) = %q, want %q", tt.level, got, tt.want)
		}
	}
}
