package main

import (
	"fmt"

	"github.com/awell-health/spire/pkg/config"
	"github.com/spf13/cobra"
)

// cmdRepoBindLocal implements `spire repo bind-local` — the local-only
// counterpart to `spire repo bind`. It differs in one critical way: it
// takes repo URL and branch explicitly on the command line instead of
// reading them from the shared dolt repos table. It writes only to the
// tower config (LocalBindings) and the global config (Instances). It
// never touches the shared repos table.
//
// This is the hook used by the wizard pod's repo-bootstrap init container:
// the pod has dolt-access credentials only for the bead/message flow, and
// the repo URL/branch are already supplied by the steward via env vars.
// Going back through the shared repos table here would be a circular
// dependency (and would break in the offline / local-test case where the
// shared dolt is not reachable from the init container before ResolveRepo
// is satisfied).
func cmdRepoBindLocal(prefix, localPath, repoURL, branch string) error {
	if prefix == "" {
		return fmt.Errorf("--prefix is required")
	}
	if localPath == "" {
		return fmt.Errorf("--path is required")
	}
	if repoURL == "" {
		return fmt.Errorf("--repo-url is required")
	}
	if branch == "" {
		return fmt.Errorf("--branch is required")
	}

	cfg, err := loadConfig()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	tower, err := config.ResolveTowerConfig()
	if err != nil {
		return fmt.Errorf("resolve tower: %w", err)
	}
	if tower.LocalBindings == nil {
		tower.LocalBindings = make(map[string]*config.LocalRepoBinding)
	}

	if err := performRepoBind(tower, cfg, prefix, localPath, repoURL, branch); err != nil {
		return err
	}
	if err := config.SaveTowerConfig(tower); err != nil {
		return err
	}
	if err := saveConfig(cfg); err != nil {
		return err
	}

	absPath := tower.LocalBindings[prefix].LocalPath
	fmt.Printf("Bound prefix %s → %s (local-only)\n", prefix, absPath)
	return nil
}

var repoBindLocalCmd = &cobra.Command{
	Use:    "bind-local",
	Short:  "Bind a local checkout using explicit repo URL/branch without touching shared state",
	Hidden: true, // internal: used by the wizard pod's repo-bootstrap init container.
	Long: `bind-local is the local-only counterpart to 'spire repo bind'. It takes
repo URL and branch as explicit flags instead of reading them from the shared
dolt repos table, and writes only to local tower/global config — it does not
call repo add or modify the shared repos table.

Intended use: the wizard pod repo-bootstrap init container clones the repo
into /workspace/<prefix> and then invokes bind-local to make the checkout
discoverable by ResolveRepo.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		prefix, _ := cmd.Flags().GetString("prefix")
		path, _ := cmd.Flags().GetString("path")
		repoURL, _ := cmd.Flags().GetString("repo-url")
		branch, _ := cmd.Flags().GetString("branch")
		return cmdRepoBindLocal(prefix, path, repoURL, branch)
	},
}

func init() {
	repoBindLocalCmd.Flags().String("prefix", "", "Repo prefix (required)")
	repoBindLocalCmd.Flags().String("path", "", "Local checkout path (required)")
	repoBindLocalCmd.Flags().String("repo-url", "", "Git remote URL (required)")
	repoBindLocalCmd.Flags().String("branch", "", "Default branch (required)")
	repoCmd.AddCommand(repoBindLocalCmd)
}
