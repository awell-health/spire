package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Credential keys (user-facing names).
const (
	CredKeyAnthropicKey    = "anthropic-key"
	CredKeyGithubToken     = "github-token"
	CredKeyDolthubUser     = "dolthub-user"
	CredKeyDolthubPassword = "dolthub-password"
)

// credEnvVars maps credential keys to their standard environment variable names.
// Each key also checks a SPIRE_-prefixed variant (e.g. SPIRE_ANTHROPIC_KEY).
var credEnvVars = map[string]string{
	CredKeyAnthropicKey:    "ANTHROPIC_API_KEY",
	CredKeyGithubToken:     "GITHUB_TOKEN",
	CredKeyDolthubUser:     "DOLT_REMOTE_USER",
	CredKeyDolthubPassword: "DOLT_REMOTE_PASSWORD",
}

// credSpireEnvVars maps credential keys to their SPIRE_-prefixed env var names.
var credSpireEnvVars = map[string]string{
	CredKeyAnthropicKey:    "SPIRE_ANTHROPIC_KEY",
	CredKeyGithubToken:     "SPIRE_GITHUB_TOKEN",
	CredKeyDolthubUser:     "SPIRE_DOLTHUB_USER",
	CredKeyDolthubPassword: "SPIRE_DOLTHUB_PASSWORD",
}

// credentialsPath returns the path to the credentials file (~/.config/spire/credentials).
func credentialsPath() (string, error) {
	dir, err := configDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "credentials"), nil
}

// isCredentialKey returns true if the key is a recognized credential key.
func isCredentialKey(key string) bool {
	_, ok := credEnvVars[key]
	return ok
}

// validCredentialKeys returns a sorted list of valid credential keys.
func validCredentialKeys() []string {
	keys := make([]string, 0, len(credEnvVars))
	for k := range credEnvVars {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// loadCredentials reads the flat credentials file and returns a key-value map.
// Returns an empty map (not an error) if the file does not exist.
func loadCredentials() (map[string]string, error) {
	return loadCredentialsFrom("")
}

// loadCredentialsFrom reads credentials from a specific path, or the default if path is empty.
func loadCredentialsFrom(path string) (map[string]string, error) {
	if path == "" {
		var err error
		path, err = credentialsPath()
		if err != nil {
			return nil, err
		}
	}

	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return make(map[string]string), nil
		}
		return nil, err
	}
	defer f.Close()

	creds := make(map[string]string)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		// Skip comments and empty lines
		if strings.HasPrefix(line, "#") || strings.TrimSpace(line) == "" {
			continue
		}
		// Split on first = only
		idx := strings.Index(line, "=")
		if idx < 0 {
			continue
		}
		key := line[:idx]
		value := line[idx+1:]
		creds[key] = value
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return creds, nil
}

// saveCredentials writes the key-value map to the credentials file with chmod 600.
// Comments and empty lines from the existing file are preserved.
func saveCredentials(creds map[string]string) error {
	return saveCredentialsTo("", creds)
}

// saveCredentialsTo writes credentials to a specific path, or the default if path is empty.
func saveCredentialsTo(path string, creds map[string]string) error {
	if path == "" {
		var err error
		path, err = credentialsPath()
		if err != nil {
			return err
		}
	}

	// Read existing file to preserve comments and ordering
	var lines []string
	existing, err := os.Open(path)
	if err == nil {
		scanner := bufio.NewScanner(existing)
		for scanner.Scan() {
			lines = append(lines, scanner.Text())
		}
		existing.Close()
		if err := scanner.Err(); err != nil {
			return err
		}
	}

	// Track which keys we've already written (to update in-place)
	written := make(map[string]bool)

	// Update existing key lines in-place, preserve comments/blanks
	var output []string
	for _, line := range lines {
		if strings.HasPrefix(line, "#") || strings.TrimSpace(line) == "" {
			output = append(output, line)
			continue
		}
		idx := strings.Index(line, "=")
		if idx < 0 {
			output = append(output, line)
			continue
		}
		key := line[:idx]
		if val, ok := creds[key]; ok {
			output = append(output, key+"="+val)
			written[key] = true
		}
		// If key not in creds, it was deleted — omit it
	}

	// Add any new keys not already in the file
	newKeys := make([]string, 0)
	for k := range creds {
		if !written[k] {
			newKeys = append(newKeys, k)
		}
	}
	sort.Strings(newKeys)
	for _, k := range newKeys {
		output = append(output, k+"="+creds[k])
	}

	// If no header comment exists, add one
	hasHeader := false
	for _, line := range output {
		if strings.HasPrefix(line, "# Spire credentials") {
			hasHeader = true
			break
		}
	}
	if !hasHeader {
		output = append([]string{"# Spire credentials — chmod 600, do not commit to version control"}, output...)
	}

	content := strings.Join(output, "\n") + "\n"
	return os.WriteFile(path, []byte(content), 0600)
}

// getCredential returns the effective value for a credential key.
// It checks environment variables first (SPIRE_-prefixed takes precedence),
// then falls back to the credentials file. Returns empty string if not found.
func getCredential(key string) string {
	return getCredentialFrom("", key)
}

// getCredentialFrom returns the effective credential value, using a specific file path.
func getCredentialFrom(path, key string) string {
	// Check SPIRE_-prefixed env var first (highest precedence)
	if spireEnv, ok := credSpireEnvVars[key]; ok {
		if val := os.Getenv(spireEnv); val != "" {
			return val
		}
	}

	// Check standard env var
	if stdEnv, ok := credEnvVars[key]; ok {
		if val := os.Getenv(stdEnv); val != "" {
			return val
		}
	}

	// Fall back to file
	creds, err := loadCredentialsFrom(path)
	if err != nil {
		return ""
	}
	return creds[key]
}

// credentialSource returns "env" if an env var is providing the value, "file" if from file, or "".
func credentialSource(key string) string {
	if spireEnv, ok := credSpireEnvVars[key]; ok {
		if os.Getenv(spireEnv) != "" {
			return "env"
		}
	}
	if stdEnv, ok := credEnvVars[key]; ok {
		if os.Getenv(stdEnv) != "" {
			return "env"
		}
	}
	creds, err := loadCredentials()
	if err != nil {
		return ""
	}
	if _, ok := creds[key]; ok {
		return "file"
	}
	return ""
}

// setCredential validates the key and writes the value to the credentials file.
func setCredential(key, value string) error {
	return setCredentialTo("", key, value)
}

// setCredentialTo writes a credential to a specific file path.
func setCredentialTo(path, key, value string) error {
	if !isCredentialKey(key) {
		return fmt.Errorf("unknown credential key: %q\nValid keys: %s", key, strings.Join(validCredentialKeys(), ", "))
	}
	creds, err := loadCredentialsFrom(path)
	if err != nil {
		return fmt.Errorf("load credentials: %w", err)
	}
	creds[key] = value
	return saveCredentialsTo(path, creds)
}

// deleteCredential removes a key from the credentials file.
func deleteCredential(key string) error {
	return deleteCredentialFrom("", key)
}

// deleteCredentialFrom removes a credential from a specific file path.
func deleteCredentialFrom(path, key string) error {
	if !isCredentialKey(key) {
		return fmt.Errorf("unknown credential key: %q\nValid keys: %s", key, strings.Join(validCredentialKeys(), ", "))
	}
	creds, err := loadCredentialsFrom(path)
	if err != nil {
		return fmt.Errorf("load credentials: %w", err)
	}
	delete(creds, key)
	return saveCredentialsTo(path, creds)
}

// listCredentials returns all credentials from the file (not from env vars).
func listCredentials() (map[string]string, error) {
	return loadCredentials()
}

// maskValue returns a masked version of a credential value.
// Shows first 4 and last 4 characters with "..." in between.
// Returns "****" if the value is shorter than 12 characters.
func maskValue(value string) string {
	if len(value) < 12 {
		return "****"
	}
	return value[:4] + "..." + value[len(value)-4:]
}
