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
}

// BundleStoreConfig mirrors pkg/bundlestore.Config as JSON-serializable
// tower state. Zero values are filled from bundlestore defaults at
// construction time; nothing here is required in a fresh tower config.
type BundleStoreConfig struct {
	// Backend selects the implementation. Currently only "local" ships.
	Backend string `json:"backend,omitempty"`
	// LocalRoot is the filesystem root for the local backend. Empty
	// falls back to $XDG_DATA_HOME/spire/bundles.
	LocalRoot string `json:"local_root,omitempty"`
	// MaxBytes caps individual bundle size. 0 means use the package
	// default (10 MiB).
	MaxBytes int64 `json:"max_bytes,omitempty"`
	// JanitorInterval is parsed with time.ParseDuration. Empty means
	// use the package default (5m).
	JanitorInterval string `json:"janitor_interval,omitempty"`
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

// LoadTowerConfig reads a tower config by name.
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

// ActiveTowerConfig finds the tower for the current context.
// If SPIRE_TOWER is set in the environment, it loads that tower directly —
// this ensures subprocess chains (wizard → apprentice) inherit explicit tower
// context instead of re-resolving from CWD.
// Otherwise, falls back to CWD-based resolution via Instance.Database matching.
func ActiveTowerConfig() (*TowerConfig, error) {
	// Fast path: explicit tower from environment (set by parent spawner).
	if name := os.Getenv("SPIRE_TOWER"); name != "" {
		return LoadTowerConfig(name)
	}

	cwd, err := RealCwd()
	if err != nil {
		return nil, err
	}
	cfg, err := Load()
	if err != nil {
		return nil, err
	}
	inst := FindInstanceByPath(cfg, cwd)
	if inst == nil {
		return nil, fmt.Errorf("no spire instance registered for %s", cwd)
	}

	towers, err := ListTowerConfigs()
	if err != nil {
		return nil, err
	}
	for i := range towers {
		if towers[i].Database == inst.Database || towers[i].Database == "beads_"+inst.Database {
			return &towers[i], nil
		}
	}
	return nil, fmt.Errorf("no tower config found for database %q", inst.Database)
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
// Handles tabular output from dolt sql -q by looking for data rows.
func ExtractSQLValue(output string) string {
	lines := strings.Split(strings.TrimSpace(output), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		// Skip header separators, empty lines, and column headers
		if line == "" || strings.HasPrefix(line, "+") || strings.HasPrefix(line, "|") && strings.Contains(line, "value") {
			continue
		}
		// Look for data row in pipe-delimited format: | value |
		if strings.HasPrefix(line, "|") {
			parts := strings.Split(line, "|")
			for _, p := range parts {
				p = strings.TrimSpace(p)
				if p != "" && p != "value" && p != "COUNT(*)" {
					return p
				}
			}
		}
	}
	// Fallback: return the last non-empty line
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line != "" && !strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "|") {
			return line
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
