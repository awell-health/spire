package steward

import (
	"sync"
	"time"
)

// CycleStatsSnapshot is a point-in-time copy of cycle statistics.
type CycleStatsSnapshot struct {
	LastCycleAt      time.Time
	CycleDuration    time.Duration
	ActiveAgents     int
	IdleAgents       int
	QueueDepth       int // merge queue depth
	SchedulableWork  int // beads ready for assignment
	SpawnedThisCycle int
	Tower            string
}

// CycleStats tracks telemetry from the most recent steward cycle.
// Thread-safe for concurrent read by metrics server.
type CycleStats struct {
	mu   sync.RWMutex
	snap CycleStatsSnapshot
}

// NewCycleStats creates a new CycleStats tracker.
func NewCycleStats() *CycleStats {
	return &CycleStats{}
}

// Record updates the stats atomically. Called at the end of each TowerCycle.
func (cs *CycleStats) Record(snap CycleStatsSnapshot) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	cs.snap = snap
}

// Snapshot returns a thread-safe copy of the latest stats.
func (cs *CycleStats) Snapshot() CycleStatsSnapshot {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return cs.snap
}
