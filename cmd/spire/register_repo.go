package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/awell-health/spire/pkg/repoconfig"
)

// cmdRegisterRepo registers a repository under an existing tower.
// It writes a row to the dolt repos table and sets up the local .beads/ directory.
func cmdRegisterRepo(args []string) error {
	var flagPrefix, flagRepoURL, flagBranch, flagDatabase string
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--prefix" && i+1 < len(args):
			i++
			flagPrefix = args[i]
		case strings.HasPrefix(args[i], "--prefix="):
			flagPrefix = strings.TrimPrefix(args[i], "--prefix=")
		case args[i] == "--repo-url" && i+1 < len(args):
			i++
			flagRepoURL = args[i]
		case strings.HasPrefix(args[i], "--repo-url="):
			flagRepoURL = strings.TrimPrefix(args[i], "--repo-url=")
		case args[i] == "--branch" && i+1 < len(args):
			i++
			flagBranch = args[i]
		case strings.HasPrefix(args[i], "--branch="):
			flagBranch = strings.TrimPrefix(args[i], "--branch=")
		case args[i] == "--database" && i+1 < len(args):
			i++
			flagDatabase = args[i]
		case strings.HasPrefix(args[i], "--database="):
			flagDatabase = strings.TrimPrefix(args[i], "--database=")
		case args[i] == "--help" || args[i] == "-h":
			printRegisterRepoUsage()
			return nil
		default:
			return fmt.Errorf("unknown flag: %s\nusage: spire register-repo [--prefix <pfx>] [--repo-url <url>] [--branch <branch>] [--database <db>]", args[i])
		}
	}

	cwd, err := realCwd()
	if err != nil {
		return fmt.Errorf("cannot determine working directory: %w", err)
	}

	// --- Auto-detect values ---
	prefix := flagPrefix
	if prefix == "" {
		prefix = detectPrefix(cwd)
	}

	repoURL := flagRepoURL
	if repoURL == "" {
		repoURL = detectRepoURL(cwd)
	}

	branch := flagBranch
	if branch == "" {
		branch = detectBranch(cwd)
	}

	language := detectLanguage(cwd)

	cfg, err := loadConfig()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	database := flagDatabase
	if database == "" {
		database = detectDatabase(cfg, prefix)
	}
	if database == "" || database == prefix {
		// No tower found and no existing instances — fail clearly
		return fmt.Errorf("no tower found — run 'spire tower create' or 'spire tower attach' first")
	}

	user := detectUser()

	// --- Validate ---
	if err := validatePrefix(prefix); err != nil {
		return err
	}

	if repoURL == "" {
		return fmt.Errorf("cannot detect repo URL (no git remote); use --repo-url")
	}

	if _, exists := cfg.Instances[prefix]; exists {
		return fmt.Errorf("prefix %q already registered in config; use a different --prefix or run: spire repo remove %s", prefix, prefix)
	}

	// --- Check dolt reachability ---
	if err := requireDolt(); err != nil {
		return err
	}

	// --- Write to repos table ---
	sql := fmt.Sprintf(
		"INSERT INTO repos (prefix, repo_url, branch, language, registered_by) VALUES ('%s', '%s', '%s', '%s', '%s')",
		sqlEscape(prefix), sqlEscape(repoURL), sqlEscape(branch), sqlEscape(language), sqlEscape(user),
	)
	if _, err := bd("sql", "-q", sql); err != nil {
		// If the table doesn't exist, give a clear error
		if strings.Contains(err.Error(), "repos") && strings.Contains(err.Error(), "not found") {
			return fmt.Errorf("repos table not found — run: spire tower create\n  %w", err)
		}
		return fmt.Errorf("insert into repos table: %w", err)
	}

	// --- Set up .beads/ directory ---
	beadsDir := filepath.Join(cwd, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		return fmt.Errorf("create .beads/: %w", err)
	}

	// metadata.json — tells bd how to connect to the dolt server
	// Read project_id from tower config (no SQL needed)
	var projectID string
	if cfg.ActiveTower != "" {
		if tower, err := loadTowerConfig(cfg.ActiveTower); err == nil {
			projectID = tower.ProjectID
		}
	}
	metadata := map[string]any{
		"database":      "dolt",
		"backend":       "dolt",
		"dolt_mode":     "server",
		"dolt_database": database,
	}
	if projectID != "" {
		metadata["project_id"] = projectID
	}
	metaBytes, _ := json.MarshalIndent(metadata, "", "  ")
	metaPath := filepath.Join(beadsDir, "metadata.json")
	if err := os.WriteFile(metaPath, append(metaBytes, '\n'), 0644); err != nil {
		return fmt.Errorf("write metadata.json: %w", err)
	}

	// config.yaml — dolt server connection
	configYAML := fmt.Sprintf("dolt.host: %q\ndolt.port: %s\n", doltHost(), doltPort())
	configPath := filepath.Join(beadsDir, "config.yaml")
	if err := os.WriteFile(configPath, []byte(configYAML), 0644); err != nil {
		return fmt.Errorf("write config.yaml: %w", err)
	}

	// routes.jsonl — prefix routing
	routesContent := fmt.Sprintf("{\"prefix\":\"%s-\",\"path\":\".\"}\n", prefix)
	routesPath := filepath.Join(beadsDir, "routes.jsonl")
	if err := os.WriteFile(routesPath, []byte(routesContent), 0644); err != nil {
		return fmt.Errorf("write routes.jsonl: %w", err)
	}

	// .gitignore — keep machine-specific files out of git
	gitignorePath := filepath.Join(beadsDir, ".gitignore")
	if _, err := os.Stat(gitignorePath); os.IsNotExist(err) {
		gitignoreContent := "metadata.json\nconfig.yaml\nroutes.jsonl\n"
		if writeErr := os.WriteFile(gitignorePath, []byte(gitignoreContent), 0644); writeErr != nil {
			fmt.Printf("  Warning: could not write .beads/.gitignore: %s\n", writeErr)
		}
	}

	// --- Register in global config ---
	cfg.Instances[prefix] = &Instance{
		Path:     cwd,
		Prefix:   prefix,
		Role:     "standalone",
		Database: database,
	}
	if err := saveConfig(cfg); err != nil {
		return fmt.Errorf("save config: %w", err)
	}

	// --- Generate spire.yaml if missing ---
	spireYAMLPath := filepath.Join(cwd, "spire.yaml")
	if _, err := os.Stat(spireYAMLPath); os.IsNotExist(err) {
		content := repoconfig.GenerateYAML(cwd)
		if writeErr := os.WriteFile(spireYAMLPath, []byte(content), 0644); writeErr != nil {
			fmt.Printf("  Warning: could not write spire.yaml: %s\n", writeErr)
		} else {
			fmt.Println("  spire.yaml generated")
		}
	}

	// --- Commit dolt changes ---
	if _, err := bd("dolt", "commit", "-m", "register: "+prefix); err != nil {
		// Non-fatal: commit may fail if no changes or dolt not configured for commits
		fmt.Printf("  Warning: dolt commit skipped: %s\n", err)
	}

	// --- Push to DoltHub (if remote configured) ---
	if cfg.ActiveTower != "" {
		if tower, err := loadTowerConfig(cfg.ActiveTower); err == nil && tower.DolthubRemote != "" {
			// Set credentials
			if user := getCredential(CredKeyDolthubUser); user != "" {
				os.Setenv("DOLT_REMOTE_USER", user)
			}
			if pass := getCredential(CredKeyDolthubPassword); pass != "" {
				os.Setenv("DOLT_REMOTE_PASSWORD", pass)
			}

			dataDir := filepath.Join(doltDataDir(), tower.Database)
			setDoltCLIRemote(dataDir, "origin", tower.DolthubRemote)

			fmt.Println("  Pushing registration to DoltHub...")
			if err := doltCLIPush(dataDir, false); err != nil {
				fmt.Printf("  Warning: DoltHub push skipped: %s\n", err)
				fmt.Println("  Run 'spire push' later to sync.")
			}
		}
	}

	// --- Print summary ---
	fmt.Println()
	fmt.Printf("Repo registered: %s\n", prefix)
	fmt.Printf("  prefix:   %s\n", prefix)
	fmt.Printf("  repo:     %s\n", repoURL)
	fmt.Printf("  branch:   %s\n", branch)
	fmt.Printf("  language: %s\n", language)
	fmt.Printf("  database: %s\n", database)
	fmt.Println()
	fmt.Println("Next steps:")
	fmt.Printf("  spire file \"My first task\" -t task -p 2\n")
	fmt.Printf("  spire up\n")

	return nil
}

// --- Auto-detection helpers ---

// detectPrefix generates a prefix from the directory base name.
// Takes the first 3 lowercase alphanumeric characters.
func detectPrefix(dir string) string {
	base := filepath.Base(dir)
	base = strings.ToLower(base)

	// Strip non-alphanumeric characters
	var clean []byte
	for i := 0; i < len(base); i++ {
		c := base[i]
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			clean = append(clean, c)
		}
	}

	if len(clean) == 0 {
		return "repo"
	}

	// Take first 3 characters
	if len(clean) > 3 {
		clean = clean[:3]
	}

	return string(clean)
}

// detectRepoURL runs git remote get-url origin in the given directory.
func detectRepoURL(dir string) string {
	cmd := exec.Command("git", "-C", dir, "remote", "get-url", "origin")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// detectBranch runs git rev-parse --abbrev-ref HEAD in the given directory.
func detectBranch(dir string) string {
	cmd := exec.Command("git", "-C", dir, "rev-parse", "--abbrev-ref", "HEAD")
	out, err := cmd.Output()
	if err != nil {
		return "main"
	}
	b := strings.TrimSpace(string(out))
	if b == "" || b == "HEAD" {
		return "main"
	}
	return b
}

// detectLanguage inspects the directory for known project markers.
func detectLanguage(dir string) string {
	markers := []struct {
		file string
		lang string
	}{
		{"go.mod", "go"},
		{"Cargo.toml", "rust"},
		{"pyproject.toml", "python"},
		{"requirements.txt", "python"},
		{"package.json", "typescript"},
	}
	for _, m := range markers {
		if _, err := os.Stat(filepath.Join(dir, m.file)); err == nil {
			return m.lang
		}
	}
	return ""
}

// detectDatabase determines the database name.
// Priority: 1) active tower config, 2) existing instances, 3) empty string.
func detectDatabase(cfg *SpireConfig, prefix string) string {
	// Priority 1: active tower config
	if cfg != nil && cfg.ActiveTower != "" {
		tower, err := loadTowerConfig(cfg.ActiveTower)
		if err == nil && tower.Database != "" {
			return tower.Database
		}
	}
	// Priority 2: existing instances (all repos in a tower share a database)
	if cfg != nil {
		for _, inst := range cfg.Instances {
			if inst.Database != "" {
				return inst.Database
			}
		}
	}
	return ""
}

// detectUser returns the current user for the registered_by field.
func detectUser() string {
	if id := os.Getenv("SPIRE_IDENTITY"); id != "" {
		return id
	}
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	return "unknown"
}

// --- Validation ---

// prefixPattern matches valid prefixes: 2-16 lowercase alphanumeric characters.
var prefixPattern = regexp.MustCompile(`^[a-z0-9]{2,16}$`)

// validatePrefix checks that a prefix is valid.
func validatePrefix(prefix string) error {
	if !prefixPattern.MatchString(prefix) {
		return fmt.Errorf("invalid prefix %q: must be 2-16 lowercase alphanumeric characters", prefix)
	}
	return nil
}

// --- SQL helpers ---

// sqlEscape escapes single quotes in a string for safe SQL insertion.
func sqlEscape(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

// --- Dolt helpers ---

func printRegisterRepoUsage() {
	fmt.Println(`Usage: spire register-repo [flags]

Register a repository under an existing tower. Detects prefix, repo URL,
branch, and language automatically from the current directory.

Flags:
  --prefix <pfx>      Repo prefix (default: first 3 chars of directory name)
  --repo-url <url>    Git remote URL (default: git remote get-url origin)
  --branch <branch>   Default branch (default: current branch or "main")
  --database <db>     Dolt database name (default: detected from config)

Examples:
  spire register-repo
  spire register-repo --prefix web --repo-url https://github.com/org/web-app
  spire register-repo --prefix api --database beads_hub`)
}
