package bundlestore

import (
	"context"
	"errors"
	"log"
	"time"
)

// BeadInfo is the projection the janitor needs from the bead store. Kept
// minimal so we don't import pkg/store into pkg/bundlestore (that would
// risk an import cycle once store code eventually references bundle
// handles). The composition layer supplies the adapter.
type BeadInfo struct {
	// Status is the bead's current status (e.g. "closed").
	Status string
	// SealedAt is non-zero once the wizard has sealed the formula on
	// this bead. Until spi-rfee2 lands, this is always zero — the
	// janitor's sealed-branch is intentional dead code today.
	SealedAt time.Time
}

// Closed reports whether the bead is in a terminal status.
func (b BeadInfo) Closed() bool { return b.Status == "closed" }

// ErrBeadNotFound is returned by BeadLookup.GetBead when the bead
// referenced by a bundle no longer exists. The janitor treats this as
// "orphan" and eventually reaps the bundle after OrphanAge elapses.
var ErrBeadNotFound = errors.New("bundlestore: bead not found")

// BeadLookup resolves a bead ID to the projection the janitor needs.
// Implementations MUST return ErrBeadNotFound (not a wrapped variant,
// nor nil+empty BeadInfo) when the bead does not exist, so the janitor
// can distinguish orphans from transient lookup errors.
type BeadLookup interface {
	GetBead(ctx context.Context, beadID string) (BeadInfo, error)
}

// BeadLookupFunc adapts a function to the BeadLookup interface.
type BeadLookupFunc func(ctx context.Context, beadID string) (BeadInfo, error)

// GetBead implements BeadLookup.
func (f BeadLookupFunc) GetBead(ctx context.Context, beadID string) (BeadInfo, error) {
	return f(ctx, beadID)
}

// Janitor runs a periodic retention sweep over a BundleStore. Two rules:
//
//  1. Bead closed AND sealed_at set AND now - sealed_at > SealedGrace → delete.
//     This is the normal happy path: the wizard sealed the bead, we've given
//     downstream consumers 30 minutes to grab the bundle, now reclaim.
//
//  2. Bead lookup returns ErrBeadNotFound AND file mtime > OrphanAge → delete.
//     This handles the "bead deleted under us" case. 7 days is generous; the
//     idea is correctness, not aggressive reclamation.
//
// In-process Delete (when the wizard cleans up after merge) is the
// optimization. The janitor is the correctness net for everything that
// crashes, times out, or otherwise leaks.
type Janitor struct {
	Store       BundleStore
	Lookup      BeadLookup
	Interval    time.Duration // 0 = DefaultJanitorInterval
	SealedGrace time.Duration // 0 = DefaultSealedGrace
	OrphanAge   time.Duration // 0 = DefaultOrphanAge
	// Now is the clock source. nil = time.Now. Injected for tests.
	Now func() time.Time
	// Logger is used for per-handle failures. nil disables logging.
	Logger *log.Logger
}

// Run starts the janitor ticker loop. It returns when ctx is cancelled.
// The first sweep fires one Interval after start, not immediately, so
// that tests and boot sequences aren't perturbed by a sweep racing with
// other startup work.
func (j *Janitor) Run(ctx context.Context) {
	interval := j.Interval
	if interval <= 0 {
		interval = DefaultJanitorInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			j.Sweep(ctx)
		}
	}
}

// Sweep runs a single retention pass. Exposed so tests (and future
// spire CLI commands) can trigger reclamation without waiting on the
// ticker. Errors on a single handle are logged and skipped; one bad
// bundle must not stop the cycle.
func (j *Janitor) Sweep(ctx context.Context) {
	now := j.now()
	sealedGrace := j.SealedGrace
	if sealedGrace <= 0 {
		sealedGrace = DefaultSealedGrace
	}
	orphanAge := j.OrphanAge
	if orphanAge <= 0 {
		orphanAge = DefaultOrphanAge
	}

	handles, err := j.Store.List(ctx)
	if err != nil {
		j.logf("list: %s", err)
		return
	}
	for _, h := range handles {
		if ctx.Err() != nil {
			return
		}
		if !j.shouldReap(ctx, h, now, sealedGrace, orphanAge) {
			continue
		}
		if err := j.Store.Delete(ctx, h); err != nil {
			j.logf("delete %s/%s: %s", h.BeadID, h.Key, err)
		}
	}
}

// shouldReap evaluates the retention rules for a single handle.
func (j *Janitor) shouldReap(ctx context.Context, h BundleHandle, now time.Time, sealedGrace, orphanAge time.Duration) bool {
	bead, err := j.Lookup.GetBead(ctx, h.BeadID)
	if errors.Is(err, ErrBeadNotFound) {
		info, err := j.Store.Stat(ctx, h)
		if err != nil {
			// Lost race with another delete — nothing to do.
			return false
		}
		return now.Sub(info.ModTime) > orphanAge
	}
	if err != nil {
		j.logf("lookup %s: %s", h.BeadID, err)
		return false
	}
	if !bead.Closed() {
		return false
	}
	if bead.SealedAt.IsZero() {
		// Sealed-branch: dead code until spi-rfee2 lands. Leave it for
		// now; the orphan path handles truly stranded bundles.
		return false
	}
	return now.Sub(bead.SealedAt) > sealedGrace
}

func (j *Janitor) now() time.Time {
	if j.Now != nil {
		return j.Now()
	}
	return time.Now()
}

func (j *Janitor) logf(format string, args ...any) {
	if j.Logger == nil {
		return
	}
	j.Logger.Printf("bundlestore janitor: "+format, args...)
}
