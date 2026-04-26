package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/awell-health/spire/pkg/agent"
	"github.com/awell-health/spire/pkg/board"
	"github.com/awell-health/spire/pkg/dolt"
	"github.com/awell-health/spire/pkg/repoconfig"
	"github.com/spf13/cobra"
)

var rosterCmd = &cobra.Command{
	Use:   "roster",
	Short: "List work by epic and agent status",
	RunE: func(cmd *cobra.Command, args []string) error {
		var fullArgs []string
		if jsonOut, _ := cmd.Flags().GetBool("json"); jsonOut {
			fullArgs = append(fullArgs, "--json")
		}
		return cmdRoster(fullArgs)
	},
}

func init() {
	rosterCmd.Flags().Bool("json", false, "Output as JSON")
}

// rosterTowerConfigFunc is the indirection used by cmdRoster so tests
// can drive dispatch through a fake tower config without touching the
// real config dir. Production callers leave this alone.
var rosterTowerConfigFunc = activeTowerConfig

func cmdRoster(args []string) error {
	if d := resolveBeadsDir(); d != "" {
		os.Setenv("BEADS_DIR", d)
	}

	flagJSON := false
	for _, arg := range args {
		switch arg {
		case "--json":
			flagJSON = true
		default:
			return fmt.Errorf("unknown flag: %s\nusage: spire roster [--json]", arg)
		}
	}

	cwd, _ := os.Getwd()
	cfg, _ := repoconfig.Load(cwd)
	stale := 10 * time.Minute
	timeout := 15 * time.Minute
	if cfg != nil {
		if cfg.Agent.Stale != "" {
			if d, err := time.ParseDuration(cfg.Agent.Stale); err == nil {
				stale = d
			}
		}
		if cfg.Agent.Timeout != "" {
			if d, err := time.ParseDuration(cfg.Agent.Timeout); err == nil {
				timeout = d
			}
		}
	}
	_ = stale

	rosterDeps := board.RosterDeps{
		LoadWizardRegistry: func() ([]board.LocalAgent, error) {
			return agent.RegistryList()
		},
		SaveWizardRegistry: func(agents []board.LocalAgent) {
			saveWizardRegistry(wizardRegistry{Wizards: agents})
		},
		CleanDeadWizards: func(agents []board.LocalAgent) []board.LocalAgent {
			// Filter out dead entries for display. OrphanSweep handles bead-level
			// cleanup; this only prunes entries whose PID is no longer alive.
			var live []board.LocalAgent
			for _, w := range agents {
				if w.PID > 0 && processAlive(w.PID) {
					live = append(live, w)
				}
			}
			return live
		},
		ProcessAlive: func(pid int) bool {
			return dolt.ProcessAlive(pid)
		},
	}

	// Dispatch on the active tower's deployment mode rather than on
	// kubectl reachability + registry presence. Same switch shape as
	// the gateway handleRoster (spi-rx6bf6), cmdSummon / cmdDismiss
	// (spi-jsxa3v), and the steward orphan gate (spi-40rtru): the
	// tower's declared topology, not whatever environment happens to
	// respond first, decides the source.
	tower, err := rosterTowerConfigFunc()
	if err != nil {
		return fmt.Errorf("roster: resolve active tower: %w", err)
	}
	mode := tower.EffectiveDeploymentMode()
	agents, err := board.LiveRoster(context.Background(), mode, timeout, rosterDeps)
	if err != nil {
		return fmt.Errorf("roster: %w", err)
	}
	agents = board.EnrichRosterAgents(agents)
	summary := board.BuildSummary(agents, timeout)
	if flagJSON {
		return board.JSONOut(summary)
	}
	board.PrintRoster(summary)
	return nil
}
