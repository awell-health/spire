package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/awell-health/spire/pkg/repoconfig"
	"github.com/spf13/cobra"
)

var configCmd = &cobra.Command{
	Use:                "config <get|set|list|repo|auth> [key] [value]",
	Short:              "Read/write config values, credentials, and auth slots",
	Args:               cobra.MinimumNArgs(1),
	DisableFlagParsing: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		return cmdConfig(args)
	},
}

func init() {
	configCmd.Flags().Bool("repo", false, "Print resolved spire.yaml")
	configCmd.Flags().Bool("unmask", false, "Show full credential values")
}

func cmdConfig(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: spire config <get|set|list|repo|auth> [key] [value]\n\nConfig keys: identity, dolt.port, daemon.interval, editor.cursor, editor.claude, mcp-server.path, dolthub.remote\nCredential keys: %s\n\nFlags:\n  --unmask  Show full credential values (default: masked)\n\nSubcommands:\n  repo    Print resolved spire.yaml (repo-level agent config)\n  auth    Manage Anthropic auth credential slots (subscription, api-key)", strings.Join(validCredentialKeys(), ", "))
	}

	// `auth` has its own flag grammar (--token, --key, --token-stdin,
	// --key-stdin). Dispatch directly with the unmodified tail so the
	// --repo/--unmask flag scanner below doesn't eat auth flags.
	if args[0] == "auth" {
		return cmdConfigAuth(args[1:])
	}

	// Parse flags
	var useRepo bool
	var unmask bool
	var remaining []string
	for _, a := range args {
		switch a {
		case "--repo":
			useRepo = true
		case "--unmask":
			unmask = true
		default:
			remaining = append(remaining, a)
		}
	}
	args = remaining

	if useRepo {
		return fmt.Errorf("--repo flag targets .beads/config.yaml (not yet implemented)")
	}

	switch args[0] {
	case "get":
		if len(args) < 2 {
			return fmt.Errorf("usage: spire config get <key>")
		}
		return configGet(args[1], unmask)
	case "set":
		if len(args) < 3 {
			return fmt.Errorf("usage: spire config set <key> <value>")
		}
		return configSet(args[1], args[2])
	case "list":
		return configList()
	case "repo":
		return configRepo()
	case "auth":
		return cmdConfigAuth(args[1:])
	default:
		return fmt.Errorf("unknown config subcommand: %q\nusage: spire config <get|set|list|repo|auth> [key] [value]", args[0])
	}
}

// currentInstance returns the instance for the current working directory.
func currentInstance(cfg *SpireConfig) (*Instance, string, error) {
	cwd, err := realCwd()
	if err != nil {
		return nil, "", fmt.Errorf("cannot determine working directory: %w", err)
	}
	inst := findInstanceByPath(cfg, cwd)
	if inst == nil {
		// Fallback: try SPIRE_IDENTITY
		if id := os.Getenv("SPIRE_IDENTITY"); id != "" {
			if i, ok := cfg.Instances[id]; ok {
				return i, id, nil
			}
		}
		return nil, "", fmt.Errorf("not in a spire-managed directory (run `spire repo add` first)")
	}
	return inst, inst.Prefix, nil
}

// configGet reads a config value by key and prints it.
func configGet(key string, unmask bool) error {
	// Route credential keys to the credential store
	if isCredentialKey(key) {
		return credentialGet(key, unmask)
	}

	cfg, err := loadConfig()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	switch key {
	case "identity":
		inst, _, err := currentInstance(cfg)
		if err != nil {
			return err
		}
		val := inst.Identity
		if val == "" {
			val = inst.Prefix
		}
		fmt.Println(val)

	case "dolt.port":
		inst, _, err := currentInstance(cfg)
		if err != nil {
			return err
		}
		port := inst.DoltPort
		if port == 0 {
			port = 3307
		}
		fmt.Println(port)

	case "daemon.interval":
		inst, _, err := currentInstance(cfg)
		if err != nil {
			return err
		}
		interval := inst.DaemonInterval
		if interval == "" {
			interval = "2m"
		}
		fmt.Println(interval)

	case "dolthub.remote":
		inst, _, err := currentInstance(cfg)
		if err != nil {
			return err
		}
		if inst.DolthubRemote == "" {
			fmt.Println("(not set)")
		} else {
			fmt.Println(inst.DolthubRemote)
		}

	case "editor.cursor":
		val := true
		if cfg.EditorCursor != nil {
			val = *cfg.EditorCursor
		}
		fmt.Println(val)

	case "editor.claude":
		val := true
		if cfg.EditorClaude != nil {
			val = *cfg.EditorClaude
		}
		fmt.Println(val)

	case "mcp-server.path":
		if cfg.MCPServer == "" {
			fmt.Println("(not set)")
		} else {
			fmt.Println(cfg.MCPServer)
		}

	default:
		return fmt.Errorf("unknown config key: %q\nValid keys: identity, dolt.port, daemon.interval, editor.cursor, editor.claude, mcp-server.path, dolthub.remote", key)
	}

	return nil
}

// configSet writes a config value by key.
func configSet(key, value string) error {
	// Route credential keys to the credential store
	if isCredentialKey(key) {
		if err := setCredential(key, value); err != nil {
			return err
		}
		fmt.Printf("%s = %s (saved to credentials file)\n", key, maskValue(value))
		return nil
	}

	cfg, err := loadConfig()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	switch key {
	case "identity":
		inst, _, err := currentInstance(cfg)
		if err != nil {
			return err
		}
		inst.Identity = value

	case "dolt.port":
		inst, _, err := currentInstance(cfg)
		if err != nil {
			return err
		}
		port, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("dolt.port must be an integer: %w", err)
		}
		inst.DoltPort = port

	case "daemon.interval":
		inst, _, err := currentInstance(cfg)
		if err != nil {
			return err
		}
		inst.DaemonInterval = value

	case "dolthub.remote":
		inst, _, err := currentInstance(cfg)
		if err != nil {
			return err
		}
		inst.DolthubRemote = value

	case "editor.cursor":
		b, err := strconv.ParseBool(value)
		if err != nil {
			return fmt.Errorf("editor.cursor must be true or false: %w", err)
		}
		cfg.EditorCursor = &b

	case "editor.claude":
		b, err := strconv.ParseBool(value)
		if err != nil {
			return fmt.Errorf("editor.claude must be true or false: %w", err)
		}
		cfg.EditorClaude = &b

	case "mcp-server.path":
		cfg.MCPServer = value

	default:
		return fmt.Errorf("unknown config key: %q\nValid keys: identity, dolt.port, daemon.interval, editor.cursor, editor.claude, mcp-server.path, dolthub.remote", key)
	}

	if err := saveConfig(cfg); err != nil {
		return fmt.Errorf("save config: %w", err)
	}

	fmt.Printf("%s = %s\n", key, value)
	return nil
}

// configList prints all config values.
func configList() error {
	cfg, err := loadConfig()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	// Global settings
	fmt.Println("# Global settings")

	editorCursor := true
	if cfg.EditorCursor != nil {
		editorCursor = *cfg.EditorCursor
	}
	fmt.Printf("editor.cursor = %v\n", editorCursor)

	editorClaude := true
	if cfg.EditorClaude != nil {
		editorClaude = *cfg.EditorClaude
	}
	fmt.Printf("editor.claude = %v\n", editorClaude)

	if cfg.MCPServer != "" {
		fmt.Printf("mcp-server.path = %s\n", cfg.MCPServer)
	} else {
		fmt.Println("mcp-server.path = (not set)")
	}

	// Credentials
	fmt.Println()
	fmt.Println("# Credentials")
	for _, key := range validCredentialKeys() {
		val := getCredential(key)
		if val == "" {
			fmt.Printf("%s = (not set)\n", key)
		} else {
			source := credentialSource(key)
			fmt.Printf("%s = %s (from %s)\n", key, maskValue(val), source)
		}
	}

	// Per-instance settings
	if len(cfg.Instances) > 0 {
		fmt.Println()

		// Try to highlight the current instance
		currentPrefix := ""
		if inst, _, err := currentInstance(cfg); err == nil {
			currentPrefix = inst.Prefix
		}

		for prefix, inst := range cfg.Instances {
			marker := ""
			if prefix == currentPrefix {
				marker = " (current)"
			}
			fmt.Printf("# Instance: %s%s\n", prefix, marker)

			identity := inst.Identity
			if identity == "" {
				identity = inst.Prefix
			}
			fmt.Printf("  identity = %s\n", identity)

			port := inst.DoltPort
			if port == 0 {
				port = 3307
			}
			fmt.Printf("  dolt.port = %d\n", port)

			interval := inst.DaemonInterval
			if interval == "" {
				interval = "2m"
			}
			fmt.Printf("  daemon.interval = %s\n", interval)

			if inst.DolthubRemote != "" {
				fmt.Printf("  dolthub.remote = %s\n", inst.DolthubRemote)
			} else {
				fmt.Println("  dolthub.remote = (not set)")
			}

			fmt.Printf("  path = %s\n", inst.Path)
			if len(inst.Paths) > 0 {
				fmt.Printf("  paths = %s\n", strings.Join(inst.Paths, ", "))
			}
		}
	}

	return nil
}

// credentialGet prints a credential value with masking and source info.
func credentialGet(key string, unmask bool) error {
	val := getCredential(key)
	if val == "" {
		fmt.Printf("%s = (not set)\n", key)
		return nil
	}

	display := maskValue(val)
	if unmask {
		display = val
	}

	source := credentialSource(key)
	fmt.Printf("%s = %s (from %s)\n", key, display, source)
	return nil
}

// configRepo loads spire.yaml (with auto-detection fallbacks) and prints
// the fully resolved repo configuration.
func configRepo() error {
	cwd, err := realCwd()
	if err != nil {
		return fmt.Errorf("cannot determine working directory: %w", err)
	}

	cfg, err := repoconfig.Load(cwd)
	if err != nil {
		return fmt.Errorf("load repo config: %w", err)
	}

	fmt.Print(repoconfig.FormatResolved(cfg))
	return nil
}
