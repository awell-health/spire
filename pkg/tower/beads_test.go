package tower

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/awell-health/spire/pkg/config"
)

func TestBootstrapBeadsDir_Validation(t *testing.T) {
	tests := []struct {
		name    string
		opts    BootstrapOpts
		wantErr string
	}{
		{
			name:    "nil tower",
			opts:    BootstrapOpts{BeadsDir: t.TempDir()},
			wantErr: "tower is nil",
		},
		{
			name:    "empty BeadsDir",
			opts:    BootstrapOpts{Tower: &config.TowerConfig{Database: "beads_t1"}},
			wantErr: "BeadsDir is required",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := BootstrapBeadsDir(tc.opts)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("got %v, want substring %q", err, tc.wantErr)
			}
		})
	}
}

func TestBootstrapBeadsDir_WritesFiles(t *testing.T) {
	baseTower := &config.TowerConfig{
		Name:      "t1",
		ProjectID: "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee",
		HubPrefix: "spi",
		Database:  "beads_t1",
	}

	tests := []struct {
		name          string
		opts          BootstrapOpts
		wantHost      string
		wantPort      string
		wantAutoPush  string
		wantMetaPort  float64
		wantPrefixRow bool
		wantProjectID string
	}{
		{
			name: "full config with prefix + auto_push",
			opts: BootstrapOpts{
				Tower:    baseTower,
				DoltHost: "dolt.example",
				DoltPort: "3307",
				Prefix:   "spi",
				AutoPush: true,
			},
			wantHost:      "dolt.example",
			wantPort:      "3307",
			wantAutoPush:  "true",
			wantMetaPort:  3307,
			wantPrefixRow: true,
			wantProjectID: baseTower.ProjectID,
		},
		{
			name: "defaults when host/port blank, no prefix, no push",
			opts: BootstrapOpts{
				Tower: baseTower,
			},
			wantHost:      "127.0.0.1",
			wantPort:      "3306",
			wantAutoPush:  "false",
			wantMetaPort:  0, // blank port -> strconv.Atoi returns 0
			wantPrefixRow: false,
			wantProjectID: baseTower.ProjectID,
		},
		{
			name: "tower with no ProjectID -> key omitted",
			opts: BootstrapOpts{
				Tower: &config.TowerConfig{Database: "beads_noproj"},
				DoltPort: "3306",
			},
			wantHost:      "127.0.0.1",
			wantPort:      "3306",
			wantAutoPush:  "false",
			wantMetaPort:  3306,
			wantPrefixRow: false,
			wantProjectID: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			tc.opts.BeadsDir = dir

			if err := BootstrapBeadsDir(tc.opts); err != nil {
				t.Fatalf("BootstrapBeadsDir: %v", err)
			}

			// metadata.json
			metaBytes, err := os.ReadFile(filepath.Join(dir, "metadata.json"))
			if err != nil {
				t.Fatalf("read metadata.json: %v", err)
			}
			var meta map[string]any
			if err := json.Unmarshal(metaBytes, &meta); err != nil {
				t.Fatalf("parse metadata.json: %v", err)
			}
			if meta["database"] != "dolt" || meta["backend"] != "dolt" || meta["dolt_mode"] != "server" {
				t.Errorf("unexpected fixed fields: %v", meta)
			}
			if meta["dolt_database"] != tc.opts.Tower.Database {
				t.Errorf("dolt_database = %v, want %q", meta["dolt_database"], tc.opts.Tower.Database)
			}
			if got, _ := meta["dolt_server_port"].(float64); got != tc.wantMetaPort {
				t.Errorf("dolt_server_port = %v, want %v", meta["dolt_server_port"], tc.wantMetaPort)
			}
			if tc.wantProjectID == "" {
				if _, present := meta["project_id"]; present {
					t.Errorf("project_id should be omitted when blank; got %v", meta["project_id"])
				}
			} else if meta["project_id"] != tc.wantProjectID {
				t.Errorf("project_id = %v, want %q", meta["project_id"], tc.wantProjectID)
			}

			// config.yaml
			cfg, err := os.ReadFile(filepath.Join(dir, "config.yaml"))
			if err != nil {
				t.Fatalf("read config.yaml: %v", err)
			}
			got := string(cfg)
			for _, want := range []string{
				`dolt.host: "` + tc.wantHost + `"`,
				`dolt.port: ` + tc.wantPort,
				`auto_push: ` + tc.wantAutoPush,
			} {
				if !strings.Contains(got, want) {
					t.Errorf("config.yaml missing %q\ngot:\n%s", want, got)
				}
			}

			// routes.jsonl
			routesPath := filepath.Join(dir, "routes.jsonl")
			_, err = os.Stat(routesPath)
			if tc.wantPrefixRow {
				if err != nil {
					t.Fatalf("routes.jsonl missing: %v", err)
				}
				routes, _ := os.ReadFile(routesPath)
				wantLine := `{"prefix":"` + tc.opts.Prefix + `-","path":"."}`
				if !strings.Contains(string(routes), wantLine) {
					t.Errorf("routes.jsonl = %q, want substring %q", routes, wantLine)
				}
			} else {
				if err == nil {
					t.Errorf("routes.jsonl should not be written when Prefix is empty")
				}
			}
		})
	}
}

func TestBootstrapBeadsDir_CreatesMissingDir(t *testing.T) {
	parent := t.TempDir()
	target := filepath.Join(parent, "new-nested", ".beads")

	err := BootstrapBeadsDir(BootstrapOpts{
		BeadsDir: target,
		Tower:    &config.TowerConfig{Database: "beads_t1"},
	})
	if err != nil {
		t.Fatalf("BootstrapBeadsDir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(target, "metadata.json")); err != nil {
		t.Fatalf("metadata.json not written into created dir: %v", err)
	}
}

func TestBootstrapBeadsDir_RemovesStaleServerPortFile(t *testing.T) {
	dir := t.TempDir()
	stalePath := filepath.Join(dir, "dolt-server.port")
	if err := os.WriteFile(stalePath, []byte("9999"), 0644); err != nil {
		t.Fatalf("seed dolt-server.port: %v", err)
	}

	err := BootstrapBeadsDir(BootstrapOpts{
		BeadsDir: dir,
		Tower:    &config.TowerConfig{Database: "beads_t1"},
		DoltPort: "3306",
	})
	if err != nil {
		t.Fatalf("BootstrapBeadsDir: %v", err)
	}
	if _, err := os.Stat(stalePath); !os.IsNotExist(err) {
		t.Errorf("dolt-server.port should be removed; stat err=%v", err)
	}
}
