package executor

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/awell-health/spire/pkg/agent"
)

// fakeAuthPool is a stub AuthPool for the dispatch-auth tests. It records
// the dispatchID Acquire is called with and returns a canned outcome.
type fakeAuthPool struct {
	pickedDispatchID string
	pickedSlot       string
	pickedPool       string
	authEnv          []string
	stateDir         string

	// returnErr, when non-nil, is returned from Acquire instead of the
	// pickedSlot lease.
	returnErr error

	// releaseCalls counts release invocations so tests can verify
	// idempotence and deferred-release semantics.
	releaseCalls int32
}

func (f *fakeAuthPool) Acquire(ctx context.Context, dispatchID string) (PoolLease, error) {
	if f.returnErr != nil {
		return PoolLease{}, f.returnErr
	}
	f.pickedDispatchID = dispatchID
	return PoolLease{
		SlotName:     f.pickedSlot,
		PoolName:     f.pickedPool,
		AuthEnv:      f.authEnv,
		PoolStateDir: f.stateDir,
		Release: func() {
			atomic.AddInt32(&f.releaseCalls, 1)
		},
	}, nil
}

func TestAcquireAuthPoolSlot_NilDeps(t *testing.T) {
	cfg := agent.SpawnConfig{Name: "wizard-spi-x"}
	got, release, err := acquireAuthPoolSlot(context.Background(), nil, cfg, "wizard-spi-x")
	if err != nil {
		t.Fatalf("nil deps acquireAuthPoolSlot: unexpected err = %v", err)
	}
	if release == nil {
		t.Fatal("nil deps: release must be a no-op (non-nil) so callers can defer unconditionally")
	}
	release() // must not panic
	if got.AuthEnv != nil {
		t.Errorf("nil deps: cfg.AuthEnv = %v, want nil (cfg unchanged)", got.AuthEnv)
	}
}

func TestAcquireAuthPoolSlot_NilAuthPool(t *testing.T) {
	deps := &Deps{} // AuthPool is nil
	cfg := agent.SpawnConfig{Name: "wizard-spi-x"}
	got, release, err := acquireAuthPoolSlot(context.Background(), deps, cfg, "wizard-spi-x")
	if err != nil {
		t.Fatalf("nil AuthPool: unexpected err = %v", err)
	}
	if release == nil {
		t.Fatal("nil AuthPool: release must be a no-op (non-nil)")
	}
	release()
	if got.AuthSlot != "" {
		t.Errorf("nil AuthPool: cfg.AuthSlot = %q, want empty", got.AuthSlot)
	}
}

func TestAcquireAuthPoolSlot_StampsLease(t *testing.T) {
	pool := &fakeAuthPool{
		pickedSlot:  "slot-a",
		pickedPool:  "subscription",
		authEnv:     []string{"CLAUDE_CODE_OAUTH_TOKEN=tok-a"},
		stateDir:    "/tmp/auth-state",
	}
	deps := &Deps{AuthPool: pool}
	cfg := agent.SpawnConfig{Name: "wizard-spi-x-impl-1"}
	got, release, err := acquireAuthPoolSlot(context.Background(), deps, cfg, "wizard-spi-x-impl-1")
	if err != nil {
		t.Fatalf("acquireAuthPoolSlot: %v", err)
	}
	defer release()

	if pool.pickedDispatchID != "wizard-spi-x-impl-1" {
		t.Errorf("dispatchID = %q, want %q", pool.pickedDispatchID, "wizard-spi-x-impl-1")
	}
	if got.AuthSlot != "slot-a" {
		t.Errorf("cfg.AuthSlot = %q, want %q", got.AuthSlot, "slot-a")
	}
	if got.PoolStateDir != "/tmp/auth-state" {
		t.Errorf("cfg.PoolStateDir = %q, want %q", got.PoolStateDir, "/tmp/auth-state")
	}
	if len(got.AuthEnv) != 1 || got.AuthEnv[0] != "CLAUDE_CODE_OAUTH_TOKEN=tok-a" {
		t.Errorf("cfg.AuthEnv = %v, want [CLAUDE_CODE_OAUTH_TOKEN=tok-a]", got.AuthEnv)
	}
}

func TestAcquireAuthPoolSlot_PropagatesError(t *testing.T) {
	want := errors.New("acquire failed")
	deps := &Deps{AuthPool: &fakeAuthPool{returnErr: want}}
	cfg := agent.SpawnConfig{Name: "wizard-spi-x"}
	_, release, err := acquireAuthPoolSlot(context.Background(), deps, cfg, "wizard-spi-x")
	if !errors.Is(err, want) {
		t.Errorf("err = %v, want %v", err, want)
	}
	if release == nil {
		t.Fatal("release must be non-nil even on Acquire failure (no-op)")
	}
	release() // must not panic
}

func TestAcquireAuthPoolSlot_PropagatesRateLimitedError(t *testing.T) {
	resets := time.Now().Add(time.Hour)
	rateLimited := &RateLimitedError{ResetsAt: resets}
	deps := &Deps{AuthPool: &fakeAuthPool{returnErr: rateLimited}}
	cfg := agent.SpawnConfig{Name: "wizard-spi-x"}
	_, _, err := acquireAuthPoolSlot(context.Background(), deps, cfg, "wizard-spi-x")
	var rl *RateLimitedError
	if !errors.As(err, &rl) {
		t.Fatalf("err = %v, want *RateLimitedError", err)
	}
	if !rl.ResetsAt.Equal(resets) {
		t.Errorf("ResetsAt = %v, want %v", rl.ResetsAt, resets)
	}
}

func TestAcquireAuthPoolSlot_EmptyDispatchID(t *testing.T) {
	deps := &Deps{AuthPool: &fakeAuthPool{pickedSlot: "slot-a"}}
	cfg := agent.SpawnConfig{Name: ""}
	_, release, err := acquireAuthPoolSlot(context.Background(), deps, cfg, "")
	if err == nil {
		t.Fatal("empty dispatchID: want error, got nil")
	}
	if release == nil {
		t.Fatal("empty dispatchID: release must still be non-nil")
	}
}

func TestRateLimitedError_Error(t *testing.T) {
	t.Run("with ResetsAt", func(t *testing.T) {
		resets := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
		e := &RateLimitedError{ResetsAt: resets}
		if got := e.Error(); got == "" {
			t.Error("Error() empty, want non-empty")
		}
	})
	t.Run("zero ResetsAt", func(t *testing.T) {
		e := &RateLimitedError{}
		if got := e.Error(); got == "" {
			t.Error("Error() empty, want non-empty")
		}
	})
	t.Run("with wrapped error", func(t *testing.T) {
		inner := errors.New("inner")
		e := &RateLimitedError{Wrapped: inner}
		if !errors.Is(e, inner) {
			t.Error("errors.Is should match wrapped inner error")
		}
	})
}
