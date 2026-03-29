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
	"github.com/spf13/cobra"
)

var watchCmd = &cobra.Command{
	Use:   "watch [epic-id]",
	Short: "Live-updating activity view",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		var fullArgs []string
		if v, _ := cmd.Flags().GetString("interval"); v != "" {
			fullArgs = append(fullArgs, "--interval", v)
		}
		if once, _ := cmd.Flags().GetBool("once"); once {
			fullArgs = append(fullArgs, "--once")
		}
		fullArgs = append(fullArgs, args...)
		return cmdWatch(fullArgs)
	},
}

func init() {
	watchCmd.Flags().String("interval", "", "Refresh interval (e.g. 5s)")
	watchCmd.Flags().Bool("once", false, "Print once and exit")
}

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
