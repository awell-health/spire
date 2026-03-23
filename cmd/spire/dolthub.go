package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// normalizeDolthubURL expands a short "org/repo" form to the full DoltHub API URL.
// Full URLs (http/https) are returned unchanged.
func normalizeDolthubURL(url string) string {
	if strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://") {
		return url
	}
	return "https://doltremoteapi.dolthub.com/" + url
}

// readBeadsDBName reads the dolt_database field from .beads/metadata.json,
// which is the actual database name bd uses to connect to the dolt server.
func readBeadsDBName() string {
	cwd, err := realCwd()
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

// parseOriginURL extracts the URL for the 'origin' remote from 'bd dolt remote list' output.
func parseOriginURL(out string) string {
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

// resolveDataDir returns the dolt data directory for the current beads database.
func resolveDataDir() (string, error) {
	dbName := readBeadsDBName()
	if dbName == "" {
		return "", fmt.Errorf("could not determine database name — run from a directory with .beads/")
	}
	dataDir := filepath.Join(doltDataDir(), dbName)
	if _, err := os.Stat(dataDir); err != nil {
		return "", fmt.Errorf("database directory not found: %s", dataDir)
	}
	return dataDir, nil
}
