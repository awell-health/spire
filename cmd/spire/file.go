package main

import (
	"fmt"
	"os"
	"strings"
)

func cmdFile(args []string) error {
	if err := requireDolt(); err != nil {
		return err
	}

	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		fmt.Println("usage: spire file <title> [--prefix <prefix>] [bd create flags...]")
		return nil
	}

	// Extract --prefix from args; pass everything else to bd create
	var prefix string
	remaining := []string{}

	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--prefix":
			if i+1 >= len(args) {
				return fmt.Errorf("--prefix requires a value")
			}
			i++
			prefix = args[i]
		case strings.HasPrefix(args[i], "--prefix="):
			prefix = strings.TrimPrefix(args[i], "--prefix=")
		default:
			remaining = append(remaining, args[i])
		}
	}

	// Fall back to CWD detection
	if prefix == "" {
		if cwd, err := os.Getwd(); err == nil {
			if cfg, err := loadConfig(); err == nil {
				if inst := findInstanceByPath(cfg, cwd); inst != nil {
					prefix = inst.Prefix
				}
			}
		}
	}

	// Still no prefix — list available and error
	if prefix == "" {
		cfg, _ := loadConfig()
		var prefixes []string
		for p := range cfg.Instances {
			prefixes = append(prefixes, p)
		}
		return fmt.Errorf("--prefix required (registered: %s)", strings.Join(prefixes, ", "))
	}

	bdArgs := append([]string{"create", "--prefix", prefix}, remaining...)
	id, err := bdSilent(bdArgs...)
	if err != nil {
		return fmt.Errorf("file: %w", err)
	}

	fmt.Println(id)
	return nil
}
