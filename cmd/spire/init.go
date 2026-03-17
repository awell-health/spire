package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func cmdInit(args []string) error {
	fmt.Println(spireLogo)

	// Parse flags
	var flagPrefix, flagSatellite string
	var flagStandalone, flagHub bool
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--prefix" && i+1 < len(args):
			i++
			flagPrefix = args[i]
		case strings.HasPrefix(args[i], "--prefix="):
			flagPrefix = strings.TrimPrefix(args[i], "--prefix=")
		case args[i] == "--satellite" && i+1 < len(args):
			i++
			flagSatellite = args[i]
		case strings.HasPrefix(args[i], "--satellite="):
			flagSatellite = strings.TrimPrefix(args[i], "--satellite=")
		case args[i] == "--standalone":
			flagStandalone = true
		case args[i] == "--hub":
			flagHub = true
		default:
			return fmt.Errorf("unknown flag: %s\nusage: spire init [--prefix=<pfx>] [--hub|--standalone|--satellite=<hub>]", args[i])
		}
	}

	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("cannot determine working directory: %w", err)
	}

	cfg, err := loadConfig()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	// Check if already init'd
	if existing := findInstanceByPath(cfg, cwd); existing != nil {
		fmt.Printf("  This directory is already init'd as %q (%s).\n", existing.Prefix, existing.Role)
		fmt.Printf("    prefix:   %s\n", existing.Prefix)
		fmt.Printf("    role:     %s\n", existing.Role)
		fmt.Printf("    database: %s\n", existing.Database)
		if existing.Hub != "" {
			fmt.Printf("    hub:      %s\n", existing.Hub)
		}
		fmt.Println()
		fmt.Print("  Re-initialize? [y/N] ")
		reader := bufio.NewReader(os.Stdin)
		answer, _ := reader.ReadString('\n')
		if !strings.HasPrefix(strings.TrimSpace(strings.ToLower(answer)), "y") {
			return nil
		}
		// Remove old entry so we can re-create
		delete(cfg.Instances, existing.Prefix)
	}

	firstInit := len(cfg.Instances) == 0

	// --- Shell env injection ---
	if !cfg.Shell.Configured {
		profile, injected := injectShellEnv()
		if injected {
			cfg.Shell.Configured = true
			cfg.Shell.Profile = profile
			fmt.Printf("  Shell env vars added to %s\n", profile)
		}
	} else {
		fmt.Printf("  Shell env: already configured (%s)\n", cfg.Shell.Profile)
	}
	fmt.Println()

	reader := bufio.NewReader(os.Stdin)

	// --- Prefix ---
	prefix := flagPrefix
	if prefix == "" {
		defaultPrefix := currentDirName()
		if len(defaultPrefix) > 3 {
			defaultPrefix = defaultPrefix[:3]
		}
		fmt.Printf("  Prefix for this repo [%s]: ", defaultPrefix)
		input, _ := reader.ReadString('\n')
		prefix = strings.TrimSpace(input)
		if prefix == "" {
			prefix = defaultPrefix
		}
	}

	// Check prefix uniqueness
	if _, exists := cfg.Instances[prefix]; exists {
		return fmt.Errorf("prefix %q is already in use by %s", prefix, cfg.Instances[prefix].Path)
	}

	// --- Role ---
	role := ""
	hubPrefix := ""

	switch {
	case flagSatellite != "":
		role = "satellite"
		hubPrefix = flagSatellite
		if _, ok := cfg.Instances[hubPrefix]; !ok {
			return fmt.Errorf("hub %q not found in config — init the hub first", hubPrefix)
		}
		if cfg.Instances[hubPrefix].Role != "hub" {
			return fmt.Errorf("%q is not a hub (role: %s)", hubPrefix, cfg.Instances[hubPrefix].Role)
		}
	case flagStandalone:
		role = "standalone"
	case flagHub:
		role = "hub"
	default:
		// Interactive role selection
		if firstInit {
			fmt.Println("  Role for this repo:")
			fmt.Println("    1. Hub (other repos can connect as satellites)")
			fmt.Println("    2. Standalone (single repo)")
			fmt.Print("  [1]: ")
			input, _ := reader.ReadString('\n')
			input = strings.TrimSpace(input)
			switch input {
			case "", "1":
				role = "hub"
			case "2":
				role = "standalone"
			default:
				return fmt.Errorf("invalid selection: %q", input)
			}
		} else {
			// Find existing hubs for satellite option
			var hubs []string
			for k, inst := range cfg.Instances {
				if inst.Role == "hub" {
					hubs = append(hubs, k)
				}
			}

			fmt.Println("  Role for this repo:")
			optNum := 1
			if len(hubs) > 0 {
				for _, h := range hubs {
					fmt.Printf("    %d. Satellite of %s (%s)\n", optNum, h, cfg.Instances[h].Path)
					optNum++
				}
			}
			hubOpt := optNum
			fmt.Printf("    %d. Hub (other repos can connect as satellites)\n", optNum)
			optNum++
			standaloneOpt := optNum
			fmt.Printf("    %d. Standalone (single repo)\n", optNum)

			defaultOpt := "1"
			fmt.Printf("  [%s]: ", defaultOpt)
			input, _ := reader.ReadString('\n')
			input = strings.TrimSpace(input)
			if input == "" {
				input = defaultOpt
			}

			choice := 0
			fmt.Sscanf(input, "%d", &choice)
			switch {
			case len(hubs) > 0 && choice >= 1 && choice <= len(hubs):
				role = "satellite"
				hubPrefix = hubs[choice-1]
			case choice == hubOpt:
				role = "hub"
			case choice == standaloneOpt:
				role = "standalone"
			default:
				return fmt.Errorf("invalid selection: %q", input)
			}
		}
	}

	fmt.Println()
	fmt.Printf("  Initializing %s as %s (prefix: %s-)...\n", currentDirName(), role, prefix)
	fmt.Println()

	// --- Set up beads ---
	database := prefix
	if role == "satellite" {
		database = hubPrefix
	}

	if role == "satellite" {
		// Satellite: create .beads/redirect
		hubInst := cfg.Instances[hubPrefix]
		hubBeads := filepath.Join(hubInst.Path, ".beads")
		relPath, err := filepath.Rel(cwd, hubBeads)
		if err != nil {
			relPath = hubBeads // fallback to absolute
		}
		os.MkdirAll(filepath.Join(cwd, ".beads"), 0755)
		if err := os.WriteFile(filepath.Join(cwd, ".beads", "redirect"), []byte(relPath+"\n"), 0644); err != nil {
			return fmt.Errorf("write redirect: %w", err)
		}
		fmt.Printf("  Redirect → %s\n", relPath)
	} else {
		// Hub/standalone: run bd init (or detect already-initialized)
		_, initErr := bd("init", "--prefix", prefix)
		if initErr != nil {
			// Check if already initialized — that's fine, not an error
			existingPrefix, _ := bd("config", "get", "issue-prefix")
			existingPrefix = strings.TrimSpace(existingPrefix)
			if existingPrefix != "" && !strings.Contains(existingPrefix, "(not set)") {
				if existingPrefix != prefix {
					return fmt.Errorf("beads already initialized with prefix %q (wanted %q) — use bd init --force --prefix %s to reinitialize", existingPrefix, prefix, prefix)
				}
				fmt.Printf("  Beads already initialized (prefix: %s-)\n", prefix)
			} else {
				return fmt.Errorf("bd init failed: %w\n  Try: bd init --prefix %s", initErr, prefix)
			}
		} else {
			fmt.Printf("  Beads initialized (prefix: %s-)\n", prefix)
		}
	}

	// --- Write .envrc ---
	envrcPath := filepath.Join(cwd, ".envrc")
	envrcContent := fmt.Sprintf("export SPIRE_IDENTITY=\"%s\"\n", prefix)
	if data, err := os.ReadFile(envrcPath); err == nil {
		if !strings.Contains(string(data), "SPIRE_IDENTITY") {
			// Append
			os.WriteFile(envrcPath, append(data, []byte("\n"+envrcContent)...), 0644)
		}
	} else {
		os.WriteFile(envrcPath, []byte(envrcContent), 0644)
	}
	fmt.Printf("  .envrc written (SPIRE_IDENTITY=%s)\n", prefix)

	// --- Register in config ---
	inst := &Instance{
		Path:     cwd,
		Prefix:   prefix,
		Role:     role,
		Database: database,
	}
	if role == "satellite" {
		inst.Hub = hubPrefix
		// Add to hub's satellite list
		hubInst := cfg.Instances[hubPrefix]
		if !containsStr(hubInst.Satellites, prefix) {
			hubInst.Satellites = append(hubInst.Satellites, prefix)
		}
	}
	cfg.Instances[prefix] = inst

	// --- Regenerate routes for satellite/hub ---
	if role == "satellite" {
		if err := regenerateRoutes(cfg, hubPrefix); err != nil {
			fmt.Printf("  Warning: route regeneration failed: %s\n", err)
		} else {
			fmt.Println("  Routes regenerated")
		}
	}

	// --- Save config ---
	if err := saveConfig(cfg); err != nil {
		return fmt.Errorf("save config: %w", err)
	}
	fmt.Println("  Config saved")

	// --- Next steps ---
	fmt.Println()
	fmt.Println("  Next steps:")
	fmt.Println("    spire up                     # start dolt server + daemon")
	if role == "hub" {
		fmt.Println("    spire init --satellite=" + prefix + "   # connect a satellite repo")
	}
	fmt.Println("    spire connect linear         # sync epics to Linear")
	fmt.Println("    spire repo list              # see all init'd repos")
	fmt.Println()

	return nil
}

// injectShellEnv detects the user's shell, finds the profile, and appends env vars.
// Returns (profile path, true) if injected, or ("", false) if skipped/failed.
func injectShellEnv() (string, bool) {
	shell := os.Getenv("SHELL")
	profile := ""

	home, err := os.UserHomeDir()
	if err != nil {
		return "", false
	}

	switch {
	case strings.HasSuffix(shell, "/zsh"):
		profile = filepath.Join(home, ".zshrc")
	case strings.HasSuffix(shell, "/bash"):
		// Prefer .bash_profile on macOS, .bashrc on Linux
		profile = filepath.Join(home, ".bash_profile")
		if _, err := os.Stat(profile); os.IsNotExist(err) {
			profile = filepath.Join(home, ".bashrc")
		}
	case strings.HasSuffix(shell, "/fish"):
		profile = filepath.Join(home, ".config", "fish", "config.fish")
	default:
		profile = filepath.Join(home, ".profile")
	}

	// Check if already present
	if data, err := os.ReadFile(profile); err == nil {
		if strings.Contains(string(data), "BEADS_DOLT_SERVER_HOST") {
			return profile, false
		}
	}

	block := `
# Beads — central dolt server (added by spire init)
export BEADS_DOLT_SERVER_HOST="127.0.0.1"
export BEADS_DOLT_SERVER_PORT="3307"
export BEADS_DOLT_SERVER_MODE=1
export BEADS_DOLT_AUTO_START=0
`

	f, err := os.OpenFile(profile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return "", false
	}
	defer f.Close()
	f.WriteString(block)

	return profile, true
}

// regenerateRoutes rebuilds routes.jsonl for a hub and all its satellites.
func regenerateRoutes(cfg *SpireConfig, hubPrefix string) error {
	hubInst, ok := cfg.Instances[hubPrefix]
	if !ok {
		return fmt.Errorf("hub %q not in config", hubPrefix)
	}

	// Build routes: hub + all its satellites
	var lines []string
	lines = append(lines, fmt.Sprintf(`{"prefix":"%s-","path":"."}`, hubPrefix))
	for _, satPrefix := range hubInst.Satellites {
		lines = append(lines, fmt.Sprintf(`{"prefix":"%s-","path":"."}`, satPrefix))
	}
	routesContent := strings.Join(lines, "\n") + "\n"

	// Write to hub
	hubRoutes := filepath.Join(hubInst.Path, ".beads", "routes.jsonl")
	if err := os.WriteFile(hubRoutes, []byte(routesContent), 0644); err != nil {
		return fmt.Errorf("write hub routes: %w", err)
	}

	// Write to each satellite
	for _, satPrefix := range hubInst.Satellites {
		satInst, ok := cfg.Instances[satPrefix]
		if !ok {
			continue
		}
		satRoutes := filepath.Join(satInst.Path, ".beads", "routes.jsonl")
		os.WriteFile(satRoutes, []byte(routesContent), 0644)
	}

	return nil
}

// containsStr checks if a slice contains a string.
func containsStr(slice []string, s string) bool {
	for _, v := range slice {
		if v == s {
			return true
		}
	}
	return false
}
