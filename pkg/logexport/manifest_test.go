package logexport

import (
	"context"
	"errors"
	"testing"
	"time"

	pkgstore "github.com/awell-health/spire/pkg/store"

	"github.com/awell-health/spire/pkg/logartifact"
)

// TestRetry_SucceedsOnFirstAttempt asserts the happy path: a non-error
// op returns immediately with retries=0.
func TestRetry_SucceedsOnFirstAttempt(t *testing.T) {
	ctx := context.Background()
	calls := 0
	retries, err := retry(ctx, retryPolicy{MaxAttempts: 3}, func() error {
		calls++
		return nil
	})
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if retries != 0 {
		t.Errorf("retries = %d, want 0", retries)
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1", calls)
	}
}

// TestRetry_RetriesTransientThenSucceeds drives the retry loop through
// two transient failures before the third call succeeds. Verifies that
// the retries counter excludes the initial attempt and the successful
// final attempt.
func TestRetry_RetriesTransientThenSucceeds(t *testing.T) {
	ctx := context.Background()
	calls := 0
	retries, err := retry(ctx, retryPolicy{MaxAttempts: 5, BaseDelay: 0, MaxDelay: 0}, func() error {
		calls++
		if calls < 3 {
			return errors.New("transient")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if calls != 3 {
		t.Errorf("calls = %d, want 3", calls)
	}
	if retries != 2 {
		t.Errorf("retries = %d, want 2", retries)
	}
}

// TestRetry_TerminalErrorShortCircuits asserts that a non-transient
// error (logartifact.ErrNotFound or pkgstore.ErrLogArtifactExists)
// stops the loop on the first attempt.
func TestRetry_TerminalErrorShortCircuits(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name string
		err  error
	}{
		{"ErrNotFound", logartifact.ErrNotFound},
		{"ErrLogArtifactExists", pkgstore.ErrLogArtifactExists},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			retries, err := retry(ctx, retryPolicy{MaxAttempts: 5}, func() error {
				calls++
				return tc.err
			})
			if !errors.Is(err, tc.err) {
				t.Fatalf("retry returned %v, want %v", err, tc.err)
			}
			if calls != 1 {
				t.Errorf("calls = %d, want 1 (no retries on terminal error)", calls)
			}
			if retries != 0 {
				t.Errorf("retries = %d, want 0", retries)
			}
		})
	}
}

// TestRetry_ExhaustsAttempts verifies the loop returns the most recent
// error after MaxAttempts and surfaces the count of retries (excluding
// the initial attempt).
func TestRetry_ExhaustsAttempts(t *testing.T) {
	ctx := context.Background()
	calls := 0
	retries, err := retry(ctx, retryPolicy{MaxAttempts: 3, BaseDelay: 0}, func() error {
		calls++
		return errors.New("still broken")
	})
	if err == nil {
		t.Fatalf("retry returned nil, want error")
	}
	if calls != 3 {
		t.Errorf("calls = %d, want 3", calls)
	}
	if retries != 2 {
		t.Errorf("retries = %d, want 2", retries)
	}
}

// TestRetry_HonorsContextCancel exits the loop when ctx is cancelled
// before backoff. The retry surface checks ctx.Err at the start of each
// iteration, so an immediate cancel exits without invoking op — the
// design favors fast cancel propagation over guaranteeing one attempt.
func TestRetry_HonorsContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // immediate cancel

	calls := 0
	_, err := retry(ctx, retryPolicy{MaxAttempts: 3, BaseDelay: 100 * time.Millisecond}, func() error {
		calls++
		return errors.New("transient")
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("retry returned %v, want context.Canceled", err)
	}
	if calls != 0 {
		t.Errorf("calls = %d, want 0 (cancel before first attempt)", calls)
	}
}

// TestRetry_CancelDuringBackoff verifies a cancel that fires after the
// first attempt's transient failure surfaces context.Canceled and
// preserves the underlying error in the message.
func TestRetry_CancelDuringBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	calls := 0
	_, err := retry(ctx, retryPolicy{MaxAttempts: 3, BaseDelay: 200 * time.Millisecond, MaxDelay: 200 * time.Millisecond}, func() error {
		calls++
		if calls == 1 {
			// Cancel before the next attempt's backoff completes so
			// the timer.Stop path exits cleanly.
			go cancel()
		}
		return errors.New("transient")
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("retry returned %v, want context.Canceled", err)
	}
	if calls < 1 {
		t.Errorf("calls = %d, want >= 1", calls)
	}
}

// TestErrIsTransient covers the predicate used by retry to short-
// circuit terminal errors.
func TestErrIsTransient(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"ErrNotFound", logartifact.ErrNotFound, false},
		{"ErrLogArtifactExists", pkgstore.ErrLogArtifactExists, false},
		{"generic error", errors.New("dial tcp: connection refused"), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := errIsTransient(tc.err); got != tc.want {
				t.Errorf("errIsTransient(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestMarkManifestFailed_NilDBIsNoop asserts the best-effort guard:
// calling markManifestFailed with a nil DB returns (0, nil) without
// touching the wire.
func TestMarkManifestFailed_NilDBIsNoop(t *testing.T) {
	retries, err := markManifestFailed(context.Background(), nil, "log-abc123", DefaultRetryPolicy)
	if err != nil {
		t.Errorf("markManifestFailed(nil db) = %v, want nil", err)
	}
	if retries != 0 {
		t.Errorf("retries = %d, want 0", retries)
	}
}
