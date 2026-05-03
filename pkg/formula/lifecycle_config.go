package formula

// LifecycleConfig declares per-step lifecycle hooks that map executor step
// transitions to bead status updates. The data lives in pkg/formula because
// formulas declare it; pkg/lifecycle consumes it via the evaluator.
type LifecycleConfig struct {
	OnStart         string        `toml:"on_start,omitempty"`
	OnComplete      string        `toml:"on_complete,omitempty"`
	OnFail          *FailAction   `toml:"on_fail,omitempty"`
	OnCompleteMatch []MatchClause `toml:"on_complete_match,omitempty"`
}

// FailAction declares how a step failure should be reflected in bead status.
// Status sets the new status directly; Event delegates to a core lifecycle
// event (e.g. Event="Escalated" routes through the escalation rule).
type FailAction struct {
	Status string `toml:"status,omitempty"`
	Event  string `toml:"event,omitempty"`
}

// MatchClause is a single conditional arm of OnCompleteMatch. The first clause
// whose When expression evaluates true wins; its Status is applied. The When
// expression is evaluated by Eval (see match.go).
type MatchClause struct {
	When   string `toml:"when,omitempty"`
	Status string `toml:"status,omitempty"`
}
