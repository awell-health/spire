package steward

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"

	"github.com/awell-health/spire/pkg/dolt"
)

// MetricsServer exposes Prometheus /metrics, /healthz, k8s liveness/readiness,
// and a detailed health JSON endpoint using the steward's shared Dolt database connection.
type MetricsServer struct {
	port       int
	db         *sql.DB
	server     *http.Server
	cycleStats *CycleStats // reference to steward's cycle stats (may be nil)
	mergeQueue *MergeQueue // reference to merge queue (may be nil)
}

// MetricsServerOption configures optional MetricsServer dependencies.
type MetricsServerOption func(*MetricsServer)

// WithCycleStats attaches a CycleStats tracker to the metrics server.
func WithCycleStats(cs *CycleStats) MetricsServerOption {
	return func(m *MetricsServer) { m.cycleStats = cs }
}

// WithMergeQueue attaches a MergeQueue to the metrics server for depth/active reporting.
func WithMergeQueue(mq *MergeQueue) MetricsServerOption {
	return func(m *MetricsServer) { m.mergeQueue = mq }
}

// NewMetricsServer creates a MetricsServer that will listen on the given port
// and query metrics from the provided database connection.
func NewMetricsServer(port int, db *sql.DB, opts ...MetricsServerOption) *MetricsServer {
	m := &MetricsServer{
		port: port,
		db:   db,
	}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// Start launches the HTTP server in a background goroutine. Non-blocking.
func (m *MetricsServer) Start() error {
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", m.handleMetrics)
	mux.HandleFunc("/healthz", m.handleHealth)
	mux.HandleFunc("/livez", m.handleLivez)
	mux.HandleFunc("/readyz", m.handleReadyz)
	mux.HandleFunc("/health/detailed", m.handleDetailedHealth)

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
		// Write port file so `spire status` can discover us.
		writeMetricsPortFile(m.port)
		return nil
	}
}

// Stop gracefully shuts down the HTTP server.
func (m *MetricsServer) Stop(ctx context.Context) error {
	removeMetricsPortFile()
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

	// Steward cycle stats gauges
	if m.cycleStats != nil {
		cs := m.cycleStats.Snapshot()
		fmt.Fprintf(w, "# HELP spire_steward_active_agents Currently active agents.\n")
		fmt.Fprintf(w, "# TYPE spire_steward_active_agents gauge\n")
		fmt.Fprintf(w, "spire_steward_active_agents %d\n", cs.ActiveAgents)

		fmt.Fprintf(w, "# HELP spire_steward_schedulable_work Beads ready for assignment.\n")
		fmt.Fprintf(w, "# TYPE spire_steward_schedulable_work gauge\n")
		fmt.Fprintf(w, "spire_steward_schedulable_work %d\n", cs.SchedulableWork)

		fmt.Fprintf(w, "# HELP spire_steward_cycle_duration_seconds Duration of last steward cycle.\n")
		fmt.Fprintf(w, "# TYPE spire_steward_cycle_duration_seconds gauge\n")
		fmt.Fprintf(w, "spire_steward_cycle_duration_seconds %.3f\n", cs.CycleDuration.Seconds())

		fmt.Fprintf(w, "# HELP spire_steward_merge_queue_depth Pending merge requests.\n")
		fmt.Fprintf(w, "# TYPE spire_steward_merge_queue_depth gauge\n")
		fmt.Fprintf(w, "spire_steward_merge_queue_depth %d\n", cs.QueueDepth)
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

// handleLivez is a trivial liveness probe — if the server responds, it's alive.
func (m *MetricsServer) handleLivez(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}

// handleReadyz checks dolt connectivity. Returns 503 if dolt is unreachable.
func (m *MetricsServer) handleReadyz(w http.ResponseWriter, r *http.Request) {
	if err := m.db.PingContext(r.Context()); err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprintf(w, "not ready: %s", err)
		return
	}
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}

// handleDetailedHealth returns a JSON payload with cycle stats, queue depth,
// active agents, and last cycle time. Used by `spire status` to display
// steward health remotely.
func (m *MetricsServer) handleDetailedHealth(w http.ResponseWriter, r *http.Request) {
	resp := map[string]interface{}{"status": "ok"}

	// Dolt status
	if err := m.db.PingContext(r.Context()); err != nil {
		resp["status"] = "degraded"
		resp["dolt"] = "error: " + err.Error()
	} else {
		resp["dolt"] = "connected"
	}

	// Cycle stats
	if m.cycleStats != nil {
		snap := m.cycleStats.Snapshot()
		resp["last_cycle_at"] = snap.LastCycleAt
		resp["cycle_duration_ms"] = snap.CycleDuration.Milliseconds()
		resp["active_agents"] = snap.ActiveAgents
		resp["schedulable_work"] = snap.SchedulableWork
		resp["spawned_last_cycle"] = snap.SpawnedThisCycle
	}

	// Merge queue
	if m.mergeQueue != nil {
		resp["merge_queue_depth"] = m.mergeQueue.Depth()
		if active := m.mergeQueue.Active(); active != nil {
			resp["merge_active"] = active.BeadID
		}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// metricsPortFilePath returns the path to the file that records the active metrics port.
func metricsPortFilePath() string {
	return filepath.Join(dolt.GlobalDir(), "steward.metrics.port")
}

// writeMetricsPortFile writes the metrics port to a well-known file so
// `spire status` can discover and query the health endpoint.
func writeMetricsPortFile(port int) {
	path := metricsPortFilePath()
	os.WriteFile(path, []byte(fmt.Sprintf("%d", port)), 0644)
}

// removeMetricsPortFile removes the metrics port file on shutdown.
func removeMetricsPortFile() {
	os.Remove(metricsPortFilePath())
}

// ReadMetricsPort reads the metrics port from the well-known port file.
// Returns 0 if the file doesn't exist or can't be read.
func ReadMetricsPort() int {
	data, err := os.ReadFile(metricsPortFilePath())
	if err != nil {
		return 0
	}
	var port int
	if _, err := fmt.Sscanf(string(data), "%d", &port); err != nil {
		return 0
	}
	return port
}
