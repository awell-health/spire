package config

import (
	"errors"
	"os/exec"
	"runtime"
	"strings"
)

// KeychainSet stores a value in the system keychain.
// macOS: uses `security add-generic-password`
// Linux: uses `secret-tool store`
func KeychainSet(key, value string) error {
	if runtime.GOOS == "darwin" {
		return exec.Command("security", "add-generic-password",
			"-a", "spire", "-s", key, "-w", value, "-U").Run()
	}
	return exec.Command("secret-tool", "store",
		"--label=spire: "+key, "service", "spire", "key", key).Run()
}

// KeychainGet retrieves a value from the system keychain.
// Returns empty string on error (not found, no keychain, etc.).
func KeychainGet(key string) (string, error) {
	if runtime.GOOS == "darwin" {
		out, err := exec.Command("security", "find-generic-password",
			"-a", "spire", "-s", key, "-w").Output()
		return strings.TrimSpace(string(out)), err
	}
	out, err := exec.Command("secret-tool", "lookup",
		"service", "spire", "key", key).Output()
	return strings.TrimSpace(string(out)), err
}

// KeychainDelete removes a value from the system keychain.
func KeychainDelete(key string) error {
	if runtime.GOOS == "darwin" {
		return exec.Command("security", "delete-generic-password",
			"-a", "spire", "-s", key).Run()
	}
	// secret-tool doesn't have a direct delete; clear by storing empty
	return exec.Command("secret-tool", "store",
		"--label=spire: "+key, "service", "spire", "key", key).Run()
}

// ErrTokenNotFound is returned by GetTowerToken when no token is stored for a
// tower. Callers can use errors.Is(err, ErrTokenNotFound) to distinguish a
// missing token from a platform error (no keychain backend, exec failure, etc).
var ErrTokenNotFound = errors.New("tower token not found in keychain")

// towerKeychainService is the keychain service name used for tower API tokens.
// A namespace distinct from the general "spire" service keeps tower tokens
// listable/manageable separately from other Spire credentials.
const towerKeychainService = "spire-tower"

// towerTokenAccount derives the keychain account name for a tower's API token.
// Per-tower accounts let multiple attached towers coexist (e.g. dev + prod).
func towerTokenAccount(towerName string) string {
	return sanitizeTowerCredKey(towerName) + "-token"
}

// Low-level keychain operations for tower tokens. Stored as function variables
// so tests can substitute an in-process fake without shelling out.
var (
	towerKeychainSetFn    = towerKeychainSetReal
	towerKeychainGetFn    = towerKeychainGetReal
	towerKeychainDeleteFn = towerKeychainDeleteReal
)

// SetTowerToken persists a gateway bearer token for the named tower in the OS
// keychain under a dedicated service namespace.
func SetTowerToken(towerName, token string) error {
	return towerKeychainSetFn(towerTokenAccount(towerName), token)
}

// GetTowerToken returns the stored gateway bearer token for a tower.
// Returns ErrTokenNotFound if no token is stored. Other errors indicate a
// platform-level failure (keychain unavailable, exec error).
func GetTowerToken(towerName string) (string, error) {
	return towerKeychainGetFn(towerTokenAccount(towerName))
}

// DeleteTowerToken removes a tower's gateway bearer token from the keychain.
// Idempotent: returns nil if no token is stored for the tower.
func DeleteTowerToken(towerName string) error {
	return towerKeychainDeleteFn(towerTokenAccount(towerName))
}

func towerKeychainSetReal(account, value string) error {
	if runtime.GOOS == "darwin" {
		return exec.Command("security", "add-generic-password",
			"-a", account, "-s", towerKeychainService, "-w", value, "-U").Run()
	}
	return exec.Command("secret-tool", "store",
		"--label=spire tower token: "+account,
		"service", towerKeychainService, "account", account).Run()
}

func towerKeychainGetReal(account string) (string, error) {
	if runtime.GOOS == "darwin" {
		out, err := exec.Command("security", "find-generic-password",
			"-a", account, "-s", towerKeychainService, "-w").Output()
		if err != nil {
			// `security` exits 44 when the item isn't in the keychain.
			var ee *exec.ExitError
			if errors.As(err, &ee) && ee.ExitCode() == 44 {
				return "", ErrTokenNotFound
			}
			return "", err
		}
		return strings.TrimSpace(string(out)), nil
	}
	out, err := exec.Command("secret-tool", "lookup",
		"service", towerKeychainService, "account", account).Output()
	if err != nil {
		// secret-tool exits non-zero with empty stdout when the entry isn't
		// present. Any error paired with empty output is treated as not-found.
		if len(strings.TrimSpace(string(out))) == 0 {
			return "", ErrTokenNotFound
		}
		return "", err
	}
	value := strings.TrimSpace(string(out))
	if value == "" {
		return "", ErrTokenNotFound
	}
	return value, nil
}

func towerKeychainDeleteReal(account string) error {
	if runtime.GOOS == "darwin" {
		err := exec.Command("security", "delete-generic-password",
			"-a", account, "-s", towerKeychainService).Run()
		// Idempotent: "not found" (exit 44) is not an error.
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == 44 {
			return nil
		}
		return err
	}
	err := exec.Command("secret-tool", "clear",
		"service", towerKeychainService, "account", account).Run()
	// secret-tool clear exits 0 even when nothing matches, so idempotency is
	// already handled by the backend.
	return err
}
