package dolt

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/awell-health/spire/pkg/config"
)

// CLIPush runs `dolt push origin main` directly from the database data
// directory, inheriting the caller's environment so DOLT_REMOTE_USER /
// DOLT_REMOTE_PASSWORD are available. The context controls the command
// deadline — use context.WithTimeout to prevent indefinite hangs.
//
// gateway-mode: rejected with ErrGatewayDirectMutation. CLI push/pull/sync
// are gated upstream (config.RejectIfGateway in cmd/spire), and the daemon
// runDoltSync skips gateway-mode towers; this guard is defense-in-depth so
// any caller that reaches CLIPush directly still fails closed instead of
// pushing the laptop's local Dolt to a DoltHub remote that the cluster
// owns in cluster-as-truth deployments.
func CLIPush(ctx context.Context, dataDir string, force bool) error {
	if err := config.EnsureNotGatewayResolved("dolt.CLIPush"); err != nil {
		return err
	}
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
//
// gateway-mode: no-op. Configuring a CLI remote on a gateway-mode tower would
// stage credentials/state for a direct push that EnsureNotGateway would
// subsequently reject; better to short-circuit here so the laptop's
// .dolt/config.json is not mutated for towers the cluster owns.
func SetCLIRemote(dataDir, name, url string) {
	if err := config.EnsureNotGatewayResolved("dolt.SetCLIRemote"); err != nil {
		return
	}
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
