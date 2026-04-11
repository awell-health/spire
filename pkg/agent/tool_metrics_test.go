package agent

import (
	"testing"
)

// Tool metrics are now collected via the OTel pipeline. These tests verify
// the stub functions return gracefully.

func TestSetupToolMetrics_NoOp(t *testing.T) {
	// All providers should be a no-op now.
	for _, provider := range []string{"claude", "codex", "cursor", "unknown"} {
		if err := SetupToolMetrics(provider, t.TempDir()); err != nil {
			t.Errorf("SetupToolMetrics(%q) returned error: %v", provider, err)
		}
	}
}

func TestCollectToolMetrics_NoOp(t *testing.T) {
	for _, provider := range []string{"claude", "codex", "cursor", "unknown"} {
		counts, err := CollectToolMetrics(provider, t.TempDir())
		if err != nil {
			t.Errorf("CollectToolMetrics(%q) returned error: %v", provider, err)
		}
		if counts != nil {
			t.Errorf("CollectToolMetrics(%q) = %v, want nil", provider, counts)
		}
	}
}
