package main

import (
	"fmt"

	"github.com/awell-health/spire/pkg/observability"
	"github.com/spf13/cobra"
)

var metricsCmd = &cobra.Command{
	Use:   "metrics",
	Short: "Agent run metrics (--bead, --model, --phase, --json)",
	RunE: func(cmd *cobra.Command, args []string) error {
		var fullArgs []string
		if jsonOut, _ := cmd.Flags().GetBool("json"); jsonOut {
			fullArgs = append(fullArgs, "--json")
		}
		if model, _ := cmd.Flags().GetBool("model"); model {
			fullArgs = append(fullArgs, "--model")
		}
		if phase, _ := cmd.Flags().GetBool("phase"); phase {
			fullArgs = append(fullArgs, "--phase")
		}
		if v, _ := cmd.Flags().GetString("bead"); v != "" {
			fullArgs = append(fullArgs, "--bead", v)
		}
		return cmdMetrics(fullArgs)
	},
}

func init() {
	metricsCmd.Flags().Bool("json", false, "Output as JSON")
	metricsCmd.Flags().Bool("model", false, "Show model breakdown")
	metricsCmd.Flags().Bool("phase", false, "Show per-phase breakdown")
	metricsCmd.Flags().String("bead", "", "Show metrics for a specific bead")
}

func cmdMetrics(args []string) error {
	var (
		flagJSON  bool
		flagBead  string
		flagModel bool
		flagPhase bool
	)
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--json":
			flagJSON = true
		case "--model":
			flagModel = true
		case "--phase":
			flagPhase = true
		case "--bead":
			if i+1 >= len(args) {
				return fmt.Errorf("--bead requires a value")
			}
			i++
			flagBead = args[i]
		default:
			return fmt.Errorf("unknown flag: %s\nusage: spire metrics [--bead <id>] [--model] [--phase] [--json]", args[i])
		}
	}

	if flagBead != "" {
		return observability.MetricsBead(flagBead, flagJSON)
	}
	if flagModel {
		return observability.MetricsModel(flagJSON)
	}
	if flagPhase {
		return observability.MetricsPhase(flagJSON)
	}
	return observability.MetricsSummary(flagJSON)
}
