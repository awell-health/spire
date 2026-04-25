package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// setupAuthTest isolates auth config I/O to a temp dir and clears env vars
// that could leak state across tests. Returns the auth TOML file path.
func setupAuthTest(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("SPIRE_CONFIG_DIR", dir)
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	return filepath.Join(dir, "credentials.toml")
}

func TestReadAuthConfig_MissingFile_ReturnsDefaults(t *testing.T) {
	setupAuthTest(t)

	cfg, err := ReadAuthConfig()
	if err != nil {
		t.Fatalf("ReadAuthConfig: %v", err)
	}
	if cfg == nil {
		t.Fatal("ReadAuthConfig returned nil cfg")
	}
	if !cfg.AutoPromoteOn429 {
		t.Error("AutoPromoteOn429 default = false, want true")
	}
	if cfg.Subscription != nil || cfg.APIKey != nil {
		t.Errorf("missing file should yield empty slots, got Subscription=%v APIKey=%v", cfg.Subscription, cfg.APIKey)
	}
}

func TestWriteReadAuthConfig_RoundTrip(t *testing.T) {
	path := setupAuthTest(t)

	want := &AuthConfig{
		Default:          AuthSlotSubscription,
		AutoPromoteOn429: true,
		Subscription:     &AuthCredential{Slot: AuthSlotSubscription, Secret: "sk-ant-oat01-abcdef1234567890"},
		APIKey:           &AuthCredential{Slot: AuthSlotAPIKey, Secret: "sk-ant-api03-zzzzzzzz9999"},
	}
	if err := WriteAuthConfig(want); err != nil {
		t.Fatalf("WriteAuthConfig: %v", err)
	}

	got, err := ReadAuthConfig()
	if err != nil {
		t.Fatalf("ReadAuthConfig: %v", err)
	}
	if got.Default != want.Default {
		t.Errorf("Default = %q, want %q", got.Default, want.Default)
	}
	if got.AutoPromoteOn429 != want.AutoPromoteOn429 {
		t.Errorf("AutoPromoteOn429 = %v, want %v", got.AutoPromoteOn429, want.AutoPromoteOn429)
	}
	if got.Subscription == nil || got.Subscription.Secret != want.Subscription.Secret || got.Subscription.Slot != AuthSlotSubscription {
		t.Errorf("Subscription = %+v, want %+v", got.Subscription, want.Subscription)
	}
	if got.APIKey == nil || got.APIKey.Secret != want.APIKey.Secret || got.APIKey.Slot != AuthSlotAPIKey {
		t.Errorf("APIKey = %+v, want %+v", got.APIKey, want.APIKey)
	}

	// Verify file permissions — skipped on Windows where perm bits differ.
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat credentials.toml: %v", err)
		}
		if mode := info.Mode().Perm(); mode != 0600 {
			t.Errorf("credentials.toml mode = %o, want 0600", mode)
		}
	}
}

func TestWriteAuthConfig_AutoPromoteFalseRoundTrip(t *testing.T) {
	setupAuthTest(t)

	want := &AuthConfig{
		Default:          AuthSlotAPIKey,
		AutoPromoteOn429: false,
		APIKey:           &AuthCredential{Slot: AuthSlotAPIKey, Secret: "sk-ant-api03-foo"},
	}
	if err := WriteAuthConfig(want); err != nil {
		t.Fatalf("WriteAuthConfig: %v", err)
	}
	got, err := ReadAuthConfig()
	if err != nil {
		t.Fatalf("ReadAuthConfig: %v", err)
	}
	if got.AutoPromoteOn429 {
		t.Error("AutoPromoteOn429 = true, want false (explicit false must round-trip)")
	}
}

func TestWriteAuthConfig_NilErrors(t *testing.T) {
	setupAuthTest(t)
	if err := WriteAuthConfig(nil); err == nil {
		t.Error("WriteAuthConfig(nil) = nil error, want error")
	}
}

func TestMigrateFromFlatFormat(t *testing.T) {
	tests := []struct {
		name        string
		flatContent string
		wantSub     string
		wantAPI     string
		wantDefault string
	}{
		{
			name: "api key only (env var name)",
			flatContent: "" +
				"# some comment\n" +
				"ANTHROPIC_API_KEY=sk-ant-api03-abcdef\n" +
				"github-token=ghp_xxx\n",
			wantSub:     "",
			wantAPI:     "sk-ant-api03-abcdef",
			wantDefault: AuthSlotAPIKey,
		},
		{
			name: "api key only (short key name)",
			flatContent: "" +
				"anthropic-key=sk-ant-api03-short\n" +
				"github-token=ghp_xxx\n",
			wantSub:     "",
			wantAPI:     "sk-ant-api03-short",
			wantDefault: AuthSlotAPIKey,
		},
		{
			name: "subscription via CLAUDE_CODE_OAUTH_TOKEN",
			flatContent: "" +
				"CLAUDE_CODE_OAUTH_TOKEN=sk-ant-oat01-zzz\n" +
				"github-token=ghp_yyy\n",
			wantSub:     "sk-ant-oat01-zzz",
			wantAPI:     "",
			wantDefault: AuthSlotSubscription,
		},
		{
			name: "subscription via ANTHROPIC_AUTH_TOKEN",
			flatContent: "" +
				"ANTHROPIC_AUTH_TOKEN=sk-ant-oat01-legacy\n",
			wantSub:     "sk-ant-oat01-legacy",
			wantAPI:     "",
			wantDefault: AuthSlotSubscription,
		},
		{
			name: "both slots — subscription wins as default",
			flatContent: "" +
				"ANTHROPIC_API_KEY=sk-ant-api03-k\n" +
				"CLAUDE_CODE_OAUTH_TOKEN=sk-ant-oat01-t\n",
			wantSub:     "sk-ant-oat01-t",
			wantAPI:     "sk-ant-api03-k",
			wantDefault: AuthSlotSubscription,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tomlPath := setupAuthTest(t)
			dir := filepath.Dir(tomlPath)
			flatPath := filepath.Join(dir, "credentials")
			if err := os.WriteFile(flatPath, []byte(tc.flatContent), 0600); err != nil {
				t.Fatalf("write flat fixture: %v", err)
			}

			cfg, err := ReadAuthConfig()
			if err != nil {
				t.Fatalf("ReadAuthConfig: %v", err)
			}
			if tc.wantSub == "" {
				if cfg.Subscription != nil {
					t.Errorf("Subscription = %+v, want nil", cfg.Subscription)
				}
			} else {
				if cfg.Subscription == nil || cfg.Subscription.Secret != tc.wantSub {
					t.Errorf("Subscription = %+v, want secret=%q", cfg.Subscription, tc.wantSub)
				}
			}
			if tc.wantAPI == "" {
				if cfg.APIKey != nil {
					t.Errorf("APIKey = %+v, want nil", cfg.APIKey)
				}
			} else {
				if cfg.APIKey == nil || cfg.APIKey.Secret != tc.wantAPI {
					t.Errorf("APIKey = %+v, want secret=%q", cfg.APIKey, tc.wantAPI)
				}
			}
			if cfg.Default != tc.wantDefault {
				t.Errorf("Default = %q, want %q", cfg.Default, tc.wantDefault)
			}

			// Auth TOML file should exist and be 0600.
			if runtime.GOOS != "windows" {
				info, statErr := os.Stat(tomlPath)
				if statErr != nil {
					t.Fatalf("stat %s: %v", tomlPath, statErr)
				}
				if mode := info.Mode().Perm(); mode != 0600 {
					t.Errorf("auth file mode = %o, want 0600", mode)
				}
			}
			data, _ := os.ReadFile(tomlPath)
			if !strings.Contains(string(data), "[auth]") {
				t.Errorf("auth TOML missing [auth] section, got:\n%s", data)
			}
		})
	}
}

func TestMigrateFromFlatFormat_PreservesUnrelatedInFlatFile(t *testing.T) {
	tomlPath := setupAuthTest(t)
	dir := filepath.Dir(tomlPath)
	flatPath := filepath.Join(dir, "credentials")

	input := "" +
		"# Spire credentials — chmod 600\n" +
		"ANTHROPIC_API_KEY=sk-ant-api03-abc\n" +
		"github-token=ghp_preserved\n" +
		"dolthub-user=alice\n"
	if err := os.WriteFile(flatPath, []byte(input), 0600); err != nil {
		t.Fatalf("write flat fixture: %v", err)
	}

	if _, err := ReadAuthConfig(); err != nil {
		t.Fatalf("ReadAuthConfig: %v", err)
	}

	flatData, err := os.ReadFile(flatPath)
	if err != nil {
		t.Fatalf("read flat: %v", err)
	}
	flat := string(flatData)
	if !strings.Contains(flat, "github-token=ghp_preserved") {
		t.Errorf("github-token not preserved in flat file, got:\n%s", flat)
	}
	if !strings.Contains(flat, "dolthub-user=alice") {
		t.Errorf("dolthub-user not preserved in flat file, got:\n%s", flat)
	}
	// The migrated auth key must be gone from the flat file.
	if strings.Contains(flat, "ANTHROPIC_API_KEY=") {
		t.Errorf("ANTHROPIC_API_KEY should have been migrated out of flat file, got:\n%s", flat)
	}
	// Unrelated entries should still be readable via the flat loader.
	creds, err := LoadCredentials()
	if err != nil {
		t.Fatalf("LoadCredentials after migration: %v", err)
	}
	if creds["github-token"] != "ghp_preserved" {
		t.Errorf("github-token = %q, want ghp_preserved", creds["github-token"])
	}
	if creds["dolthub-user"] != "alice" {
		t.Errorf("dolthub-user = %q, want alice", creds["dolthub-user"])
	}
}

func TestMigrateFromFlatFormat_Idempotent(t *testing.T) {
	tomlPath := setupAuthTest(t)
	dir := filepath.Dir(tomlPath)
	flatPath := filepath.Join(dir, "credentials")

	// Flat file with an auth key; run migration twice; second run must be no-op.
	input := "ANTHROPIC_API_KEY=sk-ant-api03-abc\nother-key=value\n"
	if err := os.WriteFile(flatPath, []byte(input), 0600); err != nil {
		t.Fatalf("write flat: %v", err)
	}
	if err := MigrateFromFlatFormat(); err != nil {
		t.Fatalf("MigrateFromFlatFormat 1: %v", err)
	}
	firstTOML, _ := os.ReadFile(tomlPath)
	firstFlat, _ := os.ReadFile(flatPath)

	if err := MigrateFromFlatFormat(); err != nil {
		t.Fatalf("MigrateFromFlatFormat 2: %v", err)
	}
	secondTOML, _ := os.ReadFile(tomlPath)
	secondFlat, _ := os.ReadFile(flatPath)

	if string(firstTOML) != string(secondTOML) {
		t.Errorf("idempotent migration mutated TOML\nfirst:\n%s\nsecond:\n%s", firstTOML, secondTOML)
	}
	if string(firstFlat) != string(secondFlat) {
		t.Errorf("idempotent migration mutated flat\nfirst:\n%s\nsecond:\n%s", firstFlat, secondFlat)
	}
}

func TestMigrateFromFlatFormat_MissingFile(t *testing.T) {
	setupAuthTest(t)
	// No flat file exists — migration should be silently no-op.
	if err := MigrateFromFlatFormat(); err != nil {
		t.Errorf("MigrateFromFlatFormat with missing file = %v, want nil", err)
	}
	// ReadAuthConfig should still work and return defaults.
	cfg, err := ReadAuthConfig()
	if err != nil {
		t.Fatalf("ReadAuthConfig: %v", err)
	}
	if !cfg.AutoPromoteOn429 {
		t.Error("AutoPromoteOn429 default = false, want true")
	}
}

func TestMigrateFromFlatFormat_NoAuthKeys(t *testing.T) {
	tomlPath := setupAuthTest(t)
	dir := filepath.Dir(tomlPath)
	flatPath := filepath.Join(dir, "credentials")

	// Flat file with no auth keys — migration must not create the TOML file.
	input := "github-token=ghp_xxx\ndolthub-user=alice\n"
	if err := os.WriteFile(flatPath, []byte(input), 0600); err != nil {
		t.Fatalf("write flat: %v", err)
	}
	if err := MigrateFromFlatFormat(); err != nil {
		t.Fatalf("MigrateFromFlatFormat: %v", err)
	}
	if _, err := os.Stat(tomlPath); !os.IsNotExist(err) {
		t.Errorf("TOML file should not exist when no auth keys present, got err=%v", err)
	}
	// Flat file should be untouched.
	got, _ := os.ReadFile(flatPath)
	if string(got) != input {
		t.Errorf("flat file mutated when no auth keys present\nbefore:\n%s\nafter:\n%s", input, got)
	}
}

func TestMaskSecret(t *testing.T) {
	tests := []struct {
		name, in  string
		wantEmpty bool
	}{
		{"empty returns empty", "", true},
		{"short value", "abc", false},
		{"long api key", "sk-ant-api03-1234567890abcdef", false},
		{"long oauth token", "sk-ant-oat01-zzzzzzzzzzzzzzzzzzzz", false},
		{"plain long string", "abcdefghijklmnopqrstuvwxyz", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := MaskSecret(tc.in)
			if tc.wantEmpty {
				if got != "" {
					t.Errorf("MaskSecret(%q) = %q, want empty", tc.in, got)
				}
				return
			}
			if got == "" {
				t.Errorf("MaskSecret(%q) = empty, must be non-empty for non-empty input", tc.in)
			}
			if got == tc.in {
				t.Errorf("MaskSecret(%q) returned the full secret", tc.in)
			}
			// If the input is long enough, make sure the middle of the
			// secret isn't exposed in the output.
			if len(tc.in) >= 14 {
				middle := tc.in[7 : len(tc.in)-4]
				if middle != "" && strings.Contains(got, middle) {
					t.Errorf("MaskSecret(%q) leaked middle %q in output %q", tc.in, middle, got)
				}
			}
		})
	}
}

func TestAuthContext_InjectEnv(t *testing.T) {
	tests := []struct {
		name    string
		ctx     *AuthContext
		wantKey string
		wantVal string
	}{
		{
			name: "api-key slot injects ANTHROPIC_API_KEY",
			ctx: &AuthContext{
				Active: &AuthCredential{Slot: AuthSlotAPIKey, Secret: "sk-ant-api03-k"},
			},
			wantKey: "ANTHROPIC_API_KEY",
			wantVal: "sk-ant-api03-k",
		},
		{
			name: "subscription slot injects CLAUDE_CODE_OAUTH_TOKEN",
			ctx: &AuthContext{
				Active: &AuthCredential{Slot: AuthSlotSubscription, Secret: "sk-ant-oat01-t"},
			},
			wantKey: "CLAUDE_CODE_OAUTH_TOKEN",
			wantVal: "sk-ant-oat01-t",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			base := []string{"FOO=bar"}
			got := tc.ctx.InjectEnv(base)
			if len(got) != len(base)+1 {
				t.Fatalf("InjectEnv returned %d entries, want %d", len(got), len(base)+1)
			}
			want := tc.wantKey + "=" + tc.wantVal
			if got[len(got)-1] != want {
				t.Errorf("InjectEnv last entry = %q, want %q", got[len(got)-1], want)
			}
			// The other slot's env var must not appear.
			other := "ANTHROPIC_API_KEY"
			if tc.wantKey == "ANTHROPIC_API_KEY" {
				other = "CLAUDE_CODE_OAUTH_TOKEN"
			}
			for _, e := range got {
				if strings.HasPrefix(e, other+"=") {
					t.Errorf("InjectEnv leaked %q env var into output: %q", other, e)
				}
			}
		})
	}
}

func TestAuthContext_InjectEnv_NilOrEmpty(t *testing.T) {
	base := []string{"FOO=bar"}

	var nilCtx *AuthContext
	if got := nilCtx.InjectEnv(base); len(got) != 1 {
		t.Errorf("nil ctx InjectEnv = %v, want unchanged", got)
	}

	ctx := &AuthContext{}
	if got := ctx.InjectEnv(base); len(got) != 1 {
		t.Errorf("nil Active InjectEnv = %v, want unchanged", got)
	}

	ctx = &AuthContext{Active: &AuthCredential{Slot: AuthSlotAPIKey, Secret: ""}}
	if got := ctx.InjectEnv(base); len(got) != 1 {
		t.Errorf("empty secret InjectEnv = %v, want unchanged", got)
	}
}

func TestAuthContext_SwapToAPIKey(t *testing.T) {
	apiCred := &AuthCredential{Slot: AuthSlotAPIKey, Secret: "sk-ant-api03-fallback"}
	subCred := &AuthCredential{Slot: AuthSlotSubscription, Secret: "sk-ant-oat01-primary"}

	t.Run("swap succeeds", func(t *testing.T) {
		ctx := &AuthContext{Active: subCred, APIKey: apiCred}
		if err := ctx.SwapToAPIKey(); err != nil {
			t.Fatalf("SwapToAPIKey: %v", err)
		}
		if ctx.Active != apiCred {
			t.Errorf("Active = %+v, want %+v", ctx.Active, apiCred)
		}
		if ctx.SlotName() != AuthSlotAPIKey {
			t.Errorf("SlotName after swap = %q, want %q", ctx.SlotName(), AuthSlotAPIKey)
		}
	})

	t.Run("swap fails when APIKey nil", func(t *testing.T) {
		ctx := &AuthContext{Active: subCred, APIKey: nil}
		if err := ctx.SwapToAPIKey(); err == nil {
			t.Error("SwapToAPIKey with nil APIKey = nil error, want error")
		}
		if ctx.Active != subCred {
			t.Errorf("Active was mutated on failed swap: %+v", ctx.Active)
		}
	})

	t.Run("swap fails when Ephemeral", func(t *testing.T) {
		ctx := &AuthContext{Active: subCred, APIKey: apiCred, Ephemeral: true}
		if err := ctx.SwapToAPIKey(); err == nil {
			t.Error("SwapToAPIKey on ephemeral = nil error, want error")
		}
		if ctx.Active != subCred {
			t.Errorf("Active was mutated on ephemeral swap: %+v", ctx.Active)
		}
	})

	t.Run("swap on nil context errors", func(t *testing.T) {
		var ctx *AuthContext
		if err := ctx.SwapToAPIKey(); err == nil {
			t.Error("SwapToAPIKey on nil ctx = nil error, want error")
		}
	})
}

func TestAuthContext_SlotName(t *testing.T) {
	tests := []struct {
		name string
		ctx  *AuthContext
		want string
	}{
		{"nil ctx", nil, ""},
		{"nil Active", &AuthContext{}, ""},
		{"api-key", &AuthContext{Active: &AuthCredential{Slot: AuthSlotAPIKey}}, AuthSlotAPIKey},
		{"subscription", &AuthContext{Active: &AuthCredential{Slot: AuthSlotSubscription}}, AuthSlotSubscription},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.ctx.SlotName(); got != tc.want {
				t.Errorf("SlotName = %q, want %q", got, tc.want)
			}
		})
	}
}
