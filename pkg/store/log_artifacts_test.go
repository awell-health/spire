package store

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// makeRecord returns a populated LogArtifactRecord suitable for INSERT
// fixtures. All required identity fields are filled with deterministic
// values so the same record can be reinserted in idempotency tests.
func makeRecord() LogArtifactRecord {
	return LogArtifactRecord{
		ID:        "log-test12345678",
		Tower:     "awell-test",
		BeadID:    "spi-b986in",
		AttemptID: "spi-attempt",
		RunID:     "run-001",
		AgentName: "wizard-spi-b986in",
		Role:      "wizard",
		Phase:     "implement",
		Provider:  "claude",
		Stream:    "transcript",
		Sequence:  0,
		ObjectURI: "file:///tmp/wizards/wizard-spi-b986in/claude/transcript-0.jsonl",
		Status:    LogArtifactStatusWriting,
	}
}

func TestEnsureAgentLogArtifactsTable(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectExec(`CREATE TABLE IF NOT EXISTS agent_log_artifacts`).
		WillReturnResult(sqlmock.NewResult(0, 0))

	if err := EnsureAgentLogArtifactsTable(db); err != nil {
		t.Fatalf("EnsureAgentLogArtifactsTable: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestAgentLogArtifactsTableSQL_Shape(t *testing.T) {
	if AgentLogArtifactsTableSQL == "" {
		t.Fatal("AgentLogArtifactsTableSQL is empty")
	}
	// Pin every column the design requires so a future schema edit can't
	// silently drop one. spi-egw26j / spi-k1cnof / spi-j3r694 will all
	// rely on this exact identity tuple.
	required := []string{
		"CREATE TABLE",
		"IF NOT EXISTS",
		"agent_log_artifacts",
		"id",
		"tower",
		"bead_id",
		"attempt_id",
		"run_id",
		"agent_name",
		"role",
		"phase",
		"provider",
		"stream",
		"sequence",
		"object_uri",
		"byte_size",
		"checksum",
		"status",
		"started_at",
		"ended_at",
		"redaction_version",
		"summary",
		"tail",
		"PRIMARY KEY",
		"UNIQUE KEY uniq_log_artifact_identity",
	}
	for _, fragment := range required {
		if !strings.Contains(AgentLogArtifactsTableSQL, fragment) {
			t.Errorf("AgentLogArtifactsTableSQL missing %q", fragment)
		}
	}
}

func TestInsertLogArtifact_FillsDefaults(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	rec := makeRecord()
	rec.ID = "" // expect auto-generated ID
	mock.ExpectExec(`INSERT INTO agent_log_artifacts`).
		WithArgs(
			sqlmock.AnyArg(), // id
			rec.Tower, rec.BeadID, rec.AttemptID, rec.RunID, rec.AgentName,
			rec.Role, rec.Phase, rec.Provider, rec.Stream, rec.Sequence,
			rec.ObjectURI,
			nil, nil,             // byte_size, checksum
			LogArtifactStatusWriting,
			nil, nil,             // started_at, ended_at
			sqlmock.AnyArg(),     // created_at
			sqlmock.AnyArg(),     // updated_at
			0,                    // redaction_version
			nil, nil,             // summary, tail
		).
		WillReturnResult(sqlmock.NewResult(1, 1))

	id, err := InsertLogArtifact(context.Background(), db, rec)
	if err != nil {
		t.Fatalf("InsertLogArtifact: %v", err)
	}
	if id == "" {
		t.Error("expected non-empty auto-generated ID")
	}
	if !strings.HasPrefix(id, "log-") {
		t.Errorf("expected id prefix 'log-', got %q", id)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestInsertLogArtifact_DuplicateIdentity(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectExec(`INSERT INTO agent_log_artifacts`).
		WillReturnError(errors.New("Error 1062: Duplicate entry 'spi-b986in-...' for key 'uniq_log_artifact_identity'"))

	_, err = InsertLogArtifact(context.Background(), db, makeRecord())
	if !errors.Is(err, ErrLogArtifactExists) {
		t.Fatalf("expected ErrLogArtifactExists, got %v", err)
	}
}

func TestInsertLogArtifact_RejectsOversizeSummary(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	rec := makeRecord()
	rec.Summary = strings.Repeat("x", LogArtifactSummaryMaxBytes+1)
	if _, err := InsertLogArtifact(context.Background(), db, rec); err == nil {
		t.Fatal("expected error on oversize summary")
	} else if !strings.Contains(err.Error(), "summary exceeds") {
		t.Errorf("expected summary-cap error, got %v", err)
	}
}

func TestInsertLogArtifact_RejectsOversizeTail(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	rec := makeRecord()
	rec.Tail = strings.Repeat("y", LogArtifactTailMaxBytes+1)
	if _, err := InsertLogArtifact(context.Background(), db, rec); err == nil {
		t.Fatal("expected error on oversize tail")
	} else if !strings.Contains(err.Error(), "tail exceeds") {
		t.Errorf("expected tail-cap error, got %v", err)
	}
}

func TestGetLogArtifact_NotFound(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery(`SELECT .+ FROM agent_log_artifacts WHERE id = \?`).
		WithArgs("log-missing").
		WillReturnRows(sqlmock.NewRows(logArtifactColumnNames()))

	rec, err := GetLogArtifact(context.Background(), db, "log-missing")
	if err != nil {
		t.Fatalf("GetLogArtifact: %v", err)
	}
	if rec != nil {
		t.Errorf("expected nil for missing row, got %+v", rec)
	}
}

func TestGetLogArtifact_Found(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	rows := sqlmock.NewRows(logArtifactColumnNames()).
		AddRow(
			"log-abc", "awell-test", "spi-b986in", "spi-attempt", "run-1",
			"wizard-spi-b986in", "wizard", "implement", "claude", "transcript",
			0, "file:///tmp/x.jsonl", int64(1234), "sha256:deadbeef",
			LogArtifactStatusFinalized, "2026-04-28 01:00:00", "2026-04-28 01:01:00",
			"2026-04-28 01:00:00", "2026-04-28 01:01:00",
			0, "summary text", "tail bytes",
		)
	mock.ExpectQuery(`SELECT .+ FROM agent_log_artifacts WHERE id = \?`).
		WithArgs("log-abc").
		WillReturnRows(rows)

	rec, err := GetLogArtifact(context.Background(), db, "log-abc")
	if err != nil {
		t.Fatalf("GetLogArtifact: %v", err)
	}
	if rec == nil {
		t.Fatal("expected non-nil record")
	}
	if rec.Status != LogArtifactStatusFinalized {
		t.Errorf("status = %q, want %q", rec.Status, LogArtifactStatusFinalized)
	}
	if rec.ByteSize == nil || *rec.ByteSize != 1234 {
		t.Errorf("byte_size = %v, want 1234", rec.ByteSize)
	}
	if rec.Checksum != "sha256:deadbeef" {
		t.Errorf("checksum = %q, want sha256:deadbeef", rec.Checksum)
	}
	if rec.Summary != "summary text" {
		t.Errorf("summary = %q", rec.Summary)
	}
	if rec.Tail != "tail bytes" {
		t.Errorf("tail = %q", rec.Tail)
	}
}

func TestGetLogArtifactByIdentity_Found(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	rec := makeRecord()
	rows := sqlmock.NewRows(logArtifactColumnNames()).
		AddRow(
			rec.ID, rec.Tower, rec.BeadID, rec.AttemptID, rec.RunID,
			rec.AgentName, rec.Role, rec.Phase, rec.Provider, rec.Stream,
			rec.Sequence, rec.ObjectURI, nil, nil,
			rec.Status, nil, nil,
			"2026-04-28 01:00:00", "2026-04-28 01:01:00",
			0, nil, nil,
		)
	mock.ExpectQuery(`SELECT .+ FROM agent_log_artifacts WHERE bead_id = \?`).
		WithArgs(
			rec.BeadID, rec.AttemptID, rec.RunID, rec.AgentName, rec.Role,
			rec.Phase, rec.Provider, rec.Stream, rec.Sequence,
		).
		WillReturnRows(rows)

	got, err := GetLogArtifactByIdentity(
		context.Background(), db,
		rec.BeadID, rec.AttemptID, rec.RunID, rec.AgentName, rec.Role,
		rec.Phase, rec.Provider, rec.Stream, rec.Sequence,
	)
	if err != nil {
		t.Fatalf("GetLogArtifactByIdentity: %v", err)
	}
	if got == nil || got.ID != rec.ID {
		t.Errorf("got %+v, want id=%s", got, rec.ID)
	}
}

func TestListLogArtifactsForBead(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	rows := sqlmock.NewRows(logArtifactColumnNames()).
		AddRow(
			"log-a", "awell", "spi-x", "spi-att1", "run-1",
			"wizard-spi-x", "wizard", "implement", "claude", "transcript",
			0, "file:///a.jsonl", nil, nil,
			LogArtifactStatusWriting, nil, nil,
			"2026-04-28 01:00:00", "2026-04-28 01:00:00",
			0, nil, nil,
		).
		AddRow(
			"log-b", "awell", "spi-x", "spi-att1", "run-1",
			"wizard-spi-x", "wizard", "implement", "claude", "transcript",
			1, "file:///b.jsonl", int64(100), "sha256:cafe",
			LogArtifactStatusFinalized, nil, nil,
			"2026-04-28 01:00:00", "2026-04-28 01:01:00",
			0, nil, nil,
		)
	mock.ExpectQuery(`SELECT .+ FROM agent_log_artifacts\s+WHERE bead_id = \?`).
		WithArgs("spi-x").
		WillReturnRows(rows)

	got, err := ListLogArtifactsForBead(context.Background(), db, "spi-x")
	if err != nil {
		t.Fatalf("ListLogArtifactsForBead: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 records, got %d", len(got))
	}
	if got[0].Sequence != 0 || got[1].Sequence != 1 {
		t.Errorf("expected sequences 0,1 got %d,%d", got[0].Sequence, got[1].Sequence)
	}
	if got[1].ByteSize == nil || *got[1].ByteSize != 100 {
		t.Errorf("expected byte_size=100 on second row, got %v", got[1].ByteSize)
	}
}

func TestListLogArtifactsForAttempt(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	rows := sqlmock.NewRows(logArtifactColumnNames())
	mock.ExpectQuery(`SELECT .+ FROM agent_log_artifacts\s+WHERE attempt_id = \?`).
		WithArgs("spi-att-empty").
		WillReturnRows(rows)

	got, err := ListLogArtifactsForAttempt(context.Background(), db, "spi-att-empty")
	if err != nil {
		t.Fatalf("ListLogArtifactsForAttempt: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected 0 records, got %d", len(got))
	}
}

func TestUpdateLogArtifactStatus(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectExec(`UPDATE agent_log_artifacts SET status = \?, updated_at = \? WHERE id = \?`).
		WithArgs(LogArtifactStatusFailed, sqlmock.AnyArg(), "log-x").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := UpdateLogArtifactStatus(context.Background(), db, "log-x", LogArtifactStatusFailed); err != nil {
		t.Fatalf("UpdateLogArtifactStatus: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestFinalizeLogArtifact(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectExec(`UPDATE agent_log_artifacts SET\s+byte_size`).
		WithArgs(
			int64(2048),
			"sha256:abc123",
			LogArtifactStatusFinalized,
			sqlmock.AnyArg(), // ended_at
			sqlmock.AnyArg(), // updated_at
			"summary", "tail",
			"log-x",
		).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := FinalizeLogArtifact(context.Background(), db, "log-x", 2048, "sha256:abc123", "summary", "tail"); err != nil {
		t.Fatalf("FinalizeLogArtifact: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestFinalizeLogArtifact_RejectsOversizeSummary(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	tooBig := strings.Repeat("x", LogArtifactSummaryMaxBytes+1)
	if err := FinalizeLogArtifact(context.Background(), db, "log-x", 100, "sha256:abc", tooBig, ""); err == nil {
		t.Fatal("expected error on oversize summary")
	}
}

func TestFinalizeLogArtifact_RejectsOversizeTail(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	tooBig := strings.Repeat("y", LogArtifactTailMaxBytes+1)
	if err := FinalizeLogArtifact(context.Background(), db, "log-x", 100, "sha256:abc", "", tooBig); err == nil {
		t.Fatal("expected error on oversize tail")
	}
}

func TestIsDuplicateKeyError(t *testing.T) {
	if isDuplicateKeyError(nil) {
		t.Error("nil should not be a duplicate-key error")
	}
	if !isDuplicateKeyError(errors.New("Error 1062: Duplicate entry 'foo' for key 'bar'")) {
		t.Error("MySQL 1062 message should be detected")
	}
	if !isDuplicateKeyError(errors.New("constraint violation: duplicate key value violates uniqueness")) {
		t.Error("uniqueness substring should be detected (sqlmock fallback)")
	}
	if isDuplicateKeyError(errors.New("connection refused")) {
		t.Error("unrelated errors should not be classified as duplicates")
	}
}

// logArtifactColumnNames returns the column slice the SELECT helpers
// expect, in the order produced by logArtifactColumns. Test-only helper
// kept next to the consumers so a column drift surfaces here.
func logArtifactColumnNames() []string {
	return []string{
		"id", "tower", "bead_id", "attempt_id", "run_id", "agent_name",
		"role", "phase", "provider", "stream", "sequence", "object_uri",
		"byte_size", "checksum", "status", "started_at", "ended_at",
		"created_at", "updated_at", "redaction_version", "summary", "tail",
	}
}
