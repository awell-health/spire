package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestApprenticeConfig_EffectiveTransport(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{name: "empty defaults to bundle", in: "", want: ApprenticeTransportBundle},
		{name: "push explicit", in: ApprenticeTransportPush, want: ApprenticeTransportPush},
		{name: "bundle explicit", in: ApprenticeTransportBundle, want: ApprenticeTransportBundle},
		{name: "arbitrary value passes through", in: "carrier-pigeon", want: "carrier-pigeon"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := ApprenticeConfig{Transport: tc.in}
			if got := c.EffectiveTransport(); got != tc.want {
				t.Fatalf("EffectiveTransport() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestLoadTowerConfig_MissingDeploymentMode confirms that a tower config
// written before the deployment_mode field existed (no key in JSON) loads as
// local-native. This is the only backward-compat handling the field needs:
// the loader fills in Default() so downstream callers can read the field
// directly instead of each site knowing about the fallback.
func TestLoadTowerConfig_MissingDeploymentMode(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	// Write a JSON config that omits deployment_mode entirely (simulating a
	// tower created before the field was added).
	dir, err := TowerConfigDir()
	if err != nil {
		t.Fatalf("TowerConfigDir: %v", err)
	}
	legacy := `{
  "name": "legacy-tower",
  "project_id": "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee",
  "hub_prefix": "leg",
  "database": "beads_leg",
  "created_at": "2026-03-01T10:00:00Z"
}
`
	if err := os.WriteFile(filepath.Join(dir, "legacy-tower.json"), []byte(legacy), 0644); err != nil {
		t.Fatalf("write legacy config: %v", err)
	}

	tc, err := LoadTowerConfig("legacy-tower")
	if err != nil {
		t.Fatalf("LoadTowerConfig: %v", err)
	}
	if tc.DeploymentMode != DeploymentModeLocalNative {
		t.Errorf("DeploymentMode = %q, want %q", tc.DeploymentMode, DeploymentModeLocalNative)
	}
	if got := tc.EffectiveDeploymentMode(); got != DeploymentModeLocalNative {
		t.Errorf("EffectiveDeploymentMode() = %q, want %q", got, DeploymentModeLocalNative)
	}
}

// TestTowerConfig_DeploymentModeRoundTrip confirms that writing cluster-native
// (the cluster-bootstrap value) to disk and reading it back preserves the
// value verbatim. Matters because a round-trip drift would cause pods to
// silently re-default to local-native after a restart.
func TestTowerConfig_DeploymentModeRoundTrip(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	tower := &TowerConfig{
		Name:           "cluster-tower",
		ProjectID:      "11111111-2222-4333-8444-555555555555",
		HubPrefix:      "clu",
		Database:       "beads_clu",
		CreatedAt:      "2026-03-21T10:00:00Z",
		DeploymentMode: DeploymentModeClusterNative,
	}
	if err := SaveTowerConfig(tower); err != nil {
		t.Fatalf("SaveTowerConfig: %v", err)
	}

	// The persisted JSON must contain the field on the wire, not just the
	// in-memory struct. A missing field would fall back to local-native on
	// read and hide the bug.
	p, _ := TowerConfigPath("cluster-tower")
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read written config: %v", err)
	}
	var wire map[string]any
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("parse written config: %v", err)
	}
	if got, _ := wire["deployment_mode"].(string); got != string(DeploymentModeClusterNative) {
		t.Errorf("on-disk deployment_mode = %q, want %q", got, DeploymentModeClusterNative)
	}

	loaded, err := LoadTowerConfig("cluster-tower")
	if err != nil {
		t.Fatalf("LoadTowerConfig: %v", err)
	}
	if loaded.DeploymentMode != DeploymentModeClusterNative {
		t.Errorf("DeploymentMode after round-trip = %q, want %q", loaded.DeploymentMode, DeploymentModeClusterNative)
	}
}

// TestTowerConfig_EffectiveDeploymentMode covers the accessor directly so
// downstream packages have a reliable fallback even when constructing a
// TowerConfig value outside the loader path (tests, in-memory tower).
func TestTowerConfig_EffectiveDeploymentMode(t *testing.T) {
	cases := []struct {
		name string
		in   DeploymentMode
		want DeploymentMode
	}{
		{name: "empty falls back to default", in: "", want: DeploymentModeLocalNative},
		{name: "local-native passes through", in: DeploymentModeLocalNative, want: DeploymentModeLocalNative},
		{name: "cluster-native passes through", in: DeploymentModeClusterNative, want: DeploymentModeClusterNative},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := TowerConfig{DeploymentMode: tc.in}.EffectiveDeploymentMode()
			if got != tc.want {
				t.Fatalf("EffectiveDeploymentMode() = %q, want %q", got, tc.want)
			}
		})
	}
}
