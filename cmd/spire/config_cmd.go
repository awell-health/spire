package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

func cmdConfig(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: spire config <get|set|list> [key] [value]\n\nKeys: identity, dolt.port, daemon.interval, editor.cursor, editor.claude, mcp-server.path, dolthub.remote")
	}

	// Parse --repo flag
	var useRepo bool
	var remaining []string
	for _, a := range args {
		if a == "--repo" {
			useRepo = true
		} else {
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
		return configGet(args[1])
	case "set":
		if len(args) < 3 {
			return fmt.Errorf("usage: spire config set <key> <value>")
		}
		return configSet(args[1], args[2])
	case "list":
		return configList()
	default:
		return fmt.Errorf("unknown config subcommand: %q\nusage: spire config <get|set|list> [key] [value]", args[0])
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
		return nil, "", fmt.Errorf("not in a spire-managed directory (run `spire init` first)")
	}
	return inst, inst.Prefix, nil
}

// configGet reads a config value by key and prints it.
func configGet(key string) error {
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

			// Also show existing instance fields for reference
			fmt.Printf("  role = %s\n", inst.Role)
			fmt.Printf("  path = %s\n", inst.Path)
			if len(inst.Paths) > 0 {
				fmt.Printf("  paths = %s\n", strings.Join(inst.Paths, ", "))
			}
		}
	}

	return nil
}
