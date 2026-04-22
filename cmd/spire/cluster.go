// cluster.go defines the `spire cluster` cobra parent and its cluster-only
// subcommands. These commands run inside cluster pods (init containers,
// Jobs) — not in agent sessions on a laptop — so they never appear in any
// SPIRE_ROLE catalog in pkg/scaffold/. The dispatcher is deliberately
// skeletal: upcoming cluster-only verbs will land here as siblings to
// cache-bootstrap.
package main

import (
	"github.com/spf13/cobra"
)

var clusterCmd = &cobra.Command{
	Use:   "cluster",
	Short: "Cluster-only commands (init containers, Jobs, reconciler helpers)",
	Long: `Cluster-only commands.

Verbs under "spire cluster" are invoked from inside cluster workloads —
the wizard pod's init container, reconciler-managed Jobs, and similar
cluster-internal entrypoints — not from agent sessions. They are grouped
here to keep cluster-internal surface out of the laptop/agent CLI.

Current subcommands:
  cache-bootstrap   Materialize a writable workspace from the guild repo
                    cache and bind it locally (wizard pod init container).`,
}

func init() {
	rootCmd.AddCommand(clusterCmd)
}
