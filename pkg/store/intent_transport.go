package store

import (
	"fmt"
)

// WorkloadIntentsTableSQL is the canonical DDL for the workload_intents
// table — the dolt-backed outbox that carries WorkloadIntent values from
// the steward (publisher) to the operator's intent reconciler (consumer).
//
// The table is an append-then-mark outbox: steward INSERTs one row per
// dispatch keyed by (task_id, dispatch_seq), with reconciled_at NULL.
// The operator SELECTs un-reconciled rows, emits them on its
// IntentConsumer channel, and UPDATEs reconciled_at after the downstream
// reconciler has had a chance to apply the row (the reconciler's Create
// is idempotent via IsAlreadyExists, so consumer-side retries are safe).
//
// Schema notes:
//
//   - (task_id, dispatch_seq) is the composite primary key. Task identity
//     is authoritative for "what to dispatch"; attempt beads are a
//     wizard-internal concept and are not referenced here. A retry after
//     a failed attempt bumps dispatch_seq for the same task_id, giving
//     every redispatch a fresh row (and, via naming derived from the
//     PK, a fresh pod name).
//   - reason is an optional annotation explaining why this dispatch was
//     emitted (e.g. "retry-after-pod-death"). Empty on first dispatch.
//   - The repo_* and resources_* columns carry a flat projection of the
//     WorkloadIntent fields so the consumer can reconstruct the intent
//     value with a straightforward row scan (no JSON unmarshal needed).
//   - emitted_at is stamped with server time on insert and never updated.
//   - reconciled_at starts NULL; the consumer sets it once per row.
//   - idx_unreconciled indexes the column the consumer polls on so the
//     "un-reconciled first" select stays cheap as the table grows.
//
// Exported so cmd/spire/up.go and operator startup can both run the DDL
// idempotently as part of their tower migration step.
const WorkloadIntentsTableSQL = `CREATE TABLE IF NOT EXISTS workload_intents (
    task_id                 VARCHAR(64) NOT NULL,
    dispatch_seq            INT         NOT NULL,
    reason                  VARCHAR(64),
    repo_url                VARCHAR(512) NOT NULL,
    repo_base_branch        VARCHAR(128) NOT NULL,
    repo_prefix             VARCHAR(64)  NOT NULL,
    formula_phase           VARCHAR(32)  NOT NULL,
    handoff_mode            VARCHAR(32)  NOT NULL,
    resources_cpu_request   VARCHAR(32),
    resources_cpu_limit     VARCHAR(32),
    resources_memory_request VARCHAR(32),
    resources_memory_limit  VARCHAR(32),
    emitted_at              DATETIME NOT NULL,
    reconciled_at           DATETIME NULL,
    PRIMARY KEY (task_id, dispatch_seq),
    INDEX idx_unreconciled (reconciled_at, emitted_at)
)`

// NextDispatchSeq returns the next dispatch_seq to use when emitting a
// new workload_intents row for taskID. Fresh tasks (no prior intents)
// return 1; re-dispatches return max(dispatch_seq)+1 for that task.
//
// The returned value is advisory: the authoritative uniqueness check is
// the (task_id, dispatch_seq) PK on workload_intents itself. Two
// steward replicas racing for the same task will both compute the same
// seq; whoever INSERTs first wins, and the loser's Publish returns a
// duplicate-key error that callers log and skip.
func NextDispatchSeq(taskID string) (int, error) {
	db, ok := ActiveDB()
	if !ok || db == nil {
		// No active DB (e.g. test harness with mocked store) — return
		// 1 so the caller can proceed; the harness is responsible for
		// its own uniqueness semantics.
		return 1, nil
	}
	row := db.QueryRow(
		`SELECT COALESCE(MAX(dispatch_seq), 0) FROM workload_intents WHERE task_id = ?`,
		taskID,
	)
	var maxSeq int
	if err := row.Scan(&maxSeq); err != nil {
		return 0, fmt.Errorf("store: next dispatch seq for %s: %w", taskID, err)
	}
	return maxSeq + 1, nil
}
