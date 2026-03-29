package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	bdpkg "github.com/awell-health/spire/pkg/bd"
	"github.com/awell-health/spire/pkg/config"
	"github.com/awell-health/spire/pkg/dolt"
	"github.com/spf13/cobra"
)

// --- Type aliases so existing cmd/spire code compiles unchanged ---

type TowerConfig = config.TowerConfig
type ArchmageConfig = config.ArchmageConfig

// --- Wrappers delegating to pkg/config ---

func towerConfigDir() (string, error) {
	return config.TowerConfigDir()
}

func towerConfigPath(name string) (string, error) {
	return config.TowerConfigPath(name)
}

func loadTowerConfig(name string) (*TowerConfig, error) {
	return config.LoadTowerConfig(name)
}

func saveTowerConfig(tower *TowerConfig) error {
	return config.SaveTowerConfig(tower)
}

func listTowerConfigs() ([]TowerConfig, error) {
	return config.ListTowerConfigs()
}

func activeTowerConfig() (*TowerConfig, error) {
	return config.ActiveTowerConfig()
}

func towerConfigForDatabase(database string) (*TowerConfig, error) {
	return config.TowerConfigForDatabase(database)
}

func readBeadsProjectID(beadsDir string) (string, error) {
	return config.ReadBeadsProjectID(beadsDir)
}

func derivePrefixFromName(name string) string {
	return config.DerivePrefixFromName(name)
}

func archmageGitEnv(tower *TowerConfig) []string {
	return config.ArchmageGitEnv(tower)
}

func nameFromDolthubURL(input string) string {
	return config.NameFromDolthubURL(input)
}

func extractSQLValue(output string) string {
	return config.ExtractSQLValue(output)
}

func must(s string, err error) string {
	return config.Must(s, err)
}

// --- Functions that remain in cmd/spire (depend on dolt/bd/git/IO) ---

// rawDoltQuery runs a SQL query against the dolt server without --use-db.
// Delegates to pkg/dolt.RawQuery.
func rawDoltQuery(query string) (string, error) {
	return dolt.RawQuery(query)
}

// promptArchmageIdentity prompts for the tower owner's identity.
// This is used for merge commits so CI/CD and deployment platforms
// attribute the merge to the right person.
func promptArchmageIdentity() ArchmageConfig {
	reader := bufio.NewReader(os.Stdin)

	fmt.Println("\nSpire needs your identity for merge commits.")
	fmt.Println("Use your GitHub username — CI workflows and deployment platforms")
	fmt.Println("use this to validate whether workflows should run.")
	fmt.Print("\nGitHub username: ")
	name, _ := reader.ReadString('\n')
	name = strings.TrimSpace(name)

	fmt.Print("Git email: ")
	email, _ := reader.ReadString('\n')
	email = strings.TrimSpace(email)

	if name == "" {
		name = gitConfigGet("user.name")
	}
	if email == "" {
		email = gitConfigGet("user.email")
	}

	return ArchmageConfig{Name: name, Email: email}
}

const reposTableSQL = `CREATE TABLE IF NOT EXISTS repos (
    prefix       VARCHAR(16) PRIMARY KEY,
    repo_url     VARCHAR(512) NOT NULL,
    branch       VARCHAR(128) NOT NULL DEFAULT 'main',
    language     VARCHAR(32),
    registered_by VARCHAR(64),
    registered_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
)`

const agentRunsTableSQL = `CREATE TABLE IF NOT EXISTS agent_runs (
    id VARCHAR(32) PRIMARY KEY,
    bead_id VARCHAR(64) NOT NULL,
    epic_id VARCHAR(64),
    agent_name VARCHAR(128),
    model VARCHAR(64) NOT NULL,
    role VARCHAR(16) NOT NULL,
    context_tokens_in INT,
    context_tokens_out INT,
    total_tokens INT,
    turns INT,
    duration_seconds INT,
    startup_seconds INT,
    working_seconds INT,
    queue_seconds INT,
    review_seconds INT,
    result VARCHAR(32) NOT NULL,
    review_rounds INT DEFAULT 0,
    artificer_verdict VARCHAR(32),
    spec_file VARCHAR(256),
    spec_size_tokens INT,
    focus_context_tokens INT,
    files_changed INT,
    lines_added INT,
    lines_removed INT,
    tests_added INT,
    tests_passed BOOLEAN,
    system_prompt_hash VARCHAR(64),
    golden_run BOOLEAN DEFAULT FALSE,
    started_at DATETIME NOT NULL,
    completed_at DATETIME,
    INDEX idx_bead (bead_id),
    INDEX idx_epic (epic_id),
    INDEX idx_result (result),
    INDEX idx_golden (golden_run),
    INDEX idx_model (model)
)`

const goldenPromptsTableSQL = `CREATE TABLE IF NOT EXISTS golden_prompts (
    run_id VARCHAR(32) PRIMARY KEY,
    bead_id VARCHAR(64) NOT NULL,
    system_prompt TEXT,
    spec_excerpt TEXT,
    focus_context TEXT,
    tags JSON,
    context_tokens INT,
    CONSTRAINT fk_run FOREIGN KEY (run_id) REFERENCES agent_runs(id)
)`

// requiredCustomTypes are the bead types that Spire registers on every tower.
// These supplement bd's built-in types (task, bug, feature, epic, chore).
var requiredCustomTypes = []string{"design"}

// ensureBootstrapCustomTypesFn exists so bootstrap helpers can be tested
// without shelling out to the real bd binary.
var ensureBootstrapCustomTypesFn = ensureCustomBeadTypes

// bootstrapTowerBeadsDir writes the minimum .beads workspace needed for a
// tower-backed store and ensures Spire's required custom bead types exist.
func bootstrapTowerBeadsDir(beadsDir string, tower *TowerConfig) error {
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		return fmt.Errorf("create .beads/: %w", err)
	}

	beadsMeta := map[string]any{
		"database":      "dolt",
		"backend":       "dolt",
		"dolt_mode":     "server",
		"dolt_database": tower.Database,
	}
	if tower.ProjectID != "" {
		beadsMeta["project_id"] = tower.ProjectID
	}
	metaBytes, _ := json.MarshalIndent(beadsMeta, "", "  ")
	metaPath := filepath.Join(beadsDir, "metadata.json")
	if err := os.WriteFile(metaPath, append(metaBytes, '\n'), 0644); err != nil {
		return fmt.Errorf("write .beads/metadata.json: %w", err)
	}

	configYAML := fmt.Sprintf("dolt.host: %q\ndolt.port: %s\n", doltHost(), doltPort())
	configPath := filepath.Join(beadsDir, "config.yaml")
	if err := os.WriteFile(configPath, []byte(configYAML), 0644); err != nil {
		return fmt.Errorf("write .beads/config.yaml: %w", err)
	}

	if err := ensureBootstrapCustomTypesFn(beadsDir); err != nil {
		return fmt.Errorf("register custom bead types: %w", err)
	}

	return nil
}

// bootstrapRepoBeadsDir writes repo-local beads config and routes, then ensures
// the shared tower custom bead types are available immediately.
func bootstrapRepoBeadsDir(beadsDir string, tower *TowerConfig, prefix string) error {
	if err := bootstrapTowerBeadsDir(beadsDir, tower); err != nil {
		return err
	}

	routesContent := fmt.Sprintf("{\"prefix\":\"%s-\",\"path\":\".\"}\n", prefix)
	routesPath := filepath.Join(beadsDir, "routes.jsonl")
	if err := os.WriteFile(routesPath, []byte(routesContent), 0644); err != nil {
		return fmt.Errorf("write routes.jsonl: %w", err)
	}

	gitignorePath := filepath.Join(beadsDir, ".gitignore")
	if _, err := os.Stat(gitignorePath); os.IsNotExist(err) {
		gitignoreContent := "metadata.json\nconfig.yaml\nroutes.jsonl\n"
		if writeErr := os.WriteFile(gitignorePath, []byte(gitignoreContent), 0644); writeErr != nil {
			return fmt.Errorf("write .beads/.gitignore: %w", writeErr)
		}
	}

	return nil
}

// ensureCustomBeadTypes registers Spire's required custom bead types in the
// given .beads directory. Idempotent — merges with any existing custom types.
func ensureCustomBeadTypes(beadsDir string) error {
	client := bdpkg.NewClient()
	client.BeadsDir = beadsDir

	// Read current custom types to avoid clobbering user additions.
	current, err := client.ConfigGet("types.custom")
	if err != nil {
		// Key may not exist yet — treat as empty.
		current = ""
	}

	// Build set of existing custom types.
	existing := make(map[string]bool)
	for _, t := range strings.Split(current, ",") {
		t = strings.TrimSpace(t)
		if t != "" {
			existing[t] = true
		}
	}

	// Add any missing required types.
	changed := false
	for _, t := range requiredCustomTypes {
		if !existing[t] {
			existing[t] = true
			changed = true
		}
	}

	if !changed {
		return nil
	}

	var types []string
	for t := range existing {
		types = append(types, t)
	}
	sort.Strings(types)

	return client.ConfigSet("types.custom", strings.Join(types, ","))
}

var towerCmd = &cobra.Command{
	Use:   "tower",
	Short: "Manage towers",
	RunE: func(cmd *cobra.Command, args []string) error {
		return cmdTowerList()
	},
}

var towerCreateCmd = &cobra.Command{
	Use:   "create",
	Short: "Create a new tower",
	RunE: func(cmd *cobra.Command, args []string) error {
		var fullArgs []string
		if v, _ := cmd.Flags().GetString("name"); v != "" {
			fullArgs = append(fullArgs, "--name", v)
		}
		if v, _ := cmd.Flags().GetString("dolthub"); v != "" {
			fullArgs = append(fullArgs, "--dolthub", v)
		}
		if v, _ := cmd.Flags().GetString("prefix"); v != "" {
			fullArgs = append(fullArgs, "--prefix", v)
		}
		return cmdTowerCreate(fullArgs)
	},
}

var towerAttachCmd = &cobra.Command{
	Use:   "attach <dolthub-url>",
	Short: "Clone a tower from DoltHub",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		fullArgs := []string{args[0]}
		if v, _ := cmd.Flags().GetString("name"); v != "" {
			fullArgs = append(fullArgs, "--name", v)
		}
		return cmdTowerAttach(fullArgs)
	},
}

var towerListCmd = &cobra.Command{
	Use:   "list",
	Short: "List configured towers",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return cmdTowerList()
	},
}

var towerUseCmd = &cobra.Command{
	Use:   "use <name>",
	Short: "Set the active tower",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return cmdTowerUse(args[0])
	},
}

func init() {
	towerCreateCmd.Flags().String("name", "", "Tower name (required)")
	towerCreateCmd.Flags().String("dolthub", "", "DoltHub remote (user/repo)")
	towerCreateCmd.Flags().String("prefix", "", "Hub prefix")

	towerAttachCmd.Flags().String("name", "", "Local name override")

	towerCmd.AddCommand(towerCreateCmd, towerAttachCmd, towerListCmd, towerUseCmd)
}

// cmdTower dispatches tower subcommands (kept for backward compat with tests).
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
	case "use":
		if len(args) < 2 {
			return fmt.Errorf("usage: spire tower use <name>")
		}
		return cmdTowerUse(args[1])
	default:
		return fmt.Errorf("unknown tower subcommand: %q\nusage: spire tower <create|attach|list|use>", args[0])
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

	// Pre-create database on the server so bd init can connect to it.
	// bd init tries to USE the database it's creating — chicken-and-egg without this.
	// The directory must NOT exist yet — dolt CREATE DATABASE creates it.
	dbDataDir := filepath.Join(doltDataDir(), database)
	fmt.Printf("initializing database %s...\n", database)
	if _, err := rawDoltQuery(fmt.Sprintf("CREATE DATABASE IF NOT EXISTS `%s`", database)); err != nil {
		return fmt.Errorf("create database: %w", err)
	}

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

	// bd init writes a default config.yaml. Overwrite with dolt server connection
	// so subsequent bd commands can connect. The database name lives in metadata.json
	// (written by bd init --database), not config.yaml.
	configYAML := fmt.Sprintf("dolt.host: %q\ndolt.port: %s\n", doltHost(), doltPort())
	if err := os.WriteFile(filepath.Join(beadsDir, "config.yaml"), []byte(configYAML), 0644); err != nil {
		return fmt.Errorf("write .beads/config.yaml: %w", err)
	}

	// Register required custom bead types (e.g. "design").
	fmt.Println("registering custom bead types...")
	if err := ensureCustomBeadTypes(beadsDir); err != nil {
		fmt.Printf("  warning: could not register custom types: %s\n", err)
	}

	// Prompt for archmage identity — used for merge commits and CI validation.
	archmage := promptArchmageIdentity()

	tower := &TowerConfig{
		Name:      name,
		ProjectID: projectID,
		HubPrefix: prefix,
		Database:  database,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
		Archmage:  archmage,
	}

	// Create repos table — use rawDoltQuery (bd dolt sql doesn't exist in bd 0.62)
	fmt.Println("creating repos table...")
	if _, err := rawDoltQuery(fmt.Sprintf("USE `%s`; %s", database, reposTableSQL)); err != nil {
		return fmt.Errorf("create repos table: %w", err)
	}

	// Create agent_runs + golden_prompts tables for metrics pipeline
	fmt.Println("creating agent_runs tables...")
	if _, err := rawDoltQuery(fmt.Sprintf("USE `%s`; %s", database, agentRunsTableSQL)); err != nil {
		return fmt.Errorf("create agent_runs table: %w", err)
	}
	if _, err := rawDoltQuery(fmt.Sprintf("USE `%s`; %s", database, goldenPromptsTableSQL)); err != nil {
		return fmt.Errorf("create golden_prompts table: %w", err)
	}

	// Commit via dolt server stored procedures
	if _, err := rawDoltQuery(fmt.Sprintf("USE `%s`; CALL DOLT_ADD('-A'); CALL DOLT_COMMIT('-m', 'tower: initialize %s')", database, sqlEscape(tower.Name))); err != nil {
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
	configPathStr := must(towerConfigPath(name))
	dolthubDisplay := "local only"
	if tower.DolthubRemote != "" {
		dolthubDisplay = tower.DolthubRemote
	}

	fmt.Printf("\nTower created: %s\n", tower.Name)
	fmt.Printf("  project_id: %s\n", tower.ProjectID)
	fmt.Printf("  prefix:     %s\n", tower.HubPrefix)
	fmt.Printf("  database:   %s\n", tower.Database)
	fmt.Printf("  dolthub:    %s\n", dolthubDisplay)
	fmt.Printf("  config:     %s\n", configPathStr)
	fmt.Printf("\nNext steps:\n")
	fmt.Printf("  cd ~/your-repo && spire repo add --prefix=web\n")
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
			pullCmd := exec.Command(doltBin(), "pull", "origin", "main")
			pullCmd.Dir = filepath.Join(dataDir, dbName)
			pullCmd.Env = os.Environ()
			if pullOut, pullErr := pullCmd.CombinedOutput(); pullErr != nil {
				return fmt.Errorf("pull from DoltHub: %w (clone error: %s)\n%s", pullErr, outStr, strings.TrimSpace(string(pullOut)))
			}
		} else {
			return fmt.Errorf("clone from DoltHub: %s\n%s", err, outStr)
		}
	}

	// Read tower identity from cloned database using raw dolt CLI.
	// No --use-db: on a clean machine DetectDBName() would fail since no
	// tower is configured yet. Fully-qualified queries against dbName instead.
	fmt.Println("reading tower identity...")

	var projectID, hubPrefix string

	// Try to read project_id from metadata
	metaOut, err := rawDoltQuery(fmt.Sprintf("SELECT `value` FROM `%s`.metadata WHERE `key` = '_project_id'", dbName))
	if err == nil {
		projectID = extractSQLValue(metaOut)
	}
	if projectID == "" {
		return fmt.Errorf("no project_id found in tower database — was it created with 'spire tower create'?")
	}

	// Try to read prefix from config
	prefixOut, err := rawDoltQuery(fmt.Sprintf("SELECT `value` FROM `%s`.metadata WHERE `key` = 'prefix'", dbName))
	if err == nil {
		hubPrefix = extractSQLValue(prefixOut)
	}
	if hubPrefix == "" {
		// Derive from database name
		hubPrefix = derivePrefixFromName(dbName)
	}

	// Get bead count for display
	countOut, _ := rawDoltQuery(fmt.Sprintf("SELECT COUNT(*) FROM `%s`.issues", dbName))
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

	// Materialize the tower's .beads workspace in the cloned data dir.
	beadsDir := filepath.Join(dataDir, dbName, ".beads")
	if err := bootstrapTowerBeadsDir(beadsDir, tower); err != nil {
		return err
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
	configPathStr := must(towerConfigPath(name))
	fmt.Printf("\nTower attached: %s\n", tower.Name)
	fmt.Printf("  project_id: %s\n", tower.ProjectID)
	fmt.Printf("  prefix:     %s\n", tower.HubPrefix)
	fmt.Printf("  database:   %s\n", tower.Database)
	fmt.Printf("  remote:     %s\n", tower.DolthubRemote)
	fmt.Printf("  beads:      %s\n", beadCount)
	fmt.Printf("  config:     %s\n", configPathStr)

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

	cfg, _ := loadConfig()
	activeTower := ""
	if cfg != nil {
		activeTower = cfg.ActiveTower
	}

	// Also resolve CWD tower for display.
	cwdTower := ""
	if tc, err := activeTowerConfig(); err == nil {
		cwdTower = tc.Name
	}

	fmt.Printf("  %-16s %-8s %-20s %s\n", "NAME", "PREFIX", "DATABASE", "REMOTE")
	fmt.Printf("  %-16s %-8s %-20s %s\n", "----", "------", "--------", "------")
	for _, t := range towers {
		remote := "local"
		if t.DolthubRemote != "" {
			remote = t.DolthubRemote
		}
		marker := " "
		if t.Name == activeTower && t.Name == cwdTower {
			marker = "*" // both active and CWD
		} else if t.Name == cwdTower {
			marker = ">" // CWD-resolved (not the global default)
		} else if t.Name == activeTower {
			marker = "~" // global default (not CWD)
		}
		fmt.Printf("%s %-16s %-8s %-20s %s\n", marker, t.Name, t.HubPrefix, t.Database, remote)
	}

	fmt.Println()
	if cwdTower != "" && cwdTower != activeTower {
		fmt.Printf("  > = current directory    ~ = global default\n")
	} else {
		fmt.Printf("  * = active tower\n")
	}
	return nil
}

// cmdTowerUse sets the active tower.
func cmdTowerUse(name string) error {
	// Verify the tower config exists
	if _, err := loadTowerConfig(name); err != nil {
		return fmt.Errorf("tower %q not found — run 'spire tower list' to see available towers", name)
	}

	cfg, err := loadConfig()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	// Warn about running wizards for the old tower.
	if cfg.ActiveTower != "" && cfg.ActiveTower != name {
		reg := loadWizardRegistry()
		reg = cleanDeadWizards(reg)
		var running []localWizard
		for _, w := range reg.Wizards {
			// Check if wizard's bead prefix matches old tower's instances.
			for _, inst := range cfg.Instances {
				if inst.Tower == cfg.ActiveTower {
					prefix := inst.Prefix + "-"
					if strings.HasPrefix(w.BeadID, prefix) {
						running = append(running, w)
						break
					}
				}
			}
		}
		if len(running) > 0 {
			fmt.Printf("Warning: %d wizard(s) running for tower %q:\n", len(running), cfg.ActiveTower)
			for _, w := range running {
				fmt.Printf("  %s (pid %d) working on %s\n", w.Name, w.PID, w.BeadID)
			}
			fmt.Println("These will continue using the old tower until they complete.")
			fmt.Println()
		}
	}

	cfg.ActiveTower = name
	if err := saveConfig(cfg); err != nil {
		return fmt.Errorf("save config: %w", err)
	}

	fmt.Printf("Active tower set to %q\n", name)
	return nil
}
