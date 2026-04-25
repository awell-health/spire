package agent

import (
	"os/exec"
	"strings"
	"testing"
)

// TestApplyAuthEnv_ReplacesInheritedAnthropicVars verifies that when
// SpawnConfig.AuthEnv carries an api-key entry, it replaces any
// inherited ANTHROPIC_API_KEY / CLAUDE_CODE_OAUTH_TOKEN / ANTHROPIC_AUTH_TOKEN
// already present in cmd.Env.
func TestApplyAuthEnv_ReplacesInheritedAnthropicVars(t *testing.T) {
	cmd := &exec.Cmd{Env: []string{
		"PATH=/usr/bin",
		"ANTHROPIC_API_KEY=old-api",
		"CLAUDE_CODE_OAUTH_TOKEN=old-token",
		"ANTHROPIC_AUTH_TOKEN=old-auth",
	}}

	applyProcessEnv(cmd, SpawnConfig{
		AuthEnv:  []string{"ANTHROPIC_API_KEY=new-api"},
		AuthSlot: "api-key",
	})

	count := 0
	for _, e := range cmd.Env {
		if strings.HasPrefix(e, "ANTHROPIC_API_KEY=") {
			count++
			if e != "ANTHROPIC_API_KEY=new-api" {
				t.Errorf("unexpected ANTHROPIC_API_KEY value: %q", e)
			}
		}
		if strings.HasPrefix(e, "CLAUDE_CODE_OAUTH_TOKEN=") {
			t.Errorf("CLAUDE_CODE_OAUTH_TOKEN should be stripped, got %q", e)
		}
		if strings.HasPrefix(e, "ANTHROPIC_AUTH_TOKEN=") {
			t.Errorf("ANTHROPIC_AUTH_TOKEN should be stripped, got %q", e)
		}
	}
	if count != 1 {
		t.Errorf("want 1 ANTHROPIC_API_KEY entry, got %d (env: %v)", count, cmd.Env)
	}
	// PATH must be preserved.
	if !envContains(cmd.Env, "PATH=/usr/bin") {
		t.Errorf("PATH must be preserved, env: %v", cmd.Env)
	}
}

// TestApplyAuthEnv_SubscriptionSlotInjectsOAuthToken verifies that when
// AuthEnv carries CLAUDE_CODE_OAUTH_TOKEN, it lands and ANTHROPIC_API_KEY
// is removed from inherited env.
func TestApplyAuthEnv_SubscriptionSlotInjectsOAuthToken(t *testing.T) {
	cmd := &exec.Cmd{Env: []string{
		"ANTHROPIC_API_KEY=stale-key",
		"PATH=/usr/bin",
	}}

	applyProcessEnv(cmd, SpawnConfig{
		AuthEnv:  []string{"CLAUDE_CODE_OAUTH_TOKEN=fresh-oauth"},
		AuthSlot: "subscription",
	})

	if !envContains(cmd.Env, "CLAUDE_CODE_OAUTH_TOKEN=fresh-oauth") {
		t.Errorf("missing CLAUDE_CODE_OAUTH_TOKEN, env: %v", cmd.Env)
	}
	if envContainsKey(cmd.Env, "ANTHROPIC_API_KEY") {
		t.Errorf("ANTHROPIC_API_KEY should be stripped, env: %v", cmd.Env)
	}
}

// TestApplyAuthEnv_NilAuthEnvLeavesInheritedAlone verifies that when
// AuthEnv is empty, applyProcessEnv does not touch existing env entries
// for the managed Anthropic variables — preserving the legacy
// inherit-from-parent behavior.
func TestApplyAuthEnv_NilAuthEnvLeavesInheritedAlone(t *testing.T) {
	cmd := &exec.Cmd{Env: []string{
		"ANTHROPIC_API_KEY=inherited-key",
		"PATH=/usr/bin",
	}}

	applyProcessEnv(cmd, SpawnConfig{}) // no AuthEnv

	if !envContains(cmd.Env, "ANTHROPIC_API_KEY=inherited-key") {
		t.Errorf("inherited ANTHROPIC_API_KEY should be preserved when AuthEnv is nil, env: %v", cmd.Env)
	}
}

func envContains(env []string, entry string) bool {
	for _, e := range env {
		if e == entry {
			return true
		}
	}
	return false
}

func envContainsKey(env []string, key string) bool {
	prefix := key + "="
	for _, e := range env {
		if strings.HasPrefix(e, prefix) {
			return true
		}
	}
	return false
}
