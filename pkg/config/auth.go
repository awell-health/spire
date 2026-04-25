package config

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	toml "github.com/pelletier/go-toml/v2"
)

// Auth slot identifiers. Slots name which credential kind the AuthContext
// currently holds; the 429 auto-promote path swaps Active from subscription
// to api-key.
const (
	AuthSlotSubscription = "subscription"
	AuthSlotAPIKey       = "api-key"
)

// Env var names used by the Claude Code CLI / spawned subprocesses. The
// subscription env var is `CLAUDE_CODE_OAUTH_TOKEN` — the variable the
// `claude` CLI reads for Max/Team OAuth tokens, already established in
// the helm chart (`helm/spire/templates/secret.yaml`) and agent pod
// spec. The api-key env var is `ANTHROPIC_API_KEY`. These must stay
// aligned with what the downstream subprocess expects — do not invent
// new names.
const (
	EnvAnthropicAPIKey      = "ANTHROPIC_API_KEY"
	EnvClaudeCodeOAuthToken = "CLAUDE_CODE_OAUTH_TOKEN"
	EnvAnthropicAuthToken   = "ANTHROPIC_AUTH_TOKEN" // legacy alias recognized by migration only
)

// AuthCredential is one stored credential. Slot distinguishes kind
// ("subscription" or "api-key") so downstream code knows which env var
// to inject.
type AuthCredential struct {
	Slot   string
	Secret string
}

// AuthConfig is the in-memory shape of the auth credentials TOML file.
// AutoPromoteOn429 defaults to true — callers that build an AuthConfig
// from scratch (as opposed to reading from disk) should preserve that
// default; NewAuthConfig handles this for constructors.
type AuthConfig struct {
	Default          string
	AutoPromoteOn429 bool
	Subscription     *AuthCredential
	APIKey           *AuthCredential
}

// NewAuthConfig returns an AuthConfig with defaults applied
// (AutoPromoteOn429=true, no slots configured).
func NewAuthConfig() *AuthConfig {
	return &AuthConfig{AutoPromoteOn429: true}
}

// authConfigTOML is the on-disk TOML shape. Separate from AuthConfig so
// the public Go type can hold *AuthCredential pointers (nil = unconfigured)
// while the TOML file uses nested tables.
type authConfigTOML struct {
	Auth authSection `toml:"auth"`
}

type authSection struct {
	Default          string            `toml:"default,omitempty"`
	AutoPromoteOn429 *bool             `toml:"auto_promote_on_429,omitempty"`
	Subscription     *subscriptionSlot `toml:"subscription,omitempty"`
	APIKey           *apiKeySlot       `toml:"api-key,omitempty"`
}

type subscriptionSlot struct {
	Token string `toml:"token"`
}

type apiKeySlot struct {
	Key string `toml:"key"`
}

// AuthContext is the per-run selected auth state. Active is the credential
// the spawned subprocess should use. APIKey is the configured api-key
// credential (if any) retained alongside for the 429 auto-promote path.
// Ephemeral is set to true when the context was synthesized from a summon
// `-H` header override; the 429 handler must NOT swap ephemeral contexts
// (an inline override is an explicit, one-shot instruction from the caller).
type AuthContext struct {
	Active           *AuthCredential
	APIKey           *AuthCredential
	AutoPromoteOn429 bool
	Ephemeral        bool
}

// InjectEnv appends the Anthropic env var for the active slot to env and
// returns the new slice. For api-key it sets ANTHROPIC_API_KEY. For
// subscription it sets CLAUDE_CODE_OAUTH_TOKEN (the env var the `claude`
// CLI already reads for Max/Team OAuth). Returns env unchanged when
// Active is nil or Secret is empty.
func (c *AuthContext) InjectEnv(env []string) []string {
	if c == nil || c.Active == nil || c.Active.Secret == "" {
		return env
	}
	switch c.Active.Slot {
	case AuthSlotAPIKey:
		return append(env, EnvAnthropicAPIKey+"="+c.Active.Secret)
	case AuthSlotSubscription:
		return append(env, EnvClaudeCodeOAuthToken+"="+c.Active.Secret)
	default:
		return env
	}
}

// SwapToAPIKey switches Active to the retained APIKey credential. Errors
// if APIKey is nil (caller never configured one) or the context is
// ephemeral (inline `-H` credential — an explicit user instruction that
// must not be silently replaced by the 429 handler).
func (c *AuthContext) SwapToAPIKey() error {
	if c == nil {
		return errors.New("nil AuthContext")
	}
	if c.Ephemeral {
		return errors.New("cannot swap ephemeral auth context")
	}
	if c.APIKey == nil {
		return errors.New("api-key slot not configured")
	}
	c.Active = c.APIKey
	return nil
}

// SlotName returns the Active credential's Slot for logging/observability
// ("subscription" or "api-key"). Returns "" when Active is nil.
func (c *AuthContext) SlotName() string {
	if c == nil || c.Active == nil {
		return ""
	}
	return c.Active.Slot
}

// AuthConfigPath returns the path to the auth credentials TOML file.
// Lives alongside the legacy flat credentials file (CredentialsPath) in
// ~/.config/spire/. A separate file keeps TOML auth config and the
// flat key=value credentials file from colliding; migration moves
// auth-specific entries out of the flat file into this one.
func AuthConfigPath() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "credentials.toml"), nil
}

// ReadAuthConfig loads the auth config from AuthConfigPath. If the file
// is missing, returns an AuthConfig with AutoPromoteOn429=true and no
// slots configured. On first call it also lazily migrates any
// auth-related flat-format entries found in the legacy credentials file.
func ReadAuthConfig() (*AuthConfig, error) {
	// Run lazy migration first so a legacy flat file's auth entries get
	// promoted into the new TOML file before we read it.
	if err := MigrateFromFlatFormat(); err != nil {
		return nil, fmt.Errorf("migrate credentials: %w", err)
	}

	path, err := AuthConfigPath()
	if err != nil {
		return nil, err
	}
	return readAuthConfigFrom(path)
}

func readAuthConfigFrom(path string) (*AuthConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return NewAuthConfig(), nil
		}
		return nil, err
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return NewAuthConfig(), nil
	}

	var raw authConfigTOML
	if err := toml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}

	cfg := NewAuthConfig()
	cfg.Default = raw.Auth.Default
	if raw.Auth.AutoPromoteOn429 != nil {
		cfg.AutoPromoteOn429 = *raw.Auth.AutoPromoteOn429
	}
	if raw.Auth.Subscription != nil && raw.Auth.Subscription.Token != "" {
		cfg.Subscription = &AuthCredential{
			Slot:   AuthSlotSubscription,
			Secret: raw.Auth.Subscription.Token,
		}
	}
	if raw.Auth.APIKey != nil && raw.Auth.APIKey.Key != "" {
		cfg.APIKey = &AuthCredential{
			Slot:   AuthSlotAPIKey,
			Secret: raw.Auth.APIKey.Key,
		}
	}
	return cfg, nil
}

// WriteAuthConfig serializes cfg as TOML and writes to AuthConfigPath
// with 0600 file mode.
func WriteAuthConfig(cfg *AuthConfig) error {
	path, err := AuthConfigPath()
	if err != nil {
		return err
	}
	return writeAuthConfigTo(path, cfg)
}

func writeAuthConfigTo(path string, cfg *AuthConfig) error {
	if cfg == nil {
		return errors.New("nil AuthConfig")
	}

	var buf bytes.Buffer
	buf.WriteString("# Spire auth credentials — chmod 600, do not commit to version control\n")

	raw := authConfigTOML{
		Auth: authSection{
			Default: cfg.Default,
		},
	}
	// Always emit auto_promote_on_429 explicitly so the value survives round-trip.
	ap := cfg.AutoPromoteOn429
	raw.Auth.AutoPromoteOn429 = &ap
	if cfg.Subscription != nil && cfg.Subscription.Secret != "" {
		raw.Auth.Subscription = &subscriptionSlot{Token: cfg.Subscription.Secret}
	}
	if cfg.APIKey != nil && cfg.APIKey.Secret != "" {
		raw.Auth.APIKey = &apiKeySlot{Key: cfg.APIKey.Secret}
	}

	enc := toml.NewEncoder(&buf)
	enc.SetIndentTables(true)
	if err := enc.Encode(raw); err != nil {
		return fmt.Errorf("encode auth toml: %w", err)
	}

	return os.WriteFile(path, buf.Bytes(), 0600)
}

// MaskSecret returns a masked version of a secret suitable for display.
// Never returns empty for non-empty input — at minimum returns "****".
// For secrets long enough to reveal a prefix/suffix, shows the leading
// Anthropic-style prefix (e.g. "sk-ant-") and trailing characters.
func MaskSecret(s string) string {
	if s == "" {
		return ""
	}
	if len(s) < 12 {
		return "****"
	}
	prefix := s[:4]
	// If the secret looks like an Anthropic token ("sk-ant-..."), keep the
	// full "sk-ant-" prefix so users can tell subscription/api-key tokens
	// apart at a glance without revealing more of the secret.
	if strings.HasPrefix(s, "sk-ant-") && len(s) > 12 {
		prefix = s[:7]
	}
	suffix := s[len(s)-4:]
	return prefix + "…" + suffix
}

// MigrateFromFlatFormat moves auth-related entries out of the legacy
// flat credentials file (CredentialsPath) into the new TOML auth file
// (AuthConfigPath). Recognized flat keys:
//   - ANTHROPIC_API_KEY or "anthropic-key" → [auth.api-key].key
//   - ANTHROPIC_AUTH_TOKEN or CLAUDE_CODE_OAUTH_TOKEN → [auth.subscription].token
// Non-auth entries in the flat file are left untouched. Idempotent: if
// the flat file has no recognized auth keys (or doesn't exist), returns
// nil without changes.
func MigrateFromFlatFormat() error {
	flatPath, err := CredentialsPath()
	if err != nil {
		return err
	}
	tomlPath, err := AuthConfigPath()
	if err != nil {
		return err
	}
	return migrateFromFlatFormatAt(flatPath, tomlPath)
}

func migrateFromFlatFormatAt(flatPath, tomlPath string) error {
	data, err := os.ReadFile(flatPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	var apiKey, subToken string
	apiKeyKey, subTokenKey := "", ""

	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := scanner.Text()
		trim := strings.TrimSpace(line)
		if trim == "" || strings.HasPrefix(trim, "#") {
			continue
		}
		idx := strings.Index(line, "=")
		if idx < 0 {
			continue
		}
		key := strings.TrimSpace(line[:idx])
		val := line[idx+1:]
		switch key {
		case EnvAnthropicAPIKey, CredKeyAnthropicKey:
			if apiKey == "" {
				apiKey = val
				apiKeyKey = key
			}
		case EnvAnthropicAuthToken, EnvClaudeCodeOAuthToken:
			if subToken == "" {
				subToken = val
				subTokenKey = key
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}

	if apiKey == "" && subToken == "" {
		return nil
	}

	// Build and write the TOML file, merging with any existing TOML.
	existing, rerr := readAuthConfigFrom(tomlPath)
	if rerr != nil {
		return fmt.Errorf("read existing auth config: %w", rerr)
	}
	if existing == nil {
		existing = NewAuthConfig()
	}
	if apiKey != "" && existing.APIKey == nil {
		existing.APIKey = &AuthCredential{Slot: AuthSlotAPIKey, Secret: apiKey}
	}
	if subToken != "" && existing.Subscription == nil {
		existing.Subscription = &AuthCredential{Slot: AuthSlotSubscription, Secret: subToken}
	}
	if existing.Default == "" {
		switch {
		case existing.Subscription != nil:
			existing.Default = AuthSlotSubscription
		case existing.APIKey != nil:
			existing.Default = AuthSlotAPIKey
		}
	}
	if err := writeAuthConfigTo(tomlPath, existing); err != nil {
		return err
	}

	// Remove the migrated keys from the flat file, preserving everything else.
	// We drop blank lines adjacent to removed entries to keep the file tidy,
	// but comments and unrelated key=value lines are preserved verbatim.
	var outLines []string
	scanner = bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := scanner.Text()
		trim := strings.TrimSpace(line)
		if trim == "" || strings.HasPrefix(trim, "#") {
			outLines = append(outLines, line)
			continue
		}
		idx := strings.Index(line, "=")
		if idx < 0 {
			outLines = append(outLines, line)
			continue
		}
		key := strings.TrimSpace(line[:idx])
		if (key == apiKeyKey && apiKey != "") || (key == subTokenKey && subToken != "") {
			continue // drop migrated line
		}
		outLines = append(outLines, line)
	}

	out := strings.Join(outLines, "\n")
	if !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	return os.WriteFile(flatPath, []byte(out), 0600)
}
