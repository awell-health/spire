package tower

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/awell-health/spire/pkg/config"
)

// BootstrapOpts configures the .beads/ workspace write. All paths and
// connection details are explicit so the function has no ambient state.
type BootstrapOpts struct {
	BeadsDir string             // target directory, created if absent
	Tower    *config.TowerConfig // database name + project_id
	DoltHost string             // written into config.yaml and metadata.json
	DoltPort string             // written as string; parsed into metadata.json as int
	Prefix   string             // when non-empty, also writes routes.jsonl
	AutoPush bool               // config.yaml auto_push setting
}

// BootstrapBeadsDir writes the minimum `.beads/` workspace that bd needs to
// talk to a running dolt server: metadata.json (with dolt_server_port set so
// beads uses ServerModeExternal and does not spawn a shadow server),
// config.yaml, and optional routes.jsonl.
//
// It does not register custom bead types — callers that need that should run
// their own `bd type add` step after the server is reachable.
func BootstrapBeadsDir(opts BootstrapOpts) error {
	if opts.Tower == nil {
		return fmt.Errorf("BootstrapBeadsDir: tower is nil")
	}
	if opts.BeadsDir == "" {
		return fmt.Errorf("BootstrapBeadsDir: BeadsDir is required")
	}

	if err := os.MkdirAll(opts.BeadsDir, 0755); err != nil {
		return fmt.Errorf("create .beads/: %w", err)
	}

	// A stale dolt-server.port file would override everything we write below
	// (beads resolves port as env > dolt-server.port > config.yaml > metadata.json).
	os.Remove(filepath.Join(opts.BeadsDir, "dolt-server.port"))

	serverPort, _ := strconv.Atoi(opts.DoltPort)
	meta := map[string]any{
		"database":         "dolt",
		"backend":          "dolt",
		"dolt_mode":        "server",
		"dolt_database":    opts.Tower.Database,
		"dolt_server_port": serverPort,
	}
	if opts.Tower.ProjectID != "" {
		meta["project_id"] = opts.Tower.ProjectID
	}
	metaBytes, _ := json.MarshalIndent(meta, "", "  ")
	if err := os.WriteFile(filepath.Join(opts.BeadsDir, "metadata.json"), append(metaBytes, '\n'), 0644); err != nil {
		return fmt.Errorf("write .beads/metadata.json: %w", err)
	}

	host := opts.DoltHost
	if host == "" {
		host = "127.0.0.1"
	}
	port := opts.DoltPort
	if port == "" {
		port = "3306"
	}
	configYAML := fmt.Sprintf("dolt.host: %q\ndolt.port: %s\nauto_push: %v\n", host, port, opts.AutoPush)
	if err := os.WriteFile(filepath.Join(opts.BeadsDir, "config.yaml"), []byte(configYAML), 0644); err != nil {
		return fmt.Errorf("write .beads/config.yaml: %w", err)
	}

	if opts.Prefix != "" {
		routes := fmt.Sprintf("{\"prefix\":\"%s-\",\"path\":\".\"}\n", opts.Prefix)
		if err := os.WriteFile(filepath.Join(opts.BeadsDir, "routes.jsonl"), []byte(routes), 0644); err != nil {
			return fmt.Errorf("write .beads/routes.jsonl: %w", err)
		}
	}

	return nil
}
