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

// TestTowerConfig_BundleStoreRoundTrip confirms a TowerConfig carrying a
// gcs BundleStore selection round-trips through SaveTowerConfig /
// LoadTowerConfig without field drift. The cluster path
// (`spire tower attach-cluster --bundle-store-backend=gcs ...`) writes
// this block, the steward and apprentice pods read it back; a silent
// marshal/unmarshal regression would turn every GCS-backed tower into a
// local-backend tower after the first pod restart.
func TestTowerConfig_BundleStoreRoundTrip(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)

	tower := &TowerConfig{
		Name:      "bundle-tower",
		ProjectID: "22222222-3333-4444-8555-666666666666",
		HubPrefix: "bdl",
		Database:  "beads_bdl",
		CreatedAt: "2026-04-22T10:00:00Z",
		BundleStore: BundleStoreConfig{
			Backend: "gcs",
			GCS: BundleStoreGCSConfig{
				Bucket: "spire-awell",
				Prefix: "smoke",
			},
		},
	}
	if err := SaveTowerConfig(tower); err != nil {
		t.Fatalf("SaveTowerConfig: %v", err)
	}

	// Wire-format assertion: the JSON on disk MUST contain the
	// bundle_store block with the nested gcs object. A drift in the
	// json tags would silently erase the selection after reload.
	p, _ := TowerConfigPath("bundle-tower")
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read written config: %v", err)
	}
	var wire map[string]any
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("parse written config: %v", err)
	}
	bs, ok := wire["bundle_store"].(map[string]any)
	if !ok {
		t.Fatalf("bundle_store missing or wrong type in %s: %v", p, wire["bundle_store"])
	}
	if got, _ := bs["backend"].(string); got != "gcs" {
		t.Errorf("bundle_store.backend = %q, want gcs", got)
	}
	gcs, ok := bs["gcs"].(map[string]any)
	if !ok {
		t.Fatalf("bundle_store.gcs missing or wrong type: %v", bs["gcs"])
	}
	if got, _ := gcs["bucket"].(string); got != "spire-awell" {
		t.Errorf("bundle_store.gcs.bucket = %q, want spire-awell", got)
	}
	if got, _ := gcs["prefix"].(string); got != "smoke" {
		t.Errorf("bundle_store.gcs.prefix = %q, want smoke", got)
	}

	loaded, err := LoadTowerConfig("bundle-tower")
	if err != nil {
		t.Fatalf("LoadTowerConfig: %v", err)
	}
	if loaded.BundleStore.Backend != "gcs" {
		t.Errorf("BundleStore.Backend = %q, want gcs", loaded.BundleStore.Backend)
	}
	if loaded.BundleStore.GCS.Bucket != "spire-awell" {
		t.Errorf("BundleStore.GCS.Bucket = %q, want spire-awell", loaded.BundleStore.GCS.Bucket)
	}
	if loaded.BundleStore.GCS.Prefix != "smoke" {
		t.Errorf("BundleStore.GCS.Prefix = %q, want smoke", loaded.BundleStore.GCS.Prefix)
	}
}

// TestTowerConfig_IsGatewayIsDirect pins the defaulting contract for the
// Mode field: empty and "direct" both count as direct so tower configs
// written before the field existed keep working, and only an explicit
// "gateway" flips IsGateway. The helpers are the supported guard for
// callers routing between raw Dolt and the gateway client — they must
// not fall out of sync.
func TestTowerConfig_IsGatewayIsDirect(t *testing.T) {
	cases := []struct {
		name      string
		mode      string
		isGateway bool
		isDirect  bool
	}{
		{name: "empty defaults to direct", mode: "", isGateway: false, isDirect: true},
		{name: "explicit direct", mode: TowerModeDirect, isGateway: false, isDirect: true},
		{name: "explicit gateway", mode: TowerModeGateway, isGateway: true, isDirect: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tower := TowerConfig{Mode: tc.mode}
			if got := tower.IsGateway(); got != tc.isGateway {
				t.Errorf("IsGateway() = %v, want %v", got, tc.isGateway)
			}
			if got := tower.IsDirect(); got != tc.isDirect {
				t.Errorf("IsDirect() = %v, want %v", got, tc.isDirect)
			}
		})
	}
}

// TestLoadTowerConfig_LegacyNoMode confirms a tower config written
// before the Mode/URL/TokenRef fields existed — where the on-disk
// JSON has no `mode` key at all — loads as a direct-mode tower.
// This is the backward-compat contract: existing
// ~/.config/spire/towers/*.json files keep working unchanged.
func TestLoadTowerConfig_LegacyNoMode(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	t.Setenv("SPIRE_CONFIG_DIR", "")

	dir, err := TowerConfigDir()
	if err != nil {
		t.Fatalf("TowerConfigDir: %v", err)
	}
	legacy := `{
  "name": "legacy-direct",
  "project_id": "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee",
  "hub_prefix": "ldr",
  "database": "beads_ldr",
  "created_at": "2026-03-01T10:00:00Z"
}
`
	if err := os.WriteFile(filepath.Join(dir, "legacy-direct.json"), []byte(legacy), 0644); err != nil {
		t.Fatalf("write legacy config: %v", err)
	}

	tc, err := LoadTowerConfig("legacy-direct")
	if err != nil {
		t.Fatalf("LoadTowerConfig: %v", err)
	}
	if tc.Mode != "" {
		t.Errorf("Mode = %q, want empty (preserve absent field)", tc.Mode)
	}
	if !tc.IsDirect() {
		t.Errorf("IsDirect() = false, want true (empty mode is direct)")
	}
	if tc.IsGateway() {
		t.Errorf("IsGateway() = true, want false")
	}
	if tc.URL != "" {
		t.Errorf("URL = %q, want empty", tc.URL)
	}
	if tc.TokenRef != "" {
		t.Errorf("TokenRef = %q, want empty", tc.TokenRef)
	}
}

// TestTowerConfig_DirectRoundTrip confirms an explicit direct-mode
// tower round-trips through SaveTowerConfig / LoadTowerConfig with
// the Mode field preserved and no gateway-only fields leaking to
// disk. Empty URL/TokenRef must stay omitted so direct configs don't
// grow useless keys.
func TestTowerConfig_DirectRoundTrip(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	t.Setenv("SPIRE_CONFIG_DIR", "")

	tower := &TowerConfig{
		Name:      "direct-tower",
		ProjectID: "33333333-4444-4555-8666-777777777777",
		HubPrefix: "drc",
		Database:  "beads_drc",
		CreatedAt: "2026-04-24T10:00:00Z",
		Mode:      TowerModeDirect,
	}
	if err := SaveTowerConfig(tower); err != nil {
		t.Fatalf("SaveTowerConfig: %v", err)
	}

	p, _ := TowerConfigPath("direct-tower")
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read written config: %v", err)
	}
	var wire map[string]any
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("parse written config: %v", err)
	}
	if got, _ := wire["mode"].(string); got != TowerModeDirect {
		t.Errorf("on-disk mode = %q, want %q", got, TowerModeDirect)
	}
	if _, present := wire["url"]; present {
		t.Errorf("url key should be omitted for direct-mode tower: %v", wire["url"])
	}
	if _, present := wire["token_ref"]; present {
		t.Errorf("token_ref key should be omitted for direct-mode tower: %v", wire["token_ref"])
	}

	loaded, err := LoadTowerConfig("direct-tower")
	if err != nil {
		t.Fatalf("LoadTowerConfig: %v", err)
	}
	if loaded.Mode != TowerModeDirect {
		t.Errorf("Mode after round-trip = %q, want %q", loaded.Mode, TowerModeDirect)
	}
	if !loaded.IsDirect() {
		t.Errorf("IsDirect() = false after round-trip, want true")
	}
}

// TestTowerConfig_GatewayRoundTrip confirms a gateway-mode tower
// round-trips URL and TokenRef through disk. Matters because the
// attach-cluster command writes this block once, then every
// subsequent CLI invocation reads it to reach the gateway —
// silent field drift would break every remote op after a reload.
func TestTowerConfig_GatewayRoundTrip(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	t.Setenv("SPIRE_CONFIG_DIR", "")

	tower := &TowerConfig{
		Name:      "gw-tower",
		ProjectID: "44444444-5555-4666-8777-888888888888",
		HubPrefix: "gwt",
		Database:  "beads_gwt",
		CreatedAt: "2026-04-24T11:00:00Z",
		Mode:      TowerModeGateway,
		URL:       "https://spire.example.com",
		TokenRef:  "gw-tower",
	}
	if err := SaveTowerConfig(tower); err != nil {
		t.Fatalf("SaveTowerConfig: %v", err)
	}

	p, _ := TowerConfigPath("gw-tower")
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read written config: %v", err)
	}
	var wire map[string]any
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatalf("parse written config: %v", err)
	}
	if got, _ := wire["mode"].(string); got != TowerModeGateway {
		t.Errorf("on-disk mode = %q, want %q", got, TowerModeGateway)
	}
	if got, _ := wire["url"].(string); got != "https://spire.example.com" {
		t.Errorf("on-disk url = %q, want https://spire.example.com", got)
	}
	if got, _ := wire["token_ref"].(string); got != "gw-tower" {
		t.Errorf("on-disk token_ref = %q, want gw-tower", got)
	}

	loaded, err := LoadTowerConfig("gw-tower")
	if err != nil {
		t.Fatalf("LoadTowerConfig: %v", err)
	}
	if loaded.Mode != TowerModeGateway {
		t.Errorf("Mode after round-trip = %q, want %q", loaded.Mode, TowerModeGateway)
	}
	if !loaded.IsGateway() {
		t.Errorf("IsGateway() = false after round-trip, want true")
	}
	if loaded.IsDirect() {
		t.Errorf("IsDirect() = true for gateway tower, want false")
	}
	if loaded.URL != "https://spire.example.com" {
		t.Errorf("URL after round-trip = %q, want https://spire.example.com", loaded.URL)
	}
	if loaded.TokenRef != "gw-tower" {
		t.Errorf("TokenRef after round-trip = %q, want gw-tower", loaded.TokenRef)
	}
}

// TestListTowerConfigs_GatewayAndDirectCoexist confirms that both
// mode variants survive the list helper's unmarshal path — not just
// the individual Load* path. ListTowerConfigs is what `spire tower
// list` and ResolveTowerConfigWith's sole-tower fallback call, so a
// decode drift here would make gateway towers invisible to the CLI.
func TestListTowerConfigs_GatewayAndDirectCoexist(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	t.Setenv("SPIRE_CONFIG_DIR", "")

	if err := SaveTowerConfig(&TowerConfig{
		Name: "t-direct", ProjectID: "55555555-6666-4777-8888-999999999999",
		HubPrefix: "tdi", Database: "beads_tdi", CreatedAt: "2026-04-24T12:00:00Z",
		Mode: TowerModeDirect,
	}); err != nil {
		t.Fatalf("save direct: %v", err)
	}
	if err := SaveTowerConfig(&TowerConfig{
		Name: "t-gw", ProjectID: "66666666-7777-4888-8999-aaaaaaaaaaaa",
		HubPrefix: "tgw", Database: "beads_tgw", CreatedAt: "2026-04-24T13:00:00Z",
		Mode: TowerModeGateway, URL: "https://gw.example.com", TokenRef: "t-gw",
	}); err != nil {
		t.Fatalf("save gateway: %v", err)
	}

	towers, err := ListTowerConfigs()
	if err != nil {
		t.Fatalf("ListTowerConfigs: %v", err)
	}
	byName := make(map[string]TowerConfig, len(towers))
	for _, tc := range towers {
		byName[tc.Name] = tc
	}
	direct, ok := byName["t-direct"]
	if !ok {
		t.Fatalf("direct tower missing from list: %v", towers)
	}
	if !direct.IsDirect() {
		t.Errorf("listed direct tower: IsDirect() = false, want true")
	}
	gw, ok := byName["t-gw"]
	if !ok {
		t.Fatalf("gateway tower missing from list: %v", towers)
	}
	if !gw.IsGateway() {
		t.Errorf("listed gateway tower: IsGateway() = false, want true")
	}
	if gw.URL != "https://gw.example.com" || gw.TokenRef != "t-gw" {
		t.Errorf("listed gateway URL/TokenRef drift: URL=%q TokenRef=%q", gw.URL, gw.TokenRef)
	}
}

// TestInstance_IsGatewayIsDirect covers the instance-side helpers.
// A sibling store-dispatch task branches on the instance's mode when
// resolving ops for the current CWD, so the defaulting contract has
// to match TowerConfig exactly: empty and "direct" are direct, only
// explicit "gateway" flips IsGateway. Nil receiver is treated as
// direct so callers that hold an optional *Instance don't need a
// separate nil check before branching.
func TestInstance_IsGatewayIsDirect(t *testing.T) {
	cases := []struct {
		name      string
		inst      *Instance
		isGateway bool
		isDirect  bool
	}{
		{name: "nil receiver counts as direct", inst: nil, isGateway: false, isDirect: true},
		{name: "empty mode counts as direct", inst: &Instance{}, isGateway: false, isDirect: true},
		{name: "explicit direct", inst: &Instance{Mode: TowerModeDirect}, isGateway: false, isDirect: true},
		{name: "explicit gateway", inst: &Instance{Mode: TowerModeGateway, URL: "https://x", TokenRef: "x"}, isGateway: true, isDirect: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.inst.IsGateway(); got != tc.isGateway {
				t.Errorf("IsGateway() = %v, want %v", got, tc.isGateway)
			}
			if got := tc.inst.IsDirect(); got != tc.isDirect {
				t.Errorf("IsDirect() = %v, want %v", got, tc.isDirect)
			}
		})
	}
}

// TestInstance_GatewayFieldsRoundTrip confirms the Mode/URL/TokenRef
// fields on an Instance survive a Save/Load cycle through the global
// SpireConfig. This is the path `spire repo add --url ...` writes and
// that ResolveBeadsDir / store-dispatch later reads; a marshal drift
// would silently demote gateway instances back to direct after a
// restart.
func TestInstance_GatewayFieldsRoundTrip(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("SPIRE_CONFIG_DIR", tmpDir)

	cfg := &SpireConfig{Instances: map[string]*Instance{
		"my-repo": {
			Path:     "/tmp/my-repo",
			Prefix:   "myr",
			Database: "beads_myr",
			Tower:    "gw-tower",
			Mode:     TowerModeGateway,
			URL:      "https://spire.example.com",
			TokenRef: "gw-tower",
		},
	}}
	if err := Save(cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	inst, ok := loaded.Instances["my-repo"]
	if !ok {
		t.Fatalf("instance missing after reload: %v", loaded.Instances)
	}
	if inst.Mode != TowerModeGateway {
		t.Errorf("Mode after round-trip = %q, want %q", inst.Mode, TowerModeGateway)
	}
	if inst.URL != "https://spire.example.com" {
		t.Errorf("URL after round-trip = %q, want https://spire.example.com", inst.URL)
	}
	if inst.TokenRef != "gw-tower" {
		t.Errorf("TokenRef after round-trip = %q, want gw-tower", inst.TokenRef)
	}
	if !inst.IsGateway() {
		t.Errorf("IsGateway() = false after round-trip, want true")
	}
}

// TestTowerConfig_EffectiveDeploymentMode pins the contract that the
// accessor surfaces DeploymentModeUnknown for in-memory TowerConfig{}
// values without a DeploymentMode set, instead of silently routing into
// LocalNative machinery. The empty case here is the spi-od41sr
// regression class: callers that switch on the result MUST add an
// explicit DeploymentModeUnknown branch and error loudly rather than
// fall through to the LocalNative behavior. See pkg/config/tower.go
// EffectiveDeploymentMode comment and bead spi-eep81n.
func TestTowerConfig_EffectiveDeploymentMode(t *testing.T) {
	cases := []struct {
		name string
		in   DeploymentMode
		want DeploymentMode
	}{
		{name: "empty surfaces unknown sentinel", in: "", want: DeploymentModeUnknown},
		{name: "local-native passes through", in: DeploymentModeLocalNative, want: DeploymentModeLocalNative},
		{name: "cluster-native passes through", in: DeploymentModeClusterNative, want: DeploymentModeClusterNative},
		{name: "attached-reserved passes through", in: DeploymentModeAttachedReserved, want: DeploymentModeAttachedReserved},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := TowerConfig{DeploymentMode: tc.in}.EffectiveDeploymentMode()
			if got != tc.want {
				t.Fatalf("EffectiveDeploymentMode() = %q, want %q", got, tc.want)
			}
		})
	}

	// Zero-value TowerConfig literal — the canonical "test author
	// forgot DeploymentMode" shape from spi-od41sr — must surface
	// Unknown rather than LocalNative.
	if got := (TowerConfig{}).EffectiveDeploymentMode(); got != DeploymentModeUnknown {
		t.Fatalf("(TowerConfig{}).EffectiveDeploymentMode() = %q, want %q", got, DeploymentModeUnknown)
	}
}
