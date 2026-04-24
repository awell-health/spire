package main

import (
	"bytes"
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/awell-health/spire/pkg/config"
)

// setupAuthCLITest isolates auth config I/O to a temp dir and rewires the
// auth CLI's stdin/stdout to test-controlled buffers. Returns the output
// buffer and a function the test uses to set stdin input (pass "" for no
// stdin). The temp dir's credentials.toml is reachable via config.AuthConfigPath.
func setupAuthCLITest(t *testing.T) (*bytes.Buffer, func(string)) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("SPIRE_CONFIG_DIR", dir)
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")

	out := &bytes.Buffer{}
	origStdin, origStdout := authStdinReader, authStdoutWriter
	t.Cleanup(func() {
		authStdinReader = origStdin
		authStdoutWriter = origStdout
	})
	authStdoutWriter = out

	setStdin := func(s string) {
		if s == "" {
			authStdinReader = strings.NewReader("")
			return
		}
		authStdinReader = strings.NewReader(s)
	}
	setStdin("")
	return out, setStdin
}

// authTOMLPath returns the current test's credentials.toml path (for file-
// system assertions such as perm checks).
func authTOMLPath(t *testing.T) string {
	t.Helper()
	p, err := config.AuthConfigPath()
	if err != nil {
		t.Fatalf("AuthConfigPath: %v", err)
	}
	return p
}

func TestConfigAuth_Usage(t *testing.T) {
	setupAuthCLITest(t)
	if err := cmdConfigAuth(nil); err == nil {
		t.Error("cmdConfigAuth(nil) = nil error, want usage error")
	}
	if err := cmdConfigAuth([]string{"bogus"}); err == nil {
		t.Error("cmdConfigAuth(bogus) = nil error, want unknown-subcommand error")
	}
}

func TestConfigAuthSet_Subscription_Literal(t *testing.T) {
	out, _ := setupAuthCLITest(t)
	err := cmdConfigAuth([]string{"set", "subscription", "--token", "sk-ant-oat01-abc1234567890"})
	if err != nil {
		t.Fatalf("set subscription: %v", err)
	}
	if strings.Contains(out.String(), "sk-ant-oat01-abc1234567890") {
		t.Errorf("set output leaked full token:\n%s", out.String())
	}

	cfg, err := config.ReadAuthConfig()
	if err != nil {
		t.Fatalf("ReadAuthConfig: %v", err)
	}
	if cfg.Subscription == nil || cfg.Subscription.Secret != "sk-ant-oat01-abc1234567890" {
		t.Errorf("Subscription = %+v, want secret=sk-ant-oat01-abc1234567890", cfg.Subscription)
	}
	if cfg.Default != config.AuthSlotSubscription {
		t.Errorf("Default = %q, want %q (first-configured slot becomes default)", cfg.Default, config.AuthSlotSubscription)
	}
}

func TestConfigAuthSet_APIKey_Literal(t *testing.T) {
	_, _ = setupAuthCLITest(t)
	err := cmdConfigAuth([]string{"set", "api-key", "--key", "sk-ant-api03-xyz1234567890"})
	if err != nil {
		t.Fatalf("set api-key: %v", err)
	}
	cfg, err := config.ReadAuthConfig()
	if err != nil {
		t.Fatalf("ReadAuthConfig: %v", err)
	}
	if cfg.APIKey == nil || cfg.APIKey.Secret != "sk-ant-api03-xyz1234567890" {
		t.Errorf("APIKey = %+v", cfg.APIKey)
	}
	if cfg.Default != config.AuthSlotAPIKey {
		t.Errorf("Default = %q, want %q", cfg.Default, config.AuthSlotAPIKey)
	}
}

func TestConfigAuthSet_Subscription_Stdin(t *testing.T) {
	_, setStdin := setupAuthCLITest(t)
	// Trailing newline must be stripped.
	setStdin("sk-ant-oat01-stdinvalue\n")
	err := cmdConfigAuth([]string{"set", "subscription", "--token-stdin"})
	if err != nil {
		t.Fatalf("set subscription --token-stdin: %v", err)
	}
	cfg, _ := config.ReadAuthConfig()
	if cfg.Subscription == nil || cfg.Subscription.Secret != "sk-ant-oat01-stdinvalue" {
		t.Errorf("Subscription = %+v, want secret=sk-ant-oat01-stdinvalue (trailing newline stripped)", cfg.Subscription)
	}
}

func TestConfigAuthSet_APIKey_Stdin_CRLF(t *testing.T) {
	_, setStdin := setupAuthCLITest(t)
	// Windows-style line ending from a piped file must also be stripped.
	setStdin("sk-ant-api03-stdincrlf\r\n")
	err := cmdConfigAuth([]string{"set", "api-key", "--key-stdin"})
	if err != nil {
		t.Fatalf("set api-key --key-stdin: %v", err)
	}
	cfg, _ := config.ReadAuthConfig()
	if cfg.APIKey == nil || cfg.APIKey.Secret != "sk-ant-api03-stdincrlf" {
		t.Errorf("APIKey = %+v, want secret with CRLF stripped", cfg.APIKey)
	}
}

func TestConfigAuthSet_MissingValue(t *testing.T) {
	setupAuthCLITest(t)
	// No --token / --token-stdin at all.
	if err := cmdConfigAuth([]string{"set", "subscription"}); err == nil {
		t.Error("set subscription without flag = nil error, want required-flag error")
	}
	if err := cmdConfigAuth([]string{"set", "api-key"}); err == nil {
		t.Error("set api-key without flag = nil error, want required-flag error")
	}
}

func TestConfigAuthSet_BothLiteralAndStdin(t *testing.T) {
	_, setStdin := setupAuthCLITest(t)
	setStdin("from-stdin")
	err := cmdConfigAuth([]string{"set", "subscription", "--token", "literal", "--token-stdin"})
	if err == nil {
		t.Error("expected error when both --token and --token-stdin set")
	}
}

func TestConfigAuthSet_UnknownSlot(t *testing.T) {
	setupAuthCLITest(t)
	if err := cmdConfigAuth([]string{"set", "bogus-slot", "--token", "x"}); err == nil {
		t.Error("expected error for unknown slot")
	}
}

func TestConfigAuthSet_WrongFlagForSlot(t *testing.T) {
	setupAuthCLITest(t)
	// --key with subscription slot is a scripting mistake; reject loudly.
	if err := cmdConfigAuth([]string{"set", "subscription", "--key", "x"}); err == nil {
		t.Error("subscription + --key: expected error, got nil")
	}
	if err := cmdConfigAuth([]string{"set", "api-key", "--token", "x"}); err == nil {
		t.Error("api-key + --token: expected error, got nil")
	}
}

func TestConfigAuthSet_FilePerm0600(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("perm bits differ on windows")
	}
	setupAuthCLITest(t)
	if err := cmdConfigAuth([]string{"set", "subscription", "--token", "sk-ant-oat01-permcheck"}); err != nil {
		t.Fatalf("set: %v", err)
	}
	info, err := os.Stat(authTOMLPath(t))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0600 {
		t.Errorf("credentials.toml mode = %o, want 0600", mode)
	}
}

func TestConfigAuthDefault_Switch(t *testing.T) {
	setupAuthCLITest(t)
	// Configure both slots — first-written becomes default.
	mustNoErr(t, cmdConfigAuth([]string{"set", "subscription", "--token", "sk-ant-oat01-aaaaaaaaaaaa"}))
	mustNoErr(t, cmdConfigAuth([]string{"set", "api-key", "--key", "sk-ant-api03-bbbbbbbbbbbb"}))

	cfg, _ := config.ReadAuthConfig()
	if cfg.Default != config.AuthSlotSubscription {
		t.Fatalf("precondition: Default = %q, want subscription", cfg.Default)
	}

	// Switch default to api-key.
	if err := cmdConfigAuth([]string{"default", "api-key"}); err != nil {
		t.Fatalf("default api-key: %v", err)
	}
	cfg, _ = config.ReadAuthConfig()
	if cfg.Default != config.AuthSlotAPIKey {
		t.Errorf("Default = %q, want %q after switch", cfg.Default, config.AuthSlotAPIKey)
	}
}

func TestConfigAuthDefault_UnconfiguredSlot(t *testing.T) {
	setupAuthCLITest(t)
	// Only subscription configured; switching default to api-key must fail
	// with an actionable hint.
	mustNoErr(t, cmdConfigAuth([]string{"set", "subscription", "--token", "sk-ant-oat01-zzzz1234"}))

	err := cmdConfigAuth([]string{"default", "api-key"})
	if err == nil {
		t.Fatal("default api-key with unconfigured slot = nil error, want error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "api-key") {
		t.Errorf("error should name the slot, got %q", msg)
	}
	if !strings.Contains(msg, "spire config auth set api-key") {
		t.Errorf("error should include the set-hint command, got %q", msg)
	}
}

func TestConfigAuthDefault_Usage(t *testing.T) {
	setupAuthCLITest(t)
	if err := cmdConfigAuth([]string{"default"}); err == nil {
		t.Error("default with no arg = nil error, want usage error")
	}
	if err := cmdConfigAuth([]string{"default", "bogus"}); err == nil {
		t.Error("default with unknown slot = nil error, want error")
	}
}

func TestConfigAuthShow_Empty(t *testing.T) {
	out, _ := setupAuthCLITest(t)
	if err := cmdConfigAuth([]string{"show"}); err != nil {
		t.Fatalf("show: %v", err)
	}
	s := out.String()
	if !strings.Contains(s, "subscription") || !strings.Contains(s, "api-key") {
		t.Errorf("show output missing slot names:\n%s", s)
	}
	if !strings.Contains(s, "not configured") {
		t.Errorf("show output on empty config must say 'not configured', got:\n%s", s)
	}
	if !strings.Contains(s, "auto_promote_on_429") {
		t.Errorf("show output must display auto_promote_on_429, got:\n%s", s)
	}
}

func TestConfigAuthShow_MasksAndMarksDefault(t *testing.T) {
	out, _ := setupAuthCLITest(t)
	subSecret := "sk-ant-oat01-subscribedsecretvalue"
	apiSecret := "sk-ant-api03-apikeysecretvalue"
	mustNoErr(t, cmdConfigAuth([]string{"set", "subscription", "--token", subSecret}))
	mustNoErr(t, cmdConfigAuth([]string{"set", "api-key", "--key", apiSecret}))
	mustNoErr(t, cmdConfigAuth([]string{"default", "api-key"}))

	out.Reset()
	if err := cmdConfigAuth([]string{"show"}); err != nil {
		t.Fatalf("show: %v", err)
	}
	s := out.String()

	// Mask invariant: neither full secret must appear in the show output.
	if strings.Contains(s, subSecret) {
		t.Errorf("show leaked subscription secret:\n%s", s)
	}
	if strings.Contains(s, apiSecret) {
		t.Errorf("show leaked api-key secret:\n%s", s)
	}

	// Default marker: the api-key line must carry the `*` prefix.
	if !hasDefaultMark(s, config.AuthSlotAPIKey) {
		t.Errorf("show missing '*' default marker for api-key line:\n%s", s)
	}
	if hasDefaultMark(s, config.AuthSlotSubscription) {
		t.Errorf("show has '*' marker on non-default subscription line:\n%s", s)
	}
	if !strings.Contains(s, "default") {
		t.Errorf("show missing default summary line:\n%s", s)
	}
}

// hasDefaultMark reports whether any line in out starts with "* " and then
// contains slot (with optional intervening whitespace from column padding).
func hasDefaultMark(out, slot string) bool {
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "* ") && strings.Contains(line, slot) {
			return true
		}
	}
	return false
}

func TestConfigAuthShow_RejectsArgs(t *testing.T) {
	setupAuthCLITest(t)
	if err := cmdConfigAuth([]string{"show", "extra"}); err == nil {
		t.Error("show with extra args = nil error, want usage error")
	}
}

func TestConfigAuthRemove_NonDefault(t *testing.T) {
	setupAuthCLITest(t)
	mustNoErr(t, cmdConfigAuth([]string{"set", "subscription", "--token", "sk-ant-oat01-aaaaaaaa"}))
	mustNoErr(t, cmdConfigAuth([]string{"set", "api-key", "--key", "sk-ant-api03-bbbbbbbb"}))
	// Default is subscription; removing api-key (non-default) is allowed.
	if err := cmdConfigAuth([]string{"remove", "api-key"}); err != nil {
		t.Fatalf("remove api-key: %v", err)
	}
	cfg, _ := config.ReadAuthConfig()
	if cfg.APIKey != nil {
		t.Errorf("APIKey = %+v, want nil after remove", cfg.APIKey)
	}
	if cfg.Subscription == nil {
		t.Error("Subscription should survive removing api-key")
	}
	if cfg.Default != config.AuthSlotSubscription {
		t.Errorf("Default = %q, want %q after removing non-default slot", cfg.Default, config.AuthSlotSubscription)
	}
}

func TestConfigAuthRemove_RefusesDefault(t *testing.T) {
	setupAuthCLITest(t)
	mustNoErr(t, cmdConfigAuth([]string{"set", "subscription", "--token", "sk-ant-oat01-aaaaaaaa"}))
	mustNoErr(t, cmdConfigAuth([]string{"set", "api-key", "--key", "sk-ant-api03-bbbbbbbb"}))
	// subscription is default; remove must refuse.
	err := cmdConfigAuth([]string{"remove", "subscription"})
	if err == nil {
		t.Fatal("remove default slot = nil error, want refusal")
	}
	msg := err.Error()
	if !strings.Contains(msg, "default") {
		t.Errorf("error should mention default, got: %q", msg)
	}
	if !strings.Contains(msg, "spire config auth default api-key") {
		t.Errorf("error should suggest switching default to the other slot, got: %q", msg)
	}
	// Slot must still be configured after a refused remove.
	cfg, _ := config.ReadAuthConfig()
	if cfg.Subscription == nil {
		t.Error("Subscription was cleared despite remove refusal")
	}
}

func TestConfigAuthRemove_Unconfigured(t *testing.T) {
	setupAuthCLITest(t)
	// Nothing configured; removing any slot should error cleanly, not panic.
	if err := cmdConfigAuth([]string{"remove", "api-key"}); err == nil {
		t.Error("remove unconfigured slot = nil error, want error")
	}
}

func TestConfigAuthRemove_Usage(t *testing.T) {
	setupAuthCLITest(t)
	if err := cmdConfigAuth([]string{"remove"}); err == nil {
		t.Error("remove with no arg = nil error, want usage error")
	}
	if err := cmdConfigAuth([]string{"remove", "bogus"}); err == nil {
		t.Error("remove with unknown slot = nil error, want error")
	}
}

// TestCmdConfig_AuthDispatches verifies that `spire config auth …` routes
// through cmdConfig into cmdConfigAuth without losing auth flags to the
// --repo/--unmask scanner.
func TestCmdConfig_AuthDispatches(t *testing.T) {
	setupAuthCLITest(t)
	err := cmdConfig([]string{"auth", "set", "api-key", "--key", "sk-ant-api03-viadispatch"})
	if err != nil {
		t.Fatalf("cmdConfig auth set: %v", err)
	}
	cfg, _ := config.ReadAuthConfig()
	if cfg.APIKey == nil || cfg.APIKey.Secret != "sk-ant-api03-viadispatch" {
		t.Errorf("APIKey = %+v, want secret=sk-ant-api03-viadispatch", cfg.APIKey)
	}
}

// TestNoTopLevelAuthCmd guards the MUST NOT constraint: the namespace must
// live under `spire config`, never as a top-level `spire auth`.
func TestNoTopLevelAuthCmd(t *testing.T) {
	for _, c := range rootCmd.Commands() {
		if c.Name() == "auth" {
			t.Errorf("rootCmd has a top-level `auth` command; the namespace must live under `spire config`")
		}
	}
}

// mustNoErr fails the test if err is non-nil. Named to avoid colliding with
// tower.go's `must(string, error) string` helper.
func mustNoErr(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
}
