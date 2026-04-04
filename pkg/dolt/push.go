package dolt

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// CLIPush runs `dolt push origin main` directly from the database data
// directory, inheriting the caller's environment so DOLT_REMOTE_USER /
// DOLT_REMOTE_PASSWORD are available. The context controls the command
// deadline — use context.WithTimeout to prevent indefinite hangs.
func CLIPush(ctx context.Context, dataDir string, force bool) error {
	bin := Bin()
	if bin == "" {
		return fmt.Errorf("dolt not found — run spire up to install")
	}

	args := []string{"push", "origin", "main"}
	if force {
		args = []string{"push", "--force", "origin", "main"}
	}

	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dataDir
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w\n%s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// SetCLIRemote adds or updates a remote in the dolt CLI config (.dolt/config.json)
// inside the database data directory. This is separate from the SQL-level remote
// managed by bd, which lives in the dolt database tables.
func SetCLIRemote(dataDir, name, url string) {
	bin := Bin()
	if bin == "" {
		return
	}

	// Remove existing, ignore error
	removeCmd := exec.Command(bin, "remote", "remove", name)
	removeCmd.Dir = dataDir
	removeCmd.Env = os.Environ()
	removeCmd.Run() //nolint

	addCmd := exec.Command(bin, "remote", "add", name, url)
	addCmd.Dir = dataDir
	addCmd.Env = os.Environ()
	addCmd.Run() //nolint
}
