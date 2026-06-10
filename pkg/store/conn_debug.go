package store

import (
	"log"
	"os"
	"sync/atomic"
)

// SPIRE_DEBUG_CONN, when set to a non-empty value, enables connection-source
// logging. Every place that opens a raw Dolt connection (bypassing the pooled
// store) calls ConnOpen with a stable site label; every warm-cache hit/miss in
// the tower store cache logs too. The point is post-hoc attribution: when the
// dolt server is burning CPU on a connection storm, flip this on and the logs
// tell you exactly which call site is opening connections and how fast.
//
// This was added alongside the steward connection-storm fix (releases/v0.52.0):
// the steward and daemon used to rebuild the connection pool every cycle
// (store.OpenAt + defer store.Reset), defeating pooling. The fix keeps the pool
// warm via UseTowerStore; this logging lets you confirm the churn is gone — and
// catch any residual raw-connection site — without re-deriving the diagnosis.
const debugConnEnv = "SPIRE_DEBUG_CONN"

var connSeq int64

func connDebugEnabled() bool { return os.Getenv(debugConnEnv) != "" }

// ConnOpen records that a raw Dolt connection was opened at the given site.
// site is a stable, human-readable label like "steward.getDBForRouting".
// No-op unless SPIRE_DEBUG_CONN is set. The running counter makes the open
// *rate* visible (diff the last number over a time window).
func ConnOpen(site string) {
	if !connDebugEnabled() {
		return
	}
	n := atomic.AddInt64(&connSeq, 1)
	log.Printf("[store-conn] open #%d site=%s", n, site)
}
