package pool

import (
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"
)

// fixed reference time used across the table tests so ResetsAt
// arithmetic is deterministic.
var testNow = time.Date(2026, 5, 5, 12, 0, 0, 0, time.UTC)

func slotCfg(name string, max int) SlotConfig {
	return SlotConfig{Name: name, MaxConcurrent: max}
}

func mkInFlight(n int) []InFlightClaim {
	if n == 0 {
		return nil
	}
	out := make([]InFlightClaim, n)
	for i := range out {
		out[i] = InFlightClaim{DispatchID: fmt.Sprintf("d-%d", i)}
	}
	return out
}

// stateAt builds a SlotState with both buckets at the given status.
// inFlight controls the number of synthetic claims; resetsAt sets
// FiveHour.ResetsAt (Overage left zero) for tie-break tests.
func stateAt(slot string, status RateLimitStatus, inFlight int, resetsAt time.Time) *SlotState {
	return &SlotState{
		Slot: slot,
		RateLimit: RateLimitInfo{
			FiveHour: RateLimitBucket{Status: status, ResetsAt: resetsAt},
			Overage:  RateLimitBucket{Status: status},
		},
		InFlight: mkInFlight(inFlight),
	}
}

func names(slots []SlotConfig) []string {
	out := make([]string, len(slots))
	for i, s := range slots {
		out[i] = s.Name
	}
	return out
}

func TestIsEligible(t *testing.T) {
	cases := []struct {
		name  string
		slot  SlotConfig
		state *SlotState
		want  bool
	}{
		{
			name: "nil state with capacity is eligible",
			slot: slotCfg("a", 1),
			want: true,
		},
		{
			name: "nil state with zero MaxConcurrent is not eligible",
			slot: slotCfg("a", 0),
			want: false,
		},
		{
			name:  "five-hour rejected",
			slot:  slotCfg("a", 1),
			state: &SlotState{RateLimit: RateLimitInfo{FiveHour: RateLimitBucket{Status: RateLimitStatusRejected}}},
			want:  false,
		},
		{
			name:  "overage rejected",
			slot:  slotCfg("a", 1),
			state: &SlotState{RateLimit: RateLimitInfo{Overage: RateLimitBucket{Status: RateLimitStatusRejected}}},
			want:  false,
		},
		{
			name:  "at cap not eligible",
			slot:  slotCfg("a", 2),
			state: stateAt("a", RateLimitStatusAllowed, 2, time.Time{}),
			want:  false,
		},
		{
			name:  "above cap not eligible",
			slot:  slotCfg("a", 1),
			state: stateAt("a", RateLimitStatusAllowed, 3, time.Time{}),
			want:  false,
		},
		{
			name:  "below cap allowed",
			slot:  slotCfg("a", 2),
			state: stateAt("a", RateLimitStatusAllowed, 1, time.Time{}),
			want:  true,
		},
		{
			name:  "below cap warning",
			slot:  slotCfg("a", 2),
			state: stateAt("a", RateLimitStatusAllowedWarning, 1, time.Time{}),
			want:  true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isEligible(tc.slot, tc.state); got != tc.want {
				t.Fatalf("isEligible: got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestPreemptivePolicy_Rank(t *testing.T) {
	cases := []struct {
		name   string
		slots  []SlotConfig
		states map[string]*SlotState
		want   []string
	}{
		{
			name:  "empty input",
			slots: nil,
			want:  []string{},
		},
		{
			name: "all eligible, all allowed, equal ratios — input order preserved",
			slots: []SlotConfig{
				slotCfg("a", 4),
				slotCfg("b", 4),
				slotCfg("c", 4),
			},
			states: map[string]*SlotState{},
			want:   []string{"a", "b", "c"},
		},
		{
			name: "allowed ranks above allowed_warning",
			slots: []SlotConfig{
				slotCfg("warn", 4),
				slotCfg("clean", 4),
			},
			states: map[string]*SlotState{
				"warn":  stateAt("warn", RateLimitStatusAllowedWarning, 0, testNow.Add(time.Hour)),
				"clean": stateAt("clean", RateLimitStatusAllowed, 0, time.Time{}),
			},
			want: []string{"clean", "warn"},
		},
		{
			name: "rejected slots filtered out, eligibles ranked",
			slots: []SlotConfig{
				slotCfg("dead", 4),
				slotCfg("warn", 4),
				slotCfg("clean", 4),
			},
			states: map[string]*SlotState{
				"dead":  stateAt("dead", RateLimitStatusRejected, 0, testNow.Add(time.Hour)),
				"warn":  stateAt("warn", RateLimitStatusAllowedWarning, 0, testNow.Add(time.Hour)),
				"clean": stateAt("clean", RateLimitStatusAllowed, 0, time.Time{}),
			},
			want: []string{"clean", "warn"},
		},
		{
			name: "at-cap slots filtered out",
			slots: []SlotConfig{
				slotCfg("full", 2),
				slotCfg("free", 2),
			},
			states: map[string]*SlotState{
				"full": stateAt("full", RateLimitStatusAllowed, 2, time.Time{}),
				"free": stateAt("free", RateLimitStatusAllowed, 0, time.Time{}),
			},
			want: []string{"free"},
		},
		{
			name: "warning tie broken by further-away ResetsAt",
			slots: []SlotConfig{
				slotCfg("soon", 4),
				slotCfg("later", 4),
			},
			states: map[string]*SlotState{
				"soon":  stateAt("soon", RateLimitStatusAllowedWarning, 0, testNow.Add(1*time.Hour)),
				"later": stateAt("later", RateLimitStatusAllowedWarning, 0, testNow.Add(2*time.Hour)),
			},
			want: []string{"later", "soon"},
		},
		{
			name: "soonest reset uses min across buckets",
			slots: []SlotConfig{
				slotCfg("a", 4),
				slotCfg("b", 4),
			},
			states: map[string]*SlotState{
				"a": {
					RateLimit: RateLimitInfo{
						FiveHour: RateLimitBucket{Status: RateLimitStatusAllowedWarning, ResetsAt: testNow.Add(3 * time.Hour)},
						Overage:  RateLimitBucket{Status: RateLimitStatusAllowedWarning, ResetsAt: testNow.Add(1 * time.Hour)},
					},
				},
				"b": {
					RateLimit: RateLimitInfo{
						FiveHour: RateLimitBucket{Status: RateLimitStatusAllowedWarning, ResetsAt: testNow.Add(2 * time.Hour)},
						Overage:  RateLimitBucket{Status: RateLimitStatusAllowedWarning, ResetsAt: testNow.Add(2 * time.Hour)},
					},
				},
			},
			// soonest(a) = 1h, soonest(b) = 2h; b is further away.
			want: []string{"b", "a"},
		},
		{
			name: "tie on status and ResetsAt broken by lower ratio",
			slots: []SlotConfig{
				slotCfg("loaded", 4),
				slotCfg("light", 4),
			},
			states: map[string]*SlotState{
				"loaded": stateAt("loaded", RateLimitStatusAllowed, 3, time.Time{}),
				"light":  stateAt("light", RateLimitStatusAllowed, 1, time.Time{}),
			},
			want: []string{"light", "loaded"},
		},
		{
			name: "ratio uses MaxConcurrent, not absolute count",
			slots: []SlotConfig{
				slotCfg("big-busy", 8), // 4/8 = 0.5
				slotCfg("small-idle", 2), // 0/2 = 0
			},
			states: map[string]*SlotState{
				"big-busy":  stateAt("big-busy", RateLimitStatusAllowed, 4, time.Time{}),
				"small-idle": stateAt("small-idle", RateLimitStatusAllowed, 0, time.Time{}),
			},
			want: []string{"small-idle", "big-busy"},
		},
		{
			name: "missing state entry treated as fully eligible",
			slots: []SlotConfig{
				slotCfg("known-warn", 4),
				slotCfg("unknown", 4),
			},
			states: map[string]*SlotState{
				"known-warn": stateAt("known-warn", RateLimitStatusAllowedWarning, 0, testNow.Add(time.Hour)),
			},
			want: []string{"unknown", "known-warn"},
		},
		{
			name: "all rejected returns empty",
			slots: []SlotConfig{
				slotCfg("a", 4),
				slotCfg("b", 4),
			},
			states: map[string]*SlotState{
				"a": stateAt("a", RateLimitStatusRejected, 0, testNow.Add(time.Hour)),
				"b": stateAt("b", RateLimitStatusRejected, 0, testNow.Add(2*time.Hour)),
			},
			want: []string{},
		},
		{
			name: "stable ordering on full tie",
			slots: []SlotConfig{
				slotCfg("first", 4),
				slotCfg("second", 4),
				slotCfg("third", 4),
			},
			states: map[string]*SlotState{},
			want:   []string{"first", "second", "third"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &PreemptivePolicy{}
			got := names(p.Rank(tc.slots, tc.states, testNow))
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Rank: got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRoundRobinPolicy_AdvancesAcrossCalls(t *testing.T) {
	slots := []SlotConfig{
		slotCfg("a", 1),
		slotCfg("b", 1),
		slotCfg("c", 1),
	}
	p := &RoundRobinPolicy{}

	want := [][]string{
		{"a", "b", "c"},
		{"b", "c", "a"},
		{"c", "a", "b"},
		{"a", "b", "c"},
		{"b", "c", "a"},
	}
	for i, w := range want {
		got := names(p.Rank(slots, nil, testNow))
		if !reflect.DeepEqual(got, w) {
			t.Fatalf("call %d: got %v, want %v", i+1, got, w)
		}
	}
}

func TestRoundRobinPolicy_FilterAndAdvance(t *testing.T) {
	slots := []SlotConfig{
		slotCfg("a", 1),
		slotCfg("b", 1),
		slotCfg("c", 1),
	}
	// b is rejected — eligible set is [a, c] in pool order.
	states := map[string]*SlotState{
		"b": stateAt("b", RateLimitStatusRejected, 0, testNow.Add(time.Hour)),
	}
	p := &RoundRobinPolicy{}

	want := [][]string{
		{"a", "c"},
		{"c", "a"},
		{"a", "c"},
		{"c", "a"},
	}
	for i, w := range want {
		got := names(p.Rank(slots, states, testNow))
		if !reflect.DeepEqual(got, w) {
			t.Fatalf("call %d: got %v, want %v", i+1, got, w)
		}
	}
}

func TestRoundRobinPolicy_EmptyAndNoEligible(t *testing.T) {
	p := &RoundRobinPolicy{}

	if got := p.Rank(nil, nil, testNow); got != nil {
		t.Fatalf("empty input: got %v, want nil", got)
	}

	slots := []SlotConfig{slotCfg("a", 1)}
	states := map[string]*SlotState{
		"a": stateAt("a", RateLimitStatusRejected, 0, testNow.Add(time.Hour)),
	}
	if got := p.Rank(slots, states, testNow); got != nil {
		t.Fatalf("all rejected: got %v, want nil", got)
	}
}

func TestRoundRobinPolicy_ConcurrentCallsAreSafe(t *testing.T) {
	slots := []SlotConfig{
		slotCfg("a", 1),
		slotCfg("b", 1),
		slotCfg("c", 1),
	}
	p := &RoundRobinPolicy{}

	const goroutines = 16
	const callsPer = 50
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func() {
			defer wg.Done()
			for i := 0; i < callsPer; i++ {
				ranked := p.Rank(slots, nil, testNow)
				if len(ranked) != 3 {
					t.Errorf("expected 3 eligible, got %d", len(ranked))
					return
				}
			}
		}()
	}
	wg.Wait()

	// After goroutines*callsPer concurrent calls, the cursor should
	// equal that exact count (each call increments once under lock).
	p.mu.Lock()
	got := p.cursor
	p.mu.Unlock()
	if got != goroutines*callsPer {
		t.Fatalf("cursor: got %d, want %d", got, goroutines*callsPer)
	}
}

func TestNewPolicy(t *testing.T) {
	cases := []struct {
		in   SelectionPolicy
		want string
	}{
		{in: PolicyRoundRobin, want: "*pool.RoundRobinPolicy"},
		{in: PolicyPreemptive, want: "*pool.PreemptivePolicy"},
		{in: "", want: "*pool.PreemptivePolicy"},
		{in: "unknown-policy", want: "*pool.PreemptivePolicy"},
	}
	for _, tc := range cases {
		t.Run(string(tc.in), func(t *testing.T) {
			got := fmt.Sprintf("%T", NewPolicy(tc.in))
			if got != tc.want {
				t.Fatalf("NewPolicy(%q): got %s, want %s", tc.in, got, tc.want)
			}
		})
	}
}
