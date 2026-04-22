//go:build e2e

package helpers

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"testing"
	"time"
)

// DoltRemotePort is the in-cluster Service port the dolt StatefulSet
// listens on. Stable across chart versions; mirrors test/smoke.
const DoltRemotePort = 3307

// PortForwardDolt starts a background `kubectl port-forward svc/spire-dolt`
// on a free local port, waits briefly for the forward to establish, and
// returns the local port + cancel func. The cancel is registered via
// t.Cleanup by the caller (seedFixture), so tests do not need to invoke
// it directly.
func PortForwardDolt(t *testing.T, namespace string) (localPort int, cancel func()) {
	t.Helper()
	local := FreePort(t)

	ctx, cancelFn := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, "kubectl", "port-forward",
		"-n", namespace,
		"svc/spire-dolt",
		fmt.Sprintf("%d:%d", local, DoltRemotePort),
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		cancelFn()
		t.Fatalf("kubectl port-forward svc/spire-dolt in %s: %v", namespace, err)
	}

	// kubectl port-forward takes ~1s to bind the local socket. Poll the
	// local port briefly so we don't race subsequent sql.Open calls.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		conn, derr := net.DialTimeout("tcp",
			fmt.Sprintf("127.0.0.1:%d", local), 200*time.Millisecond)
		if derr == nil {
			conn.Close()
			break
		}
		time.Sleep(200 * time.Millisecond)
	}

	return local, func() {
		cancelFn()
		_ = cmd.Wait()
	}
}

// FreePort returns an ephemeral TCP port bound to 127.0.0.1. The
// listener is closed before returning, so the port may be reused by
// subsequent calls. For test scoping this is acceptable: the port is
// consumed by the port-forward immediately.
func FreePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("allocate free port: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}
