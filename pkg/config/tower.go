package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Remote kinds for TowerConfig.RemoteKind. Empty defaults to RemoteKindDoltHub
// for backwards compatibility with towers created before this field existed.
const (
	RemoteKindDoltHub    = "dolthub"
	RemoteKindRemotesAPI = "remotesapi"
)

// Apprentice transport modes for ApprenticeConfig.Transport. Empty defaults to
// ApprenticeTransportBundle so new towers (and any config that forgets to set
// the field) go through the git-bundle artifact flow driven by
// pkg/bundlestore and pkg/apprentice.Submit. The legacy "push" transport is
// still honored when set explicitly.
const (
	ApprenticeTransportPush   = "push"
	ApprenticeTransportBundle = "bundle"
)

// Tower modes for TowerConfig.Mode. Empty defaults to TowerModeDirect so
// ~/.config/spire/towers/*.json files written before this field existed
// keep working as direct-Dolt towers. TowerModeGateway means the tower is
// reached through an HTTPS gateway (pkg/gatewayclient); in that mode URL
// and TokenRef carry the endpoint and keychain identifier for the bearer
// token and the Dolt host/port fields are unused.
const (
	TowerModeDirect  = "direct"
	TowerModeGateway = "gateway"
)

// TowerConfig represents a tower's identity and configuration.
type TowerConfig struct {
	Name          string                       `json:"name"`
	ProjectID     string                       `json:"project_id"`
	HubPrefix     string                       `json:"hub_prefix"`
	DolthubRemote string                       `json:"dolthub_remote,omitempty"`
	Database      string                       `json:"database"`
	CreatedAt     string                       `json:"created_at"`
	Archmage      ArchmageConfig               `json:"archmage,omitempty"`
	LocalBindings map[string]*LocalRepoBinding `json:"local_bindings,omitempty"`
	MaxConcurrent int                          `json:"max_concurrent,omitempty"` // max simultaneous wizards; 0 = unlimited
	Clusters      []ClusterAttachment          `json:"clusters,omitempty"`

	// RemoteKind selects the auth / transport convention for DolthubRemote.
	// Empty = "dolthub" (legacy). "remotesapi" means a self-hosted dolt-sql-server
	// reached over the remotesapi gRPC port (typically :50051), authed with
	// MySQL-style user+password stored in the keychain.
	RemoteKind string `json:"remote_kind,omitempty"`

	// RemoteUser is the remotesapi username (or DoltHub user) for convenience.
	// The password is never persisted here — it lives in the credentials file.
	RemoteUser string `json:"remote_user,omitempty"`

	// BundleStore configures the git-bundle artifact store used by the
	// apprentice submit / wizard fetch flow. See pkg/bundlestore.
	BundleStore BundleStoreConfig `json:"bundle_store,omitempty"`

	// Apprentice toggles apprentice-side behavior, notably the transport
	// mode apprentices use to deliver work back to the wizard.
	Apprentice ApprenticeConfig `json:"apprentice,omitempty"`

	// DeploymentMode names the control-plane topology for this tower
	// (local-native / cluster-native / attached-reserved). Empty resolves to
	// Default() via LoadTowerConfig and the EffectiveDeploymentMode accessor.
	// See pkg/config/deployment_mode.go for the semantics and the explicit
	// orthogonality to worker backend and sync transport.
	DeploymentMode DeploymentMode `toml:"deployment_mode" yaml:"deployment_mode" json:"deployment_mode,omitempty"`

	// Mode selects how the local CLI talks to this tower. Empty and
	// TowerModeDirect both mean direct Dolt (existing behavior).
	// TowerModeGateway means go through an HTTPS gateway — URL and
	// TokenRef below carry the endpoint and the keychain identifier
	// for the bearer token. See IsGateway / IsDirect for the guard
	// callers should use instead of comparing the string themselves.
	Mode string `json:"mode,omitempty"`

	// URL is the gateway base URL (e.g. https://spire.example.com).
	// Populated only when Mode == TowerModeGateway.
	URL string `json:"url,omitempty"`

	// TokenRef is the keychain identifier (typically the tower name)
	// used to look up the bearer token via config.GetTowerToken.
	// Populated only when Mode == TowerModeGateway. The token itself
	// is never persisted here — it lives in the OS keychain.
	TokenRef string `json:"token_ref,omitempty"`
}

// IsGateway reports whether the tower is attached through the HTTPS
// gateway (Mode == TowerModeGateway). Callers routing bead/message ops
// should use this rather than comparing the string themselves.
func (t TowerConfig) IsGateway() bool {
	return t.Mode == TowerModeGateway
}

// IsDirect reports whether the tower speaks raw Dolt. Empty Mode counts
// as direct so ~/.config/spire/towers/*.json files written before the
// Mode field existed keep working without a migration.
func (t TowerConfig) IsDirect() bool {
	return t.Mode == "" || t.Mode == TowerModeDirect
}

// EffectiveDeploymentMode returns the tower's control-plane topology, falling
// back to Default() when the field is unset. Callers MUST use this accessor
// rather than reading TowerConfig.DeploymentMode directly so legacy tower
// configs that predate the field behave as local-native. The method name
// follows the codebase pattern (see EffectiveTransport, EffectiveRemoteKind)
// and is required here because a method cannot share a name with the struct
// field it reads.
func (t TowerConfig) EffectiveDeploymentMode() DeploymentMode {
	if t.DeploymentMode == "" {
		return Default()
	}
	return t.DeploymentMode
}

// ApprenticeConfig controls apprentice-side behavior for a tower. Currently
// only the delivery transport is configurable. Zero value is valid and
// resolves to ApprenticeTransportBundle via EffectiveTransport.
type ApprenticeConfig struct {
	// Transport selects how apprentices deliver work back to the wizard.
	// Empty resolves to ApprenticeTransportBundle. See EffectiveTransport.
	Transport string `toml:"transport" yaml:"transport" json:"transport,omitempty"`
}

// EffectiveTransport returns the configured transport, defaulting to
// ApprenticeTransportBundle when unset. Callers should use this rather than
// reading Transport directly so configs that predate the field default to
// the bundle transport. Unknown values pass through unchanged — validation
// is not performed here.
func (c ApprenticeConfig) EffectiveTransport() string {
	if c.Transport == "" {
		return ApprenticeTransportBundle
	}
	return c.Transport
}

// BundleStoreConfig mirrors pkg/bundlestore.Config as JSON-serializable
// tower state. Zero values are filled from bundlestore defaults at
// construction time; nothing here is required in a fresh tower config.
type BundleStoreConfig struct {
	// Backend selects the implementation. Ships today: "local", "gcs".
	Backend string `json:"backend,omitempty"`
	// LocalRoot is the filesystem root for the local backend. Empty
	// falls back to $XDG_DATA_HOME/spire/bundles.
	LocalRoot string `json:"local_root,omitempty"`
	// GCS holds backend-specific settings for the gcs backend. Nested
	// (rather than flat) so future s3-backend config can be symmetrical.
	GCS BundleStoreGCSConfig `json:"gcs,omitempty"`
	// MaxBytes caps individual bundle size. 0 means use the package
	// default (10 MiB).
	MaxBytes int64 `json:"max_bytes,omitempty"`
	// JanitorInterval is parsed with time.ParseDuration. Empty means
	// use the package default (5m).
	JanitorInterval string `json:"janitor_interval,omitempty"`
}

// BundleStoreGCSConfig holds GCS-specific tower configuration for the
// gcs bundlestore backend. Authentication uses Application Default
// Credentials — no credential fields live here.
type BundleStoreGCSConfig struct {
	// Bucket is the pre-existing GCS bucket name. Required when Backend
	// is "gcs". The backend does NOT create the bucket.
	Bucket string `json:"bucket,omitempty"`
	// Prefix is an optional object-name prefix within the bucket.
	// Empty stores objects at the bucket root.
	Prefix string `json:"prefix,omitempty"`
}

// EffectiveRemoteKind returns the remote kind, defaulting to "dolthub" when
// the field is empty. All sync paths should use this rather than reading
// RemoteKind directly so legacy configs keep working.
func (t TowerConfig) EffectiveRemoteKind() string {
	if t.RemoteKind == "" {
		return RemoteKindDoltHub
	}
	return t.RemoteKind
}

// ClusterAttachment records a Kubernetes cluster a tower can dispatch work to.
// One entry per namespace — the namespace is the natural identifier because a
// single physical cluster may host several isolated Spire installs.
type ClusterAttachment struct {
	Namespace  string `json:"namespace"`
	Kubeconfig string `json:"kubeconfig,omitempty"` // path; empty means in-cluster
	Context    string `json:"context,omitempty"`    // kubeconfig context name; empty means current
	InCluster  bool   `json:"in_cluster,omitempty"`
}

// LocalRepoBinding records machine-local state for a shared tower repo.
// States: "unbound" (discovered, no local path yet), "bound" (has local path),
// "skipped" (user deferred), "unmanaged" (permanently excluded on this machine).
type LocalRepoBinding struct {
	Prefix       string    `json:"prefix"`
	LocalPath    string    `json:"local_path,omitempty"`
	State        string    `json:"state"` // unbound | bound | skipped | unmanaged
	RepoURL      string    `json:"repo_url,omitempty"`
	SharedBranch string    `json:"shared_branch,omitempty"`
	DiscoveredAt time.Time `json:"discovered_at,omitempty"`
	BoundAt      time.Time `json:"bound_at,omitempty"`
}

// ArchmageConfig stores the tower owner's identity.
// Used for merge commits so deployments and CI attribute to the right person.
type ArchmageConfig struct {
	Name  string `json:"name"`            // GitHub username (used by CI to validate workflows)
	Email string `json:"email,omitempty"` // Git commit email
}

// OLAPPath returns the path to this tower's DuckDB analytics database.
// The file lives under XDG_DATA_HOME/spire/<tower-slug>/analytics.db.
func (t TowerConfig) OLAPPath() string {
	slug := strings.ToLower(regexp.MustCompile(`[^a-z0-9]+`).ReplaceAllString(t.Name, "-"))
	slug = strings.Trim(slug, "-")
	if slug == "" {
		slug = "default"
	}
	base := os.Getenv("XDG_DATA_HOME")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(base, "spire", slug, "analytics.db")
}

// TowerConfigDir returns ~/.config/spire/towers/, creating it if needed.
func TowerConfigDir() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	td := filepath.Join(dir, "towers")
	if err := os.MkdirAll(td, 0755); err != nil {
		return "", err
	}
	return td, nil
}

// TowerConfigPath returns ~/.config/spire/towers/<name>.json.
func TowerConfigPath(name string) (string, error) {
	dir, err := TowerConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, name+".json"), nil
}

// LoadTowerConfig reads a tower config by name. When the persisted config
// omits deployment_mode (e.g. files written before the field existed), the
// loader fills it in with Default() so callers can read the field directly
// and still see the canonical default.
func LoadTowerConfig(name string) (*TowerConfig, error) {
	p, err := TowerConfigPath(name)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	var tc TowerConfig
	if err := json.Unmarshal(data, &tc); err != nil {
		return nil, fmt.Errorf("parse tower config %s: %w", p, err)
	}
	if tc.DeploymentMode == "" {
		tc.DeploymentMode = Default()
	}
	return &tc, nil
}

// SaveTowerConfig writes a tower config to disk.
func SaveTowerConfig(tower *TowerConfig) error {
	p, err := TowerConfigPath(tower.Name)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(tower, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, append(data, '\n'), 0644)
}

// ListTowerConfigs reads all tower configs from the towers directory.
func ListTowerConfigs() ([]TowerConfig, error) {
	dir, err := TowerConfigDir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var towers []TowerConfig
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var tc TowerConfig
		if err := json.Unmarshal(data, &tc); err != nil {
			continue
		}
		towers = append(towers, tc)
	}
	return towers, nil
}

// DeleteTowerConfig removes a tower's config file from disk.
// Returns nil if the file does not exist (idempotent).
func DeleteTowerConfig(name string) error {
	p, err := TowerConfigPath(name)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// ActiveTowerConfig finds the tower for the current context. This is a thin
// wrapper around ResolveTowerConfig — the single canonical resolver — so
// CLI-side callers and store-side dispatch see exactly the same precedence:
//
//  1. SPIRE_TOWER env var
//  2. cfg.ActiveTower (set by `spire tower use`)
//  3. CWD → registered instance → instance's tower
//  4. Sole tower on disk
//
// Before spi-43q7hp this helper had its own CWD-first resolution that
// silently outranked an explicitly selected gateway tower whenever the
// shell happened to sit inside a same-prefix direct local repo. It now
// shares the resolver used by store dispatch so no CLI/store path can
// fall back to direct local Dolt when the operator selected a gateway.
func ActiveTowerConfig() (*TowerConfig, error) {
	return ResolveTowerConfig()
}

// TowerConfigForDatabase finds the tower owning a given database name.
// Reuses the same matching logic as ActiveTowerConfig: exact match or beads_ prefix.
func TowerConfigForDatabase(database string) (*TowerConfig, error) {
	towers, err := ListTowerConfigs()
	if err != nil {
		return nil, err
	}
	for i := range towers {
		if towers[i].Database == database || towers[i].Database == "beads_"+database {
			return &towers[i], nil
		}
	}
	return nil, fmt.Errorf("no tower config found for database %q", database)
}

// ReadBeadsProjectID reads project_id from a .beads/metadata.json file.
// Used after bd init to adopt the identity that beads created.
func ReadBeadsProjectID(beadsDir string) (string, error) {
	metaPath := filepath.Join(beadsDir, "metadata.json")
	data, err := os.ReadFile(metaPath)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", metaPath, err)
	}
	var meta map[string]any
	if err := json.Unmarshal(data, &meta); err != nil {
		return "", fmt.Errorf("parse %s: %w", metaPath, err)
	}
	pid, _ := meta["project_id"].(string)
	if pid == "" {
		return "", fmt.Errorf("no project_id in %s", metaPath)
	}
	return pid, nil
}

// DerivePrefixFromName extracts the first 3 lowercase alphanumeric characters from a name.
func DerivePrefixFromName(name string) string {
	var prefix []byte
	for _, r := range strings.ToLower(name) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			prefix = append(prefix, byte(r))
			if len(prefix) == 3 {
				break
			}
		}
	}
	if len(prefix) == 0 {
		return "hub"
	}
	return string(prefix)
}

// ArchmageGitEnv returns environment variables that set BOTH the git author
// and committer identity to the archmage. This ensures all commits merged
// to main are attributed to the archmage on GitHub (which shows author).
//
// This intentionally does NOT handle GitHub push authentication. Cluster
// wizard pods configure push auth separately via
// git.ConfigureGitHubTokenAuth (called from CmdWizardRun) using the
// GITHUB_TOKEN env var wired through pod_builder. Local archmage flows
// rely on the user's existing git credential helper or SSH keys.
func ArchmageGitEnv(tower *TowerConfig) []string {
	env := os.Environ()
	if tower.Archmage.Name != "" {
		env = append(env, "GIT_AUTHOR_NAME="+tower.Archmage.Name)
		env = append(env, "GIT_COMMITTER_NAME="+tower.Archmage.Name)
	}
	if tower.Archmage.Email != "" {
		env = append(env, "GIT_AUTHOR_EMAIL="+tower.Archmage.Email)
		env = append(env, "GIT_COMMITTER_EMAIL="+tower.Archmage.Email)
	}
	return env
}

// NameFromDolthubURL extracts the repo name from a DoltHub URL or org/repo string.
func NameFromDolthubURL(input string) string {
	input = strings.TrimSpace(input)
	// Strip URL prefix if present
	input = strings.TrimPrefix(input, "https://doltremoteapi.dolthub.com/")
	input = strings.TrimPrefix(input, "https://www.dolthub.com/repositories/")
	input = strings.TrimPrefix(input, "http://")
	// Take the last path component
	parts := strings.Split(input, "/")
	if len(parts) >= 2 {
		return parts[len(parts)-1]
	}
	if len(parts) == 1 && parts[0] != "" {
		return parts[0]
	}
	return ""
}

// ExtractSQLValue extracts a single value from SQL output.
//
// Parses dolt's tabular output positionally, not by column name:
//
//	+-------+
//	| value |   ← header (name ignored)
//	+-------+
//	| abc   |   ← first data row, first non-empty cell returned
//	+-------+
//
// The parser is column-name-agnostic — any alias works (spi-69b6ge
// regression: `SELECT COUNT(*) AS cnt` produced header `| cnt |`
// which the previous allowlist-based parser mistook for a data value).
//
// Returns an empty string for empty input, empty result sets
// (header + separator but no data row), or plain-text dolt output
// that doesn't contain a table (e.g. `Query OK, 0 rows affected`).
// Callers (IsBlankDB, ReadMetadata) treat empty-string as the
// "no value" signal; this function intentionally never returns an
// error to keep the single-value contract narrow.
//
// A plain-text non-table input (no `+---+` separators) is returned
// verbatim — this preserves legacy behavior for callers that pipe
// raw strings through the function.
func ExtractSQLValue(output string) string {
	lines := strings.Split(strings.TrimSpace(output), "\n")

	// Walk the lines looking for the dolt table shape:
	//   rule → header → rule → data-row
	// Anything before the first rule (leading warnings/log lines
	// dolt sometimes emits) is skipped.
	for i := 0; i < len(lines); i++ {
		if !isTableRule(lines[i]) {
			continue
		}
		// lines[i] is the top rule; lines[i+1] is the header row
		// (content ignored — we parse positionally, not by name).
		// Then the next rule closes the header, and the first
		// `| … |` line after that is the data row.
		headerIdx := i + 1
		if headerIdx >= len(lines) {
			break
		}
		// Advance past the header-closing rule.
		closeIdx := headerIdx + 1
		for closeIdx < len(lines) && !isTableRule(lines[closeIdx]) {
			closeIdx++
		}
		if closeIdx >= len(lines) {
			break
		}
		// Scan for the first data row between the header-closing
		// rule and the table-closing rule. A rule here means the
		// result set is empty (e.g. `SELECT value WHERE …` with
		// no match) — return "" per the "no value" contract.
		for j := closeIdx + 1; j < len(lines); j++ {
			trimmed := strings.TrimSpace(lines[j])
			if isTableRule(trimmed) {
				return ""
			}
			if strings.HasPrefix(trimmed, "|") {
				return firstTableCell(trimmed)
			}
		}
		break
	}

	// Fallback: no table shape detected. Return the last non-empty,
	// non-table line so callers that pass plain dolt text (e.g.
	// a bare query result or a log line) still get something back.
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line != "" && !strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "|") {
			return line
		}
	}
	return ""
}

// isTableRule reports whether the line is a dolt table separator
// (e.g. `+-------+` or `+---+---+` for multi-column tables). Rules
// consist entirely of `+` and `-` characters and start with `+`.
func isTableRule(line string) bool {
	line = strings.TrimSpace(line)
	if len(line) < 2 || line[0] != '+' {
		return false
	}
	for _, r := range line {
		if r != '+' && r != '-' {
			return false
		}
	}
	return true
}

// firstTableCell returns the first non-empty cell from a dolt data
// row like `| a | b | c |`. Multi-column rows collapse to the first
// cell — ExtractSQLValue's contract is single-value; callers wanting
// more than one column should parse the output themselves.
func firstTableCell(row string) string {
	parts := strings.Split(row, "|")
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			return p
		}
	}
	return ""
}

// Must returns the value or empty string on error (for display purposes only).
func Must(s string, err error) string {
	if err != nil {
		return ""
	}
	return s
}
