package steward

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/awell-health/spire/pkg/store"
)

func TestRequiresSageReview(t *testing.T) {
	tc := NewTrustChecker()
	tests := []struct {
		level store.TrustLevel
		want  bool
	}{
		{store.TrustSandbox, true},
		{store.TrustSupervised, true},
		{store.TrustTrusted, false},
		{store.TrustAutonomous, false},
	}
	for _, tt := range tests {
		got := tc.RequiresSageReview(tt.level)
		if got != tt.want {
			t.Errorf("RequiresSageReview(%d) = %v, want %v", tt.level, got, tt.want)
		}
	}
}

func TestAllowsAutoMerge(t *testing.T) {
	tc := NewTrustChecker()
	tests := []struct {
		level store.TrustLevel
		want  bool
	}{
		{store.TrustSandbox, false},
		{store.TrustSupervised, false},
		{store.TrustTrusted, true},
		{store.TrustAutonomous, true},
	}
	for _, tt := range tests {
		got := tc.AllowsAutoMerge(tt.level)
		if got != tt.want {
			t.Errorf("AllowsAutoMerge(%d) = %v, want %v", tt.level, got, tt.want)
		}
	}
}

// expectGetTrustRecord sets up sqlmock expectations for a GetTrustRecord call.
func expectGetTrustRecord(mock sqlmock.Sqlmock, tower, prefix string, level store.TrustLevel, consecutive, merges, reverts int) {
	mock.ExpectQuery(`SELECT .+ FROM trust_levels WHERE tower = \? AND repo_prefix = \?`).
		WithArgs(tower, prefix).
		WillReturnRows(sqlmock.NewRows([]string{
			"level", "consecutive_clean", "total_merges", "total_reverts", "last_change_at", "updated_at",
		}).AddRow(int(level), consecutive, merges, reverts, nil, "2026-04-13 08:00:00"))
}

// expectUpsert sets up sqlmock expectations for an UpsertTrustRecord call.
func expectUpsert(mock sqlmock.Sqlmock) {
	mock.ExpectExec(`INSERT INTO trust_levels`).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))
}

func TestRecordAndEvaluate_PromotesAfterThreshold(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	tc := &TrustChecker{PromotionThreshold: 3}

	// Start at supervised with 2 consecutive clean. A clean merge makes it 3 = threshold.
	// RecordMergeOutcome: GetTrustRecord + UpsertTrustRecord
	expectGetTrustRecord(mock, "tower", "spi", store.TrustSupervised, 2, 10, 0)
	expectUpsert(mock) // RecordMergeOutcome upsert (consecutive_clean=3)
	expectUpsert(mock) // RecordAndEvaluate promotion upsert (level++)

	rec, err := tc.RecordAndEvaluate(context.Background(), db, "tower", "spi", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.Level != store.TrustTrusted {
		t.Errorf("expected level TrustTrusted (2), got %d", rec.Level)
	}
	if rec.ConsecutiveClean != 0 {
		t.Errorf("expected consecutive_clean reset to 0 after promotion, got %d", rec.ConsecutiveClean)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRecordAndEvaluate_NoPromotionBelowThreshold(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	tc := &TrustChecker{PromotionThreshold: 5}

	// Start at sandbox with 3 consecutive clean. Clean merge → 4, below threshold 5.
	expectGetTrustRecord(mock, "tower", "spi", store.TrustSandbox, 3, 3, 0)
	expectUpsert(mock) // RecordMergeOutcome upsert

	rec, err := tc.RecordAndEvaluate(context.Background(), db, "tower", "spi", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.Level != store.TrustSandbox {
		t.Errorf("expected level to stay at TrustSandbox (0), got %d", rec.Level)
	}
	if rec.ConsecutiveClean != 4 {
		t.Errorf("expected consecutive_clean 4, got %d", rec.ConsecutiveClean)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRecordAndEvaluate_DemotesOnFailure(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	tc := NewTrustChecker()

	// Start at trusted (2) with 7 consecutive clean.
	expectGetTrustRecord(mock, "tower", "web", store.TrustTrusted, 7, 20, 1)
	expectUpsert(mock) // RecordMergeOutcome upsert (consecutive_clean=0)
	expectUpsert(mock) // RecordAndEvaluate demotion upsert (level--)

	rec, err := tc.RecordAndEvaluate(context.Background(), db, "tower", "web", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.Level != store.TrustSupervised {
		t.Errorf("expected level TrustSupervised (1), got %d", rec.Level)
	}
	if rec.ConsecutiveClean != 0 {
		t.Errorf("expected consecutive_clean 0, got %d", rec.ConsecutiveClean)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRecordAndEvaluate_PromotionCapsAtAutonomous(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	tc := &TrustChecker{PromotionThreshold: 2}

	// Already at autonomous (3) with 1 consecutive clean. Clean → 2 = threshold,
	// but can't promote past autonomous.
	expectGetTrustRecord(mock, "tower", "spi", store.TrustAutonomous, 1, 30, 0)
	expectUpsert(mock) // RecordMergeOutcome upsert

	rec, err := tc.RecordAndEvaluate(context.Background(), db, "tower", "spi", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.Level != store.TrustAutonomous {
		t.Errorf("expected level to stay at TrustAutonomous (3), got %d", rec.Level)
	}
	if rec.ConsecutiveClean != 2 {
		t.Errorf("expected consecutive_clean 2 (no reset since no promotion), got %d", rec.ConsecutiveClean)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRecordAndEvaluate_DemotionFloorsAtSandbox(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	tc := NewTrustChecker()

	// Already at sandbox (0). Failure should not demote below 0.
	expectGetTrustRecord(mock, "tower", "spi", store.TrustSandbox, 5, 5, 0)
	expectUpsert(mock) // RecordMergeOutcome upsert (consecutive_clean=0)

	rec, err := tc.RecordAndEvaluate(context.Background(), db, "tower", "spi", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.Level != store.TrustSandbox {
		t.Errorf("expected level to stay at TrustSandbox (0), got %d", rec.Level)
	}
	if rec.ConsecutiveClean != 0 {
		t.Errorf("expected consecutive_clean 0, got %d", rec.ConsecutiveClean)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRecordAndEvaluate_ConsecutiveCleanResetsAfterPromotion(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	tc := &TrustChecker{PromotionThreshold: 2}

	// Supervised (1) with 1 consecutive clean. Clean → 2 = threshold → promote to trusted.
	expectGetTrustRecord(mock, "tower", "spi", store.TrustSupervised, 1, 5, 0)
	expectUpsert(mock) // RecordMergeOutcome upsert
	expectUpsert(mock) // promotion upsert

	rec, err := tc.RecordAndEvaluate(context.Background(), db, "tower", "spi", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.Level != store.TrustTrusted {
		t.Errorf("expected TrustTrusted (2), got %d", rec.Level)
	}
	if rec.ConsecutiveClean != 0 {
		t.Errorf("consecutive_clean should reset to 0 after promotion, got %d", rec.ConsecutiveClean)
	}

	// Now simulate another round: need another 2 consecutive clean to promote again.
	// Trusted (2) with 0 consecutive clean. Clean → 1, no promotion.
	expectGetTrustRecord(mock, "tower", "spi", store.TrustTrusted, 0, 6, 0)
	expectUpsert(mock) // RecordMergeOutcome upsert

	rec, err = tc.RecordAndEvaluate(context.Background(), db, "tower", "spi", true)
	if err != nil {
		t.Fatalf("unexpected error on second call: %v", err)
	}
	if rec.Level != store.TrustTrusted {
		t.Errorf("expected level to stay TrustTrusted (2), got %d", rec.Level)
	}
	if rec.ConsecutiveClean != 1 {
		t.Errorf("expected consecutive_clean 1, got %d", rec.ConsecutiveClean)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRecordAndEvaluate_ConsecutiveCleanResetsOnFailure(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	tc := &TrustChecker{PromotionThreshold: 5}

	// Supervised (1) with 4 consecutive clean. Failure → consecutive_clean=0, demote to sandbox.
	expectGetTrustRecord(mock, "tower", "spi", store.TrustSupervised, 4, 10, 0)
	expectUpsert(mock) // RecordMergeOutcome upsert
	expectUpsert(mock) // demotion upsert

	rec, err := tc.RecordAndEvaluate(context.Background(), db, "tower", "spi", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.Level != store.TrustSandbox {
		t.Errorf("expected TrustSandbox (0), got %d", rec.Level)
	}
	if rec.ConsecutiveClean != 0 {
		t.Errorf("expected consecutive_clean 0, got %d", rec.ConsecutiveClean)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestNewTrustChecker_DefaultThreshold(t *testing.T) {
	tc := NewTrustChecker()
	if tc.PromotionThreshold != DefaultPromotionThreshold {
		t.Errorf("expected default threshold %d, got %d", DefaultPromotionThreshold, tc.PromotionThreshold)
	}
}
