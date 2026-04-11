package main

import (
	"fmt"
	"os"
	"strings"

	"golang.org/x/mod/semver"

	"github.com/awell-health/spire/pkg/store"
)

const configKeySpireVersion = "spire_version"

// versionAction describes what to do after comparing stored and binary versions.
type versionAction struct {
	skipMigrations bool
	writeVersion   bool
	warn           bool
	storedVersion  string
	binaryVersion  string
}

// normalizeVersion ensures the version string has a "v" prefix for semver compatibility.
func normalizeVersion(v string) string {
	if v == "" {
		return ""
	}
	if !strings.HasPrefix(v, "v") {
		return "v" + v
	}
	return v
}

// isValidSemver returns true if the version is valid semver (after normalization).
// Returns false for empty strings, "dev", and other non-semver values.
func isValidSemver(v string) bool {
	if v == "" || v == "dev" {
		return false
	}
	return semver.IsValid(normalizeVersion(v))
}

// decideVersionAction compares the stored database version with the binary version
// and returns the appropriate action:
//   - stored missing or invalid → run migrations, write binary version
//   - stored == binary → skip migrations (schema is up to date)
//   - stored < binary → run migrations, write binary version (upgrade)
//   - stored > binary → skip migrations, warn (binary is behind)
//   - invalid binary version (dev build) → run migrations, don't write
func decideVersionAction(storedVersion string) versionAction {
	binaryVer := normalizeVersion(version)

	// Dev/empty binary version — can't do anything meaningful.
	// Run migrations (safe, idempotent) but don't write garbage to config.
	if !isValidSemver(version) {
		return versionAction{}
	}

	// No stored version or invalid — first run or corrupted.
	if storedVersion == "" || !isValidSemver(storedVersion) {
		return versionAction{
			writeVersion:  true,
			binaryVersion: binaryVer,
		}
	}

	storedVer := normalizeVersion(storedVersion)

	switch semver.Compare(storedVer, binaryVer) {
	case 0:
		// Exact match — schema is up to date, skip migrations.
		return versionAction{
			skipMigrations: true,
			binaryVersion:  binaryVer,
			storedVersion:  storedVer,
		}
	case 1:
		// Stored > binary — a newer Spire already ran migrations.
		// Skip migrations (they're a subset), warn the user.
		return versionAction{
			skipMigrations: true,
			warn:           true,
			binaryVersion:  binaryVer,
			storedVersion:  storedVer,
		}
	case -1:
		// Stored < binary — normal upgrade path.
		// Run migrations and write the new version.
		return versionAction{
			writeVersion:  true,
			binaryVersion: binaryVer,
			storedVersion: storedVer,
		}
	}

	return versionAction{}
}

// readSpireVersion reads the stored spire version from the config table for a tower.
// Opens and resets the store. Returns "" on any error (graceful degradation).
func readSpireVersion(beadsDir string) string {
	if _, err := store.OpenAt(beadsDir); err != nil {
		return ""
	}
	defer store.Reset()

	val, err := store.GetConfig(configKeySpireVersion)
	if err != nil {
		return ""
	}
	return val
}

// writeSpireVersion writes the given version to the config table for a tower.
// Opens and resets the store. Errors are logged to stderr but not propagated
// (version tracking is advisory and must never block startup).
func writeSpireVersion(beadsDir, ver string) {
	if _, err := store.OpenAt(beadsDir); err != nil {
		return
	}
	defer store.Reset()

	if err := store.SetConfig(configKeySpireVersion, ver); err != nil {
		fmt.Fprintf(os.Stderr, "  warning: failed to write spire version: %s\n", err)
	}
}
