package steward

import (
	"bytes"
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/awell-health/spire/pkg/auth/pool"
	"github.com/awell-health/spire/pkg/config"
)

// stubAuthPoolWake is a no-op pool.PoolWake used by the sweep tests
// so sweepOneTower / runOneAuthPoolSweep can exercise the full call
// chain without standing up a real LocalPoolWake.
type stubAuthPoolWake struct{}

func (stubAuthPoolWake) Wait(ctx context.Context, pool string) error { return nil }
func (stubAuthPoolWake) Broadcast(string) error                      { return nil }

// runAuthPoolSweepWithDeadline starts RunAuthPoolSweep in a goroutine
// and returns a stop function that cancels the context and asserts a
// clean exit within waitFor — the no-goroutine-leak guarantee on
// every test that drives the loop.
func runAuthPoolSweepWithDeadline(t *testing.T, waitFor time.Duration) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		RunAuthPoolSweep(ctx)
		close(done)
	}()
	return func() {
		cancel()
		select {
		case <-done:
		case <-time.After(waitFor):
			t.Fatalf("RunAuthPoolSweep did not return within %s after cancel — goroutine leak", waitFor)
		}
	}
}

func setAuthPoolSweepInterval(t *testing.T, d time.Duration) {
	t.Helper()
	prev := authPoolSweepInterval
	authPoolSweepInterval = d
	t.Cleanup(func() { authPoolSweepInterval = prev })
}

func setAuthPoolSweepTickFunc(t *testing.T, fn func()) {
	t.Helper()
	prev := authPoolSweepTickFunc
	authPoolSweepTickFunc = fn
	t.Cleanup(func() { authPoolSweepTickFunc = prev })
}

func setAuthPoolSweepSeams(
	t *testing.T,
	listTowers func() ([]config.TowerConfig, error),
	loadConfig func(string) (*pool.Config, error),
	newWake func(string) pool.PoolWake,
	sweep func(string, time.Duration, pool.PoolWake, *pool.Config) (int, error),
) {
	t.Helper()
	prevList := authPoolListTowersFn
	prevLoad := authPoolLoadConfigFn
	prevWake := authPoolNewWakeFn
	prevSweep := authPoolSweepFn
	if listTowers != nil {
		authPoolListTowersFn = listTowers
	}
	if loadConfig != nil {
		authPoolLoadConfigFn = loadConfig
	}
	if newWake != nil {
		authPoolNewWakeFn = newWake
	}
	if sweep != nil {
		authPoolSweepFn = sweep
	}
	t.Cleanup(func() {
		authPoolListTowersFn = prevList
		authPoolLoadConfigFn = prevLoad
		authPoolNewWakeFn = prevWake
		authPoolSweepFn = prevSweep
	})
}

func captureAuthPoolSweepLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prevOut := log.Writer()
	prevFlags := log.Flags()
	prevPrefix := log.Prefix()
	log.SetOutput(&buf)
	log.SetFlags(0)
	log.SetPrefix("")
	t.Cleanup(func() {
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
		log.SetPrefix(prevPrefix)
	})
	return &buf
}

func TestRunAuthPoolSweep_TickerFires(t *testing.T) {
	setAuthPoolSweepInterval(t, 5*time.Millisecond)
	var ticks atomic.Int32
	setAuthPoolSweepTickFunc(t, func() { ticks.Add(1) })

	stop := runAuthPoolSweepWithDeadline(t, time.Second)
	defer stop()

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		// Startup tick + at least 2 ticker ticks. With a 5ms interval
		// and a 500ms ceiling we have ~100x headroom against CI
		// jitter; if this is flaky, the loop is broken, not the bound.
		if ticks.Load() >= 3 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("ticker did not fire 3 times within 500ms (got %d)", ticks.Load())
}

func TestRunAuthPoolSweep_ShutdownClean(t *testing.T) {
	setAuthPoolSweepInterval(t, 5*time.Millisecond)
	setAuthPoolSweepTickFunc(t, func() {})

	stop := runAuthPoolSweepWithDeadline(t, 200*time.Millisecond)
	stop()
}

// TestRunAuthPoolSweep_StartupTickRunsImmediately uses a deliberately
// long ticker interval so the only tick that can land within the
// observation window is the one RunAuthPoolSweep fires before
// installing the ticker — guarding the "reap leftover claims at
// startup" contract.
func TestRunAuthPoolSweep_StartupTickRunsImmediately(t *testing.T) {
	setAuthPoolSweepInterval(t, time.Hour)
	var ticks atomic.Int32
	setAuthPoolSweepTickFunc(t, func() { ticks.Add(1) })

	stop := runAuthPoolSweepWithDeadline(t, time.Second)
	defer stop()

	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		if ticks.Load() >= 1 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("startup tick did not fire within 200ms (got %d)", ticks.Load())
}

// TestSweepOneTower_LoadConfigMissingSkips covers the steady-state
// "no auth pool configured" path: pool.LoadConfig wraps
// os.ErrNotExist when both auth.toml and legacy credentials.toml are
// absent, and sweepOneTower must return silently — neither
// constructing a wake nor calling pool.Sweep.
func TestSweepOneTower_LoadConfigMissingSkips(t *testing.T) {
	var sweepCalls, wakeCalls atomic.Int32
	setAuthPoolSweepSeams(t,
		nil,
		func(string) (*pool.Config, error) { return nil, os.ErrNotExist },
		func(string) pool.PoolWake {
			wakeCalls.Add(1)
			return stubAuthPoolWake{}
		},
		func(string, time.Duration, pool.PoolWake, *pool.Config) (int, error) {
			sweepCalls.Add(1)
			return 0, nil
		},
	)
	buf := captureAuthPoolSweepLogs(t)

	sweepOneTower(config.TowerConfig{Name: "test"})

	if sweepCalls.Load() != 0 {
		t.Errorf("sweep called %d times despite missing config; want 0", sweepCalls.Load())
	}
	if wakeCalls.Load() != 0 {
		t.Errorf("newWake called %d times despite missing config; want 0", wakeCalls.Load())
	}
	if got := buf.String(); got != "" {
		t.Errorf("expected no log output for missing-config skip; got %q", got)
	}
}

// TestSweepOneTower_LoadConfigMissingSkipsViaRealLoader exercises the
// real pool.LoadConfig against a tower dir that has neither auth.toml
// nor a legacy credentials.toml. The seam-stubbed test above proves
// the contract on top of the seam; this one proves the contract on
// top of the real loader, so a future change to the wrapping in
// pool.LoadConfig (e.g. losing the os.ErrNotExist sentinel) trips
// here too.
func TestSweepOneTower_LoadConfigMissingSkipsViaRealLoader(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	tc := config.TowerConfig{Name: "ephemeral"}
	towerDir := filepath.Dir(tc.OLAPPath())
	if err := os.MkdirAll(towerDir, 0o755); err != nil {
		t.Fatalf("mkdir towerDir: %v", err)
	}

	var sweepCalls atomic.Int32
	setAuthPoolSweepSeams(t,
		nil,
		nil, // real pool.LoadConfig — should hit os.ErrNotExist
		func(string) pool.PoolWake { return stubAuthPoolWake{} },
		func(string, time.Duration, pool.PoolWake, *pool.Config) (int, error) {
			sweepCalls.Add(1)
			return 0, nil
		},
	)
	buf := captureAuthPoolSweepLogs(t)

	sweepOneTower(tc)

	if sweepCalls.Load() != 0 {
		t.Errorf("sweep called %d times against empty tower dir; want 0", sweepCalls.Load())
	}
	if got := buf.String(); got != "" {
		t.Errorf("expected no log output for missing-config skip; got %q", got)
	}
}

func TestSweepOneTower_LoadConfigOtherErrorSkips(t *testing.T) {
	var sweepCalls atomic.Int32
	setAuthPoolSweepSeams(t,
		nil,
		func(string) (*pool.Config, error) { return nil, errors.New("malformed toml") },
		func(string) pool.PoolWake { return stubAuthPoolWake{} },
		func(string, time.Duration, pool.PoolWake, *pool.Config) (int, error) {
			sweepCalls.Add(1)
			return 0, nil
		},
	)
	buf := captureAuthPoolSweepLogs(t)

	sweepOneTower(config.TowerConfig{Name: "tower-a"})

	if sweepCalls.Load() != 0 {
		t.Errorf("sweep called %d times despite LoadConfig error; want 0", sweepCalls.Load())
	}
	got := buf.String()
	if !strings.Contains(got, "[tower-a]") {
		t.Errorf("expected log to include tower name [tower-a]; got %q", got)
	}
	if !strings.Contains(got, "malformed toml") {
		t.Errorf("expected log to include underlying error 'malformed toml'; got %q", got)
	}
}

func TestSweepOneTower_RemovedCountLogged(t *testing.T) {
	setAuthPoolSweepSeams(t,
		nil,
		func(string) (*pool.Config, error) { return &pool.Config{}, nil },
		func(string) pool.PoolWake { return stubAuthPoolWake{} },
		func(string, time.Duration, pool.PoolWake, *pool.Config) (int, error) { return 3, nil },
	)
	buf := captureAuthPoolSweepLogs(t)

	sweepOneTower(config.TowerConfig{Name: "tower-b"})

	got := buf.String()
	if !strings.Contains(got, "[tower-b]") {
		t.Errorf("expected log to include tower name [tower-b]; got %q", got)
	}
	if !strings.Contains(got, "removed 3 stale claim(s)") {
		t.Errorf("expected log to mention removed count; got %q", got)
	}
}

// TestSweepOneTower_RemovedZeroSilent guards the "no spam" property:
// a steady-state sweep cycle where every claim is fresh must produce
// zero log lines, otherwise the steward log fills with noise at the
// 30s cadence and operators stop reading it.
func TestSweepOneTower_RemovedZeroSilent(t *testing.T) {
	setAuthPoolSweepSeams(t,
		nil,
		func(string) (*pool.Config, error) { return &pool.Config{}, nil },
		func(string) pool.PoolWake { return stubAuthPoolWake{} },
		func(string, time.Duration, pool.PoolWake, *pool.Config) (int, error) { return 0, nil },
	)
	buf := captureAuthPoolSweepLogs(t)

	sweepOneTower(config.TowerConfig{Name: "tower-c"})

	if got := buf.String(); got != "" {
		t.Errorf("expected no log for removed=0; got %q", got)
	}
}

func TestSweepOneTower_StateDirIsAuthStateUnderTowerDir(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	tc := config.TowerConfig{Name: "Test Tower"}
	expectedTowerDir := filepath.Dir(tc.OLAPPath())
	expectedStateDir := filepath.Join(expectedTowerDir, "auth-state")

	var seenSweepDir, seenWakeDir, seenLoadDir string
	setAuthPoolSweepSeams(t,
		nil,
		func(towerDir string) (*pool.Config, error) {
			seenLoadDir = towerDir
			return &pool.Config{}, nil
		},
		func(stateDir string) pool.PoolWake {
			seenWakeDir = stateDir
			return stubAuthPoolWake{}
		},
		func(stateDir string, _ time.Duration, _ pool.PoolWake, _ *pool.Config) (int, error) {
			seenSweepDir = stateDir
			return 0, nil
		},
	)

	sweepOneTower(tc)

	if seenLoadDir != expectedTowerDir {
		t.Errorf("LoadConfig towerDir = %q, want %q", seenLoadDir, expectedTowerDir)
	}
	if seenWakeDir != expectedStateDir {
		t.Errorf("NewPoolWake stateDir = %q, want %q", seenWakeDir, expectedStateDir)
	}
	if seenSweepDir != expectedStateDir {
		t.Errorf("Sweep stateDir = %q, want %q", seenSweepDir, expectedStateDir)
	}
}

// TestSweepOneTower_PassesStaleAge guards the "60s = 4× heartbeat"
// invariant: pool.Sweep must receive authPoolSweepStaleAge (60s)
// regardless of how the steward's own ticker is paced. A future
// refactor that reuses the ticker interval as staleAge would silently
// pull capacity out from under healthy claims.
func TestSweepOneTower_PassesStaleAge(t *testing.T) {
	var seenStaleAge time.Duration
	setAuthPoolSweepSeams(t,
		nil,
		func(string) (*pool.Config, error) { return &pool.Config{}, nil },
		func(string) pool.PoolWake { return stubAuthPoolWake{} },
		func(_ string, staleAge time.Duration, _ pool.PoolWake, _ *pool.Config) (int, error) {
			seenStaleAge = staleAge
			return 0, nil
		},
	)

	sweepOneTower(config.TowerConfig{Name: "tower-d"})

	if seenStaleAge != authPoolSweepStaleAge {
		t.Errorf("Sweep staleAge = %s, want %s", seenStaleAge, authPoolSweepStaleAge)
	}
	if authPoolSweepStaleAge != 60*time.Second {
		t.Errorf("authPoolSweepStaleAge = %s, want 60s (4× wizard heartbeat)", authPoolSweepStaleAge)
	}
}

// TestRunOneAuthPoolSweep_IteratesAllTowers confirms the cycle visits
// every tower returned by ListTowerConfigs — a typo here would mean
// some towers never get their stale claims reaped.
func TestRunOneAuthPoolSweep_IteratesAllTowers(t *testing.T) {
	var sweepCalls atomic.Int32
	var visited []string
	setAuthPoolSweepSeams(t,
		func() ([]config.TowerConfig, error) {
			return []config.TowerConfig{
				{Name: "alpha"},
				{Name: "beta"},
				{Name: "gamma"},
			}, nil
		},
		func(towerDir string) (*pool.Config, error) {
			visited = append(visited, towerDir)
			return &pool.Config{}, nil
		},
		func(string) pool.PoolWake { return stubAuthPoolWake{} },
		func(string, time.Duration, pool.PoolWake, *pool.Config) (int, error) {
			sweepCalls.Add(1)
			return 0, nil
		},
	)

	runOneAuthPoolSweep()

	if got := sweepCalls.Load(); got != 3 {
		t.Errorf("sweep call count = %d, want 3", got)
	}
	if len(visited) != 3 {
		t.Fatalf("expected 3 tower dirs visited; got %d (%v)", len(visited), visited)
	}
}

func TestRunOneAuthPoolSweep_ListTowersErrorSkips(t *testing.T) {
	var sweepCalls atomic.Int32
	setAuthPoolSweepSeams(t,
		func() ([]config.TowerConfig, error) { return nil, errors.New("disk gone") },
		func(string) (*pool.Config, error) { return &pool.Config{}, nil },
		func(string) pool.PoolWake { return stubAuthPoolWake{} },
		func(string, time.Duration, pool.PoolWake, *pool.Config) (int, error) {
			sweepCalls.Add(1)
			return 0, nil
		},
	)
	buf := captureAuthPoolSweepLogs(t)

	runOneAuthPoolSweep()

	if got := sweepCalls.Load(); got != 0 {
		t.Errorf("sweep called %d times despite ListTowers error; want 0", got)
	}
	if got := buf.String(); !strings.Contains(got, "list towers") {
		t.Errorf("expected log to mention 'list towers'; got %q", got)
	}
}

// TestRunOneAuthPoolSweep_NoTowersIsNoop covers the legacy
// single-tower / un-configured-tower-set case: ListTowerConfigs
// returns zero towers, the cycle is a no-op, and the loop must not
// crash or spam.
func TestRunOneAuthPoolSweep_NoTowersIsNoop(t *testing.T) {
	var sweepCalls atomic.Int32
	setAuthPoolSweepSeams(t,
		func() ([]config.TowerConfig, error) { return nil, nil },
		func(string) (*pool.Config, error) { return &pool.Config{}, nil },
		func(string) pool.PoolWake { return stubAuthPoolWake{} },
		func(string, time.Duration, pool.PoolWake, *pool.Config) (int, error) {
			sweepCalls.Add(1)
			return 0, nil
		},
	)
	buf := captureAuthPoolSweepLogs(t)

	runOneAuthPoolSweep()

	if got := sweepCalls.Load(); got != 0 {
		t.Errorf("sweep called %d times despite no towers; want 0", got)
	}
	if got := buf.String(); got != "" {
		t.Errorf("expected no log output for empty tower list; got %q", got)
	}
}

// TestRunOneAuthPoolSweep_OneTowerErrorDoesNotAbortCycle proves the
// per-tower failure isolation: a malformed config on tower A must not
// stop the sweep from running against tower B. Without this, a single
// bad config silently breaks the multi-token pool for every other
// tower in the steward.
func TestRunOneAuthPoolSweep_OneTowerErrorDoesNotAbortCycle(t *testing.T) {
	var sweepCalls atomic.Int32
	setAuthPoolSweepSeams(t,
		func() ([]config.TowerConfig, error) {
			return []config.TowerConfig{
				{Name: "broken"},
				{Name: "healthy"},
			}, nil
		},
		func(towerDir string) (*pool.Config, error) {
			if strings.Contains(towerDir, "broken") {
				return nil, errors.New("malformed toml")
			}
			return &pool.Config{}, nil
		},
		func(string) pool.PoolWake { return stubAuthPoolWake{} },
		func(string, time.Duration, pool.PoolWake, *pool.Config) (int, error) {
			sweepCalls.Add(1)
			return 0, nil
		},
	)

	runOneAuthPoolSweep()

	if got := sweepCalls.Load(); got != 1 {
		t.Errorf("sweep call count = %d, want 1 (only the healthy tower)", got)
	}
}
