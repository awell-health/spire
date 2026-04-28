package logexport

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	pkgstore "github.com/awell-health/spire/pkg/store"

	"github.com/awell-health/spire/pkg/logartifact"
)

// retryPolicy defines the bounded exponential backoff used by
// finalize/manifest operations. Production callers use
// DefaultRetryPolicy; tests inject narrower policies so unit tests
// don't sleep.
type retryPolicy struct {
	// MaxAttempts caps the total number of attempts including the
	// initial one. <=0 disables retry (single attempt).
	MaxAttempts int
	// BaseDelay is the first backoff interval. Each subsequent attempt
	// doubles up to MaxDelay.
	BaseDelay time.Duration
	// MaxDelay caps the per-attempt sleep so a long-running outage
	// doesn't compound into multi-minute pauses.
	MaxDelay time.Duration
}

// DefaultRetryPolicy is the production retry policy. Five attempts at
// 50ms / 100ms / 200ms / 400ms / 800ms covers transient dolt connection
// blips without holding the tailer for more than ~1.5s; if the manifest
// surface is genuinely down past that window, the exporter records
// status=failed and moves on rather than blocking agent log production.
var DefaultRetryPolicy = retryPolicy{
	MaxAttempts: 5,
	BaseDelay:   50 * time.Millisecond,
	MaxDelay:    800 * time.Millisecond,
}

// noRetry is a useful synonym for tests that want a single attempt
// without exponential backoff.
var noRetry = retryPolicy{MaxAttempts: 1}

// retry runs op with bounded exponential backoff. Returns op's last
// error if every attempt failed, plus the count of retries (NOT
// including the initial attempt) the loop performed. The retry counter
// is surfaced via Stats.ManifestRetries so operators can observe
// transient tower slowness without instrumenting every callsite.
//
// retry honors ctx.Done — a cancelled context exits early with ctx.Err
// merged with the most recent op error.
func retry(ctx context.Context, p retryPolicy, op func() error) (int, error) {
	if p.MaxAttempts <= 0 {
		p.MaxAttempts = 1
	}
	delay := p.BaseDelay
	var last error
	retries := 0
	for attempt := 0; attempt < p.MaxAttempts; attempt++ {
		if ctx.Err() != nil {
			if last != nil {
				return retries, fmt.Errorf("%w (after %d attempts; last error: %v)", ctx.Err(), attempt, last)
			}
			return retries, ctx.Err()
		}
		err := op()
		if err == nil {
			return retries, nil
		}
		last = err
		if !errIsTransient(err) {
			return retries, err
		}
		if attempt == p.MaxAttempts-1 {
			break
		}
		retries++
		// Sleep with respect to ctx.Done so a fast cancel doesn't burn
		// the whole backoff window.
		t := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			t.Stop()
			return retries, fmt.Errorf("%w (after %d attempts; last error: %v)", ctx.Err(), attempt+1, last)
		case <-t.C:
		}
		delay *= 2
		if p.MaxDelay > 0 && delay > p.MaxDelay {
			delay = p.MaxDelay
		}
	}
	return retries, last
}

// markManifestFailed flips the manifest row to status=failed when an
// upload or finalize errored out. Best-effort: if db is nil or the
// status update itself fails, we return the underlying error to the
// caller (informational) but never propagate it past the exporter
// boundary — agent success/failure must not depend on this bookkeeping.
//
// db may be nil for tests that exercise the upload path without a tower
// connection; the call becomes a no-op in that case.
func markManifestFailed(ctx context.Context, db *sql.DB, manifestID string, policy retryPolicy) (int, error) {
	if db == nil || manifestID == "" {
		return 0, nil
	}
	return retry(ctx, policy, func() error {
		return pkgstore.UpdateLogArtifactStatus(ctx, db, manifestID, pkgstore.LogArtifactStatusFailed)
	})
}

// errIsTransient is the predicate the retry path uses to decide whether
// to back off and try again. Terminal errors (NotFound, AlreadyExists)
// re-running won't fix; everything else is treated as transient because
// the exporter does not want to give up on a one-off connection blip.
func errIsTransient(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, logartifact.ErrNotFound) {
		return false
	}
	if errors.Is(err, pkgstore.ErrLogArtifactExists) {
		return false
	}
	return true
}
