package dolt

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/awell-health/spire/pkg/config"
)

// NormalizeDolthubURL expands a short "org/repo" form to the full DoltHub API URL.
// Full URLs (http/https) are returned unchanged.
func NormalizeDolthubURL(url string) string {
	if strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://") {
		return url
	}
	return "https://doltremoteapi.dolthub.com/" + url
}

// ClassifyRemoteURL decides whether a remote URL points at DoltHub (the hosted
// service, authed with JWK-signed tokens) or at a self-hosted dolt-sql-server
// reachable over the remotesapi gRPC port (authed with MySQL-style creds).
//
// Rules:
//   - short form "org/repo" (no scheme, no ':')          → dolthub
//   - scheme dolt://                                      → remotesapi
//   - http(s)://doltremoteapi.dolthub.com/...             → dolthub
//   - http(s)://(www.)?dolthub.com/...                    → dolthub
//   - any other http(s)://host[:port]/path                → remotesapi
//   - empty / malformed input                             → error
//
// The classifier keys on scheme + host, not port — port-forwarded cluster URLs
// (localhost:50051) and in-cluster DNS URLs must both work.
func ClassifyRemoteURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("remote URL is empty")
	}

	if !strings.Contains(raw, "://") {
		if strings.Contains(raw, "/") && !strings.ContainsAny(raw, ": ") {
			return config.RemoteKindDoltHub, nil
		}
		return "", fmt.Errorf("remote URL %q is missing a scheme — use http://, https://, or dolt://, or the short form \"org/repo\" for DoltHub", raw)
	}

	u, err := neturl.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("parse remote URL %q: %w", raw, err)
	}

	switch strings.ToLower(u.Scheme) {
	case "dolt":
		return config.RemoteKindRemotesAPI, nil
	case "http", "https":
		host := strings.ToLower(u.Hostname())
		if host == "doltremoteapi.dolthub.com" || host == "www.dolthub.com" || host == "dolthub.com" {
			return config.RemoteKindDoltHub, nil
		}
		if host == "" {
			return "", fmt.Errorf("remote URL %q has no host", raw)
		}
		return config.RemoteKindRemotesAPI, nil
	default:
		return "", fmt.Errorf("unsupported remote URL scheme %q in %q — use http://, https://, or dolt://", u.Scheme, raw)
	}
}

// NormalizeRemoteURL normalizes a URL based on its classified kind. For DoltHub
// it delegates to NormalizeDolthubURL (the legacy short-form expander). For
// remotesapi it trims trailing slashes so "http://h:50051/db" and
// "http://h:50051/db/" resolve the same; the scheme is preserved — dolt CLI
// accepts both http(s):// and dolt://.
func NormalizeRemoteURL(raw, kind string) string {
	raw = strings.TrimSpace(raw)
	switch kind {
	case config.RemoteKindDoltHub:
		return NormalizeDolthubURL(raw)
	case config.RemoteKindRemotesAPI:
		return strings.TrimRight(raw, "/")
	}
	return raw
}

// DatabaseFromRemoteURL extracts the database name from a remotesapi URL.
// For "http://host:50051/spi" → "spi". Returns "" if no path component.
func DatabaseFromRemoteURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if !strings.Contains(raw, "://") {
		// short form "org/repo" — take last path segment
		parts := strings.Split(strings.Trim(raw, "/"), "/")
		return parts[len(parts)-1]
	}
	u, err := neturl.Parse(raw)
	if err != nil {
		return ""
	}
	p := strings.Trim(u.Path, "/")
	if p == "" {
		return ""
	}
	// take the last path segment in case the URL has /api/v1/<db> or similar
	parts := strings.Split(p, "/")
	return parts[len(parts)-1]
}

// ReadBeadsDBName reads the dolt_database field from .beads/metadata.json,
// which is the actual database name bd uses to connect to the dolt server.
// cwdFn provides the current working directory (to allow caller injection).
func ReadBeadsDBName(cwdFn func() (string, error)) string {
	cwd, err := cwdFn()
	if err != nil {
		return ""
	}
	for dir := cwd; ; dir = filepath.Dir(dir) {
		meta := filepath.Join(dir, ".beads", "metadata.json")
		if data, err := os.ReadFile(meta); err == nil {
			var m struct {
				DoltDatabase string `json:"dolt_database"`
			}
			if err := json.Unmarshal(data, &m); err == nil && m.DoltDatabase != "" {
				return m.DoltDatabase
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
	}
	return ""
}

// ParseOriginURL extracts the URL for the 'origin' remote from 'bd dolt remote list' output.
func ParseOriginURL(out string) string {
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "origin") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				return fields[1]
			}
		}
	}
	return ""
}

// ResolveDataDir returns the dolt data directory for the current beads database.
// cwdFn provides the current working directory (to allow caller injection).
func ResolveDataDir(cwdFn func() (string, error)) (string, error) {
	dbName := ReadBeadsDBName(cwdFn)
	if dbName == "" {
		return "", fmt.Errorf("could not determine database name — run from a directory with .beads/")
	}
	dataDir := filepath.Join(DataDir(), dbName)
	if _, err := os.Stat(dataDir); err != nil {
		return "", fmt.Errorf("database directory not found: %s", dataDir)
	}
	return dataDir, nil
}

// EnsureDoltHubDB creates the DoltHub database if it doesn't exist.
// Requires DOLT_REMOTE_PASSWORD env var for auth.
// Non-fatal: if the database already exists or creation fails, push will
// surface the real error.
func EnsureDoltHubDB(remoteURL string) error {
	suffix := strings.TrimPrefix(remoteURL, "https://doltremoteapi.dolthub.com/")
	parts := strings.SplitN(suffix, "/", 2)
	if len(parts) != 2 {
		return fmt.Errorf("cannot parse org/repo from URL: %s", remoteURL)
	}
	owner, repo := parts[0], parts[1]

	token := os.Getenv("DOLT_REMOTE_PASSWORD")
	if token == "" {
		return nil // no token — let push surface auth error
	}

	// Check if database already exists
	checkURL := fmt.Sprintf("https://www.dolthub.com/api/v1alpha1/%s/%s", owner, repo)
	req, _ := http.NewRequest("GET", checkURL, nil)
	req.Header.Set("Authorization", "token "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil // network issue — let push try anyway
	}
	resp.Body.Close()
	if resp.StatusCode == 200 {
		return nil // already exists
	}

	// Create it
	fmt.Printf("  Creating remote database %s/%s on DoltHub...\n", owner, repo)
	body := fmt.Sprintf(`{"ownerName":%q,"repoName":%q,"description":"Created by spire push","visibility":"private"}`,
		owner, repo)
	req, _ = http.NewRequest("POST", "https://www.dolthub.com/api/v1alpha1/database", strings.NewReader(body))
	req.Header.Set("Authorization", "token "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("create db request: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != 200 && resp.StatusCode != 201 {
		var errResp struct {
			Message string `json:"message"`
		}
		json.Unmarshal(respBody, &errResp)
		if errResp.Message != "" {
			return fmt.Errorf("create db: %s", errResp.Message)
		}
		return fmt.Errorf("create db: HTTP %d", resp.StatusCode)
	}

	fmt.Printf("  Created %s/%s\n", owner, repo)
	return nil
}
