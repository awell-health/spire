package main

import (
	"fmt"
	"os"
	"time"

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
		LoadWizardRegistry: func() []board.LocalAgent {
			reg := loadWizardRegistry()
			return reg.Wizards
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

	// Try k8s first.
	if agents, err := board.RosterFromK8s(timeout); err == nil && len(agents) > 0 {
		agents = board.EnrichRosterAgents(agents)
		summary := board.BuildSummary(agents, timeout)
		if flagJSON {
			return board.JSONOut(summary)
		}
		board.PrintRoster(summary)
		return nil
	}

	// Local wizards from wizard registry.
	if localAgents := board.RosterFromLocalWizards(timeout, rosterDeps); len(localAgents) > 0 {
		localAgents = board.EnrichRosterAgents(localAgents)
		summary := board.BuildSummary(localAgents, timeout)
		if flagJSON {
			return board.JSONOut(summary)
		}
		board.PrintRoster(summary)
		return nil
	}

	// Fallback: beads-based roster.
	agents := board.RosterFromBeads(timeout)
	agents = board.EnrichRosterAgents(agents)
	summary := board.BuildSummary(agents, timeout)
	if flagJSON {
		return board.JSONOut(summary)
	}
	board.PrintRoster(summary)
	return nil
}
