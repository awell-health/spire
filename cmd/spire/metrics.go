package main

import (
	"fmt"

	"github.com/awell-health/spire/pkg/observability"
)

func cmdMetrics(args []string) error {
	var (
		flagJSON  bool
		flagBead  string
		flagModel bool
	)
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--json":
			flagJSON = true
		case "--model":
			flagModel = true
		case "--bead":
			if i+1 >= len(args) {
				return fmt.Errorf("--bead requires a value")
			}
			i++
			flagBead = args[i]
		default:
			return fmt.Errorf("unknown flag: %s\nusage: spire metrics [--bead <id>] [--model] [--json]", args[i])
		}
	}

	if flagBead != "" {
		return observability.MetricsBead(flagBead, flagJSON)
	}
	if flagModel {
		return observability.MetricsModel(flagJSON)
	}
	return observability.MetricsSummary(flagJSON)
}
