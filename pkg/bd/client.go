package bd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Client wraps subprocess calls to the bd binary.
type Client struct {
	// BinPath is the path to the bd binary. Defaults to "bd".
	BinPath string

	// BeadsDir points bd at a specific .beads/ directory via the BEADS_DIR
	// env var. This is the native mechanism bd uses to locate its config
	// (search order: BEADS_DIR → worktree resolution → walk up from cwd).
	BeadsDir string

	// Verbose enables command logging. Defaults to SPIRE_BD_LOG env var.
	Verbose bool

	// Logger is used for verbose logging. Defaults to the standard logger.
	Logger *log.Logger
}

// NewClient creates a Client with defaults.
// Verbose is enabled when SPIRE_BD_LOG is set.
func NewClient() *Client {
	return &Client{
		BinPath: "bd",
		Verbose: os.Getenv("SPIRE_BD_LOG") != "",
		Logger:  log.Default(),
	}
}

// defaultClient is the package-level client used by convenience functions.
var defaultClient = NewClient()

// DefaultClient returns the package-level default client.
func DefaultClient() *Client {
	return defaultClient
}

// exec runs a bd command and returns trimmed stdout.
// Stderr is included in the error on failure.
func (c *Client) exec(args ...string) (string, error) {
	label := c.BinPath + " " + strings.Join(args, " ")
	if c.Verbose {
		c.Logger.Printf("[bd] exec: %s", label)
	}
	start := time.Now()

	cmd := exec.Command(c.BinPath, args...)
	if c.BeadsDir != "" {
		cmd.Env = append(os.Environ(), "BEADS_DIR="+c.BeadsDir)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()

	dur := time.Since(start).Seconds()
	if err != nil {
		errStr := strings.TrimSpace(stderr.String())
		if c.Verbose {
			c.Logger.Printf("[bd] FAIL (%.1fs): %s — %s", dur, label, errStr)
		}
		return "", fmt.Errorf("bd %s: %s\n%s", strings.Join(args, " "), err, errStr)
	}

	out := strings.TrimSpace(stdout.String())
	if c.Verbose {
		c.Logger.Printf("[bd] OK (%.1fs): %s — %d bytes", dur, label, len(out))
	}
	return out, nil
}

// execJSON runs a bd command with --json and unmarshals the output into result.
func (c *Client) execJSON(result any, args ...string) error {
	args = append(args, "--json")
	out, err := c.exec(args...)
	if err != nil {
		return err
	}
	if out == "" {
		return nil
	}
	return json.Unmarshal([]byte(out), result)
}

// execSilent runs a bd command with --silent and returns the trimmed output.
func (c *Client) execSilent(args ...string) (string, error) {
	args = append(args, "--silent")
	return c.exec(args...)
}
