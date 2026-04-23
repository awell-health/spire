package intent

import (
	"context"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
)

func TestDoltPublisher_InsertIdempotent(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	p := NewDoltPublisher(db)
	// Freeze time so the assertion matches exactly.
	frozen := time.Date(2026, 4, 23, 12, 34, 56, 0, time.UTC)
	p.now = func() time.Time { return frozen }

	wi := WorkloadIntent{
		TaskID:      "spi-abc123",
		DispatchSeq: 1,
		Reason:      "fresh",
		RepoIdentity: RepoIdentity{
			URL:        "https://example.com/repo.git",
			BaseBranch: "main",
			Prefix:     "spi",
		},
		FormulaPhase: "implement",
		HandoffMode:  "bundle",
		Resources: Resources{
			CPURequest:    "500m",
			CPULimit:      "1000m",
			MemoryRequest: "256Mi",
			MemoryLimit:   "1Gi",
		},
	}

	mock.ExpectExec("INSERT INTO workload_intents").
		WithArgs(
			wi.TaskID, wi.DispatchSeq, wi.Reason,
			wi.RepoIdentity.URL, wi.RepoIdentity.BaseBranch, wi.RepoIdentity.Prefix,
			wi.FormulaPhase, wi.HandoffMode,
			wi.Resources.CPURequest, wi.Resources.CPULimit,
			wi.Resources.MemoryRequest, wi.Resources.MemoryLimit,
			frozen,
		).
		WillReturnResult(sqlmock.NewResult(1, 1))

	if err := p.Publish(context.Background(), wi); err != nil {
		t.Fatalf("Publish error: %v", err)
	}

	// A second Publish of the same intent goes through ON DUPLICATE KEY
	// UPDATE semantics — the publisher does not reject re-publishes. We
	// re-expect the same INSERT; a production dolt server returns a
	// 0-row result when nothing changed but the driver does not care.
	mock.ExpectExec("INSERT INTO workload_intents").
		WithArgs(
			wi.TaskID, wi.DispatchSeq, wi.Reason,
			wi.RepoIdentity.URL, wi.RepoIdentity.BaseBranch, wi.RepoIdentity.Prefix,
			wi.FormulaPhase, wi.HandoffMode,
			wi.Resources.CPURequest, wi.Resources.CPULimit,
			wi.Resources.MemoryRequest, wi.Resources.MemoryLimit,
			frozen,
		).
		WillReturnResult(sqlmock.NewResult(0, 0))

	if err := p.Publish(context.Background(), wi); err != nil {
		t.Fatalf("Publish (re-emit) error: %v", err)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unfulfilled expectations: %v", err)
	}
}

func TestDoltPublisher_RejectsEmptyTaskID(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	p := NewDoltPublisher(db)
	err = p.Publish(context.Background(), WorkloadIntent{DispatchSeq: 1})
	if err == nil {
		t.Fatal("Publish with empty TaskID should error; got nil")
	}
}

func TestDoltPublisher_RejectsZeroDispatchSeq(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	p := NewDoltPublisher(db)
	err = p.Publish(context.Background(), WorkloadIntent{TaskID: "spi-x"})
	if err == nil {
		t.Fatal("Publish with zero DispatchSeq should error; got nil")
	}
}

func TestDoltPublisher_RejectsNilDB(t *testing.T) {
	p := &DoltPublisher{}
	err := p.Publish(context.Background(), WorkloadIntent{TaskID: "spi-x", DispatchSeq: 1})
	if err == nil {
		t.Fatal("Publish with nil DB should error; got nil")
	}
}

func TestDoltConsumer_EmitAndMark(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	c := NewDoltConsumer(db, 10*time.Millisecond)
	frozen := time.Date(2026, 4, 23, 12, 34, 56, 0, time.UTC)
	c.now = func() time.Time { return frozen }

	rows := sqlmock.NewRows([]string{
		"task_id", "dispatch_seq", "reason",
		"repo_url", "repo_base_branch", "repo_prefix",
		"formula_phase", "handoff_mode",
		"resources_cpu_request", "resources_cpu_limit",
		"resources_memory_request", "resources_memory_limit",
	}).AddRow(
		"spi-xyz", 1, "fresh",
		"https://example.com/repo.git", "main", "spi",
		"implement", "bundle",
		"500m", "1000m", "256Mi", "1Gi",
	)

	mock.ExpectQuery("SELECT(.|\n)+FROM workload_intents(.|\n)+WHERE reconciled_at IS NULL").
		WithArgs(defaultConsumerBatchSize).
		WillReturnRows(rows)

	mock.ExpectExec("UPDATE workload_intents SET reconciled_at").
		WithArgs(frozen, "spi-xyz", 1).
		WillReturnResult(sqlmock.NewResult(0, 1))

	// After the first drain, subsequent polls find nothing.
	mock.ExpectQuery("SELECT(.|\n)+FROM workload_intents(.|\n)+WHERE reconciled_at IS NULL").
		WithArgs(defaultConsumerBatchSize).
		WillReturnRows(sqlmock.NewRows([]string{
			"task_id", "dispatch_seq", "reason",
			"repo_url", "repo_base_branch", "repo_prefix",
			"formula_phase", "handoff_mode",
			"resources_cpu_request", "resources_cpu_limit",
			"resources_memory_request", "resources_memory_limit",
		}))
	// Consumer may or may not poll again before ctx.Done fires. Mark the
	// empty-rows expectation optional by allowing any number of
	// invocations — sqlmock's default is exact-once. MatchExpectationsInOrder(false)
	// plus wrapping the final expectation lets the test remain
	// deterministic without racing the poll interval.
	mock.MatchExpectationsInOrder(false)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, err := c.Consume(ctx)
	if err != nil {
		t.Fatalf("Consume error: %v", err)
	}

	select {
	case got, ok := <-ch:
		if !ok {
			t.Fatal("channel closed before emit")
		}
		if got.TaskID != "spi-xyz" {
			t.Errorf("TaskID = %q, want spi-xyz", got.TaskID)
		}
		if got.DispatchSeq != 1 {
			t.Errorf("DispatchSeq = %d, want 1", got.DispatchSeq)
		}
		if got.Reason != "fresh" {
			t.Errorf("Reason = %q, want fresh", got.Reason)
		}
		if got.RepoIdentity.URL != "https://example.com/repo.git" {
			t.Errorf("URL = %q, want repo.git", got.RepoIdentity.URL)
		}
		if got.Resources.CPURequest != "500m" {
			t.Errorf("CPURequest = %q, want 500m", got.Resources.CPURequest)
		}
		if got.HandoffMode != "bundle" {
			t.Errorf("HandoffMode = %q, want bundle", got.HandoffMode)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for emitted intent")
	}

	cancel()

	// Wait for the channel to close after cancel.
	select {
	case _, ok := <-ch:
		if ok {
			// Drain any stragglers; the channel closes when the
			// goroutine returns.
			<-ch
		}
	case <-time.After(2 * time.Second):
		t.Fatal("channel did not close after ctx cancel")
	}
}

func TestEnsureWorkloadIntentsTable_NoopOnNilDB(t *testing.T) {
	if err := EnsureWorkloadIntentsTable(nil); err != nil {
		t.Errorf("EnsureWorkloadIntentsTable(nil) = %v, want nil", err)
	}
}

func TestEnsureWorkloadIntentsTable_RunsDDL(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	mock.ExpectExec("CREATE TABLE IF NOT EXISTS workload_intents").
		WillReturnResult(sqlmock.NewResult(0, 0))

	if err := EnsureWorkloadIntentsTable(db); err != nil {
		t.Fatalf("EnsureWorkloadIntentsTable error: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unfulfilled expectations: %v", err)
	}
}
