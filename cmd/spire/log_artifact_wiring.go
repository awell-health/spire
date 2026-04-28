// log_artifact_wiring.go installs the gateway's log artifact reader
// according to the chart-stamped LOGSTORE_* env vars. Without this,
// gateway.SetLogArtifactReader is never called and the bead-logs API
// degrades to empty manifest lists. See pkg/gateway/bead_logs.go.
package main

import (
	"context"
	"log"
	"os"
	"path/filepath"

	"cloud.google.com/go/storage"

	"github.com/awell-health/spire/pkg/dolt"
	"github.com/awell-health/spire/pkg/gateway"
	"github.com/awell-health/spire/pkg/logartifact"
	"github.com/awell-health/spire/pkg/store"
)

// wireGatewayLogArtifactReader builds the configured logartifact backend
// (local or GCS) and registers it on the gateway. The handlers consult
// the registration lazily, so a single call before the gateway serves
// its first request is sufficient.
//
// Failure modes are non-fatal: any construction error logs a warning
// and leaves the gateway in its default no-backend state, which the
// list endpoint surfaces as 200 with an empty list rather than 5xx. A
// misconfigured tower should not break the rest of the API.
func wireGatewayLogArtifactReader(ctx context.Context) {
	backend := os.Getenv("LOGSTORE_BACKEND")
	if backend == "" {
		backend = "local"
	}

	if _, err := ensureStore(); err != nil {
		log.Printf("[gateway/logs] ensure store failed: %s — bead-logs API will return empty lists", err)
		return
	}
	db, ok := store.ActiveDB()
	if !ok || db == nil {
		log.Printf("[gateway/logs] active dolt DB unavailable — bead-logs API will return empty lists")
		return
	}

	switch backend {
	case "local":
		root := filepath.Join(dolt.GlobalDir(), "wizards")
		st, err := logartifact.NewLocal(root, db)
		if err != nil {
			log.Printf("[gateway/logs] local backend init failed: %s", err)
			return
		}
		gateway.SetLogArtifactReader(st)
		log.Printf("[gateway/logs] local backend wired (root=%s)", root)
	case "gcs":
		bucket := os.Getenv("LOGSTORE_GCS_BUCKET")
		if bucket == "" {
			log.Printf("[gateway/logs] LOGSTORE_BACKEND=gcs but LOGSTORE_GCS_BUCKET not set — bead-logs API will return empty lists")
			return
		}
		prefix := os.Getenv("LOGSTORE_GCS_PREFIX")
		client, err := storage.NewClient(ctx)
		if err != nil {
			log.Printf("[gateway/logs] GCS client init failed: %s", err)
			return
		}
		st, err := logartifact.NewGCS(ctx, client, bucket, prefix, db)
		if err != nil {
			log.Printf("[gateway/logs] GCS backend init failed: %s", err)
			_ = client.Close()
			return
		}
		gateway.SetLogArtifactReader(st)
		log.Printf("[gateway/logs] GCS backend wired (bucket=%s prefix=%s)", bucket, prefix)
	default:
		log.Printf("[gateway/logs] unknown LOGSTORE_BACKEND=%q — bead-logs API will return empty lists", backend)
	}
}
