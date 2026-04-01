package dolt

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// SQL runs a SQL query against the Dolt server and returns the output.
// Uses dolt CLI with connection parameters from environment.
// dbName is the database to query against; if empty, detectDBFn is called.
// detectDBFn is an optional callback to resolve the database name when dbName is empty.
func SQL(query string, jsonOutput bool, dbName string, detectDBFn func() (string, error)) (string, error) {
	host := os.Getenv("BEADS_DOLT_SERVER_HOST")
	if host == "" {
		host = "127.0.0.1"
	}
	port := os.Getenv("BEADS_DOLT_SERVER_PORT")
	if port == "" {
		port = "3307"
	}

	if dbName == "" && detectDBFn != nil {
		var err error
		dbName, err = detectDBFn()
		if err != nil {
			return "", fmt.Errorf("resolve database: %w", err)
		}
	}
	if dbName == "" {
		return "", fmt.Errorf("no database name provided and no detection function available")
	}

	args := []string{
		"--host", host,
		"--port", port,
		"--user", "root",
		"--no-tls",
		"--use-db", dbName,
		"sql", "-q", query,
	}
	if jsonOutput {
		args = append(args, "-r", "json")
	}

	cmd := exec.Command(Bin(), args...)
	cmd.Env = append(os.Environ(), "DOLT_CLI_PASSWORD=")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		return "", fmt.Errorf("dolt sql: %s\n%s", err, stderr.String())
	}
	return strings.TrimSpace(stdout.String()), nil
}

// LocalQuery runs a SQL query directly against a dolt database directory,
// without connecting to the server. Used during tower attach when the server
// hasn't loaded the freshly cloned database yet.
func LocalQuery(dataDir, query string) (string, error) {
	cmd := exec.Command(Bin(), "sql", "-q", query)
	cmd.Dir = dataDir
	// Strip credential env vars so dolt uses the local embedded engine
	// instead of trying password auth against a server.
	var env []string
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, "DOLT_CLI_PASSWORD=") ||
			strings.HasPrefix(e, "DOLT_REMOTE_PASSWORD=") ||
			strings.HasPrefix(e, "DOLT_REMOTE_USER=") {
			continue
		}
		env = append(env, e)
	}
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("dolt sql (local): %w\n%s", err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

// RawQuery runs a SQL query against the dolt server without --use-db.
// For bootstrap contexts (tower attach) where no ambient database context exists.
// Queries must use fully-qualified table names (e.g. `dbname`.table).
func RawQuery(query string) (string, error) {
	cmd := exec.Command(Bin(),
		"--host", Host(), "--port", Port(),
		"--user", "root", "--no-tls",
		"sql", "-q", query)
	cmd.Env = append(os.Environ(), "DOLT_CLI_PASSWORD=")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("dolt sql: %w\n%s", err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}
