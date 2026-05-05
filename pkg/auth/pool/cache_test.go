package pool

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// envCacheChildStateDir, envCacheChildSlot, envCacheChildPrefix,
// envCacheChildN drive TestCacheCrossProcessChild — see
// TestCrossProcessConcurrentMutate. They live as env vars (not flags) so
// the parent can re-exec the same test binary with -test.run isolating
// the child function.
const (
	envCacheChildStateDir = "POOL_CACHE_CHILD_STATEDIR"
	envCacheChildSlot     = "POOL_CACHE_CHILD_SLOT"
	envCacheChildPrefix   = "POOL_CACHE_CHILD_PREFIX"
	envCacheChildN        = "POOL_CACHE_CHILD_N"
)

func TestSlotStatePath(t *testing.T) {
	got := SlotStatePath("/var/state", "default")
	want := filepath.Join("/var/state", "default.json")
	if got != want {
		t.Fatalf("SlotStatePath: got %q, want %q", got, want)
	}
}

func TestReadSlotState_MissingReturnsZeroValue(t *testing.T) {
	dir := t.TempDir()

	state, err := ReadSlotState(dir, "default")
	if err != nil {
		t.Fatalf("ReadSlotState: %v", err)
	}
	if state == nil {
		t.Fatal("ReadSlotState returned nil state for missing file")
	}
	if state.Slot != "default" {
		t.Errorf("state.Slot = %q, want %q", state.Slot, "default")
	}
	if len(state.InFlight) != 0 {
		t.Errorf("state.InFlight = %v, want empty", state.InFlight)
	}
	// JSON file should not have been created by a Read.
	if _, err := os.Stat(SlotStatePath(dir, "default")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Read created the data file (stat err=%v); it should not", err)
	}
}

func TestWriteSlotState_RoundTrip(t *testing.T) {
	dir := t.TempDir()

	now := time.Now().UTC().Truncate(time.Second)
	want := &SlotState{
		Slot:     "primary",
		LastSeen: now,
		RateLimit: RateLimitInfo{
			FiveHour: RateLimitWindow{Status: StatusAllowed},
			Overage:  RateLimitWindow{Status: StatusAllowed},
		},
		InFlight: []InFlightClaim{
			{DispatchID: "d1", ClaimedAt: now, HeartbeatAt: now},
		},
	}

	if err := WriteSlotState(dir, want); err != nil {
		t.Fatalf("WriteSlotState: %v", err)
	}

	// File exists with 0600 perms.
	info, err := os.Stat(SlotStatePath(dir, "primary"))
	if err != nil {
		t.Fatalf("stat written file: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("perm = %o, want 0600", perm)
	}

	got, err := ReadSlotState(dir, "primary")
	if err != nil {
		t.Fatalf("ReadSlotState: %v", err)
	}
	if got.Slot != want.Slot {
		t.Errorf("Slot = %q, want %q", got.Slot, want.Slot)
	}
	if !got.LastSeen.Equal(want.LastSeen) {
		t.Errorf("LastSeen = %v, want %v", got.LastSeen, want.LastSeen)
	}
	if got.RateLimit.FiveHour.Status != StatusAllowed {
		t.Errorf("FiveHour.Status = %q, want %q", got.RateLimit.FiveHour.Status, StatusAllowed)
	}
	if len(got.InFlight) != 1 || got.InFlight[0].DispatchID != "d1" {
		t.Errorf("InFlight = %v, want one claim with DispatchID=d1", got.InFlight)
	}
}

func TestWriteSlotState_RejectsNilOrEmptySlot(t *testing.T) {
	dir := t.TempDir()

	if err := WriteSlotState(dir, nil); err == nil {
		t.Error("WriteSlotState(nil): want error, got nil")
	}
	if err := WriteSlotState(dir, &SlotState{}); err == nil {
		t.Error("WriteSlotState(empty Slot): want error, got nil")
	}
}

func TestMutateSlotState_AppendsAndPersists(t *testing.T) {
	dir := t.TempDir()

	err := MutateSlotState(dir, "default", func(s *SlotState) error {
		s.InFlight = append(s.InFlight, InFlightClaim{DispatchID: "first"})
		return nil
	})
	if err != nil {
		t.Fatalf("first MutateSlotState: %v", err)
	}

	err = MutateSlotState(dir, "default", func(s *SlotState) error {
		if len(s.InFlight) != 1 || s.InFlight[0].DispatchID != "first" {
			t.Errorf("unexpected InFlight on second mutate: %v", s.InFlight)
		}
		s.InFlight = append(s.InFlight, InFlightClaim{DispatchID: "second"})
		return nil
	})
	if err != nil {
		t.Fatalf("second MutateSlotState: %v", err)
	}

	got, err := ReadSlotState(dir, "default")
	if err != nil {
		t.Fatalf("ReadSlotState: %v", err)
	}
	if len(got.InFlight) != 2 {
		t.Fatalf("InFlight len = %d, want 2 (got %v)", len(got.InFlight), got.InFlight)
	}
	if got.InFlight[0].DispatchID != "first" || got.InFlight[1].DispatchID != "second" {
		t.Errorf("InFlight order wrong: %v", got.InFlight)
	}
}

func TestMutateSlotState_FnErrorAbortsWrite(t *testing.T) {
	dir := t.TempDir()

	// Seed an existing state.
	if err := WriteSlotState(dir, &SlotState{
		Slot:     "default",
		InFlight: []InFlightClaim{{DispatchID: "seeded"}},
	}); err != nil {
		t.Fatalf("seed write: %v", err)
	}

	sentinel := errors.New("nope")
	err := MutateSlotState(dir, "default", func(s *SlotState) error {
		// Mutate something that should NOT survive.
		s.InFlight = append(s.InFlight, InFlightClaim{DispatchID: "lost"})
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("MutateSlotState err = %v, want %v", err, sentinel)
	}

	got, err := ReadSlotState(dir, "default")
	if err != nil {
		t.Fatalf("ReadSlotState: %v", err)
	}
	if len(got.InFlight) != 1 || got.InFlight[0].DispatchID != "seeded" {
		t.Errorf("state mutated despite fn error: %v", got.InFlight)
	}
}

func TestMutateSlotState_RestoresClearedSlot(t *testing.T) {
	dir := t.TempDir()

	err := MutateSlotState(dir, "rebuilt", func(s *SlotState) error {
		// Simulate a buggy caller wiping Slot — write should still target
		// the canonical filename.
		s.Slot = ""
		s.InFlight = append(s.InFlight, InFlightClaim{DispatchID: "x"})
		return nil
	})
	if err != nil {
		t.Fatalf("MutateSlotState: %v", err)
	}

	got, err := ReadSlotState(dir, "rebuilt")
	if err != nil {
		t.Fatalf("ReadSlotState: %v", err)
	}
	if got.Slot != "rebuilt" {
		t.Errorf("Slot = %q, want %q", got.Slot, "rebuilt")
	}
	if len(got.InFlight) != 1 {
		t.Errorf("InFlight len = %d, want 1", len(got.InFlight))
	}
}

func TestListSlotStates_Empty(t *testing.T) {
	dir := t.TempDir()

	out, err := ListSlotStates(dir)
	if err != nil {
		t.Fatalf("ListSlotStates: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("got %d entries, want 0: %v", len(out), out)
	}
}

func TestListSlotStates_MissingDir(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")

	out, err := ListSlotStates(missing)
	if err != nil {
		t.Fatalf("ListSlotStates(missing): %v", err)
	}
	if len(out) != 0 {
		t.Errorf("got %d entries, want 0", len(out))
	}
}

func TestListSlotStates_Populated(t *testing.T) {
	dir := t.TempDir()

	for _, slot := range []string{"alpha", "beta", "gamma"} {
		if err := WriteSlotState(dir, &SlotState{
			Slot:     slot,
			InFlight: []InFlightClaim{{DispatchID: "claim-" + slot}},
		}); err != nil {
			t.Fatalf("seed %s: %v", slot, err)
		}
	}

	// Drop a non-JSON file alongside; it must be ignored.
	if err := os.WriteFile(filepath.Join(dir, "README.txt"), []byte("ignore me"), 0o600); err != nil {
		t.Fatalf("write decoy: %v", err)
	}

	out, err := ListSlotStates(dir)
	if err != nil {
		t.Fatalf("ListSlotStates: %v", err)
	}
	if len(out) != 3 {
		t.Fatalf("got %d entries, want 3: %v", len(out), keysOf(out))
	}
	for _, slot := range []string{"alpha", "beta", "gamma"} {
		s, ok := out[slot]
		if !ok {
			t.Errorf("missing slot %q", slot)
			continue
		}
		if s.Slot != slot {
			t.Errorf("entry %q has Slot=%q", slot, s.Slot)
		}
		if len(s.InFlight) != 1 || s.InFlight[0].DispatchID != "claim-"+slot {
			t.Errorf("entry %q wrong InFlight: %v", slot, s.InFlight)
		}
	}
}

// TestConcurrentGoroutineMutate spawns multiple goroutines all calling
// MutateSlotState on the same slot, each appending its own InFlightClaim.
// With the exclusive lock honored, every append survives — no lost
// updates from interleaved read-modify-write.
func TestConcurrentGoroutineMutate(t *testing.T) {
	dir := t.TempDir()
	const goroutines = 8
	const perGoroutine = 5

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		g := g
		go func() {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				dispatchID := fmt.Sprintf("g%d-i%d", g, i)
				err := MutateSlotState(dir, "shared", func(s *SlotState) error {
					s.InFlight = append(s.InFlight, InFlightClaim{
						DispatchID: dispatchID,
						ClaimedAt:  time.Now(),
					})
					return nil
				})
				if err != nil {
					t.Errorf("MutateSlotState: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()

	got, err := ReadSlotState(dir, "shared")
	if err != nil {
		t.Fatalf("ReadSlotState: %v", err)
	}
	if len(got.InFlight) != goroutines*perGoroutine {
		t.Fatalf("InFlight len = %d, want %d (lost updates indicate broken locking): %v",
			len(got.InFlight), goroutines*perGoroutine, dispatchIDs(got.InFlight))
	}

	// Every (g,i) pair must appear exactly once.
	seen := make(map[string]bool, len(got.InFlight))
	for _, c := range got.InFlight {
		if seen[c.DispatchID] {
			t.Errorf("duplicate DispatchID %q", c.DispatchID)
		}
		seen[c.DispatchID] = true
	}
	for g := 0; g < goroutines; g++ {
		for i := 0; i < perGoroutine; i++ {
			id := fmt.Sprintf("g%d-i%d", g, i)
			if !seen[id] {
				t.Errorf("missing DispatchID %q", id)
			}
		}
	}
}

// TestCrossProcessConcurrentMutate launches two child processes that each
// run TestCacheCrossProcessChild. Each child appends N InFlightClaims to
// the same slot via MutateSlotState. With cross-process flock honored,
// the final state contains every claim from both children.
//
// The child's behavior is gated by env vars (see TestCacheCrossProcessChild)
// rather than a TestMain hook, because the package's TestMain is owned by
// lock_test.go (foundation bead spi-z3nui7) and not editable here.
func TestCrossProcessConcurrentMutate(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping cross-process test in -short mode")
	}

	dir := t.TempDir()
	const slot = "shared"
	const childN = 10
	prefixes := []string{"alpha", "beta"}

	// Launch all children, then wait — running them concurrently is the
	// whole point.
	cmds := make([]*exec.Cmd, 0, len(prefixes))
	for _, p := range prefixes {
		cmd := exec.Command(os.Args[0], "-test.run=^TestCacheCrossProcessChild$", "-test.v")
		cmd.Env = append(os.Environ(),
			envCacheChildStateDir+"="+dir,
			envCacheChildSlot+"="+slot,
			envCacheChildPrefix+"="+p,
			fmt.Sprintf("%s=%d", envCacheChildN, childN),
		)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Start(); err != nil {
			t.Fatalf("start child %s: %v", p, err)
		}
		cmds = append(cmds, cmd)
	}
	for i, cmd := range cmds {
		if err := cmd.Wait(); err != nil {
			t.Fatalf("child %s exited with error: %v", prefixes[i], err)
		}
	}

	got, err := ReadSlotState(dir, slot)
	if err != nil {
		t.Fatalf("ReadSlotState: %v", err)
	}
	want := childN * len(prefixes)
	if len(got.InFlight) != want {
		t.Fatalf("InFlight len = %d, want %d (lost updates indicate broken cross-process locking): %v",
			len(got.InFlight), want, dispatchIDs(got.InFlight))
	}

	seen := make(map[string]bool, len(got.InFlight))
	for _, c := range got.InFlight {
		if seen[c.DispatchID] {
			t.Errorf("duplicate DispatchID %q", c.DispatchID)
		}
		seen[c.DispatchID] = true
	}
	for _, p := range prefixes {
		for i := 0; i < childN; i++ {
			id := fmt.Sprintf("%s-%d", p, i)
			if !seen[id] {
				t.Errorf("missing DispatchID %q", id)
			}
		}
	}
}

// TestCacheCrossProcessChild is the helper the parent re-execs. It runs
// only when env vars are set; otherwise it skips so a normal `go test`
// invocation doesn't perform spurious mutations.
func TestCacheCrossProcessChild(t *testing.T) {
	dir := os.Getenv(envCacheChildStateDir)
	slot := os.Getenv(envCacheChildSlot)
	prefix := os.Getenv(envCacheChildPrefix)
	nStr := os.Getenv(envCacheChildN)
	if dir == "" || slot == "" || prefix == "" || nStr == "" {
		t.Skip("not invoked as cross-process child")
	}
	var n int
	if _, err := fmt.Sscanf(nStr, "%d", &n); err != nil || n <= 0 {
		t.Fatalf("invalid %s=%q", envCacheChildN, nStr)
	}

	for i := 0; i < n; i++ {
		dispatchID := fmt.Sprintf("%s-%d", prefix, i)
		err := MutateSlotState(dir, slot, func(s *SlotState) error {
			s.InFlight = append(s.InFlight, InFlightClaim{
				DispatchID: dispatchID,
				ClaimedAt:  time.Now(),
			})
			return nil
		})
		if err != nil {
			t.Fatalf("MutateSlotState %s: %v", dispatchID, err)
		}
	}
}

func keysOf(m map[string]*SlotState) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func dispatchIDs(claims []InFlightClaim) string {
	ids := make([]string, 0, len(claims))
	for _, c := range claims {
		ids = append(ids, c.DispatchID)
	}
	sort.Strings(ids)
	return strings.Join(ids, ",")
}
