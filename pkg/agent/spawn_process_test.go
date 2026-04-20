package agent

import (
	"os/exec"
	"strings"
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

// TestApplyProcessEnv_ApprenticeIdentity verifies the three identity env
// vars (SPIRE_BEAD_ID, SPIRE_ATTEMPT_ID, SPIRE_APPRENTICE_IDX) are injected
// into the child process env when populated on SpawnConfig.
func TestApplyProcessEnv_ApprenticeIdentity(t *testing.T) {
	cmd := &exec.Cmd{Env: []string{"PATH=/usr/bin"}}

	applyProcessEnv(cmd, SpawnConfig{
		Name:          "apprentice-spi-abc-0",
		BeadID:        "spi-abc",
		Role:          RoleApprentice,
		AttemptID:     "spi-att1",
		ApprenticeIdx: "0",
	})

	got := envToMap(cmd.Env)

	wantIdentity := map[string]string{
		"SPIRE_BEAD_ID":        "spi-abc",
		"SPIRE_ATTEMPT_ID":     "spi-att1",
		"SPIRE_APPRENTICE_IDX": "0",
	}
	for k, want := range wantIdentity {
		if v, ok := got[k]; !ok {
			t.Errorf("missing env var %s; env: %v", k, cmd.Env)
		} else if v != want {
			t.Errorf("env %s = %q, want %q", k, v, want)
		}
	}
}

// TestApplyProcessEnv_ApprenticeIdentity_NonZeroIdx verifies a non-zero
// fan-out index is passed through verbatim.
func TestApplyProcessEnv_ApprenticeIdentity_NonZeroIdx(t *testing.T) {
	cmd := &exec.Cmd{Env: []string{}}

	applyProcessEnv(cmd, SpawnConfig{
		BeadID:        "spi-abc",
		AttemptID:     "spi-att2",
		ApprenticeIdx: "7",
	})

	got := envToMap(cmd.Env)
	if got["SPIRE_APPRENTICE_IDX"] != "7" {
		t.Errorf("SPIRE_APPRENTICE_IDX = %q, want %q", got["SPIRE_APPRENTICE_IDX"], "7")
	}
	if got["SPIRE_ATTEMPT_ID"] != "spi-att2" {
		t.Errorf("SPIRE_ATTEMPT_ID = %q, want %q", got["SPIRE_ATTEMPT_ID"], "spi-att2")
	}
}

// TestApplyProcessEnv_OmitsEmptyIdentity verifies that identity env vars
// left unset on SpawnConfig are NOT injected — matches the pattern used
// by SPIRE_TOWER and SPIRE_PROVIDER.
func TestApplyProcessEnv_OmitsEmptyIdentity(t *testing.T) {
	cmd := &exec.Cmd{Env: []string{}}

	applyProcessEnv(cmd, SpawnConfig{
		Name: "no-identity",
	})

	for _, e := range cmd.Env {
		for _, prefix := range []string{"SPIRE_BEAD_ID=", "SPIRE_ATTEMPT_ID=", "SPIRE_APPRENTICE_IDX="} {
			if strings.HasPrefix(e, prefix) {
				t.Errorf("unexpected env var set: %s", e)
			}
		}
	}
}

// TestApplyProcessEnv_SetsSpireRole verifies SPIRE_ROLE is injected into the
// child process env for each role so the SubagentStart hook can emit the
// correct per-role command catalog.
func TestApplyProcessEnv_SetsSpireRole(t *testing.T) {
	roles := []SpawnRole{RoleApprentice, RoleSage, RoleWizard, RoleExecutor}
	for _, role := range roles {
		t.Run(string(role), func(t *testing.T) {
			cmd := &exec.Cmd{Env: []string{"PATH=/usr/bin"}}

			applyProcessEnv(cmd, SpawnConfig{
				Name: "test-" + string(role),
				Role: role,
			})

			got := envToMap(cmd.Env)
			want := string(role)
			if v, ok := got["SPIRE_ROLE"]; !ok {
				t.Errorf("missing SPIRE_ROLE; env: %v", cmd.Env)
			} else if v != want {
				t.Errorf("SPIRE_ROLE = %q, want %q", v, want)
			}
		})
	}
}

// TestApplyProcessEnv_OmitsEmptyRole verifies SPIRE_ROLE is NOT injected
// when cfg.Role is empty — matches the SPIRE_TOWER/SPIRE_PROVIDER pattern.
func TestApplyProcessEnv_OmitsEmptyRole(t *testing.T) {
	cmd := &exec.Cmd{Env: []string{}}

	applyProcessEnv(cmd, SpawnConfig{Name: "no-role"})

	for _, e := range cmd.Env {
		if strings.HasPrefix(e, "SPIRE_ROLE=") {
			t.Errorf("unexpected SPIRE_ROLE set: %s", e)
		}
	}
}

func envToMap(env []string) map[string]string {
	m := make(map[string]string, len(env))
	for _, e := range env {
		if i := strings.IndexByte(e, '='); i >= 0 {
			m[e[:i]] = e[i+1:]
		}
	}
	return m
}
