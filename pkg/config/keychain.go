package config

import (
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
