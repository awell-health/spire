// gateway.go — CLI client for talking to a pkg/gateway.Server.
//
// The server side lives in pkg/gateway and is started by `spire daemon
// --serve`. This file is the laptop-side counterpart: it's what a user
// or a wrapper script calls to ask a remote gateway to trigger a sync
// immediately (so the cluster notices new work within seconds instead
// of waiting for the next daemon tick).
//
// Boundaries kept deliberately thin: this is just HTTP plumbing. No
// sync logic, no debounce, no ticker. All of that is on the server side.
package main

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

var gatewayCmd = &cobra.Command{
	Use:   "gateway",
	Short: "Talk to a Spire gateway (send a sync trigger, check health)",
}

var gatewaySyncCmd = &cobra.Command{
	Use:   "sync <gateway-url>",
	Short: "Ask the gateway to run a sync now (POST /sync)",
	Long: `POST to <gateway-url>/sync. The gateway forwards the trigger to
its Daemon; response is 200 "triggered" or 202 "skipped: <reason>" when
the daemon is in-progress or within its debounce window.

Example:
  spire gateway sync http://spire-syncer.spire-test.svc:8082
  spire gateway sync http://localhost:8082 --reason=spire-ready
`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		reason, _ := cmd.Flags().GetString("reason")
		timeout, _ := cmd.Flags().GetDuration("timeout")
		return gatewaySend(args[0], reason, timeout)
	},
}

var gatewayPingCmd = &cobra.Command{
	Use:   "ping <gateway-url>",
	Short: "GET <gateway-url>/healthz",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		timeout, _ := cmd.Flags().GetDuration("timeout")
		return gatewayPing(args[0], timeout)
	},
}

func init() {
	gatewaySyncCmd.Flags().String("reason", "cli", "Optional reason string, logged by the gateway")
	gatewaySyncCmd.Flags().Duration("timeout", 10*time.Second, "Request timeout")
	gatewayPingCmd.Flags().Duration("timeout", 5*time.Second, "Request timeout")
	gatewayCmd.AddCommand(gatewaySyncCmd, gatewayPingCmd)
}

// gatewaySend POSTs to <base>/sync?reason=... and prints the response body.
// Exit codes: 0 on 2xx, 1 on 4xx/5xx or transport errors.
func gatewaySend(base, reason string, timeout time.Duration) error {
	target, err := joinURL(base, "/sync")
	if err != nil {
		return err
	}
	q := url.Values{}
	q.Set("reason", reason)
	target.RawQuery = q.Encode()

	req, err := http.NewRequest(http.MethodPost, target.String(), nil)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("POST %s: %w", target, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	fmt.Fprint(os.Stdout, strings.TrimRight(string(body), "\n")+"\n")
	if resp.StatusCode >= 400 {
		return fmt.Errorf("gateway returned %d", resp.StatusCode)
	}
	return nil
}

func gatewayPing(base string, timeout time.Duration) error {
	target, err := joinURL(base, "/healthz")
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: timeout}
	resp, err := client.Get(target.String())
	if err != nil {
		return fmt.Errorf("GET %s: %w", target, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	fmt.Fprint(os.Stdout, strings.TrimRight(string(body), "\n")+"\n")
	if resp.StatusCode >= 400 {
		return fmt.Errorf("gateway returned %d", resp.StatusCode)
	}
	return nil
}

func joinURL(base, path string) (*url.URL, error) {
	u, err := url.Parse(base)
	if err != nil {
		return nil, fmt.Errorf("parse %q: %w", base, err)
	}
	if u.Scheme == "" {
		u.Scheme = "http"
	}
	u.Path = strings.TrimRight(u.Path, "/") + path
	return u, nil
}
