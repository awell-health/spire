package store

import (
	"context"
	"database/sql"
	"time"
)

// FormulaExperiment defines an A/B test between two formula variants.
type FormulaExperiment struct {
	ID           string    // unique experiment ID (e.g., "exp-abc123")
	Tower        string    // tower scope
	FormulaBase  string    // the formula being tested (e.g., "task-default")
	VariantA     string    // control formula name (usually same as FormulaBase)
	VariantB     string    // treatment formula name (e.g., "task-default-v2")
	TrafficSplit float64   // 0.0-1.0 fraction going to variant B
	Status       string    // "active", "concluded", "paused"
	Winner       string    // set when concluded; empty while active
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// ExperimentComparison holds metric comparison between two variants.
type ExperimentComparison struct {
	ExperimentID    string
	VariantAStats   VariantStats
	VariantBStats   VariantStats
	SignificantDiff bool // true if sample size is sufficient (>= 20 per variant)
}

// VariantStats holds aggregate metrics for a single formula variant.
type VariantStats struct {
	FormulaName  string
	RunCount     int
	SuccessCount int
	SuccessRate  float64
	AvgCostUSD   float64
	AvgDurationS float64
}

// EnsureFormulaExperimentsTable creates the formula_experiments table if it
// doesn't exist. Safe to call on every startup.
func EnsureFormulaExperimentsTable(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS formula_experiments (
		id VARCHAR(32) PRIMARY KEY,
		tower VARCHAR(64) NOT NULL,
		formula_base VARCHAR(128) NOT NULL,
		variant_a VARCHAR(128) NOT NULL,
		variant_b VARCHAR(128) NOT NULL,
		traffic_split DOUBLE NOT NULL DEFAULT 0.2,
		status VARCHAR(16) NOT NULL DEFAULT 'active',
		winner VARCHAR(128) DEFAULT '',
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
	)`)
	return err
}

// GetActiveExperiment returns the active experiment for a given tower + formula
// base. Returns nil (not an error) if no active experiment exists.
func GetActiveExperiment(ctx context.Context, db *sql.DB, tower, formulaBase string) (*FormulaExperiment, error) {
	row := db.QueryRowContext(ctx,
		`SELECT id, tower, formula_base, variant_a, variant_b, traffic_split,
		        status, winner, created_at, updated_at
		 FROM formula_experiments
		 WHERE tower = ? AND formula_base = ? AND status = 'active'
		 LIMIT 1`,
		tower, formulaBase,
	)
	var exp FormulaExperiment
	err := row.Scan(
		&exp.ID, &exp.Tower, &exp.FormulaBase, &exp.VariantA, &exp.VariantB,
		&exp.TrafficSplit, &exp.Status, &exp.Winner, &exp.CreatedAt, &exp.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &exp, nil
}

// CreateExperiment inserts a new experiment.
func CreateExperiment(ctx context.Context, db *sql.DB, exp FormulaExperiment) error {
	_, err := db.ExecContext(ctx,
		`INSERT INTO formula_experiments
			(id, tower, formula_base, variant_a, variant_b, traffic_split, status, winner, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		exp.ID, exp.Tower, exp.FormulaBase, exp.VariantA, exp.VariantB,
		exp.TrafficSplit, exp.Status, exp.Winner, exp.CreatedAt, exp.UpdatedAt,
	)
	return err
}

// ConcludeExperiment sets the winner and marks status as concluded.
func ConcludeExperiment(ctx context.Context, db *sql.DB, id, winner string) error {
	_, err := db.ExecContext(ctx,
		`UPDATE formula_experiments SET status = 'concluded', winner = ?, updated_at = NOW() WHERE id = ?`,
		winner, id,
	)
	return err
}

// PauseExperiment sets status to paused.
func PauseExperiment(ctx context.Context, db *sql.DB, id string) error {
	_, err := db.ExecContext(ctx,
		`UPDATE formula_experiments SET status = 'paused', updated_at = NOW() WHERE id = ?`,
		id,
	)
	return err
}

// ListExperiments returns all experiments for a tower.
func ListExperiments(ctx context.Context, db *sql.DB, tower string) ([]FormulaExperiment, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT id, tower, formula_base, variant_a, variant_b, traffic_split,
		        status, winner, created_at, updated_at
		 FROM formula_experiments
		 WHERE tower = ?
		 ORDER BY created_at DESC`,
		tower,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FormulaExperiment
	for rows.Next() {
		var exp FormulaExperiment
		if err := rows.Scan(
			&exp.ID, &exp.Tower, &exp.FormulaBase, &exp.VariantA, &exp.VariantB,
			&exp.TrafficSplit, &exp.Status, &exp.Winner, &exp.CreatedAt, &exp.UpdatedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, exp)
	}
	return out, rows.Err()
}

// CompareVariants queries agent_runs for both variants and computes comparison
// metrics. Uses the existing agent_runs table columns: formula_name, result,
// cost_usd, duration_seconds, tower.
func CompareVariants(ctx context.Context, db *sql.DB, exp FormulaExperiment) (*ExperimentComparison, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT formula_name,
		        COUNT(*) AS run_count,
		        SUM(CASE WHEN result = 'success' THEN 1 ELSE 0 END) AS success_count,
		        COALESCE(AVG(cost_usd), 0) AS avg_cost,
		        COALESCE(AVG(duration_seconds), 0) AS avg_duration
		 FROM agent_runs
		 WHERE formula_name IN (?, ?) AND tower = ?
		 GROUP BY formula_name`,
		exp.VariantA, exp.VariantB, exp.Tower,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	statsMap := make(map[string]VariantStats)
	for rows.Next() {
		var vs VariantStats
		if err := rows.Scan(&vs.FormulaName, &vs.RunCount, &vs.SuccessCount, &vs.AvgCostUSD, &vs.AvgDurationS); err != nil {
			return nil, err
		}
		if vs.RunCount > 0 {
			vs.SuccessRate = float64(vs.SuccessCount) / float64(vs.RunCount)
		}
		statsMap[vs.FormulaName] = vs
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	aStats := statsMap[exp.VariantA]
	aStats.FormulaName = exp.VariantA
	bStats := statsMap[exp.VariantB]
	bStats.FormulaName = exp.VariantB

	return &ExperimentComparison{
		ExperimentID:    exp.ID,
		VariantAStats:   aStats,
		VariantBStats:   bStats,
		SignificantDiff: aStats.RunCount >= 20 && bStats.RunCount >= 20,
	}, nil
}
