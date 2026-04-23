package intent

// Dolt-backed IntentPublisher/IntentConsumer — the canonical transport
// for moving WorkloadIntent values from the steward to the operator's
// intent reconciler in cluster-native deployments.
//
// The transport is a classic outbox:
//
//   - Publish inserts one row into workload_intents keyed by attempt_id.
//     INSERT ... ON DUPLICATE KEY UPDATE makes re-publishes (e.g. after
//     a transient steward restart) a safe no-op.
//   - Consume opens a long-lived goroutine that polls the table every
//     pollInterval, emits un-reconciled rows on a channel, and stamps
//     reconciled_at once per row. The reconciler's own Create path is
//     idempotent on IsAlreadyExists, so duplicate delivery (e.g. after
//     a consumer crash between emit and UPDATE) is safe.
//
// The file stays inside the pkg/steward boundary rule: no k8s.io/*
// imports, no pkg/dolt, no pkg/config. The only external deps are
// database/sql (generic SQL) and pkg/store (for the canonical DDL
// constant).

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/awell-health/spire/pkg/store"
)

// defaultConsumerPollInterval is the fallback poll interval when the
// caller does not specify one. Small enough that the smoke test's
// 10-second steward cycle sees the operator pick up intents within a
// couple of seconds, large enough that it does not dominate dolt load.
const defaultConsumerPollInterval = 2 * time.Second

// defaultConsumerBatchSize caps how many un-reconciled rows a single
// poll iteration will emit. Prevents a backlog-filled table from
// starving ctx.Done() delivery.
const defaultConsumerBatchSize = 50

// EnsureWorkloadIntentsTable runs the workload_intents CREATE TABLE IF
// NOT EXISTS so callers can bring the transport up without depending on
// an upstream migration step having already happened. Both the steward
// factory and the operator wiring call this on startup — defense in
// depth for whichever pod starts first.
//
// Returns nil when db is nil (test / mock paths that do not expose a
// SQL connection do not need the table).
func EnsureWorkloadIntentsTable(db *sql.DB) error {
	if db == nil {
		return nil
	}
	if _, err := db.Exec(store.WorkloadIntentsTableSQL); err != nil {
		return fmt.Errorf("intent: ensure workload_intents table: %w", err)
	}
	return nil
}

// DoltPublisher is the pkg/steward-side IntentPublisher backed by the
// workload_intents dolt table. Publish writes one row per Publish call
// and is idempotent on attempt_id.
type DoltPublisher struct {
	db  *sql.DB
	now func() time.Time
}

// NewDoltPublisher constructs a DoltPublisher bound to db. The caller
// is responsible for keeping db open for the publisher's lifetime; the
// publisher does not own the connection.
func NewDoltPublisher(db *sql.DB) *DoltPublisher {
	return &DoltPublisher{db: db}
}

// Publish inserts the intent into workload_intents. Repeat publishes of
// the same attempt_id are idempotent: ON DUPLICATE KEY UPDATE refreshes
// the projection but preserves the original emitted_at and any
// reconciled_at the consumer may have already stamped.
func (p *DoltPublisher) Publish(ctx context.Context, i WorkloadIntent) error {
	if p == nil || p.db == nil {
		return fmt.Errorf("intent: DoltPublisher has nil DB")
	}
	if i.AttemptID == "" {
		return fmt.Errorf("intent: Publish requires non-empty AttemptID")
	}
	now := p.nowFn()
	_, err := p.db.ExecContext(ctx, `
        INSERT INTO workload_intents (
            attempt_id, repo_url, repo_base_branch, repo_prefix,
            formula_phase, handoff_mode,
            resources_cpu_request, resources_cpu_limit,
            resources_memory_request, resources_memory_limit,
            emitted_at
        ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
        ON DUPLICATE KEY UPDATE
            repo_url = VALUES(repo_url),
            repo_base_branch = VALUES(repo_base_branch),
            repo_prefix = VALUES(repo_prefix),
            formula_phase = VALUES(formula_phase),
            handoff_mode = VALUES(handoff_mode),
            resources_cpu_request = VALUES(resources_cpu_request),
            resources_cpu_limit = VALUES(resources_cpu_limit),
            resources_memory_request = VALUES(resources_memory_request),
            resources_memory_limit = VALUES(resources_memory_limit)
    `,
		i.AttemptID,
		i.RepoIdentity.URL, i.RepoIdentity.BaseBranch, i.RepoIdentity.Prefix,
		i.FormulaPhase, i.HandoffMode,
		i.Resources.CPURequest, i.Resources.CPULimit,
		i.Resources.MemoryRequest, i.Resources.MemoryLimit,
		now,
	)
	if err != nil {
		return fmt.Errorf("intent: insert workload_intent %s: %w", i.AttemptID, err)
	}
	return nil
}

func (p *DoltPublisher) nowFn() time.Time {
	if p == nil || p.now == nil {
		return time.Now().UTC()
	}
	return p.now().UTC()
}

// DoltConsumer is the operator-side IntentConsumer backed by
// workload_intents. It polls the table for un-reconciled rows, emits
// them on the channel returned by Consume, and stamps reconciled_at
// once a row has been handed off.
type DoltConsumer struct {
	db           *sql.DB
	pollInterval time.Duration
	batchSize    int
	now          func() time.Time
}

// NewDoltConsumer constructs a DoltConsumer bound to db. pollInterval
// is clamped to defaultConsumerPollInterval when zero; batchSize is
// clamped to defaultConsumerBatchSize when zero. The caller owns the
// DB connection's lifetime.
func NewDoltConsumer(db *sql.DB, pollInterval time.Duration) *DoltConsumer {
	if pollInterval <= 0 {
		pollInterval = defaultConsumerPollInterval
	}
	return &DoltConsumer{
		db:           db,
		pollInterval: pollInterval,
		batchSize:    defaultConsumerBatchSize,
	}
}

// Consume starts the polling loop and returns a channel that receives
// WorkloadIntent values. The channel is closed when ctx is cancelled
// (or an unrecoverable error fires); a caller that wants to stop the
// consumer cancels ctx.
//
// The loop is defensive about partial failures: a scan error on one
// row skips that row; an UPDATE error on reconciled_at logs via the
// error channel (swallowed here — the reconciler's idempotence is the
// backstop) and continues. The channel is buffered so a slow consumer
// does not block the poll loop on a single tick.
func (c *DoltConsumer) Consume(ctx context.Context) (<-chan WorkloadIntent, error) {
	if c == nil || c.db == nil {
		return nil, fmt.Errorf("intent: DoltConsumer has nil DB")
	}
	out := make(chan WorkloadIntent, c.batchSize)
	go c.run(ctx, out)
	return out, nil
}

func (c *DoltConsumer) run(ctx context.Context, out chan<- WorkloadIntent) {
	defer close(out)
	ticker := time.NewTicker(c.pollInterval)
	defer ticker.Stop()

	// First iteration runs immediately so a freshly-started operator
	// drains any rows emitted before it came up.
	c.drainOnce(ctx, out)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.drainOnce(ctx, out)
		}
	}
}

// drainOnce selects up to batchSize un-reconciled rows, emits each on
// out, and stamps reconciled_at for each successful emit. The function
// respects ctx — a cancelled ctx short-circuits the loop without
// attempting further work.
func (c *DoltConsumer) drainOnce(ctx context.Context, out chan<- WorkloadIntent) {
	if ctx.Err() != nil {
		return
	}
	rows, err := c.db.QueryContext(ctx, `
        SELECT
            attempt_id, repo_url, repo_base_branch, repo_prefix,
            formula_phase, handoff_mode,
            resources_cpu_request, resources_cpu_limit,
            resources_memory_request, resources_memory_limit
        FROM workload_intents
        WHERE reconciled_at IS NULL
        ORDER BY emitted_at
        LIMIT ?
    `, c.batchSize)
	if err != nil {
		return
	}

	type pending struct {
		wi WorkloadIntent
	}
	var batch []pending
	for rows.Next() {
		var (
			wi                                                     WorkloadIntent
			cpuReq, cpuLim, memReq, memLim                         sql.NullString
		)
		if err := rows.Scan(
			&wi.AttemptID,
			&wi.RepoIdentity.URL, &wi.RepoIdentity.BaseBranch, &wi.RepoIdentity.Prefix,
			&wi.FormulaPhase, &wi.HandoffMode,
			&cpuReq, &cpuLim, &memReq, &memLim,
		); err != nil {
			continue
		}
		wi.Resources.CPURequest = cpuReq.String
		wi.Resources.CPULimit = cpuLim.String
		wi.Resources.MemoryRequest = memReq.String
		wi.Resources.MemoryLimit = memLim.String
		batch = append(batch, pending{wi: wi})
	}
	_ = rows.Close()

	for _, p := range batch {
		select {
		case <-ctx.Done():
			return
		case out <- p.wi:
		}
		_, _ = c.db.ExecContext(ctx,
			`UPDATE workload_intents SET reconciled_at = ? WHERE attempt_id = ? AND reconciled_at IS NULL`,
			c.nowFn(), p.wi.AttemptID)
	}
}

func (c *DoltConsumer) nowFn() time.Time {
	if c == nil || c.now == nil {
		return time.Now().UTC()
	}
	return c.now().UTC()
}

// Compile-time interface conformance.
var (
	_ IntentPublisher = (*DoltPublisher)(nil)
	_ IntentConsumer  = (*DoltConsumer)(nil)
)
