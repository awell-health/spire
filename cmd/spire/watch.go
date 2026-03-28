package main

import (
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/awell-health/spire/pkg/board"
	"github.com/awell-health/spire/pkg/dolt"
)

func cmdWatch(args []string) error {
	if d := resolveBeadsDir(); d != "" {
		os.Setenv("BEADS_DIR", d)
	}
	if err := requireDolt(); err != nil {
		return err
	}

	var epicID string
	interval := 5 * time.Second
	once := false

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--interval":
			if i+1 >= len(args) {
				return fmt.Errorf("--interval requires a value")
			}
			i++
			d, err := time.ParseDuration(args[i])
			if err != nil {
				return fmt.Errorf("--interval: invalid duration %q", args[i])
			}
			interval = d
		case "--once":
			once = true
		default:
			if strings.HasPrefix(args[i], "-") {
				return fmt.Errorf("unknown flag: %s\nusage: spire watch [<epic-id>] [--interval 5s] [--once]", args[i])
			}
			epicID = args[i]
		}
	}

	deps := board.WatchDeps{
		LoadWizardRegistry: func() []board.LocalAgent {
			reg := loadWizardRegistry()
			return reg.Wizards
		},
		ProcessAlive: func(pid int) bool {
			return dolt.ProcessAlive(pid)
		},
	}

	if once {
		return board.RenderWatch(epicID, deps)
	}

	// Live mode: clear screen, render, repeat.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	board.RenderWatch(epicID, deps) //nolint:errcheck

	for {
		select {
		case <-sigCh:
			fmt.Println()
			return nil
		case <-ticker.C:
			board.ClearScreen()
			board.RenderWatch(epicID, deps) //nolint:errcheck
		}
	}
}
