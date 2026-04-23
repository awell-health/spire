package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// redact masks a credential-shaped value for safe inclusion in test failure
// messages. Real credentials can leak into assertion output when tests run in
// environments where GITHUB_TOKEN / ANTHROPIC_API_KEY are set (CI, cluster
// pods, support bundles). Last 5 chars + length disambiguates values without
// revealing them.
func redact(s string) string {
	if len(s) < 5 {
		return strings.Repeat("*", len(s))
	}
	return fmt.Sprintf("[REDACTED %d chars …%s]", len(s), s[len(s)-5:])
}

func TestIsCredentialKey(t *testing.T) {
	valid := []string{"anthropic-key", "github-token", "dolthub-user", "dolthub-password"}
	for _, k := range valid {
		if !isCredentialKey(k) {
			t.Errorf("isCredentialKey(%q) = false, want true", k)
		}
	}

	invalid := []string{"identity", "dolt.port", "foo", "", "anthropic_key", "ANTHROPIC_API_KEY"}
	for _, k := range invalid {
		if isCredentialKey(k) {
			t.Errorf("isCredentialKey(%q) = true, want false", k)
		}
	}
}

func TestValidCredentialKeys(t *testing.T) {
	keys := validCredentialKeys()
	if len(keys) != 4 {
		t.Fatalf("validCredentialKeys() returned %d keys, want 4", len(keys))
	}
	// Should be sorted
	for i := 1; i < len(keys); i++ {
		if keys[i] < keys[i-1] {
			t.Errorf("keys not sorted: %v", keys)
			break
		}
	}
}

func TestMaskValue(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"", "****"},
		{"short", "****"},
		{"12345678901", "****"},         // 11 chars — too short
		{"123456789012", "1234...9012"}, // exactly 12 chars
		{"sk-ant-api03-xxxxxxxxxxxxxxxxxxxx", "sk-a...xxxx"},
		{"ghp_abcdefghijklmnop", "ghp_...mnop"},
	}
	for _, tt := range tests {
		got := maskValue(tt.input)
		if got != tt.want {
			t.Errorf("maskValue(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestLoadCredentials(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "credentials")

	// Non-existent file returns empty map
	creds, err := loadCredentialsFrom(path)
	if err != nil {
		t.Fatalf("loadCredentialsFrom non-existent: %v", err)
	}
	if len(creds) != 0 {
		t.Errorf("expected empty map for missing file, got %v", creds)
	}

	// Write a test file and parse it
	content := `# Spire credentials — chmod 600, do not commit to version control
anthropic-key=sk-ant-api03-xxxxxxxxxxxx
github-token=ghp_xxxxxxxxxxxx

# DoltHub credentials
dolthub-user=myuser
dolthub-password=dolt_token_xxxxxxxxxxxx
`
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatalf("write test file: %v", err)
	}

	creds, err = loadCredentialsFrom(path)
	if err != nil {
		t.Fatalf("loadCredentialsFrom: %v", err)
	}

	expected := map[string]string{
		"anthropic-key":    "sk-ant-api03-xxxxxxxxxxxx",
		"github-token":     "ghp_xxxxxxxxxxxx",
		"dolthub-user":     "myuser",
		"dolthub-password": "dolt_token_xxxxxxxxxxxx",
	}
	for k, want := range expected {
		if got := creds[k]; got != want {
			t.Errorf("creds[%q] = %q, want %q", k, got, want)
		}
	}
	if len(creds) != len(expected) {
		t.Errorf("got %d credentials, want %d", len(creds), len(expected))
	}
}

func TestSaveCredentials(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "credentials")

	creds := map[string]string{
		"anthropic-key": "sk-test-key-12345",
		"github-token":  "ghp_test_token_67890",
	}

	if err := saveCredentialsTo(path, creds); err != nil {
		t.Fatalf("saveCredentialsTo: %v", err)
	}

	// Verify file permissions
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Errorf("file permissions = %o, want 600", perm)
	}

	// Verify content
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, "# Spire credentials") {
		t.Error("missing header comment")
	}
	if !strings.Contains(content, "anthropic-key=sk-test-key-12345") {
		t.Error("missing anthropic-key")
	}
	if !strings.Contains(content, "github-token=ghp_test_token_67890") {
		t.Error("missing github-token")
	}

	// Roundtrip: read it back
	loaded, err := loadCredentialsFrom(path)
	if err != nil {
		t.Fatalf("roundtrip load: %v", err)
	}
	for k, want := range creds {
		if got := loaded[k]; got != want {
			t.Errorf("roundtrip creds[%q] = %q, want %q", k, got, want)
		}
	}
}

func TestGetCredentialEnvOverride(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "credentials")

	// Write a file value
	creds := map[string]string{
		"anthropic-key": "file-value-sk-test",
	}
	if err := saveCredentialsTo(path, creds); err != nil {
		t.Fatalf("save: %v", err)
	}

	// Without env var, should get file value
	got := getCredentialFrom(path, "anthropic-key")
	if got != "file-value-sk-test" {
		t.Errorf("without env: got %s, want %s", redact(got), redact("file-value-sk-test"))
	}

	// Standard env var overrides file
	t.Setenv("ANTHROPIC_API_KEY", "env-value-standard")
	got = getCredentialFrom(path, "anthropic-key")
	if got != "env-value-standard" {
		t.Errorf("with standard env: got %s, want %s", redact(got), redact("env-value-standard"))
	}

	// SPIRE_-prefixed env var takes precedence over standard
	t.Setenv("SPIRE_ANTHROPIC_KEY", "env-value-spire")
	got = getCredentialFrom(path, "anthropic-key")
	if got != "env-value-spire" {
		t.Errorf("with SPIRE env: got %s, want %s", redact(got), redact("env-value-spire"))
	}
}

func TestGetCredentialGithubToken(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "credentials")

	// No file, no env — empty
	got := getCredentialFrom(path, "github-token")
	if got != "" {
		t.Errorf("expected empty, got %s", redact(got))
	}

	// Set env
	t.Setenv("GITHUB_TOKEN", "ghp_from_env")
	got = getCredentialFrom(path, "github-token")
	if got != "ghp_from_env" {
		t.Errorf("got %s, want %s", redact(got), redact("ghp_from_env"))
	}
}

func TestSetAndDeleteCredential(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "credentials")

	// Set a credential
	if err := setCredentialTo(path, "anthropic-key", "sk-test-value"); err != nil {
		t.Fatalf("set: %v", err)
	}

	creds, err := loadCredentialsFrom(path)
	if err != nil {
		t.Fatalf("load after set: %v", err)
	}
	if creds["anthropic-key"] != "sk-test-value" {
		t.Errorf("after set: got %q", creds["anthropic-key"])
	}

	// Delete it
	if err := deleteCredentialFrom(path, "anthropic-key"); err != nil {
		t.Fatalf("delete: %v", err)
	}

	creds, err = loadCredentialsFrom(path)
	if err != nil {
		t.Fatalf("load after delete: %v", err)
	}
	if _, ok := creds["anthropic-key"]; ok {
		t.Error("credential should be deleted")
	}
}

func TestSetCredentialInvalidKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "credentials")

	err := setCredentialTo(path, "invalid-key", "value")
	if err == nil {
		t.Fatal("expected error for invalid key")
	}
	if !strings.Contains(err.Error(), "unknown credential key") {
		t.Errorf("error = %q, want it to contain 'unknown credential key'", err.Error())
	}
}

func TestFileFormatPreservation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "credentials")

	// Write initial file with comments and blank lines
	initial := `# Spire credentials — chmod 600, do not commit to version control
anthropic-key=sk-original

# GitHub
github-token=ghp_original
`
	if err := os.WriteFile(path, []byte(initial), 0600); err != nil {
		t.Fatalf("write initial: %v", err)
	}

	// Update anthropic-key, add dolthub-user
	creds, err := loadCredentialsFrom(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	creds["anthropic-key"] = "sk-updated"
	creds["dolthub-user"] = "newuser"

	if err := saveCredentialsTo(path, creds); err != nil {
		t.Fatalf("save: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	content := string(data)

	// Header comment preserved
	if !strings.Contains(content, "# Spire credentials") {
		t.Error("header comment lost")
	}

	// Section comment preserved
	if !strings.Contains(content, "# GitHub") {
		t.Error("section comment lost")
	}

	// Updated value
	if !strings.Contains(content, "anthropic-key=sk-updated") {
		t.Error("anthropic-key not updated")
	}

	// Original ordering: anthropic-key before github-token
	aiIdx := strings.Index(content, "anthropic-key=")
	ghIdx := strings.Index(content, "github-token=")
	if aiIdx > ghIdx {
		t.Error("key ordering not preserved")
	}

	// New key added at end
	if !strings.Contains(content, "dolthub-user=newuser") {
		t.Error("new key not added")
	}

	// Roundtrip verification
	loaded, err := loadCredentialsFrom(path)
	if err != nil {
		t.Fatalf("roundtrip load: %v", err)
	}
	if loaded["anthropic-key"] != "sk-updated" {
		t.Errorf("roundtrip anthropic-key = %q", loaded["anthropic-key"])
	}
	if loaded["github-token"] != "ghp_original" {
		t.Errorf("roundtrip github-token = %q", loaded["github-token"])
	}
	if loaded["dolthub-user"] != "newuser" {
		t.Errorf("roundtrip dolthub-user = %q", loaded["dolthub-user"])
	}
}
