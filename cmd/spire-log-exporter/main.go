// Command spire-log-exporter is the passive cluster log exporter
// sidecar from bead spi-k1cnof.
//
// The binary tails a shared log directory (SPIRE_LOG_ROOT), emits one
// structured JSON line per record to stdout for Cloud Logging, and
// uploads completed artifacts via pkg/logartifact.Store while
// recording manifest rows in the tower's agent_log_artifacts table.
//
// The sidecar is intentionally small. It does NOT process messages,
// dispatch work, create or close beads, or drive any agent lifecycle.
// Failures (manifest insert / upload) mark affected artifact rows as
// status=failed and emit ERROR-severity stdout records, but the
// process always exits 0 — the agent's exit status must not depend on
// the exporter's manifest writes.
//
// Configuration (env-only):
//
//	SPIRE_LOG_ROOT                  shared log dir (required)
//	SPIRE_TOWER                     tower / database name (required)
//	BEADS_DOLT_SERVER_HOST          dolt host (default 127.0.0.1)
//	BEADS_DOLT_SERVER_PORT          dolt port (default 3307)
//	LOGSTORE_BACKEND                local | gcs (default local)
//	LOGSTORE_GCS_BUCKET             required when LOGSTORE_BACKEND=gcs
//	LOGSTORE_GCS_PREFIX             optional object-name prefix
//	GOOGLE_APPLICATION_CREDENTIALS  optional GCS SA path
//	SPIRE_LOG_EXPORTER_DRAIN_DEADLINE  flush deadline (default 25s)
//
// The sidecar binary is compiled into the same agent image as the
// `spire` CLI; the pod-builder injects an extra container running this
// binary alongside the agent.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"time"

	"cloud.google.com/go/storage"
	_ "github.com/go-sql-driver/mysql"

	"github.com/awell-health/spire/pkg/logartifact"
	"github.com/awell-health/spire/pkg/logexport"
)

func main() {
	if err := run(); err != nil {
		log.Printf("spire-log-exporter: terminal error: %v", err)
		// Exit 1 only on misconfiguration. Runtime upload/manifest
		// failures are reported via stdout/stderr records and never
		// reach this exit path — agent success/failure must not depend
		// on the exporter.
		os.Exit(1)
	}
}

// run is the body of main, separated so failure paths can return
// errors and the test surface can pin up the wiring without sys.Exit.
func run() error {
	cfg, err := loadConfig()
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("config: %w", err)
	}

	tower := os.Getenv("SPIRE_TOWER")
	if tower == "" {
		// SPIRE_TOWER is required because the exporter must know which
		// dolt database to write manifest rows into. The tailer parses
		// tower from each file's path, but that's a per-file concern;
		// the DB connection happens once at startup.
		return fmt.Errorf("SPIRE_TOWER env is required")
	}

	db, err := openDoltDB(tower)
	if err != nil {
		return fmt.Errorf("dolt: %w", err)
	}
	defer db.Close()

	store, err := openStore(cfg, db)
	if err != nil {
		return fmt.Errorf("store: %w", err)
	}
	defer closeStore(store)

	ctx, stop := logexport.SignalCancelable(context.Background())
	defer stop()

	exp, err := logexport.NewExporter(cfg, store, os.Stdout)
	if err != nil {
		return fmt.Errorf("exporter: %w", err)
	}

	log.Printf("spire-log-exporter: starting (root=%s, backend=%s, tower=%s)",
		cfg.Root, cfg.EffectiveBackend(), tower)

	runErr := exp.Run(ctx)
	if runErr != nil {
		log.Printf("spire-log-exporter: run returned %v", runErr)
	}

	flushErr, elapsed := logexport.FlushWithDeadline(context.Background(), exp, cfg.EffectiveDrainDeadline())
	if flushErr != nil {
		log.Printf("spire-log-exporter: flush after %s reported %v (informational; agent verdict unaffected)", elapsed, flushErr)
	}
	if cerr := exp.Close(); cerr != nil {
		log.Printf("spire-log-exporter: close: %v", cerr)
	}
	stats := exp.Stats()
	log.Printf("spire-log-exporter: shutdown stats finalized=%d failed=%d files=%d retries=%d",
		stats.ArtifactsFinalized, stats.ArtifactsFailed, stats.FilesTracked, stats.ManifestRetries)

	return nil
}

// loadConfig builds a logexport.Config from env. Tunables that aren't
// set fall back to the package's documented defaults; durations are
// parsed via time.ParseDuration so operators can override with the
// usual "30s" / "2m" syntax.
func loadConfig() (logexport.Config, error) {
	cfg := logexport.Config{
		Root:      os.Getenv(logexport.EnvLogRoot),
		Backend:   os.Getenv(logexport.EnvLogStoreBackend),
		GCSBucket: os.Getenv(logexport.EnvLogStoreGCSBucket),
		GCSPrefix: os.Getenv(logexport.EnvLogStoreGCSPrefix),
	}
	if v := os.Getenv(logexport.EnvDrainDeadline); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return cfg, fmt.Errorf("%s: %w", logexport.EnvDrainDeadline, err)
		}
		cfg.DrainDeadline = d
	}
	if v := os.Getenv(logexport.EnvScanInterval); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return cfg, fmt.Errorf("%s: %w", logexport.EnvScanInterval, err)
		}
		cfg.ScanInterval = d
	}
	if v := os.Getenv(logexport.EnvIdleFinalize); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return cfg, fmt.Errorf("%s: %w", logexport.EnvIdleFinalize, err)
		}
		cfg.IdleFinalize = d
	}
	return cfg, nil
}

// openDoltDB opens a *sql.DB pointing at the in-cluster dolt server.
// Mirrors the pattern in cmd/spire/formula_bridge.go: the canonical
// connection target is BEADS_DOLT_SERVER_HOST/PORT plus the tower
// database name.
func openDoltDB(tower string) (*sql.DB, error) {
	host := os.Getenv("BEADS_DOLT_SERVER_HOST")
	if host == "" {
		host = "127.0.0.1"
	}
	port := os.Getenv("BEADS_DOLT_SERVER_PORT")
	if port == "" {
		port = "3307"
	}
	dsn := fmt.Sprintf("root:@tcp(%s:%s)/%s", host, port, tower)
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, err
	}
	// Probe so misconfiguration surfaces here rather than at first
	// manifest write. The probe is cheap and runs once per process.
	pingCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping %s:%s/%s: %w", host, port, tower, err)
	}
	return db, nil
}

// openStore returns the logartifact.Store implementation chosen by the
// resolved backend.
func openStore(cfg logexport.Config, db *sql.DB) (logartifact.Store, error) {
	switch cfg.EffectiveBackend() {
	case logexport.BackendLocal:
		return logartifact.NewLocal(cfg.Root, db)
	case logexport.BackendGCS:
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		client, err := storage.NewClient(ctx)
		if err != nil {
			return nil, err
		}
		store, err := logartifact.NewGCS(ctx, client, cfg.GCSBucket, cfg.GCSPrefix, db)
		if err != nil {
			_ = client.Close()
			return nil, err
		}
		return &gcsStoreWithClient{Store: store, client: client}, nil
	default:
		return nil, fmt.Errorf("unknown backend %q", cfg.Backend)
	}
}

// gcsStoreWithClient bundles a GCSStore with the storage.Client it owns
// so closeStore can release the client when the binary exits. The
// substrate's NewGCS takes the client as a borrowed handle (the caller
// owns the client lifecycle); we wrap it here so the binary's defer
// chain has one closer to call.
type gcsStoreWithClient struct {
	logartifact.Store
	client *storage.Client
}

// closeStore releases any per-store resources. Local stores have none.
func closeStore(store logartifact.Store) {
	if g, ok := store.(*gcsStoreWithClient); ok && g.client != nil {
		_ = g.client.Close()
	}
}
