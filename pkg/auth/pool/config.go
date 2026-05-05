package pool

// SelectionPolicy names a slot-ranking strategy. Values are stored verbatim
// in auth.toml's `[auth] selection = ...` field.
type SelectionPolicy string

const (
	// PolicyRoundRobin cycles through eligible slots in pool order.
	PolicyRoundRobin SelectionPolicy = "round-robin"
	// PolicyPreemptive prefers allowed over allowed_warning, tie-breaking by
	// rate-limit reset distance and concurrency headroom.
	PolicyPreemptive SelectionPolicy = "preemptive"
)

// SlotConfig is one credential slot inside a pool. Either Token (subscription
// pool) or Key (api-key pool) is set; the other is empty. MaxConcurrent caps
// simultaneous in-flight uses — the selector skips slots already at their cap.
type SlotConfig struct {
	Name          string `toml:"name"`
	Token         string `toml:"token"`
	Key           string `toml:"key"`
	MaxConcurrent int    `toml:"max_concurrent"`
}

// Config is the in-memory shape of the per-tower auth.toml file. Subscription
// and APIKey are array-of-table sections (TOML `[[auth.subscription]]` and
// `[[auth.api_key]]`); DefaultPool and FallbackPool name which pool to draw
// from first and what to fall back to. Selection chooses the ranking policy.
type Config struct {
	Subscription []SlotConfig    `toml:"subscription"`
	APIKey       []SlotConfig    `toml:"api_key"`
	DefaultPool  string          `toml:"default_pool"`
	FallbackPool string          `toml:"fallback_pool"`
	Selection    SelectionPolicy `toml:"selection"`
}
