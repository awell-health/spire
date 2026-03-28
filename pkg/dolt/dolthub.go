package dolt

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// NormalizeDolthubURL expands a short "org/repo" form to the full DoltHub API URL.
// Full URLs (http/https) are returned unchanged.
func NormalizeDolthubURL(url string) string {
	if strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://") {
		return url
	}
	return "https://doltremoteapi.dolthub.com/" + url
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
