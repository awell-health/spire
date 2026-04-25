package tower

import (
	"errors"
	"fmt"

	"github.com/awell-health/spire/pkg/store"
)

// ReposTableSQL is the canonical DDL for the `repos` table that records
// per-prefix repository registrations. `spire repo add` writes here; the
// table must exist before any registration attempt or the operator's
// ClusterIdentityResolver will fail hard on lookup.
const ReposTableSQL = `CREATE TABLE IF NOT EXISTS repos (
    prefix       VARCHAR(16) PRIMARY KEY,
    repo_url     VARCHAR(512) NOT NULL,
    branch       VARCHAR(128) NOT NULL DEFAULT 'main',
    language     VARCHAR(32),
    registered_by VARCHAR(64),
    registered_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
)`

// AgentRunsTableSQL is the canonical DDL for the `agent_runs` table that
// backs the metrics pipeline. Every apprentice/wizard/sage/cleric/arbiter
// invocation inserts one row. Column additions are layered on via
// spireMigrations in cmd/spire at startup; this DDL reflects the shape
// produced for a freshly bootstrapped tower.
const AgentRunsTableSQL = `CREATE TABLE IF NOT EXISTS agent_runs (
    id VARCHAR(32) PRIMARY KEY,
    bead_id VARCHAR(64) NOT NULL,
    epic_id VARCHAR(64),
    agent_name VARCHAR(128),
    model VARCHAR(64) NOT NULL,
    role VARCHAR(16) NOT NULL,
    phase VARCHAR(16),
    context_tokens_in INT,
    context_tokens_out INT,
    total_tokens INT,
    turns INT,
    duration_seconds INT,
    startup_seconds INT,
    working_seconds INT,
    queue_seconds INT,
    review_seconds INT,
    result VARCHAR(32) NOT NULL,
    review_rounds INT DEFAULT 0,
    artificer_verdict VARCHAR(32),
    review_step VARCHAR(16),
    review_round INT,
    spec_file VARCHAR(256),
    spec_size_tokens INT,
    focus_context_tokens INT,
    files_changed INT,
    lines_added INT,
    lines_removed INT,
    tests_added INT,
    tests_passed BOOLEAN,
    system_prompt_hash VARCHAR(64),
    golden_run BOOLEAN DEFAULT FALSE,
    cost_usd DECIMAL(10,4),
    started_at DATETIME NOT NULL,
    completed_at DATETIME,
    formula_name VARCHAR(64),
    formula_version INT,
    formula_source VARCHAR(16),
    branch VARCHAR(128),
    commit_sha VARCHAR(64),
    bead_type VARCHAR(32),
    tower VARCHAR(64),
    parent_run_id VARCHAR(32),
    wave_index INT,
    timing_bucket VARCHAR(32),
    skip_reason VARCHAR(128),
    failure_class VARCHAR(32),
    attempt_number INT,
    recovery_bead_id VARCHAR(32),
    read_calls INT,
    edit_calls INT,
    tool_calls_json TEXT,
    max_turns INT,
    stop_reason VARCHAR(32),
    cache_read_tokens BIGINT,
    cache_write_tokens BIGINT,
    auth_profile TEXT,
    auth_profile_final TEXT,
    INDEX idx_bead (bead_id),
    INDEX idx_epic (epic_id),
    INDEX idx_result (result),
    INDEX idx_golden (golden_run),
    INDEX idx_model (model),
    INDEX idx_phase (phase),
    INDEX idx_failure_class (failure_class)
)`

// spireExtensionTables lists the schema Spire layers on top of bd's core
// tables on every tower. New extension tables MUST be appended here so
// every blank-bootstrap path (spire tower create, attach-cluster
// --bootstrap-if-blank) picks them up automatically. Ordering is preserved
// so failure attribution in ApplySpireExtensions stays deterministic.
var spireExtensionTables = []struct {
	name string
	sql  string
}{
	{"repos", ReposTableSQL},
	{"agent_runs", AgentRunsTableSQL},
	{"bead_lifecycle", store.BeadLifecycleTableSQL},
}

// ApplySpireExtensions layers Spire's schema extensions on top of bd's core
// tables in the given database. It is the single source of truth for the
// set of tables that a tower needs beyond what `bd init` creates (repo
// registrations, metrics, lifecycle sidecar) and must be invoked by every
// code path that stands up a brand-new tower database.
//
// All DDL is CREATE TABLE IF NOT EXISTS, so re-running on an already
// populated database is a safe no-op. Callers:
//   - spire tower create (cmd/spire/tower.go) — local-native path
//   - spire tower attach-cluster --bootstrap-if-blank — cluster-native path,
//     via BootstrapBlank
//
// Returns the first error encountered, with the failing table name wrapped
// so the caller can see which statement failed.
func ApplySpireExtensions(exec SQLExec, database string) error {
	if exec == nil {
		return errors.New("ApplySpireExtensions: exec is required")
	}
	if database == "" {
		return errors.New("ApplySpireExtensions: database is required")
	}
	for _, t := range spireExtensionTables {
		if _, err := exec(fmt.Sprintf("USE `%s`; %s", database, t.sql)); err != nil {
			return fmt.Errorf("create %s table: %w", t.name, err)
		}
	}
	return nil
}
