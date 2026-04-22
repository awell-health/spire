package main

import (
	"context"
	"fmt"
	"os"

	"github.com/awell-health/spire/pkg/agent"
	"github.com/spf13/cobra"
)

// cacheBootstrapCmd is the `spire cluster cache-bootstrap` entrypoint the
// wizard pod's init container invokes to derive a writable per-pod
// workspace from the guild-owned repo cache and perform the local repo
// bind. Exposing it as a CLI subcommand (rather than expecting the init
// container to shell-script the equivalent work) keeps the observability
// vocabulary and marker-file semantics from pkg/agent authoritative —
// the Go helpers own both contracts, and the init container just calls
// them in sequence.
//
// The phase-2 cluster repo-cache contract (spi-sn7o3) is: mount the
// guild cache read-only at --cache-path, mount a writable emptyDir at
// --workspace-path, then run this command before the main container
// starts. The main container then finds the repo substrate at
// WorkspaceMountPath with all the canonical runtime identity env already
// populated by the operator's pod builder.
var cacheBootstrapCmd = &cobra.Command{
	Use:   "cache-bootstrap",
	Short: "Materialize a writable workspace from the guild repo cache and bind it locally",
	Long: `cache-bootstrap is invoked by the wizard pod's init container. It runs
agent.MaterializeWorkspaceFromCache(cache-path, workspace-path, prefix) to clone
the read-only cache into a writable workspace, then agent.BindLocalRepo(workspace-path, prefix)
to register the checkout in the tower's LocalBindings so wizard.ResolveRepo
succeeds when the main container starts.

Flags default to pkg/agent.CacheMountPath / WorkspaceMountPath and
$SPIRE_REPO_PREFIX so the init container invocation stays terse.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		cachePath, _ := cmd.Flags().GetString("cache-path")
		workspacePath, _ := cmd.Flags().GetString("workspace-path")
		prefix, _ := cmd.Flags().GetString("prefix")
		if prefix == "" {
			prefix = os.Getenv("SPIRE_REPO_PREFIX")
		}
		if prefix == "" {
			return fmt.Errorf("--prefix is required (or SPIRE_REPO_PREFIX env)")
		}

		ctx := context.Background()
		if err := agent.MaterializeWorkspaceFromCache(ctx, cachePath, workspacePath, prefix); err != nil {
			return fmt.Errorf("materialize workspace from cache: %w", err)
		}
		if err := agent.BindLocalRepo(ctx, workspacePath, prefix); err != nil {
			return fmt.Errorf("bind local repo: %w", err)
		}
		return nil
	},
}

func init() {
	cacheBootstrapCmd.Flags().String("cache-path", agent.CacheMountPath, "Read-only guild cache mount path")
	cacheBootstrapCmd.Flags().String("workspace-path", agent.WorkspaceMountPath, "Writable workspace mount path")
	cacheBootstrapCmd.Flags().String("prefix", "", "Canonical repo prefix (defaults to $SPIRE_REPO_PREFIX)")
	clusterCmd.AddCommand(cacheBootstrapCmd)
}
