package pool

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	toml "github.com/pelletier/go-toml/v2"
)

// PoolNameSubscription and PoolNameAPIKey are the only legal values for
// Config.DefaultPool / Config.FallbackPool. They are the user-facing pool
// identifiers (kept hyphenated in api-key for continuity with the legacy
// auth surface in pkg/config), even though the on-disk TOML array section
// is `[[auth.api_key]]` (underscore — see config.go's struct tags).
const (
	PoolNameSubscription = "subscription"
	PoolNameAPIKey       = "api-key"
)

const (
	authConfigFilename        = "auth.toml"
	legacyCredentialsFilename = "credentials.toml"
	legacyDefaultSlotName     = "default"
)

// Path returns the canonical auth.toml path under towerDir.
func Path(towerDir string) string {
	return filepath.Join(towerDir, authConfigFilename)
}

// configTOML wraps Config under the [auth] table key so the on-disk TOML
// uses [[auth.subscription]] / [[auth.api_key]] arrays alongside the
// [auth] scalar fields (default_pool, fallback_pool, selection).
type configTOML struct {
	Auth Config `toml:"auth"`
}

// LoadConfig reads <towerDir>/auth.toml. If auth.toml is missing, it falls
// back to <towerDir>/credentials.toml and synthesizes a one-slot pool
// (named "default", MaxConcurrent=1) from whichever subscription/api-key
// entries the legacy file holds. Returns an error if both files are
// missing, if parsing fails, or if the resulting Config fails validation.
//
// The missing-both error wraps os.ErrNotExist so callers can distinguish
// "no config" from "malformed config" via errors.Is.
func LoadConfig(towerDir string) (*Config, error) {
	primary := Path(towerDir)
	data, err := os.ReadFile(primary)
	if err == nil {
		var raw configTOML
		if err := toml.Unmarshal(data, &raw); err != nil {
			return nil, fmt.Errorf("pool: parse %s: %w", primary, err)
		}
		cfg := raw.Auth
		if err := validate(&cfg); err != nil {
			return nil, fmt.Errorf("pool: validate %s: %w", primary, err)
		}
		return &cfg, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("pool: read %s: %w", primary, err)
	}

	legacy := filepath.Join(towerDir, legacyCredentialsFilename)
	cfg, err := promoteLegacy(primary, legacy)
	if err != nil {
		return nil, err
	}
	if err := validate(cfg); err != nil {
		return nil, fmt.Errorf("pool: validate (promoted from %s): %w", legacy, err)
	}
	return cfg, nil
}

// WriteConfig validates cfg and atomically writes it as TOML to
// <towerDir>/auth.toml with 0600 perms. The atomic write writes to a
// temp file in the same directory, fsyncs it, then renames over the
// target — concurrent readers see either the prior or the new contents,
// never a torn write. The directory is fsynced after rename so a crash
// between rename and dir-flush does not lose the file.
func WriteConfig(towerDir string, cfg *Config) error {
	if cfg == nil {
		return errors.New("pool: WriteConfig: nil Config")
	}
	if err := validate(cfg); err != nil {
		return fmt.Errorf("pool: WriteConfig validate: %w", err)
	}
	var buf bytes.Buffer
	enc := toml.NewEncoder(&buf)
	enc.SetIndentTables(true)
	if err := enc.Encode(configTOML{Auth: *cfg}); err != nil {
		return fmt.Errorf("pool: encode auth.toml: %w", err)
	}
	return atomicWrite(Path(towerDir), buf.Bytes(), 0o600)
}

// legacyAuthTOML mirrors the on-disk shape of the single-slot
// credentials.toml emitted by pkg/config/auth.go. Kept private to the
// loader — pkg/config remains the owner of the legacy file format. Note
// the legacy file uses [auth.api-key] (hyphen) for the slot section,
// distinct from the new auth.toml's [[auth.api_key]] (underscore).
type legacyAuthTOML struct {
	Auth legacyAuthSection `toml:"auth"`
}

type legacyAuthSection struct {
	Default      string            `toml:"default,omitempty"`
	Subscription *legacySubSlot    `toml:"subscription,omitempty"`
	APIKey       *legacyAPIKeySlot `toml:"api-key,omitempty"`
}

type legacySubSlot struct {
	Token string `toml:"token"`
}

type legacyAPIKeySlot struct {
	Key string `toml:"key"`
}

// promoteLegacy reads <towerDir>/credentials.toml and synthesizes a Config
// with one "default" slot per kind. authPath is the auth.toml path used
// only for the missing-both error message — it's the user-facing canonical
// location to point them at.
//
// When both subscription and api-key are populated in the legacy file,
// DefaultPool defaults to "subscription" (the historical primary). The
// legacy `default` field is honored when it names a populated pool — that
// way an operator who already moved their legacy default to api-key has
// it preserved across the promotion.
func promoteLegacy(authPath, legacyPath string) (*Config, error) {
	data, err := os.ReadFile(legacyPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("pool: no auth.toml or credentials.toml at %s: %w", authPath, os.ErrNotExist)
		}
		return nil, fmt.Errorf("pool: read legacy %s: %w", legacyPath, err)
	}
	var raw legacyAuthTOML
	if err := toml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("pool: parse legacy %s: %w", legacyPath, err)
	}
	cfg := &Config{}
	hasSub := raw.Auth.Subscription != nil && raw.Auth.Subscription.Token != ""
	hasKey := raw.Auth.APIKey != nil && raw.Auth.APIKey.Key != ""
	if hasSub {
		cfg.Subscription = []SlotConfig{{
			Name:          legacyDefaultSlotName,
			Token:         raw.Auth.Subscription.Token,
			MaxConcurrent: 1,
		}}
	}
	if hasKey {
		cfg.APIKey = []SlotConfig{{
			Name:          legacyDefaultSlotName,
			Key:           raw.Auth.APIKey.Key,
			MaxConcurrent: 1,
		}}
	}
	if !hasSub && !hasKey {
		return nil, fmt.Errorf("pool: legacy %s has no subscription token or api-key", legacyPath)
	}
	switch {
	case raw.Auth.Default == PoolNameSubscription && hasSub:
		cfg.DefaultPool = PoolNameSubscription
	case raw.Auth.Default == PoolNameAPIKey && hasKey:
		cfg.DefaultPool = PoolNameAPIKey
	case hasSub:
		cfg.DefaultPool = PoolNameSubscription
	default:
		cfg.DefaultPool = PoolNameAPIKey
	}
	return cfg, nil
}

// validate enforces the documented Config invariants:
//   - slot Names are non-empty and unique within each pool,
//   - MaxConcurrent >= 1 on every slot,
//   - DefaultPool/FallbackPool, when set, name a known pool
//     ("subscription" or "api-key") that has at least one slot.
func validate(cfg *Config) error {
	if cfg == nil {
		return errors.New("nil Config")
	}
	if err := validatePool(PoolNameSubscription, cfg.Subscription); err != nil {
		return err
	}
	if err := validatePool(PoolNameAPIKey, cfg.APIKey); err != nil {
		return err
	}
	if cfg.DefaultPool != "" {
		if err := validatePoolRef("default_pool", cfg.DefaultPool, cfg); err != nil {
			return err
		}
	}
	if cfg.FallbackPool != "" {
		if err := validatePoolRef("fallback_pool", cfg.FallbackPool, cfg); err != nil {
			return err
		}
	}
	return nil
}

func validatePool(name string, slots []SlotConfig) error {
	seen := make(map[string]bool, len(slots))
	for i, s := range slots {
		if s.Name == "" {
			return fmt.Errorf("%s pool: slot at index %d has empty name", name, i)
		}
		if seen[s.Name] {
			return fmt.Errorf("%s pool: duplicate slot name %q", name, s.Name)
		}
		seen[s.Name] = true
		if s.MaxConcurrent < 1 {
			return fmt.Errorf("%s pool: slot %q has max_concurrent=%d (must be >= 1)", name, s.Name, s.MaxConcurrent)
		}
	}
	return nil
}

func validatePoolRef(field, ref string, cfg *Config) error {
	switch ref {
	case PoolNameSubscription:
		if len(cfg.Subscription) == 0 {
			return fmt.Errorf("%s = %q but subscription pool has no slots", field, ref)
		}
	case PoolNameAPIKey:
		if len(cfg.APIKey) == 0 {
			return fmt.Errorf("%s = %q but api-key pool has no slots", field, ref)
		}
	default:
		return fmt.Errorf("%s = %q (must be %q or %q)", field, ref, PoolNameSubscription, PoolNameAPIKey)
	}
	return nil
}

// atomicWrite writes data to path via a same-directory temp file followed
// by rename. fsyncs the file before close and the directory after rename
// so a crash mid-write cannot leave a torn file on the disk. Cleans up
// the temp file on any error path.
func atomicWrite(path string, data []byte, mode os.FileMode) (retErr error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("pool: mkdir %s: %w", dir, err)
	}
	// CreateTemp opens with O_RDWR|O_CREATE|O_EXCL and mode 0600 — the
	// 0600 is set at open time, avoiding the umask race that a post-open
	// Chmod would have.
	tmp, err := os.CreateTemp(dir, ".auth.toml.*")
	if err != nil {
		return fmt.Errorf("pool: create temp in %s: %w", dir, err)
	}
	tmpPath := tmp.Name()
	defer func() {
		if retErr != nil {
			_ = os.Remove(tmpPath)
		}
	}()
	// CreateTemp gives 0600; only re-chmod when the caller asked for
	// something different (e.g. test override).
	if mode != 0o600 {
		if err := tmp.Chmod(mode); err != nil {
			_ = tmp.Close()
			return fmt.Errorf("pool: chmod %s: %w", tmpPath, err)
		}
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("pool: write %s: %w", tmpPath, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("pool: fsync %s: %w", tmpPath, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("pool: close %s: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("pool: rename %s -> %s: %w", tmpPath, path, err)
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
