package agent

import (
	"os/exec"
	"testing"
)

func TestSetEnv_Append(t *testing.T) {
	cmd := &exec.Cmd{Env: []string{"PATH=/usr/bin", "HOME=/home/test"}}

	setEnv(cmd, "SPIRE_TOWER", "my-tower")

	want := "SPIRE_TOWER=my-tower"
	if cmd.Env[len(cmd.Env)-1] != want {
		t.Errorf("expected %q appended, got env: %v", want, cmd.Env)
	}
	if len(cmd.Env) != 3 {
		t.Errorf("expected 3 env vars, got %d", len(cmd.Env))
	}
}

func TestSetEnv_Replace(t *testing.T) {
	cmd := &exec.Cmd{Env: []string{"PATH=/usr/bin", "SPIRE_TOWER=old-tower", "HOME=/home/test"}}

	setEnv(cmd, "SPIRE_TOWER", "new-tower")

	// Should replace in-place, not append
	if len(cmd.Env) != 3 {
		t.Errorf("expected 3 env vars (replaced in-place), got %d", len(cmd.Env))
	}

	found := false
	for _, e := range cmd.Env {
		if e == "SPIRE_TOWER=new-tower" {
			found = true
		}
		if e == "SPIRE_TOWER=old-tower" {
			t.Error("old value should have been replaced")
		}
	}
	if !found {
		t.Errorf("expected SPIRE_TOWER=new-tower in env, got: %v", cmd.Env)
	}
}

func TestSetEnv_EmptyEnv(t *testing.T) {
	cmd := &exec.Cmd{Env: []string{}}

	setEnv(cmd, "KEY", "value")

	if len(cmd.Env) != 1 {
		t.Errorf("expected 1 env var, got %d", len(cmd.Env))
	}
	if cmd.Env[0] != "KEY=value" {
		t.Errorf("expected KEY=value, got %s", cmd.Env[0])
	}
}

func TestSetEnv_PrefixCollision(t *testing.T) {
	// Ensure SPIRE_TOWER_NAME doesn't get matched when setting SPIRE_TOWER
	cmd := &exec.Cmd{Env: []string{"SPIRE_TOWER_NAME=extended"}}

	setEnv(cmd, "SPIRE_TOWER", "my-tower")

	if len(cmd.Env) != 2 {
		t.Errorf("expected 2 env vars (no collision), got %d: %v", len(cmd.Env), cmd.Env)
	}
}
