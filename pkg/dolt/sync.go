package dolt

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/awell-health/spire/pkg/config"
)

// CLIFetchMerge performs a three-way merge by running dolt fetch followed
// by dolt merge. Unlike dolt pull (fast-forward only), this can reconcile
// diverged histories by creating a merge commit. Returns the merge output.
// The context controls the deadline for both fetch and merge subcommands.
//
// gateway-mode: rejected with ErrGatewayDirectMutation. Same rationale as
// CLIPush/CLIPull — even a fetch+merge mutates the laptop's local Dolt
// graph in a way that conflicts with the cluster-owned canonical state.
func CLIFetchMerge(ctx context.Context, dataDir string) (string, error) {
	if err := config.EnsureNotGatewayResolved("dolt.CLIFetchMerge"); err != nil {
		return "", err
	}
	bin := Bin()
	if bin == "" {
		return "", fmt.Errorf("dolt not found — run spire up to install")
	}

	env := os.Environ()

	// Step 1: fetch remote commits into remotes/origin/main.
	fetchCmd := exec.CommandContext(ctx, bin, "fetch", "origin", "main")
	fetchCmd.Dir = dataDir
	fetchCmd.Env = env
	fetchOut, err := fetchCmd.CombinedOutput()
	if err != nil {
		return strings.TrimSpace(string(fetchOut)), fmt.Errorf("dolt fetch: %w\n%s", err, strings.TrimSpace(string(fetchOut)))
	}

	// Step 2: three-way merge into current branch.
	mergeCmd := exec.CommandContext(ctx, bin, "merge", "remotes/origin/main")
	mergeCmd.Dir = dataDir
	mergeCmd.Env = env
	mergeOut, err := mergeCmd.CombinedOutput()
	output := strings.TrimSpace(string(mergeOut))
	if err != nil {
		return output, fmt.Errorf("dolt merge: %w\n%s", err, output)
	}
	return output, nil
}
