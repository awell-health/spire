package pool

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestPath(t *testing.T) {
	got := Path("/tmp/tower")
	want := filepath.Join("/tmp/tower", "auth.toml")
	if got != want {
		t.Fatalf("Path = %q, want %q", got, want)
	}
}

func TestLoadConfig_Fresh(t *testing.T) {
	dir := t.TempDir()
	contents := `
[auth]
default_pool = "subscription"
fallback_pool = "api-key"
selection = "preemptive"

[[auth.subscription]]
name = "primary"
token = "tok-aaa"
max_concurrent = 2

[[auth.subscription]]
name = "secondary"
token = "tok-bbb"
max_concurrent = 3

[[auth.api_key]]
name = "fallback"
key = "sk-ant-key"
max_concurrent = 1
`
	mustWrite(t, filepath.Join(dir, "auth.toml"), contents)

	cfg, err := LoadConfig(dir)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.DefaultPool != PoolNameSubscription {
		t.Errorf("DefaultPool = %q, want %q", cfg.DefaultPool, PoolNameSubscription)
	}
	if cfg.FallbackPool != PoolNameAPIKey {
		t.Errorf("FallbackPool = %q, want %q", cfg.FallbackPool, PoolNameAPIKey)
	}
	if cfg.Selection != PolicyPreemptive {
		t.Errorf("Selection = %q, want %q", cfg.Selection, PolicyPreemptive)
	}
	if len(cfg.Subscription) != 2 {
		t.Fatalf("len(Subscription) = %d, want 2", len(cfg.Subscription))
	}
	if cfg.Subscription[0].Name != "primary" || cfg.Subscription[0].Token != "tok-aaa" || cfg.Subscription[0].MaxConcurrent != 2 {
		t.Errorf("Subscription[0] = %+v", cfg.Subscription[0])
	}
	if cfg.Subscription[1].Name != "secondary" || cfg.Subscription[1].Token != "tok-bbb" {
		t.Errorf("Subscription[1] = %+v", cfg.Subscription[1])
	}
	if len(cfg.APIKey) != 1 {
		t.Fatalf("len(APIKey) = %d, want 1", len(cfg.APIKey))
	}
	if cfg.APIKey[0].Name != "fallback" || cfg.APIKey[0].Key != "sk-ant-key" {
		t.Errorf("APIKey[0] = %+v", cfg.APIKey[0])
	}
}

func TestLoadConfig_LegacySubscriptionOnly(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "credentials.toml"), `
[auth]
default = "subscription"

[auth.subscription]
token = "legacy-sub-tok"
`)

	cfg, err := LoadConfig(dir)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.DefaultPool != PoolNameSubscription {
		t.Errorf("DefaultPool = %q, want %q", cfg.DefaultPool, PoolNameSubscription)
	}
	if len(cfg.Subscription) != 1 {
		t.Fatalf("len(Subscription) = %d, want 1", len(cfg.Subscription))
	}
	got := cfg.Subscription[0]
	want := SlotConfig{Name: legacyDefaultSlotName, Token: "legacy-sub-tok", MaxConcurrent: 1}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Subscription[0] = %+v, want %+v", got, want)
	}
	if len(cfg.APIKey) != 0 {
		t.Errorf("len(APIKey) = %d, want 0", len(cfg.APIKey))
	}
}

func TestLoadConfig_LegacyAPIKeyOnly(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "credentials.toml"), `
[auth]
default = "api-key"

[auth.api-key]
key = "sk-ant-legacy"
`)

	cfg, err := LoadConfig(dir)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.DefaultPool != PoolNameAPIKey {
		t.Errorf("DefaultPool = %q, want %q", cfg.DefaultPool, PoolNameAPIKey)
	}
	if len(cfg.APIKey) != 1 {
		t.Fatalf("len(APIKey) = %d, want 1", len(cfg.APIKey))
	}
	got := cfg.APIKey[0]
	want := SlotConfig{Name: legacyDefaultSlotName, Key: "sk-ant-legacy", MaxConcurrent: 1}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("APIKey[0] = %+v, want %+v", got, want)
	}
	if len(cfg.Subscription) != 0 {
		t.Errorf("len(Subscription) = %d, want 0", len(cfg.Subscription))
	}
}

func TestLoadConfig_LegacyBothSlotsSubscriptionDefault(t *testing.T) {
	dir := t.TempDir()
	// Legacy file with both subscription and api-key but no `default` set:
	// the loader prefers subscription as the historical primary.
	mustWrite(t, filepath.Join(dir, "credentials.toml"), `
[auth.subscription]
token = "legacy-sub"

[auth.api-key]
key = "sk-ant-key"
`)

	cfg, err := LoadConfig(dir)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.DefaultPool != PoolNameSubscription {
		t.Errorf("DefaultPool = %q, want subscription (preferred when both present)", cfg.DefaultPool)
	}
	if len(cfg.Subscription) != 1 || len(cfg.APIKey) != 1 {
		t.Errorf("expected one slot in each pool, got %d sub / %d api-key", len(cfg.Subscription), len(cfg.APIKey))
	}
}

func TestLoadConfig_LegacyHonorsDefaultAPIKey(t *testing.T) {
	dir := t.TempDir()
	// Operator already moved legacy default to api-key — promotion preserves it.
	mustWrite(t, filepath.Join(dir, "credentials.toml"), `
[auth]
default = "api-key"

[auth.subscription]
token = "legacy-sub"

[auth.api-key]
key = "sk-ant-key"
`)

	cfg, err := LoadConfig(dir)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.DefaultPool != PoolNameAPIKey {
		t.Errorf("DefaultPool = %q, want api-key (legacy default honored)", cfg.DefaultPool)
	}
}

func TestLoadConfig_BothMissingIsError(t *testing.T) {
	dir := t.TempDir()
	_, err := LoadConfig(dir)
	if err == nil {
		t.Fatal("LoadConfig: want error when both auth.toml and credentials.toml are missing")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("error = %v, want errors.Is(err, os.ErrNotExist)", err)
	}
	if !strings.Contains(err.Error(), "auth.toml") {
		t.Errorf("error = %v, want auth.toml path mention", err)
	}
}

func TestLoadConfig_LegacyEmptyIsError(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "credentials.toml"), `
[auth]
default = ""
`)

	_, err := LoadConfig(dir)
	if err == nil {
		t.Fatal("LoadConfig: want error when legacy file has neither subscription nor api-key")
	}
	if !strings.Contains(err.Error(), "no subscription token or api-key") {
		t.Errorf("error = %v, want mention of empty legacy file", err)
	}
}

func TestLoadConfig_ParseError(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "auth.toml"), "this is = not [valid toml\n")

	_, err := LoadConfig(dir)
	if err == nil {
		t.Fatal("LoadConfig: want parse error for malformed TOML")
	}
	if !strings.Contains(err.Error(), "auth.toml") {
		t.Errorf("error = %v, want path context", err)
	}
	// Parse failures must NOT be reported as os.ErrNotExist — that path is
	// reserved for the missing-both case so callers can distinguish them.
	if errors.Is(err, os.ErrNotExist) {
		t.Errorf("parse error must not wrap os.ErrNotExist: %v", err)
	}
}

func TestLoadConfig_ValidationDuplicateSlotNames(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "auth.toml"), `
[auth]
default_pool = "subscription"

[[auth.subscription]]
name = "dupe"
token = "a"
max_concurrent = 1

[[auth.subscription]]
name = "dupe"
token = "b"
max_concurrent = 1
`)

	_, err := LoadConfig(dir)
	if err == nil {
		t.Fatal("LoadConfig: want error for duplicate slot names")
	}
	if !strings.Contains(err.Error(), "duplicate slot name") {
		t.Errorf("error = %v, want duplicate slot name mention", err)
	}
}

func TestLoadConfig_ValidationMissingDefaultPoolTarget(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "auth.toml"), `
[auth]
default_pool = "api-key"

[[auth.subscription]]
name = "only-sub"
token = "tok"
max_concurrent = 1
`)

	_, err := LoadConfig(dir)
	if err == nil {
		t.Fatal("LoadConfig: want error when default_pool names an empty pool")
	}
	if !strings.Contains(err.Error(), "default_pool") || !strings.Contains(err.Error(), "api-key") {
		t.Errorf("error = %v, want mention of default_pool and api-key", err)
	}
}

func TestLoadConfig_ValidationUnknownDefaultPool(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "auth.toml"), `
[auth]
default_pool = "wat"

[[auth.subscription]]
name = "s1"
token = "t"
max_concurrent = 1
`)

	_, err := LoadConfig(dir)
	if err == nil {
		t.Fatal("LoadConfig: want error for unknown default_pool value")
	}
	if !strings.Contains(err.Error(), "default_pool") {
		t.Errorf("error = %v, want default_pool mention", err)
	}
}

func TestLoadConfig_ValidationFallbackPool(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "auth.toml"), `
[auth]
default_pool = "subscription"
fallback_pool = "api-key"

[[auth.subscription]]
name = "s1"
token = "t"
max_concurrent = 1
`)

	_, err := LoadConfig(dir)
	if err == nil {
		t.Fatal("LoadConfig: want error when fallback_pool names empty pool")
	}
	if !strings.Contains(err.Error(), "fallback_pool") {
		t.Errorf("error = %v, want fallback_pool mention", err)
	}
}

func TestLoadConfig_ValidationMaxConcurrentBelow1(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{
			name: "zero",
			body: `
[auth]
default_pool = "subscription"

[[auth.subscription]]
name = "s1"
token = "t"
max_concurrent = 0
`,
		},
		{
			name: "negative",
			body: `
[auth]
default_pool = "subscription"

[[auth.subscription]]
name = "s1"
token = "t"
max_concurrent = -3
`,
		},
		{
			name: "missing_field_defaults_to_zero",
			body: `
[auth]
default_pool = "subscription"

[[auth.subscription]]
name = "s1"
token = "t"
`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			mustWrite(t, filepath.Join(dir, "auth.toml"), tc.body)
			_, err := LoadConfig(dir)
			if err == nil {
				t.Fatalf("LoadConfig: want max_concurrent < 1 error")
			}
			if !strings.Contains(err.Error(), "max_concurrent") {
				t.Errorf("error = %v, want max_concurrent mention", err)
			}
		})
	}
}

func TestLoadConfig_ValidationEmptySlotName(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "auth.toml"), `
[[auth.subscription]]
name = ""
token = "t"
max_concurrent = 1
`)

	_, err := LoadConfig(dir)
	if err == nil {
		t.Fatal("LoadConfig: want error for empty slot name")
	}
	if !strings.Contains(err.Error(), "empty name") {
		t.Errorf("error = %v, want mention of empty name", err)
	}
}

func TestLoadConfig_PreferAuthOverLegacy(t *testing.T) {
	// When auth.toml exists, legacy credentials.toml is ignored — even if
	// both files coexist during a partial migration.
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "auth.toml"), `
[auth]
default_pool = "subscription"

[[auth.subscription]]
name = "fresh"
token = "fresh-tok"
max_concurrent = 4
`)
	mustWrite(t, filepath.Join(dir, "credentials.toml"), `
[auth.subscription]
token = "legacy-tok"
`)

	cfg, err := LoadConfig(dir)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if len(cfg.Subscription) != 1 || cfg.Subscription[0].Name != "fresh" || cfg.Subscription[0].Token != "fresh-tok" {
		t.Errorf("expected fresh auth.toml to win over legacy: got %+v", cfg.Subscription)
	}
}

func TestWriteConfig_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	original := &Config{
		Subscription: []SlotConfig{
			{Name: "sub1", Token: "tok1", MaxConcurrent: 2},
			{Name: "sub2", Token: "tok2", MaxConcurrent: 5},
		},
		APIKey: []SlotConfig{
			{Name: "apk1", Key: "sk-ant-1", MaxConcurrent: 1},
		},
		DefaultPool:  PoolNameSubscription,
		FallbackPool: PoolNameAPIKey,
		Selection:    PolicyRoundRobin,
	}
	if err := WriteConfig(dir, original); err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}

	loaded, err := LoadConfig(dir)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if !reflect.DeepEqual(loaded, original) {
		t.Errorf("round-trip mismatch:\n got = %+v\nwant = %+v", loaded, original)
	}
}

func TestWriteConfig_FilePerms0600(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{
		Subscription: []SlotConfig{{Name: "s1", Token: "t", MaxConcurrent: 1}},
		DefaultPool:  PoolNameSubscription,
	}
	if err := WriteConfig(dir, cfg); err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}
	info, err := os.Stat(Path(dir))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("file mode = %o, want 0600", perm)
	}
}

func TestWriteConfig_RejectsInvalid(t *testing.T) {
	dir := t.TempDir()
	bad := &Config{
		Subscription: []SlotConfig{{Name: "s1", Token: "t", MaxConcurrent: 0}},
		DefaultPool:  PoolNameSubscription,
	}
	if err := WriteConfig(dir, bad); err == nil {
		t.Fatal("WriteConfig: want validation error before write")
	}
	if _, err := os.Stat(Path(dir)); !os.IsNotExist(err) {
		t.Errorf("auth.toml should not exist after rejected WriteConfig: stat err = %v", err)
	}
}

func TestWriteConfig_NilConfig(t *testing.T) {
	if err := WriteConfig(t.TempDir(), nil); err == nil {
		t.Fatal("WriteConfig(nil): want error")
	}
}

func TestWriteConfig_AtomicReplaceExisting(t *testing.T) {
	dir := t.TempDir()
	first := &Config{
		Subscription: []SlotConfig{{Name: "old", Token: "old-tok", MaxConcurrent: 1}},
		DefaultPool:  PoolNameSubscription,
	}
	if err := WriteConfig(dir, first); err != nil {
		t.Fatalf("WriteConfig first: %v", err)
	}
	second := &Config{
		Subscription: []SlotConfig{{Name: "new", Token: "new-tok", MaxConcurrent: 2}},
		DefaultPool:  PoolNameSubscription,
	}
	if err := WriteConfig(dir, second); err != nil {
		t.Fatalf("WriteConfig second: %v", err)
	}
	loaded, err := LoadConfig(dir)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if loaded.Subscription[0].Name != "new" {
		t.Errorf("after second WriteConfig, slot name = %q, want new", loaded.Subscription[0].Name)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".auth.toml.") {
			t.Errorf("leftover temp file: %s", e.Name())
		}
	}
}

func TestWriteConfig_CreatesParentDir(t *testing.T) {
	root := t.TempDir()
	towerDir := filepath.Join(root, "nested", "tower")
	cfg := &Config{
		Subscription: []SlotConfig{{Name: "s1", Token: "t", MaxConcurrent: 1}},
		DefaultPool:  PoolNameSubscription,
	}
	if err := WriteConfig(towerDir, cfg); err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}
	if _, err := os.Stat(Path(towerDir)); err != nil {
		t.Errorf("auth.toml not created: %v", err)
	}
}

func mustWrite(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}
