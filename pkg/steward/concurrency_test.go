package steward

import (
	"math"
	"testing"
	"time"

	"github.com/awell-health/spire/pkg/agent"
)

func TestCanSpawn_BelowLimit(t *testing.T) {
	cl := NewConcurrencyLimiter()
	cl.Refresh("tower1", []agent.Info{
		{Name: "w1", Alive: true, StartedAt: time.Now(), Tower: "tower1"},
	})
	if !cl.CanSpawn("tower1", 3) {
		t.Fatal("expected CanSpawn=true when below limit (1 active, max 3)")
	}
}

func TestCanSpawn_AtLimit(t *testing.T) {
	cl := NewConcurrencyLimiter()
	cl.Refresh("tower1", []agent.Info{
		{Name: "w1", Alive: true, StartedAt: time.Now(), Tower: "tower1"},
		{Name: "w2", Alive: true, StartedAt: time.Now(), Tower: "tower1"},
		{Name: "w3", Alive: true, StartedAt: time.Now(), Tower: "tower1"},
	})
	if cl.CanSpawn("tower1", 3) {
		t.Fatal("expected CanSpawn=false when at limit (3 active, max 3)")
	}
}

func TestCanSpawn_Unlimited(t *testing.T) {
	cl := NewConcurrencyLimiter()
	cl.Refresh("tower1", []agent.Info{
		{Name: "w1", Alive: true, StartedAt: time.Now(), Tower: "tower1"},
		{Name: "w2", Alive: true, StartedAt: time.Now(), Tower: "tower1"},
	})
	if !cl.CanSpawn("tower1", 0) {
		t.Fatal("expected CanSpawn=true when maxConcurrent=0 (unlimited)")
	}
	if !cl.CanSpawn("tower1", -1) {
		t.Fatal("expected CanSpawn=true when maxConcurrent<0 (unlimited)")
	}
}

func TestAvailable_CorrectRemaining(t *testing.T) {
	cl := NewConcurrencyLimiter()
	cl.Refresh("tower1", []agent.Info{
		{Name: "w1", Alive: true, StartedAt: time.Now(), Tower: "tower1"},
		{Name: "w2", Alive: true, StartedAt: time.Now(), Tower: "tower1"},
	})
	got := cl.Available("tower1", 5)
	if got != 3 {
		t.Fatalf("expected Available=3, got %d", got)
	}
}

func TestAvailable_Unlimited(t *testing.T) {
	cl := NewConcurrencyLimiter()
	cl.Refresh("tower1", []agent.Info{
		{Name: "w1", Alive: true, StartedAt: time.Now(), Tower: "tower1"},
	})
	got := cl.Available("tower1", 0)
	if got != math.MaxInt32 {
		t.Fatalf("expected Available=MaxInt32 for unlimited, got %d", got)
	}
}

func TestAvailable_OverLimit(t *testing.T) {
	cl := NewConcurrencyLimiter()
	cl.Refresh("tower1", []agent.Info{
		{Name: "w1", Alive: true, StartedAt: time.Now(), Tower: "tower1"},
		{Name: "w2", Alive: true, StartedAt: time.Now(), Tower: "tower1"},
		{Name: "w3", Alive: true, StartedAt: time.Now(), Tower: "tower1"},
	})
	got := cl.Available("tower1", 2)
	if got != 0 {
		t.Fatalf("expected Available=0 when over limit, got %d", got)
	}
}

func TestRefresh_CountsOnlyAlive(t *testing.T) {
	cl := NewConcurrencyLimiter()
	cl.Refresh("tower1", []agent.Info{
		{Name: "w1", Alive: true, StartedAt: time.Now(), Tower: "tower1"},
		{Name: "w2", Alive: false, StartedAt: time.Now(), Tower: "tower1"},
		{Name: "w3", Alive: true, StartedAt: time.Now(), Tower: "tower1"},
		{Name: "w4", Alive: false, StartedAt: time.Now(), Tower: "tower1"},
	})
	got := cl.ActiveCount("tower1")
	if got != 2 {
		t.Fatalf("expected ActiveCount=2 (only alive agents), got %d", got)
	}
}

func TestRefresh_EmptyList(t *testing.T) {
	cl := NewConcurrencyLimiter()
	cl.Refresh("tower1", []agent.Info{})
	got := cl.ActiveCount("tower1")
	if got != 0 {
		t.Fatalf("expected ActiveCount=0 for empty list, got %d", got)
	}
}

func TestRefresh_FiltersByTower(t *testing.T) {
	cl := NewConcurrencyLimiter()
	// Pass a mixed list of agents from different towers.
	agents := []agent.Info{
		{Name: "w1", Alive: true, StartedAt: time.Now(), Tower: "alpha"},
		{Name: "w2", Alive: true, StartedAt: time.Now(), Tower: "beta"},
		{Name: "w3", Alive: true, StartedAt: time.Now(), Tower: "alpha"},
		{Name: "w4", Alive: false, StartedAt: time.Now(), Tower: "alpha"},
		{Name: "w5", Alive: true, StartedAt: time.Now(), Tower: "beta"},
		{Name: "w6", Alive: true, StartedAt: time.Now(), Tower: "gamma"},
	}

	// Refresh for "alpha" — should only count alive agents with Tower=="alpha".
	cl.Refresh("alpha", agents)
	if got := cl.ActiveCount("alpha"); got != 2 {
		t.Fatalf("expected alpha ActiveCount=2, got %d", got)
	}

	// Refresh for "beta" — should only count alive agents with Tower=="beta".
	cl.Refresh("beta", agents)
	if got := cl.ActiveCount("beta"); got != 2 {
		t.Fatalf("expected beta ActiveCount=2, got %d", got)
	}

	// Refresh for "gamma" — should only count alive agents with Tower=="gamma".
	cl.Refresh("gamma", agents)
	if got := cl.ActiveCount("gamma"); got != 1 {
		t.Fatalf("expected gamma ActiveCount=1, got %d", got)
	}
}

func TestMultipleTowers(t *testing.T) {
	cl := NewConcurrencyLimiter()
	cl.Refresh("alpha", []agent.Info{
		{Name: "w1", Alive: true, StartedAt: time.Now(), Tower: "alpha"},
		{Name: "w2", Alive: true, StartedAt: time.Now(), Tower: "alpha"},
	})
	cl.Refresh("beta", []agent.Info{
		{Name: "w3", Alive: true, StartedAt: time.Now(), Tower: "beta"},
	})

	if cl.ActiveCount("alpha") != 2 {
		t.Fatalf("expected alpha ActiveCount=2, got %d", cl.ActiveCount("alpha"))
	}
	if cl.ActiveCount("beta") != 1 {
		t.Fatalf("expected beta ActiveCount=1, got %d", cl.ActiveCount("beta"))
	}

	// alpha at limit, beta still has room
	if cl.CanSpawn("alpha", 2) {
		t.Fatal("expected alpha CanSpawn=false at limit")
	}
	if !cl.CanSpawn("beta", 2) {
		t.Fatal("expected beta CanSpawn=true below limit")
	}
}

func TestActiveCount_UnknownTower(t *testing.T) {
	cl := NewConcurrencyLimiter()
	got := cl.ActiveCount("nonexistent")
	if got != 0 {
		t.Fatalf("expected ActiveCount=0 for unknown tower, got %d", got)
	}
}

func TestCanSpawn_UnknownTower(t *testing.T) {
	cl := NewConcurrencyLimiter()
	if !cl.CanSpawn("nonexistent", 5) {
		t.Fatal("expected CanSpawn=true for unknown tower (0 active < 5)")
	}
}
