package agent

import (
	"testing"
)

// TestAppendIdentityDockerArgs_Populated verifies the docker-run env args
// include the three apprentice identity vars when SpawnConfig fields are
// populated.
func TestAppendIdentityDockerArgs_Populated(t *testing.T) {
	cfg := SpawnConfig{
		BeadID:        "spi-abc",
		AttemptID:     "spi-att",
		ApprenticeIdx: "3",
	}

	args := appendIdentityDockerArgs(cfg)

	want := []string{
		"-e", "SPIRE_BEAD_ID=spi-abc",
		"-e", "SPIRE_ATTEMPT_ID=spi-att",
		"-e", "SPIRE_APPRENTICE_IDX=3",
	}
	if len(args) != len(want) {
		t.Fatalf("args length = %d, want %d: %v", len(args), len(want), args)
	}
	for i, v := range want {
		if args[i] != v {
			t.Errorf("args[%d] = %q, want %q", i, args[i], v)
		}
	}
}

// TestAppendIdentityDockerArgs_Empty verifies that omitted identity fields
// produce no `-e` entries.
func TestAppendIdentityDockerArgs_Empty(t *testing.T) {
	args := appendIdentityDockerArgs(SpawnConfig{})
	if len(args) != 0 {
		t.Errorf("expected empty args, got %d: %v", len(args), args)
	}
}

// TestAppendRoleDockerArgs_SetsSpireRole verifies each role produces a
// single `-e SPIRE_ROLE=<role>` arg pair on the docker-run command line so
// the SubagentStart hook can emit the correct per-role command catalog.
func TestAppendRoleDockerArgs_SetsSpireRole(t *testing.T) {
	roles := []SpawnRole{RoleApprentice, RoleSage, RoleWizard, RoleExecutor}
	for _, role := range roles {
		t.Run(string(role), func(t *testing.T) {
			args := appendRoleDockerArgs(SpawnConfig{Role: role})

			want := []string{"-e", "SPIRE_ROLE=" + string(role)}
			if len(args) != len(want) {
				t.Fatalf("args length = %d, want %d: %v", len(args), len(want), args)
			}
			for i, v := range want {
				if args[i] != v {
					t.Errorf("args[%d] = %q, want %q", i, args[i], v)
				}
			}
		})
	}
}

// TestAppendRoleDockerArgs_EmptyRole verifies an empty role produces no
// args (matches the SPIRE_TOWER/SPIRE_PROVIDER pattern).
func TestAppendRoleDockerArgs_EmptyRole(t *testing.T) {
	if args := appendRoleDockerArgs(SpawnConfig{}); len(args) != 0 {
		t.Errorf("expected empty args for empty role, got %d: %v", len(args), args)
	}
}

// TestAppendIdentityDockerArgs_PartiallyPopulated verifies that only the
// populated identity fields produce args — matches the pattern used by
// SPIRE_TOWER/SPIRE_PROVIDER.
func TestAppendIdentityDockerArgs_PartiallyPopulated(t *testing.T) {
	args := appendIdentityDockerArgs(SpawnConfig{
		BeadID:        "spi-abc",
		ApprenticeIdx: "0",
		// AttemptID intentionally empty — e.g. review-fix re-engagement.
	})

	want := []string{
		"-e", "SPIRE_BEAD_ID=spi-abc",
		"-e", "SPIRE_APPRENTICE_IDX=0",
	}
	if len(args) != len(want) {
		t.Fatalf("args length = %d, want %d: %v", len(args), len(want), args)
	}
	for i, v := range want {
		if args[i] != v {
			t.Errorf("args[%d] = %q, want %q", i, args[i], v)
		}
	}
}
