package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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

// TestExtractSQLValue pins the positional parser's behavior across the
// alias shapes that broke the previous allowlist-based implementation
// (spi-69b6ge / spi-19v3oa). The parser must return the first cell of
// the first data row regardless of what the column header says.
func TestExtractSQLValue(t *testing.T) {
	table := func(header, data string) string {
		rule := "+" + strings.Repeat("-", len(header)+2) + "+"
		return strings.Join([]string{
			rule,
			"| " + header + " |",
			rule,
			"| " + data + padRight(data, len(header)) + " |",
			rule,
		}, "\n")
	}

	cases := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "legacy value header",
			input: table("value", "abc"),
			want:  "abc",
		},
		{
			name:  "unaliased COUNT header",
			input: table("COUNT(*)", "42"),
			want:  "42",
		},
		{
			name:  "cnt alias regression from spi-69b6ge",
			input: table("cnt", "0"),
			want:  "0",
		},
		{
			name:  "underscore alias",
			input: table("total_rows", "17"),
			want:  "17",
		},
		{
			name: "multi-column returns first cell",
			input: strings.Join([]string{
				"+----+----+",
				"| c1 | c2 |",
				"+----+----+",
				"| a  | b  |",
				"+----+----+",
			}, "\n"),
			want: "a",
		},
		{
			name: "leading log lines before table",
			input: strings.Join([]string{
				"Warning: slow query",
				"[notice] connection established",
				"+-----+",
				"| cnt |",
				"+-----+",
				"| 5   |",
				"+-----+",
			}, "\n"),
			want: "5",
		},
		{
			name: "empty result set returns empty",
			input: strings.Join([]string{
				"+-------+",
				"| value |",
				"+-------+",
				"+-------+",
			}, "\n"),
			want: "",
		},
		{
			name: "NULL data cell returned literally",
			input: strings.Join([]string{
				"+-------+",
				"| value |",
				"+-------+",
				"| NULL  |",
				"+-------+",
			}, "\n"),
			want: "NULL",
		},
		{
			name: "multi-row data returns first row",
			input: strings.Join([]string{
				"+---+",
				"| v |",
				"+---+",
				"| 1 |",
				"| 2 |",
				"| 3 |",
				"+---+",
			}, "\n"),
			want: "1",
		},
		{
			name:  "plain-text fallback passes through",
			input: "hello",
			want:  "hello",
		},
		{
			name:  "empty input returns empty",
			input: "",
			want:  "",
		},
		{
			name:  "whitespace-only input returns empty",
			input: "   \n\n  ",
			want:  "",
		},
		{
			name:  "non-table dolt message returns last line",
			input: "Query OK, 0 rows affected (0.00 sec)",
			want:  "Query OK, 0 rows affected (0.00 sec)",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ExtractSQLValue(tc.input)
			if got != tc.want {
				t.Errorf("ExtractSQLValue() = %q, want %q\ninput:\n%s", got, tc.want, tc.input)
			}
		})
	}
}

// padRight pads s with spaces so the final cell width matches headerLen.
// Mirrors dolt's fixed-width tabular output, which is what the positional
// parser walks over.
func padRight(s string, headerLen int) string {
	if len(s) >= headerLen {
		return ""
	}
	return strings.Repeat(" ", headerLen-len(s))
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
