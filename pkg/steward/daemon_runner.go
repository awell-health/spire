package steward

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/awell-health/spire/pkg/dolt"
)

// Daemon is a long-running sync worker. It owns the ticker, the debounce
// policy, and the strategy that does the actual pull/push. HTTP triggers,
// CLI wiring, and signal handling are caller concerns.
//
// Two modes today:
//   - Local multi-tower: wraps the existing DaemonCycle (per-user tower
//     iteration, CLI-based dolt push/pull). Use NewLocalDaemon.
//   - Cluster single-tower: server-side SQL pull/push against a running
//     dolt server. Use NewClusterDaemon with the database + remote + branch.
//
// Trigger() coalesces concurrent requests — in-progress or within-debounce
// calls are rejected with a clear error. This lets callers (gateway HTTP
// handler, signal handler, CLI) share one Daemon safely without duplicating
// rate-limit logic.
type Daemon struct {
	strategy syncStrategy
	interval time.Duration
	debounce time.Duration
	log      *log.Logger

	mu       sync.Mutex
	lastSync time.Time
	syncing  bool
}

// syncStrategy is the pluggable "how to sync" layer. Implementations do one
// full pull+push cycle or return an error. The Daemon handles when to call.
type syncStrategy interface {
	Sync(ctx context.Context, reason string) error
	Describe() string
}

// NewLocalDaemon constructs a Daemon that drives the existing multi-tower
// DaemonCycle — the laptop behavior. interval is the ticker cadence;
// debounce is the minimum gap between triggered syncs.
func NewLocalDaemon(interval, debounce time.Duration, logger *log.Logger) *Daemon {
	return &Daemon{
		strategy: &localStrategy{},
		interval: interval,
		debounce: debounce,
		log:      loggerOr(logger),
	}
}

// NewClusterDaemon constructs a Daemon that drives server-side SQL pull/push
// against a single dolt database. Used by the chart's syncer Deployment,
// where no local dolt repo exists.
func NewClusterDaemon(database, remote, branch string, interval, debounce time.Duration, logger *log.Logger) *Daemon {
	if remote == "" {
		remote = "origin"
	}
	if branch == "" {
		branch = "main"
	}
	return &Daemon{
		strategy: &clusterStrategy{database: database, remote: remote, branch: branch},
		interval: interval,
		debounce: debounce,
		log:      loggerOr(logger),
	}
}

// RunOnce runs one sync synchronously, bypassing debounce, and returns
// when the sync completes. Used by --once mode and tests. Not safe to
// call concurrently with Run or Trigger — it acquires the busy flag for
// its duration.
func (d *Daemon) RunOnce(reason string) error {
	d.mu.Lock()
	if d.syncing {
		d.mu.Unlock()
		return fmt.Errorf("sync already in progress")
	}
	d.syncing = true
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		d.lastSync = time.Now()
		d.syncing = false
		d.mu.Unlock()
	}()
	d.log.Printf("[daemon] sync start (trigger=%s, mode=%s)", reason, d.strategy.Describe())
	start := time.Now()
	err := d.strategy.Sync(context.Background(), reason)
	if err != nil {
		d.log.Printf("[daemon] sync error (trigger=%s): %s", reason, err)
	}
	d.log.Printf("[daemon] sync done (trigger=%s, took=%s)", reason, time.Since(start).Round(time.Millisecond))
	return err
}

// Trigger asks for an immediate sync. Returns nil when the sync has been
// started (as a goroutine — this call does not block on the sync itself),
// or a non-nil error describing why the request was declined. Safe to call
// concurrently.
func (d *Daemon) Trigger(reason string) error {
	d.mu.Lock()
	if d.syncing {
		d.mu.Unlock()
		return fmt.Errorf("sync already in progress")
	}
	if d.debounce > 0 && !d.lastSync.IsZero() && time.Since(d.lastSync) < d.debounce {
		remaining := d.debounce - time.Since(d.lastSync)
		d.mu.Unlock()
		return fmt.Errorf("debounced (retry in %s)", remaining.Round(time.Second))
	}
	d.syncing = true
	d.mu.Unlock()

	go d.runOnce(reason)
	return nil
}

func (d *Daemon) runOnce(reason string) {
	defer func() {
		d.mu.Lock()
		d.lastSync = time.Now()
		d.syncing = false
		d.mu.Unlock()
	}()
	d.log.Printf("[daemon] sync start (trigger=%s, mode=%s)", reason, d.strategy.Describe())
	start := time.Now()
	if err := d.strategy.Sync(context.Background(), reason); err != nil {
		d.log.Printf("[daemon] sync error (trigger=%s): %s", reason, err)
	}
	d.log.Printf("[daemon] sync done (trigger=%s, took=%s)", reason, time.Since(start).Round(time.Millisecond))
}

// Run loops on the ticker until ctx is done. Runs one cycle at startup so
// callers don't wait `interval` before the first sync. Ticker cycles use
// debounce=0 (a missed tick is information loss), HTTP triggers use the
// configured debounce.
func (d *Daemon) Run(ctx context.Context) error {
	d.log.Printf("[daemon] starting (interval=%s, debounce=%s, mode=%s)", d.interval, d.debounce, d.strategy.Describe())

	// Startup sync — bypass debounce since there is no prior sync.
	_ = d.Trigger("startup")

	ticker := time.NewTicker(d.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			d.log.Printf("[daemon] stopping")
			return nil
		case <-ticker.C:
			// Ticker triggers ignore debounce: if the cadence is 1m and
			// debounce is 5s, we still want the 1m tick to run. It will
			// still be blocked if an actual sync is in progress.
			d.mu.Lock()
			busy := d.syncing
			d.mu.Unlock()
			if busy {
				continue
			}
			d.mu.Lock()
			d.syncing = true
			d.mu.Unlock()
			go d.runOnce("interval")
		}
	}
}

// --- strategies ---

type localStrategy struct{}

func (localStrategy) Describe() string { return "local-multi-tower" }

func (localStrategy) Sync(_ context.Context, _ string) error {
	// DaemonCycle iterates all configured towers and runs the full
	// laptop-shaped cycle (pull/push via CLI, webhook processing,
	// agent reaping, etc.). Errors are logged inside; we don't surface
	// them since a partial failure is normal during steady-state.
	DaemonCycle()
	return nil
}

type clusterStrategy struct {
	database string
	remote   string
	branch   string
}

func (c *clusterStrategy) Describe() string {
	return fmt.Sprintf("cluster-sql:%s:%s/%s", c.database, c.remote, c.branch)
}

func (c *clusterStrategy) Sync(_ context.Context, _ string) error {
	if err := dolt.SQLPull(c.database, c.remote, c.branch); err != nil {
		// Log pull errors but continue to push — push may still succeed
		// (laptop committed, pull has nothing to fetch, push has work).
		log.Printf("[daemon] pull: %s", err)
	}
	if err := dolt.SQLPush(c.database, c.remote, c.branch); err != nil {
		return fmt.Errorf("push: %w", err)
	}
	return nil
}

func loggerOr(l *log.Logger) *log.Logger {
	if l != nil {
		return l
	}
	return log.Default()
}
