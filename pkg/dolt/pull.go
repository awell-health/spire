package dolt

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/awell-health/spire/pkg/config"
)

// CLIPull runs `dolt pull origin main` directly from the database data
// directory, inheriting the caller's environment so DOLT_REMOTE_USER /
// DOLT_REMOTE_PASSWORD are available. The context controls the command
// deadline — use context.WithTimeout to prevent indefinite hangs.
//
// gateway-mode: rejected with ErrGatewayDirectMutation. Defense-in-depth
// against the laptop daemon or a stray CLI path reaching CLIPull on a tower
// where the cluster owns the canonical Dolt state.
func CLIPull(ctx context.Context, dataDir string, force bool) error {
	if err := config.EnsureNotGatewayResolved("dolt.CLIPull"); err != nil {
		return err
	}
	bin := Bin()
	if bin == "" {
		return fmt.Errorf("dolt not found — run spire up to install")
	}

	args := []string{"pull", "origin", "main"}
	if force {
		args = []string{"pull", "--force", "origin", "main"}
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
