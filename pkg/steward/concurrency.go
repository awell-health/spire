package steward

import (
	"math"
	"sync"

	"github.com/awell-health/spire/pkg/agent"
)

// ConcurrencyLimiter tracks active agent counts per tower and enforces MaxConcurrent.
type ConcurrencyLimiter struct {
	mu     sync.Mutex
	counts map[string]int // tower name -> active count
}

// NewConcurrencyLimiter creates a new ConcurrencyLimiter.
func NewConcurrencyLimiter() *ConcurrencyLimiter {
	return &ConcurrencyLimiter{
		counts: make(map[string]int),
	}
}

// Refresh updates the active count for a tower from the backend agent list.
// Counts only agents where Alive==true and Tower matches the requested tower.
func (cl *ConcurrencyLimiter) Refresh(tower string, agents []agent.Info) {
	cl.mu.Lock()
	defer cl.mu.Unlock()
	count := 0
	for _, a := range agents {
		if a.Alive && a.Tower == tower {
			count++
		}
	}
	cl.counts[tower] = count
}

// CanSpawn returns true if spawning one more agent would not exceed maxConcurrent.
// If maxConcurrent <= 0, always returns true (unlimited).
func (cl *ConcurrencyLimiter) CanSpawn(tower string, maxConcurrent int) bool {
	if maxConcurrent <= 0 {
		return true
	}
	cl.mu.Lock()
	defer cl.mu.Unlock()
	return cl.counts[tower] < maxConcurrent
}

// ActiveCount returns the current active agent count for a tower.
func (cl *ConcurrencyLimiter) ActiveCount(tower string) int {
	cl.mu.Lock()
	defer cl.mu.Unlock()
	return cl.counts[tower]
}

// Available returns how many more agents can be spawned (maxConcurrent - active).
// Returns math.MaxInt32 if maxConcurrent <= 0 (unlimited).
func (cl *ConcurrencyLimiter) Available(tower string, maxConcurrent int) int {
	if maxConcurrent <= 0 {
		return math.MaxInt32
	}
	cl.mu.Lock()
	defer cl.mu.Unlock()
	remaining := maxConcurrent - cl.counts[tower]
	if remaining < 0 {
		return 0
	}
	return remaining
}
