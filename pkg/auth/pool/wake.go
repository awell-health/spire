// Package pool provides multi-slot auth pool primitives.
//
// PoolWake is the wake primitive used by the slot selector to block
// when no slot is currently eligible (all slots at MaxConcurrent or
// throttled, but capacity is not permanently exhausted) and to wake
// those blocked waiters when a peer Releases a claim or the rate-limit
// sweep clears a stale entry.
//
// Two implementations are provided:
//
//   - LocalPoolWake: in-process map of *sync.Cond keyed by pool name.
//     Sufficient when all wizards run as goroutines or as subprocesses
//     of one steward funneling Release events through a single PoolWake
//     instance.
//
//   - InotifyPoolWake (build tag linux): file-based wake using inotify
//     on <stateDir>/.wake-<pool>. Broadcast touches the file
//     (write-then-fsync); Wait watches IN_CLOSE_WRITE on it. Suitable
//     for cluster mode where independent wizard processes share a state
//     volume.
//
// NewPoolWake returns a LocalPoolWake by default. Set the environment
// variable SPIRE_POOL_WAKE=inotify to opt in to the inotify backend.
// On non-linux platforms the inotify backend is not available and
// NewPoolWake always returns LocalPoolWake.
package pool

import (
	"context"
	"os"
	"sync"
)

// PoolWake is the wake primitive consumed by the slot selector.
type PoolWake interface {
	// Wait blocks until either Broadcast is called for pool, or ctx
	// is cancelled. Returns ctx.Err() on cancellation, nil on wake.
	Wait(ctx context.Context, pool string) error
	// Broadcast wakes all current Waiters on pool.
	Broadcast(pool string) error
}

// NewPoolWake returns a PoolWake suitable for the current runtime.
// Defaults to LocalPoolWake. Honors SPIRE_POOL_WAKE=inotify on linux.
func NewPoolWake(stateDir string) PoolWake {
	if os.Getenv("SPIRE_POOL_WAKE") == "inotify" {
		if w, err := newInotifyPoolWake(stateDir); err == nil && w != nil {
			return w
		}
	}
	return NewLocalPoolWake()
}

// LocalPoolWake is an in-process PoolWake implementation backed by
// per-pool sync.Cond. Safe for concurrent use across goroutines.
type LocalPoolWake struct {
	mu    sync.Mutex
	conds map[string]*localCond
}

// NewLocalPoolWake constructs a LocalPoolWake with no waiters.
func NewLocalPoolWake() *LocalPoolWake {
	return &LocalPoolWake{conds: make(map[string]*localCond)}
}

// localCond pairs a sync.Cond with a generation counter so Wait can
// safely treat ctx cancellation as a wake source by bumping gen and
// broadcasting from a watcher goroutine.
type localCond struct {
	mu   sync.Mutex
	cond *sync.Cond
	gen  uint64
}

func (l *LocalPoolWake) condFor(pool string) *localCond {
	l.mu.Lock()
	defer l.mu.Unlock()
	c, ok := l.conds[pool]
	if !ok {
		c = &localCond{}
		c.cond = sync.NewCond(&c.mu)
		l.conds[pool] = c
	}
	return c
}

// Wait blocks until Broadcast is called for pool or ctx is cancelled.
func (l *LocalPoolWake) Wait(ctx context.Context, pool string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	c := l.condFor(pool)

	c.mu.Lock()
	startGen := c.gen

	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			c.mu.Lock()
			c.gen++
			c.cond.Broadcast()
			c.mu.Unlock()
		case <-done:
		}
	}()

	for c.gen == startGen {
		c.cond.Wait()
	}
	c.mu.Unlock()

	return ctx.Err()
}

// Broadcast wakes all current Waiters on pool. It is safe to call
// Broadcast on a pool with no current waiters; the call is a no-op.
func (l *LocalPoolWake) Broadcast(pool string) error {
	c := l.condFor(pool)
	c.mu.Lock()
	c.gen++
	c.cond.Broadcast()
	c.mu.Unlock()
	return nil
}
