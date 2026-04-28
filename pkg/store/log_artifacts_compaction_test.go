package store

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// TestCompactLogArtifacts_NoOpForEmptyPolicy proves a zero-value policy
// is a true no-op: no SELECT issued, no DELETE issued, zero rows
// reported. Saves the daemon a round-trip on towers configured to
// disable compaction.
func TestCompactLogArtifacts_NoOpForEmptyPolicy(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	deleted, err := CompactLogArtifacts(context.Background(), db, LogArtifactCompactionPolicy{})
	if err != nil {
		t.Fatalf("CompactLogArtifacts: %v", err)
	}
	if deleted != 0 {
		t.Errorf("deleted = %d, want 0", deleted)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unexpected SQL: %v", err)
	}
}

// TestCompactLogArtifacts_AgeCapPrunesOldRows exercises the OlderThan
// path: rows past the cutoff are deleted, rows inside the window are
// retained. We mock the SELECT and one DELETE per pruned row.
func TestCompactLogArtifacts_AgeCapPrunesOldRows(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery(`SELECT id FROM agent_log_artifacts WHERE updated_at < \?`).
		WithArgs(sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).
			AddRow("log-old-1").
			AddRow("log-old-2"))
	mock.ExpectExec(`DELETE FROM agent_log_artifacts WHERE id = \?`).
		WithArgs("log-old-1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`DELETE FROM agent_log_artifacts WHERE id = \?`).
		WithArgs("log-old-2").
		WillReturnResult(sqlmock.NewResult(0, 1))

	deleted, err := CompactLogArtifacts(context.Background(), db, LogArtifactCompactionPolicy{
		OlderThan: 30 * 24 * time.Hour,
		Now:       time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("CompactLogArtifacts: %v", err)
	}
	if deleted != 2 {
		t.Errorf("deleted = %d, want 2", deleted)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

// TestCompactLogArtifacts_PerBeadKeepRetainsRecent sets a recency
// floor: keep N=2 most recent rows per bead, prune the rest. With 3
// rows on one bead, only the 3rd-most-recent gets pruned.
func TestCompactLogArtifacts_PerBeadKeepRetainsRecent(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Per-bead scan: ordered by bead, then updated_at DESC.
	mock.ExpectQuery(`SELECT bead_id, id FROM agent_log_artifacts\s+ORDER BY`).
		WillReturnRows(sqlmock.NewRows([]string{"bead_id", "id"}).
			AddRow("spi-a", "log-a-newest").
			AddRow("spi-a", "log-a-mid").
			AddRow("spi-a", "log-a-oldest"). // pruned
			AddRow("spi-b", "log-b-only"))   // retained
	mock.ExpectExec(`DELETE FROM agent_log_artifacts WHERE id = \?`).
		WithArgs("log-a-oldest").
		WillReturnResult(sqlmock.NewResult(0, 1))

	deleted, err := CompactLogArtifacts(context.Background(), db, LogArtifactCompactionPolicy{
		PerBeadKeep: 2,
	})
	if err != nil {
		t.Fatalf("CompactLogArtifacts: %v", err)
	}
	if deleted != 1 {
		t.Errorf("deleted = %d, want 1", deleted)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

// TestCompactLogArtifacts_DeduplicatesBetweenAxes proves a row that
// matches BOTH the age cap AND the recency floor is deleted exactly
// once — the policy is set-union, not set-sum.
func TestCompactLogArtifacts_DeduplicatesBetweenAxes(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery(`SELECT id FROM agent_log_artifacts WHERE updated_at < \?`).
		WithArgs(sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("log-stale"))
	mock.ExpectQuery(`SELECT bead_id, id FROM agent_log_artifacts\s+ORDER BY`).
		WillReturnRows(sqlmock.NewRows([]string{"bead_id", "id"}).
			AddRow("spi-a", "log-fresh"). // retained (counter=1, keep=1)
			AddRow("spi-a", "log-stale")) // also matched by age — must dedupe
	mock.ExpectExec(`DELETE FROM agent_log_artifacts WHERE id = \?`).
		WithArgs("log-stale").
		WillReturnResult(sqlmock.NewResult(0, 1))
	// No second DELETE for log-stale.

	deleted, err := CompactLogArtifacts(context.Background(), db, LogArtifactCompactionPolicy{
		OlderThan:   1 * time.Hour,
		PerBeadKeep: 1,
	})
	if err != nil {
		t.Fatalf("CompactLogArtifacts: %v", err)
	}
	if deleted != 1 {
		t.Errorf("deleted = %d, want 1 (deduped)", deleted)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

// TestCompactLogArtifacts_DoesNotIssueObjectStoreCalls is the
// architectural invariant: CompactLogArtifacts never reaches into the
// byte store. The test enforces it by asserting only DELETE statements
// are issued — no UPDATE, no out-of-band calls. (sqlmock fails any
// unexpected interaction.)
func TestCompactLogArtifacts_DoesNotIssueObjectStoreCalls(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery(`SELECT id FROM agent_log_artifacts WHERE updated_at < \?`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("log-1"))
	mock.ExpectExec(`DELETE FROM agent_log_artifacts WHERE id = \?`).
		WithArgs("log-1").
		WillReturnResult(sqlmock.NewResult(0, 1))

	_, err = CompactLogArtifacts(context.Background(), db, LogArtifactCompactionPolicy{
		OlderThan: 1 * time.Hour,
	})
	if err != nil {
		t.Fatalf("CompactLogArtifacts: %v", err)
	}
	// If sqlmock had recorded any unexpected call, ExpectationsWereMet
	// would surface it here.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations (unexpected SQL beyond DELETE): %v", err)
	}
}

// TestSetLogArtifactRedaction_RejectsNegative guards the version arg.
// 0 is reserved for "no redactor"; negative values are nonsense.
func TestSetLogArtifactRedaction_RejectsNegative(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	if err := SetLogArtifactRedaction(context.Background(), db, "log-x", -1); err == nil {
		t.Error("expected error for negative version")
	}
}

// TestSetLogArtifactRedaction_StampsVersion verifies the UPDATE shape.
func TestSetLogArtifactRedaction_StampsVersion(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectExec(`UPDATE agent_log_artifacts SET redaction_version = \?, updated_at = \? WHERE id = \?`).
		WithArgs(2, sqlmock.AnyArg(), "log-x").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := SetLogArtifactRedaction(context.Background(), db, "log-x", 2); err != nil {
		t.Fatalf("SetLogArtifactRedaction: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}
