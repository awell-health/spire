package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	bdpkg "github.com/awell-health/spire/pkg/bd"
)

// TowerConfig represents a tower's identity and configuration.
type TowerConfig struct {
	Name          string `json:"name"`
	ProjectID     string `json:"project_id"`
	HubPrefix     string `json:"hub_prefix"`
	DolthubRemote string `json:"dolthub_remote,omitempty"`
	Database      string `json:"database"`
	CreatedAt     string `json:"created_at"`
}

// towerConfigDir returns ~/.config/spire/towers/, creating it if needed.
func towerConfigDir() (string, error) {
	dir, err := configDir()
	if err != nil {
		return "", err
	}
	td := filepath.Join(dir, "towers")
	if err := os.MkdirAll(td, 0755); err != nil {
		return "", err
	}
	return td, nil
}

// towerConfigPath returns ~/.config/spire/towers/<name>.json.
func towerConfigPath(name string) (string, error) {
	dir, err := towerConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, name+".json"), nil
}

// loadTowerConfig reads a tower config by name.
func loadTowerConfig(name string) (*TowerConfig, error) {
	p, err := towerConfigPath(name)
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

// saveTowerConfig writes a tower config to disk.
func saveTowerConfig(tower *TowerConfig) error {
	p, err := towerConfigPath(tower.Name)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(tower, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, append(data, '\n'), 0644)
}

// listTowerConfigs reads all tower configs from the towers directory.
func listTowerConfigs() ([]TowerConfig, error) {
	dir, err := towerConfigDir()
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

// activeTowerConfig finds the tower for the current working directory
// by looking up the Instance.Database and matching it to a tower config.
func activeTowerConfig() (*TowerConfig, error) {
	cwd, err := realCwd()
	if err != nil {
		return nil, err
	}
	cfg, err := loadConfig()
	if err != nil {
		return nil, err
	}
	inst := findInstanceByPath(cfg, cwd)
	if inst == nil {
		return nil, fmt.Errorf("no spire instance registered for %s", cwd)
	}

	towers, err := listTowerConfigs()
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

// readBeadsProjectID reads project_id from a .beads/metadata.json file.
// Used after bd init to adopt the identity that beads created.
func readBeadsProjectID(beadsDir string) (string, error) {
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

// derivePrefixFromName extracts the first 3 lowercase alphanumeric characters from a name.
func derivePrefixFromName(name string) string {
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

const reposTableSQL = `CREATE TABLE IF NOT EXISTS repos (
    prefix       VARCHAR(16) PRIMARY KEY,
    repo_url     VARCHAR(512) NOT NULL,
    branch       VARCHAR(128) NOT NULL DEFAULT 'main',
    language     VARCHAR(32),
    registered_by VARCHAR(64),
    registered_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
)`

// cmdTower dispatches tower subcommands.
func cmdTower(args []string) error {
	if len(args) == 0 {
		return cmdTowerList()
	}
	switch args[0] {
	case "create":
		return cmdTowerCreate(args[1:])
	case "attach":
		return cmdTowerAttach(args[1:])
	case "list":
		return cmdTowerList()
	default:
		return fmt.Errorf("unknown tower subcommand: %q\nusage: spire tower <create|attach|list>", args[0])
	}
}

// cmdTowerCreate creates a new tower (dolt database + identity + repos table).
func cmdTowerCreate(args []string) error {
	// Parse flags
	var name, dolthub, prefix string
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--name" && i+1 < len(args):
			i++
			name = args[i]
		case strings.HasPrefix(args[i], "--name="):
			name = strings.TrimPrefix(args[i], "--name=")
		case args[i] == "--dolthub" && i+1 < len(args):
			i++
			dolthub = args[i]
		case strings.HasPrefix(args[i], "--dolthub="):
			dolthub = strings.TrimPrefix(args[i], "--dolthub=")
		case args[i] == "--prefix" && i+1 < len(args):
			i++
			prefix = args[i]
		case strings.HasPrefix(args[i], "--prefix="):
			prefix = strings.TrimPrefix(args[i], "--prefix=")
		default:
			return fmt.Errorf("unknown flag: %s\nusage: spire tower create --name <name> [--dolthub user/repo] [--prefix hub]", args[i])
		}
	}

	if name == "" {
		return fmt.Errorf("--name is required\nusage: spire tower create --name <name> [--dolthub user/repo] [--prefix hub]")
	}

	// Check if tower already exists
	if existing, err := loadTowerConfig(name); err == nil && existing != nil {
		return fmt.Errorf("tower %q already exists (config: %s)", name, must(towerConfigPath(name)))
	}

	// Ensure dolt binary
	fmt.Println("ensuring dolt binary...")
	if _, err := doltEnsureBinary(); err != nil {
		return fmt.Errorf("ensure dolt: %w", err)
	}

	// Ensure dolt server running
	if !doltIsReachable() {
		fmt.Println("starting dolt server...")
		if _, err := doltStart(); err != nil {
			return fmt.Errorf("start dolt: %w", err)
		}
	}

	if prefix == "" {
		prefix = derivePrefixFromName(name)
	}
	database := "beads_" + prefix

	// Initialize beads database in the dolt data directory
	// (not the user's CWD — tower create should not pollute the repo)
	dbDataDir := filepath.Join(doltDataDir(), database)
	os.MkdirAll(dbDataDir, 0755)

	fmt.Printf("initializing database %s...\n", database)
	client := bdpkg.NewClient()
	// Init creates .beads/ in cwd, so use RunDir for the init call
	client.RunDir = dbDataDir
	if err := client.Init(bdpkg.InitOpts{
		Database: database,
		Prefix:   prefix,
		Force:    true,
	}); err != nil {
		return fmt.Errorf("bd init: %w", err)
	}

	// Adopt the project_id that bd init created — Spire never invents identity
	beadsDir := filepath.Join(dbDataDir, ".beads")
	projectID, err := readBeadsProjectID(beadsDir)
	if err != nil {
		return fmt.Errorf("read tower identity after init: %w", err)
	}

	// Post-init: switch to BEADS_DIR for commands that read the config
	client.RunDir = ""
	client.BeadsDir = beadsDir

	tower := &TowerConfig{
		Name:      name,
		ProjectID: projectID,
		HubPrefix: prefix,
		Database:  database,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}

	// Create repos table
	fmt.Println("creating repos table...")
	if _, err := client.DoltSQL(reposTableSQL); err != nil {
		return fmt.Errorf("create repos table: %w", err)
	}

	// Commit
	if err := client.DoltCommit("tower: initialize " + tower.Name); err != nil {
		return fmt.Errorf("dolt commit: %w", err)
	}

	// DoltHub remote setup
	if dolthub != "" {
		remoteURL := normalizeDolthubURL(dolthub)
		tower.DolthubRemote = remoteURL

		fmt.Printf("pushing to %s...\n", remoteURL)

		// Set credentials for remote operations
		if user := getCredential(CredKeyDolthubUser); user != "" {
			os.Setenv("DOLT_REMOTE_USER", user)
		}
		if pass := getCredential(CredKeyDolthubPassword); pass != "" {
			os.Setenv("DOLT_REMOTE_PASSWORD", pass)
		}

		// Use CLI-based push (inherits caller's env credentials)
		// instead of bd dolt push (server-side CALL dolt_push() lacks creds)
		dataDir := filepath.Join(doltDataDir(), tower.Database)
		if err := ensureDoltHubDB(remoteURL); err != nil {
			fmt.Printf("  Note: could not pre-create remote db: %s\n", err)
		}
		setDoltCLIRemote(dataDir, "origin", remoteURL)
		if err := doltCLIPush(dataDir, false); err != nil {
			return fmt.Errorf("push to DoltHub: %w", err)
		}
	}

	// Save tower config
	if err := saveTowerConfig(tower); err != nil {
		return fmt.Errorf("save tower config: %w", err)
	}

	// Set as active tower in global config
	cfg, cfgErr := loadConfig()
	if cfgErr == nil {
		cfg.ActiveTower = tower.Name
		saveConfig(cfg)
	}

	// Print summary
	configPath := must(towerConfigPath(name))
	dolthubDisplay := "local only"
	if tower.DolthubRemote != "" {
		dolthubDisplay = tower.DolthubRemote
	}

	fmt.Printf("\nTower created: %s\n", tower.Name)
	fmt.Printf("  project_id: %s\n", tower.ProjectID)
	fmt.Printf("  prefix:     %s\n", tower.HubPrefix)
	fmt.Printf("  database:   %s\n", tower.Database)
	fmt.Printf("  dolthub:    %s\n", dolthubDisplay)
	fmt.Printf("  config:     %s\n", configPath)
	fmt.Printf("\nNext steps:\n")
	fmt.Printf("  cd ~/your-repo && spire register-repo --prefix=web\n")
	fmt.Printf("  spire up\n")

	return nil
}

// cmdTowerAttach clones a tower from DoltHub and creates a local config.
func cmdTowerAttach(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: spire tower attach <dolthub-url> [--name local-name]")
	}

	dolthubArg := args[0]
	var name string

	// Parse remaining flags
	for i := 1; i < len(args); i++ {
		switch {
		case args[i] == "--name" && i+1 < len(args):
			i++
			name = args[i]
		case strings.HasPrefix(args[i], "--name="):
			name = strings.TrimPrefix(args[i], "--name=")
		default:
			return fmt.Errorf("unknown flag: %s\nusage: spire tower attach <dolthub-url> [--name local-name]", args[i])
		}
	}

	// Normalize DoltHub URL
	remoteURL := normalizeDolthubURL(dolthubArg)

	// Derive name from URL if not provided
	if name == "" {
		name = nameFromDolthubURL(dolthubArg)
	}
	if name == "" {
		return fmt.Errorf("could not derive tower name from %q — use --name to specify", dolthubArg)
	}

	// Database name from URL
	dbName := nameFromDolthubURL(dolthubArg)
	if dbName == "" {
		dbName = name
	}

	// Check if tower already exists
	if existing, err := loadTowerConfig(name); err == nil && existing != nil {
		return fmt.Errorf("tower %q already exists (config: %s)", name, must(towerConfigPath(name)))
	}

	// Ensure dolt binary
	fmt.Println("ensuring dolt binary...")
	if _, err := doltEnsureBinary(); err != nil {
		return fmt.Errorf("ensure dolt: %w", err)
	}

	// Ensure dolt server running
	if !doltIsReachable() {
		fmt.Println("starting dolt server...")
		if _, err := doltStart(); err != nil {
			return fmt.Errorf("start dolt: %w", err)
		}
	}

	// Set credentials for remote operations
	if user := getCredential(CredKeyDolthubUser); user != "" {
		os.Setenv("DOLT_REMOTE_USER", user)
	}
	if pass := getCredential(CredKeyDolthubPassword); pass != "" {
		os.Setenv("DOLT_REMOTE_PASSWORD", pass)
	}

	// Clone from DoltHub using dolt CLI directly in the data directory
	fmt.Printf("cloning %s...\n", remoteURL)
	dataDir := doltDataDir()
	cloneCmd := exec.Command(doltBin(), "clone", remoteURL, dbName)
	cloneCmd.Dir = dataDir
	cloneCmd.Env = os.Environ()
	if out, err := cloneCmd.CombinedOutput(); err != nil {
		outStr := strings.TrimSpace(string(out))
		// If database already exists, try pull instead
		if strings.Contains(outStr, "already exists") || strings.Contains(outStr, "directory already exists") {
			fmt.Println("  database already exists, pulling latest...")
			client := bdpkg.DefaultClient()
			if pullErr := client.DoltPull("origin", "main"); pullErr != nil {
				return fmt.Errorf("pull from DoltHub: %w (clone error: %s)", pullErr, outStr)
			}
		} else {
			return fmt.Errorf("clone from DoltHub: %s\n%s", err, outStr)
		}
	}

	// Read tower identity from cloned database
	fmt.Println("reading tower identity...")
	client := bdpkg.DefaultClient()

	var projectID, hubPrefix string

	// Try to read project_id from metadata
	metaOut, err := client.DoltSQL(fmt.Sprintf("SELECT `value` FROM `%s`.metadata WHERE `key` = '_project_id'", dbName))
	if err == nil {
		projectID = extractSQLValue(metaOut)
	}
	if projectID == "" {
		return fmt.Errorf("no project_id found in tower database — was it created with 'spire tower create'?")
	}

	// Try to read prefix from config
	prefixOut, err := client.DoltSQL(fmt.Sprintf("SELECT `value` FROM `%s`.metadata WHERE `key` = 'prefix'", dbName))
	if err == nil {
		hubPrefix = extractSQLValue(prefixOut)
	}
	if hubPrefix == "" {
		// Derive from database name
		hubPrefix = derivePrefixFromName(dbName)
	}

	// Get bead count for display
	countOut, _ := client.DoltSQL(fmt.Sprintf("SELECT COUNT(*) FROM `%s`.issues", dbName))
	beadCount := extractSQLValue(countOut)
	if beadCount == "" {
		beadCount = "0"
	}

	tower := &TowerConfig{
		Name:          name,
		ProjectID:     projectID,
		HubPrefix:     hubPrefix,
		DolthubRemote: remoteURL,
		Database:      dbName,
		CreatedAt:     time.Now().UTC().Format(time.RFC3339),
	}

	// Save tower config
	if err := saveTowerConfig(tower); err != nil {
		return fmt.Errorf("save tower config: %w", err)
	}

	// Set as active tower in global config
	cfg, cfgErr := loadConfig()
	if cfgErr == nil {
		cfg.ActiveTower = tower.Name
		saveConfig(cfg)
	}

	// Print summary
	configPath := must(towerConfigPath(name))
	fmt.Printf("\nTower attached: %s\n", tower.Name)
	fmt.Printf("  project_id: %s\n", tower.ProjectID)
	fmt.Printf("  prefix:     %s\n", tower.HubPrefix)
	fmt.Printf("  database:   %s\n", tower.Database)
	fmt.Printf("  remote:     %s\n", tower.DolthubRemote)
	fmt.Printf("  beads:      %s\n", beadCount)
	fmt.Printf("  config:     %s\n", configPath)

	return nil
}

// cmdTowerList lists all configured towers.
func cmdTowerList() error {
	towers, err := listTowerConfigs()
	if err != nil {
		return fmt.Errorf("list towers: %w", err)
	}

	if len(towers) == 0 {
		fmt.Println("No towers configured.")
		fmt.Println("  Create one: spire tower create --name my-team")
		fmt.Println("  Or attach:  spire tower attach org/repo")
		return nil
	}

	fmt.Printf("%-16s %-8s %-20s %s\n", "NAME", "PREFIX", "DATABASE", "REMOTE")
	fmt.Printf("%-16s %-8s %-20s %s\n", "----", "------", "--------", "------")
	for _, t := range towers {
		remote := "local"
		if t.DolthubRemote != "" {
			remote = t.DolthubRemote
		}
		fmt.Printf("%-16s %-8s %-20s %s\n", t.Name, t.HubPrefix, t.Database, remote)
	}
	return nil
}

// nameFromDolthubURL extracts the repo name from a DoltHub URL or org/repo string.
func nameFromDolthubURL(input string) string {
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

// extractSQLValue extracts a single value from SQL output.
// Handles tabular output from dolt sql -q by looking for data rows.
func extractSQLValue(output string) string {
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

// must returns the value or empty string on error (for display purposes only).
func must(s string, err error) string {
	if err != nil {
		return ""
	}
	return s
}
