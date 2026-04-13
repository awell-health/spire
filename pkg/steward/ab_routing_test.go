package steward

import (
	"context"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

var experimentColumns = []string{
	"id", "tower", "formula_base", "variant_a", "variant_b",
	"traffic_split", "status", "winner", "created_at", "updated_at",
}

func activeExperimentRow(mock sqlmock.Sqlmock, id, tower, base, vA, vB string, split float64) {
	now := time.Date(2026, 4, 13, 12, 0, 0, 0, time.UTC)
	rows := sqlmock.NewRows(experimentColumns).
		AddRow(id, tower, base, vA, vB, split, "active", "", now, now)
	mock.ExpectQuery(`SELECT .+ FROM formula_experiments`).
		WithArgs(tower, base).
		WillReturnRows(rows)
}

func noActiveExperiment(mock sqlmock.Sqlmock, tower, base string) {
	mock.ExpectQuery(`SELECT .+ FROM formula_experiments`).
		WithArgs(tower, base).
		WillReturnRows(sqlmock.NewRows(nil))
}

func TestSelectVariant_NoExperiment(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	noActiveExperiment(mock, "my-tower", "task-default")

	router := NewABRouter()
	variant, err := router.SelectVariant(context.Background(), db, "my-tower", "task-default", "spi-abc")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if variant != "task-default" {
		t.Errorf("variant = %q, want task-default (unchanged)", variant)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestSelectVariant_RoutesToVariantB(t *testing.T) {
	// Find a bead ID that hashes below a 50% split threshold.
	// We'll use traffic split of 1.0 to guarantee variant B.
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	activeExperimentRow(mock, "exp-001", "my-tower", "task-default",
		"task-default", "task-default-v2", 1.0)

	router := NewABRouter()
	variant, err := router.SelectVariant(context.Background(), db, "my-tower", "task-default", "spi-test1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if variant != "task-default-v2" {
		t.Errorf("variant = %q, want task-default-v2 (split=1.0 should always pick B)", variant)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestSelectVariant_RoutesToVariantA(t *testing.T) {
	// Traffic split of 0.0 means threshold=0, so hash(anything) >= 0 → always variant A.
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	activeExperimentRow(mock, "exp-001", "my-tower", "task-default",
		"task-default", "task-default-v2", 0.0)

	router := NewABRouter()
	variant, err := router.SelectVariant(context.Background(), db, "my-tower", "task-default", "spi-test1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if variant != "task-default" {
		t.Errorf("variant = %q, want task-default (split=0.0 should always pick A)", variant)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestSelectVariant_Deterministic(t *testing.T) {
	// Same beadID always produces the same result.
	beadID := "spi-deterministic-test"
	var firstResult string

	for i := 0; i < 10; i++ {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}

		activeExperimentRow(mock, "exp-001", "my-tower", "task-default",
			"task-default", "task-default-v2", 0.5)

		router := NewABRouter()
		variant, err := router.SelectVariant(context.Background(), db, "my-tower", "task-default", beadID)
		if err != nil {
			t.Fatalf("iteration %d: unexpected error: %v", i, err)
		}
		if i == 0 {
			firstResult = variant
		} else if variant != firstResult {
			t.Fatalf("iteration %d: variant = %q, want %q (not deterministic)", i, variant, firstResult)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
		db.Close()
	}
}

func TestSelectVariant_TrafficSplitZero_AlwaysA(t *testing.T) {
	for i := 0; i < 20; i++ {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}

		beadID := fmt.Sprintf("spi-zero-%d", i)
		activeExperimentRow(mock, "exp-001", "t", "f", "f", "f-v2", 0.0)

		router := NewABRouter()
		variant, err := router.SelectVariant(context.Background(), db, "t", "f", beadID)
		if err != nil {
			t.Fatalf("bead %s: unexpected error: %v", beadID, err)
		}
		if variant != "f" {
			t.Fatalf("bead %s: split=0.0 returned %q, want variantA", beadID, variant)
		}
		db.Close()
	}
}

func TestSelectVariant_TrafficSplitOne_AlwaysB(t *testing.T) {
	for i := 0; i < 20; i++ {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}

		beadID := fmt.Sprintf("spi-one-%d", i)
		activeExperimentRow(mock, "exp-001", "t", "f", "f", "f-v2", 1.0)

		router := NewABRouter()
		variant, err := router.SelectVariant(context.Background(), db, "t", "f", beadID)
		if err != nil {
			t.Fatalf("bead %s: unexpected error: %v", beadID, err)
		}
		if variant != "f-v2" {
			t.Fatalf("bead %s: split=1.0 returned %q, want variantB", beadID, variant)
		}
		db.Close()
	}
}

func TestHashBead_Distribution(t *testing.T) {
	// Test that hashBead distributes roughly uniformly across 0-99.
	buckets := make([]int, 100)
	n := 1000
	for i := 0; i < n; i++ {
		beadID := fmt.Sprintf("spi-%04d", i)
		b := hashBead(beadID)
		if b < 0 || b > 99 {
			t.Fatalf("hashBead(%q) = %d, want 0-99", beadID, b)
		}
		buckets[b]++
	}

	// Each bucket should get ~10 hits (1000/100). Allow wide tolerance.
	// With 1000 samples across 100 buckets, chi-square test or simple bounds.
	// We just check no bucket is empty and none has more than 5x expected.
	expected := float64(n) / 100.0
	for i, count := range buckets {
		if float64(count) > expected*5 {
			t.Errorf("bucket %d has %d entries (expected ~%.0f), distribution is skewed", i, count, expected)
		}
	}

	// Check that at least 80% of buckets are non-empty.
	nonEmpty := 0
	for _, count := range buckets {
		if count > 0 {
			nonEmpty++
		}
	}
	if nonEmpty < 80 {
		t.Errorf("only %d/100 buckets non-empty, expected at least 80", nonEmpty)
	}
}

func TestHashBead_Deterministic(t *testing.T) {
	for i := 0; i < 100; i++ {
		beadID := fmt.Sprintf("spi-det-%d", i)
		first := hashBead(beadID)
		second := hashBead(beadID)
		if first != second {
			t.Fatalf("hashBead(%q) not deterministic: %d != %d", beadID, first, second)
		}
	}
}

func TestHashBead_RangeAndApproximateSplit(t *testing.T) {
	// With a 20% traffic split, approximately 20% of beads should hash below 20.
	n := 10000
	belowThreshold := 0
	threshold := 20 // 20% split
	for i := 0; i < n; i++ {
		beadID := fmt.Sprintf("bead-%d", i)
		if hashBead(beadID) < threshold {
			belowThreshold++
		}
	}

	actualPct := float64(belowThreshold) / float64(n) * 100
	// Should be roughly 20%, allow +/- 5 percentage points.
	if math.Abs(actualPct-float64(threshold)) > 5.0 {
		t.Errorf("with %d%% threshold, got %.1f%% below (expected ~%d%%)", threshold, actualPct, threshold)
	}
}
