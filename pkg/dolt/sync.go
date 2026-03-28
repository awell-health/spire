package dolt

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// CLIFetchMerge performs a three-way merge by running dolt fetch followed
// by dolt merge. Unlike dolt pull (fast-forward only), this can reconcile
// diverged histories by creating a merge commit. Returns the merge output.
func CLIFetchMerge(dataDir string) (string, error) {
	bin := Bin()
	if bin == "" {
		return "", fmt.Errorf("dolt not found — run spire up to install")
	}

	env := os.Environ()

	// Step 1: fetch remote commits into remotes/origin/main.
	fetchCmd := exec.Command(bin, "fetch", "origin", "main")
	fetchCmd.Dir = dataDir
	fetchCmd.Env = env
	fetchOut, err := fetchCmd.CombinedOutput()
	if err != nil {
		return strings.TrimSpace(string(fetchOut)), fmt.Errorf("dolt fetch: %w\n%s", err, strings.TrimSpace(string(fetchOut)))
	}

	// Step 2: three-way merge into current branch.
	mergeCmd := exec.Command(bin, "merge", "remotes/origin/main")
	mergeCmd.Dir = dataDir
	mergeCmd.Env = env
	mergeOut, err := mergeCmd.CombinedOutput()
	output := strings.TrimSpace(string(mergeOut))
	if err != nil {
		return output, fmt.Errorf("dolt merge: %w\n%s", err, output)
	}
	return output, nil
}
