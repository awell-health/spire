package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/awell-health/spire/pkg/config"
	"github.com/awell-health/spire/pkg/gatewayclient"
	"github.com/spf13/cobra"
)

// newAttachClusterTestCmd builds a fresh cobra.Command that mirrors the flag
// set and RunE of towerAttachClusterCmd. Used for dispatcher tests so each
// case runs against a clean command without mutating the production global.
func newAttachClusterTestCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:  towerAttachClusterCmd.Use,
		RunE: towerAttachClusterCmd.RunE,
	}
	cmd.Flags().String("data-dir", "/data", "")
	cmd.Flags().String("database", "", "")
	cmd.Flags().String("prefix", "", "")
	cmd.Flags().String("dolthub-remote", "", "")
	cmd.Flags().Duration("dolt-wait", 120*time.Second, "")
	cmd.Flags().Bool("bootstrap-if-blank", false, "")
	cmd.Flags().String("bundle-store-backend", "", "")
	cmd.Flags().String("bundle-store-gcs-bucket", "", "")
	cmd.Flags().String("bundle-store-gcs-prefix", "", "")
	cmd.Flags().String("namespace", "", "")
	cmd.Flags().String("kubeconfig", "", "")
	cmd.Flags().String("context", "", "")
	cmd.Flags().Bool("in-cluster", false, "")
	cmd.Flags().String("tower", "", "")
	cmd.Flags().String("url", "", "")
	cmd.Flags().String("token", "", "")
	cmd.Flags().String("name", "", "")
	return cmd
}

// stubGatewayAttachDeps swaps the package-level hooks used by the
// gateway-mode attach-cluster flow for in-process fakes. Tests restore
// the originals via the returned cleanup.
func stubGatewayAttachDeps(t *testing.T, info gatewayclient.TowerInfo, fetchErr error) (capturedURL, capturedToken *string, tokenStore map[string]string, cleanup func()) {
	t.Helper()
	origFetch := gatewayAttachFetchTower
	origSet := gatewayAttachSetToken

	var gotURL, gotToken string
	capturedURL = &gotURL
	capturedToken = &gotToken

	gatewayAttachFetchTower = func(ctx context.Context, url, token string) (gatewayclient.TowerInfo, error) {
		*capturedURL = url
		*capturedToken = token
		return info, fetchErr
	}

	tokenStore = map[string]string{}
	gatewayAttachSetToken = func(towerName, token string) error {
		tokenStore[towerName] = token
		return nil
	}

	cleanup = func() {
		gatewayAttachFetchTower = origFetch
		gatewayAttachSetToken = origSet
	}
	return capturedURL, capturedToken, tokenStore, cleanup
}

func TestCmdTowerAttachClusterGateway_HappyPath(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("SPIRE_CONFIG_DIR", tmp)

	info := gatewayclient.TowerInfo{
		Name:     "prod-tower",
		Prefix:   "prd",
		DoltURL:  "https://spire.example.com/dolt",
		Archmage: "alice",
	}
	gotURL, gotToken, tokenStore, cleanup := stubGatewayAttachDeps(t, info, nil)
	defer cleanup()

	err := cmdTowerAttachClusterGateway("https://spire.example.com", "secret-token", "prod-tower", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Client was called with the provided URL/token.
	if *gotURL != "https://spire.example.com" {
		t.Errorf("gateway URL = %q, want %q", *gotURL, "https://spire.example.com")
	}
	if *gotToken != "secret-token" {
		t.Errorf("gateway token = %q, want %q", *gotToken, "secret-token")
	}

	// Token persisted under the effective name (== --tower when --name empty).
	if got := tokenStore["prod-tower"]; got != "secret-token" {
		t.Errorf("keychain token = %q, want %q", got, "secret-token")
	}

	// Tower config persisted with gateway fields.
	tower, err := loadTowerConfig("prod-tower")
	if err != nil {
		t.Fatalf("loadTowerConfig: %v", err)
	}
	if !tower.IsGateway() {
		t.Errorf("IsGateway() = false, want true")
	}
	if tower.URL != "https://spire.example.com" {
		t.Errorf("tower.URL = %q, want %q", tower.URL, "https://spire.example.com")
	}
	if tower.TokenRef != "prod-tower" {
		t.Errorf("tower.TokenRef = %q, want %q", tower.TokenRef, "prod-tower")
	}
	if tower.HubPrefix != "prd" {
		t.Errorf("tower.HubPrefix = %q, want %q", tower.HubPrefix, "prd")
	}
	if tower.Archmage.Name != "alice" {
		t.Errorf("tower.Archmage.Name = %q, want %q", tower.Archmage.Name, "alice")
	}
	if tower.Mode != config.TowerModeGateway {
		t.Errorf("tower.Mode = %q, want %q", tower.Mode, config.TowerModeGateway)
	}

	// Instance entry coherent with the tower config.
	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	inst, ok := cfg.Instances["prd"]
	if !ok {
		t.Fatalf("expected instance entry for prefix %q, got none", "prd")
	}
	if !inst.IsGateway() {
		t.Errorf("instance.IsGateway() = false, want true")
	}
	if inst.URL != "https://spire.example.com" {
		t.Errorf("instance.URL = %q, want %q", inst.URL, "https://spire.example.com")
	}
	if inst.TokenRef != "prod-tower" {
		t.Errorf("instance.TokenRef = %q, want %q", inst.TokenRef, "prod-tower")
	}
	if inst.Tower != "prod-tower" {
		t.Errorf("instance.Tower = %q, want %q", inst.Tower, "prod-tower")
	}
	if cfg.ActiveTower != "prod-tower" {
		t.Errorf("ActiveTower = %q, want %q", cfg.ActiveTower, "prod-tower")
	}
}

func TestCmdTowerAttachClusterGateway_LocalAliasOverride(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("SPIRE_CONFIG_DIR", tmp)

	info := gatewayclient.TowerInfo{
		Name:   "prod-tower",
		Prefix: "prd",
	}
	_, _, tokenStore, cleanup := stubGatewayAttachDeps(t, info, nil)
	defer cleanup()

	// --name overrides the effective alias. --tower still verifies against
	// the gateway's reported name; only the local filename/key differ.
	err := cmdTowerAttachClusterGateway("https://spire.example.com", "tok", "prod-tower", "my-prod")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if _, ok := tokenStore["my-prod"]; !ok {
		t.Errorf("keychain token not stored under alias %q", "my-prod")
	}
	if _, ok := tokenStore["prod-tower"]; ok {
		t.Errorf("keychain token should not be stored under --tower when --name is set")
	}

	// Config file written under the alias, not the remote name.
	tower, err := loadTowerConfig("my-prod")
	if err != nil {
		t.Fatalf("loadTowerConfig(my-prod): %v", err)
	}
	if tower.Name != "my-prod" {
		t.Errorf("tower.Name = %q, want %q", tower.Name, "my-prod")
	}
	if tower.TokenRef != "my-prod" {
		t.Errorf("tower.TokenRef = %q, want %q", tower.TokenRef, "my-prod")
	}

	if _, err := loadTowerConfig("prod-tower"); err == nil {
		t.Errorf("tower config for %q should not exist when --name overrides", "prod-tower")
	}
}

func TestCmdTowerAttachClusterGateway_TowerNameMismatch(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("SPIRE_CONFIG_DIR", tmp)

	info := gatewayclient.TowerInfo{
		Name:   "other-tower",
		Prefix: "oth",
	}
	_, _, tokenStore, cleanup := stubGatewayAttachDeps(t, info, nil)
	defer cleanup()

	err := cmdTowerAttachClusterGateway("https://spire.example.com", "tok", "prod-tower", "")
	if err == nil {
		t.Fatal("expected mismatch error, got nil")
	}
	msg := err.Error()
	// Error must mention both observed values so an operator can diagnose.
	if !strings.Contains(msg, "prod-tower") {
		t.Errorf("error should mention expected name %q, got: %s", "prod-tower", msg)
	}
	if !strings.Contains(msg, "other-tower") {
		t.Errorf("error should mention observed name %q, got: %s", "other-tower", msg)
	}

	// Nothing persisted on mismatch.
	if len(tokenStore) != 0 {
		t.Errorf("keychain touched on mismatch: %v", tokenStore)
	}
	if _, err := loadTowerConfig("prod-tower"); err == nil {
		t.Errorf("tower config written despite mismatch")
	}
}

func TestCmdTowerAttachClusterGateway_FetchError(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("SPIRE_CONFIG_DIR", tmp)

	_, _, tokenStore, cleanup := stubGatewayAttachDeps(t, gatewayclient.TowerInfo{}, gatewayclient.ErrUnauthorized)
	defer cleanup()

	err := cmdTowerAttachClusterGateway("https://spire.example.com", "bad-token", "prod-tower", "")
	if err == nil {
		t.Fatal("expected fetch error, got nil")
	}
	if !errors.Is(err, gatewayclient.ErrUnauthorized) {
		t.Errorf("expected wrapped ErrUnauthorized, got: %v", err)
	}

	// Fetch failure must not touch the keychain or write a tower config.
	if len(tokenStore) != 0 {
		t.Errorf("keychain touched on fetch error: %v", tokenStore)
	}
	if _, err := loadTowerConfig("prod-tower"); err == nil {
		t.Errorf("tower config written despite fetch error")
	}
}

func TestCmdTowerAttachClusterGateway_MissingFlags(t *testing.T) {
	// Keep keychain/http seams in place but never invoked — missing-flag
	// checks short-circuit before any I/O.
	_, _, _, cleanup := stubGatewayAttachDeps(t, gatewayclient.TowerInfo{}, nil)
	defer cleanup()

	cases := []struct {
		name                        string
		url, token, tower, alias    string
		wantSubstr                  string
	}{
		{"no url", "", "tok", "prod", "", "--url"},
		{"no token", "https://x", "", "prod", "", "--token"},
		{"no tower", "https://x", "tok", "", "", "--tower"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := cmdTowerAttachClusterGateway(tc.url, tc.token, tc.tower, tc.alias)
			if err == nil {
				t.Fatalf("expected error for %s, got nil", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Errorf("error should mention %q, got: %v", tc.wantSubstr, err)
			}
		})
	}
}

func TestTowerAttachClusterCmd_FlagConflicts(t *testing.T) {
	// RunE-level tests: exercise the dispatcher so --url without --token and
	// vice versa error out cleanly before reaching the gateway seam.
	_, _, _, cleanup := stubGatewayAttachDeps(t, gatewayclient.TowerInfo{}, nil)
	defer cleanup()

	cases := []struct {
		name       string
		args       []string
		wantSubstr string
	}{
		{
			name:       "token without url",
			args:       []string{"--token", "tok"},
			wantSubstr: "--url is required",
		},
		{
			name:       "url without token",
			args:       []string{"--url", "https://x"},
			wantSubstr: "--token is required",
		},
		{
			name:       "url+token without tower",
			args:       []string{"--url", "https://x", "--token", "tok"},
			wantSubstr: "--tower is required",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Build a fresh command mirroring the production one's flags so
			// tests do not mutate the global. Flags must match the set
			// defined in init() for the RunE logic to work.
			cmd := newAttachClusterTestCmd()
			cmd.SetArgs(tc.args)
			err := cmd.Execute()
			if err == nil {
				t.Fatalf("expected error for %s, got nil", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Errorf("error should mention %q, got: %v", tc.wantSubstr, err)
			}
		})
	}
}

func TestTowerAttachClusterCmd_GatewayRoutes(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("SPIRE_CONFIG_DIR", tmp)

	info := gatewayclient.TowerInfo{Name: "prod-tower", Prefix: "prd"}
	_, _, tokenStore, cleanup := stubGatewayAttachDeps(t, info, nil)
	defer cleanup()

	cmd := newAttachClusterTestCmd()
	cmd.SetArgs([]string{"--url", "https://x", "--token", "tok", "--tower", "prod-tower"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := tokenStore["prod-tower"]; !ok {
		t.Errorf("gateway path did not run — keychain untouched")
	}
	if _, err := loadTowerConfig("prod-tower"); err != nil {
		t.Errorf("tower config not written: %v", err)
	}
}
