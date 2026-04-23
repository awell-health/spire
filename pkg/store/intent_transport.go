package store

// WorkloadIntentsTableSQL is the canonical DDL for the workload_intents
// table — the dolt-backed outbox that carries WorkloadIntent values from
// the steward (publisher) to the operator's intent reconciler (consumer).
//
// The table is an append-then-mark outbox: steward INSERTs one row per
// claimed attempt, keyed by attempt_id, with reconciled_at NULL. The
// operator SELECTs un-reconciled rows, emits them on its IntentConsumer
// channel, and UPDATEs reconciled_at after the downstream reconciler has
// had a chance to apply the row (the reconciler's Create is idempotent
// via IsAlreadyExists, so consumer-side retries are safe).
//
// Schema notes:
//
//   - attempt_id is the primary key, so INSERT ... ON DUPLICATE KEY UPDATE
//     de-dupes Publish retries (e.g. after a transient steward restart).
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
    attempt_id              VARCHAR(64) PRIMARY KEY,
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
    INDEX idx_unreconciled (reconciled_at, emitted_at)
)`
