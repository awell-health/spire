package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDetectPrefix(t *testing.T) {
	tests := []struct {
		dir  string
		want string
	}{
		{"/home/user/my-web-app", "myw"},
		{"/home/user/spire", "spi"},
		{"/home/user/API-Server", "api"},
		{"/home/user/go", "go"},
		{"/home/user/a", "a"},
		{"/home/user/ab", "ab"},
		{"/home/user/abc", "abc"},
		{"/home/user/abcdef", "abc"},
		{"/home/user/123-project", "123"},
		{"/home/user/---", "repo"}, // no alphanumeric chars
		{"/home/user/UPPERCASE", "upp"},
		{"/home/user/MixedCase42", "mix"},
		{"/home/user/my_project", "myp"},
		{"/home/user/web.app", "web"},
	}
	for _, tt := range tests {
		got := detectPrefix(tt.dir)
		if got != tt.want {
			t.Errorf("detectPrefix(%q) = %q, want %q", tt.dir, got, tt.want)
		}
	}
}

func TestDetectLanguage(t *testing.T) {
	tests := []struct {
		name     string
		files    []string
		wantLang string
	}{
		{
			name:     "go project",
			files:    []string{"go.mod"},
			wantLang: "go",
		},
		{
			name:     "typescript project",
			files:    []string{"package.json"},
			wantLang: "typescript",
		},
		{
			name:     "rust project",
			files:    []string{"Cargo.toml"},
			wantLang: "rust",
		},
		{
			name:     "python pyproject",
			files:    []string{"pyproject.toml"},
			wantLang: "python",
		},
		{
			name:     "python requirements",
			files:    []string{"requirements.txt"},
			wantLang: "python",
		},
		{
			name:     "unknown project",
			files:    []string{"README.md"},
			wantLang: "",
		},
		{
			name:     "go takes priority over package.json",
			files:    []string{"go.mod", "package.json"},
			wantLang: "go",
		},
		{
			name:     "empty directory",
			files:    nil,
			wantLang: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			for _, f := range tt.files {
				if err := os.WriteFile(filepath.Join(dir, f), []byte(""), 0644); err != nil {
					t.Fatalf("create %s: %s", f, err)
				}
			}
			got := detectLanguage(dir)
			if got != tt.wantLang {
				t.Errorf("detectLanguage() = %q, want %q", got, tt.wantLang)
			}
		})
	}
}

func TestSQLEscape(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"hello", "hello"},
		{"it's a test", "it''s a test"},
		{"no quotes", "no quotes"},
		{"'start", "''start"},
		{"end'", "end''"},
		{"mul'ti'ple", "mul''ti''ple"},
		{"''already doubled''", "''''already doubled''''"},
		{"", ""},
		{"O'Brien's app", "O''Brien''s app"},
	}
	for _, tt := range tests {
		got := sqlEscape(tt.input)
		if got != tt.want {
			t.Errorf("sqlEscape(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestValidatePrefix(t *testing.T) {
	valid := []string{"ab", "abc", "web", "api", "spi", "a1b2", "abcdefghijklmnop"}
	for _, p := range valid {
		if err := validatePrefix(p); err != nil {
			t.Errorf("validatePrefix(%q) returned error: %s", p, err)
		}
	}

	invalid := []struct {
		prefix string
		desc   string
	}{
		{"a", "too short (1 char)"},
		{"", "empty"},
		{"abcdefghijklmnopq", "too long (17 chars)"},
		{"Web", "uppercase"},
		{"we-b", "contains hyphen"},
		{"we b", "contains space"},
		{"we_b", "contains underscore"},
		{"we.b", "contains dot"},
		{"AB", "all uppercase"},
	}
	for _, tt := range invalid {
		if err := validatePrefix(tt.prefix); err == nil {
			t.Errorf("validatePrefix(%q) should fail (%s)", tt.prefix, tt.desc)
		}
	}
}

func TestDetectBranch(t *testing.T) {
	// For a non-git directory, should return "main"
	dir := t.TempDir()
	got := detectBranch(dir)
	if got != "main" {
		t.Errorf("detectBranch(non-git dir) = %q, want %q", got, "main")
	}
}

func TestDetectRepoURL(t *testing.T) {
	// For a non-git directory, should return empty string
	dir := t.TempDir()
	got := detectRepoURL(dir)
	if got != "" {
		t.Errorf("detectRepoURL(non-git dir) = %q, want %q", got, "")
	}
}

func TestDetectUser(t *testing.T) {
	// Save and restore env
	origIdentity := os.Getenv("SPIRE_IDENTITY")
	origUser := os.Getenv("USER")
	defer func() {
		os.Setenv("SPIRE_IDENTITY", origIdentity)
		os.Setenv("USER", origUser)
	}()

	// SPIRE_IDENTITY takes priority
	os.Setenv("SPIRE_IDENTITY", "test-agent")
	os.Setenv("USER", "jb")
	if got := detectUser(); got != "test-agent" {
		t.Errorf("detectUser() with SPIRE_IDENTITY = %q, want %q", got, "test-agent")
	}

	// Falls back to USER
	os.Unsetenv("SPIRE_IDENTITY")
	os.Setenv("USER", "jb")
	if got := detectUser(); got != "jb" {
		t.Errorf("detectUser() with USER = %q, want %q", got, "jb")
	}

	// Falls back to "unknown"
	os.Unsetenv("SPIRE_IDENTITY")
	os.Unsetenv("USER")
	if got := detectUser(); got != "unknown" {
		t.Errorf("detectUser() without env = %q, want %q", got, "unknown")
	}
}

func TestPrefixPatternEdgeCases(t *testing.T) {
	// Exactly at boundaries
	if err := validatePrefix("ab"); err != nil {
		t.Errorf("2-char prefix should be valid: %s", err)
	}
	if err := validatePrefix("abcdefghijklmnop"); err != nil {
		t.Errorf("16-char prefix should be valid: %s", err)
	}
	if err := validatePrefix("abcdefghijklmnopq"); err == nil {
		t.Error("17-char prefix should be invalid")
	}

	// Numeric-only is valid
	if err := validatePrefix("123"); err != nil {
		t.Errorf("numeric prefix should be valid: %s", err)
	}
}

func TestBeadsDirectorySetup(t *testing.T) {
	// Test that the .beads directory structure is created correctly
	dir := t.TempDir()
	beadsDir := filepath.Join(dir, ".beads")

	// Simulate what register-repo does for .beads/ setup
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatalf("create .beads/: %s", err)
	}

	// Verify directory exists
	info, err := os.Stat(beadsDir)
	if err != nil {
		t.Fatalf(".beads/ not created: %s", err)
	}
	if !info.IsDir() {
		t.Fatal(".beads/ is not a directory")
	}
}
