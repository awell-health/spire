package steward

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
)

// MetricsServer exposes Prometheus /metrics and /healthz endpoints
// using the steward's shared Dolt database connection.
type MetricsServer struct {
	port   int
	db     *sql.DB
	server *http.Server
}

// NewMetricsServer creates a MetricsServer that will listen on the given port
// and query metrics from the provided database connection.
func NewMetricsServer(port int, db *sql.DB) *MetricsServer {
	return &MetricsServer{
		port: port,
		db:   db,
	}
}

// Start launches the HTTP server in a background goroutine. Non-blocking.
func (m *MetricsServer) Start() error {
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", m.handleMetrics)
	mux.HandleFunc("/healthz", m.handleHealth)

	m.server = &http.Server{
		Addr:    fmt.Sprintf(":%d", m.port),
		Handler: mux,
	}

	errCh := make(chan error, 1)
	go func() {
		if err := m.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("[metrics] server error: %v", err)
			errCh <- err
		}
	}()

	// Give the server a moment to fail on bind errors.
	select {
	case err := <-errCh:
		return err
	default:
		log.Printf("[metrics] listening on :%d", m.port)
		return nil
	}
}

// Stop gracefully shuts down the HTTP server.
func (m *MetricsServer) Stop(ctx context.Context) error {
	if m.server == nil {
		return nil
	}
	return m.server.Shutdown(ctx)
}

// handleMetrics writes a Prometheus text-format response with agent run metrics.
// Hand-rolled format — no external prometheus dependency.
func (m *MetricsServer) handleMetrics(w http.ResponseWriter, r *http.Request) {
	snap, err := CollectMetrics(r.Context(), m.db)
	if err != nil {
		http.Error(w, "query failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")

	// Run counters
	fmt.Fprintf(w, "# HELP spire_runs_total Total agent runs recorded.\n")
	fmt.Fprintf(w, "# TYPE spire_runs_total counter\n")
	fmt.Fprintf(w, "spire_runs_total %d\n", snap.TotalRuns)

	fmt.Fprintf(w, "# HELP spire_runs_success_total Total successful agent runs.\n")
	fmt.Fprintf(w, "# TYPE spire_runs_success_total counter\n")
	fmt.Fprintf(w, "spire_runs_success_total %d\n", snap.SuccessfulRuns)

	fmt.Fprintf(w, "# HELP spire_runs_failed_total Total failed agent runs.\n")
	fmt.Fprintf(w, "# TYPE spire_runs_failed_total counter\n")
	fmt.Fprintf(w, "spire_runs_failed_total %d\n", snap.FailedRuns)

	fmt.Fprintf(w, "# HELP spire_runs_active Currently active agent runs.\n")
	fmt.Fprintf(w, "# TYPE spire_runs_active gauge\n")
	fmt.Fprintf(w, "spire_runs_active %d\n", snap.ActiveRuns)

	// DORA merge frequency
	fmt.Fprintf(w, "# HELP spire_merges_last_7d Successful merges in the last 7 days.\n")
	fmt.Fprintf(w, "# TYPE spire_merges_last_7d gauge\n")
	fmt.Fprintf(w, "spire_merges_last_7d %d\n", snap.MergesLast7Days)

	fmt.Fprintf(w, "# HELP spire_merges_last_30d Successful merges in the last 30 days.\n")
	fmt.Fprintf(w, "# TYPE spire_merges_last_30d gauge\n")
	fmt.Fprintf(w, "spire_merges_last_30d %d\n", snap.MergesLast30Days)

	// Cost
	fmt.Fprintf(w, "# HELP spire_tokens_total Total tokens consumed across all runs.\n")
	fmt.Fprintf(w, "# TYPE spire_tokens_total counter\n")
	fmt.Fprintf(w, "spire_tokens_total %d\n", snap.TotalTokensAllTime)

	fmt.Fprintf(w, "# HELP spire_cost_usd_total Total cost in USD across all runs.\n")
	fmt.Fprintf(w, "# TYPE spire_cost_usd_total counter\n")
	fmt.Fprintf(w, "spire_cost_usd_total %.4f\n", snap.TotalCostUSDAllTime)

	// Per-formula breakdown
	if len(snap.FormulaStats) > 0 {
		fmt.Fprintf(w, "# HELP spire_formula_runs_total Total runs per formula.\n")
		fmt.Fprintf(w, "# TYPE spire_formula_runs_total counter\n")
		for _, f := range snap.FormulaStats {
			fmt.Fprintf(w, "spire_formula_runs_total{formula=%q,version=%q} %d\n",
				f.FormulaName, f.FormulaVersion, f.RunCount)
		}

		fmt.Fprintf(w, "# HELP spire_formula_success_total Successful runs per formula.\n")
		fmt.Fprintf(w, "# TYPE spire_formula_success_total counter\n")
		for _, f := range snap.FormulaStats {
			fmt.Fprintf(w, "spire_formula_success_total{formula=%q,version=%q} %d\n",
				f.FormulaName, f.FormulaVersion, f.SuccessCount)
		}

		fmt.Fprintf(w, "# HELP spire_formula_avg_cost_usd Average cost per run by formula.\n")
		fmt.Fprintf(w, "# TYPE spire_formula_avg_cost_usd gauge\n")
		for _, f := range snap.FormulaStats {
			fmt.Fprintf(w, "spire_formula_avg_cost_usd{formula=%q,version=%q} %.4f\n",
				f.FormulaName, f.FormulaVersion, f.AvgCostUSD)
		}

		fmt.Fprintf(w, "# HELP spire_formula_avg_duration_seconds Average duration per run by formula.\n")
		fmt.Fprintf(w, "# TYPE spire_formula_avg_duration_seconds gauge\n")
		for _, f := range snap.FormulaStats {
			fmt.Fprintf(w, "spire_formula_avg_duration_seconds{formula=%q,version=%q} %.1f\n",
				f.FormulaName, f.FormulaVersion, f.AvgDurationSec)
		}
	}
}

// handleHealth returns a simple health check response.
func (m *MetricsServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	status := "ok"
	doltStatus := "connected"

	if err := m.db.PingContext(r.Context()); err != nil {
		doltStatus = "error: " + err.Error()
		status = "degraded"
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"status": status,
		"dolt":   doltStatus,
	})
}
