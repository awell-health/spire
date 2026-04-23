package git

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigureGitHubTokenAuth_EmptyTokenNoOp(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "gitconfig")
	t.Setenv("GIT_CONFIG_GLOBAL", tmp)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")

	if err := ConfigureGitHubTokenAuth(""); err != nil {
		t.Fatalf("empty token should be no-op, got %v", err)
	}
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Fatalf("expected no config file to be written, got stat err=%v", err)
	}
}

func TestConfigureGitHubTokenAuth_WritesRewriteRule(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "gitconfig")
	t.Setenv("GIT_CONFIG_GLOBAL", tmp)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")

	token := "ghp_TESTTOKEN123"
	if err := ConfigureGitHubTokenAuth(token); err != nil {
		t.Fatalf("ConfigureGitHubTokenAuth: %v", err)
	}

	data, err := os.ReadFile(tmp)
	if err != nil {
		t.Fatalf("read gitconfig: %v", err)
	}
	body := string(data)
	if !strings.Contains(body, token) {
		t.Errorf("gitconfig missing token; body=%q", body)
	}
	if !strings.Contains(body, "insteadOf") {
		t.Errorf("gitconfig missing insteadOf directive; body=%q", body)
	}
	if !strings.Contains(body, "x-access-token") {
		t.Errorf("gitconfig missing x-access-token prefix; body=%q", body)
	}
	if !strings.Contains(body, "https://github.com/") {
		t.Errorf("gitconfig missing https://github.com/ target; body=%q", body)
	}
}

func TestConfigureGitHubTokenAuth_Idempotent(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "gitconfig")
	t.Setenv("GIT_CONFIG_GLOBAL", tmp)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")

	token := "ghp_TESTTOKEN123"
	if err := ConfigureGitHubTokenAuth(token); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if err := ConfigureGitHubTokenAuth(token); err != nil {
		t.Fatalf("second call: %v", err)
	}
	data, _ := os.ReadFile(tmp)
	if count := strings.Count(string(data), "insteadOf"); count != 1 {
		t.Errorf("expected one insteadOf entry after repeat call, got %d\n%s", count, data)
	}
}
