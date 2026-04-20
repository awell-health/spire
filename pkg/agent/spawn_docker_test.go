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
