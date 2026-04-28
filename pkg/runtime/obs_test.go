package runtime

import (
	"context"
	"strings"
	"testing"
)

func fullRun() RunContext {
	return RunContext{
		TowerName:       "dev",
		Prefix:          "spi",
		BeadID:          "spi-abc",
		AttemptID:       "spi-attempt-1",
		RunID:           "run-42",
		AgentName:       "apprentice-spi-abc-0",
		Role:            RoleApprentice,
		FormulaStep:     "implement",
		Backend:         "process",
		WorkspaceKind:   WorkspaceKindOwnedWorktree,
		WorkspaceName:   "feat",
		WorkspaceOrigin: WorkspaceOriginLocalBind,
		HandoffMode:     HandoffBundle,
	}
}

func TestLogFields_IncludesEveryCanonicalField(t *testing.T) {
	suffix := LogFields(fullRun())
	for _, k := range LogFieldOrder {
		if !strings.Contains(suffix, " "+k+"=") {
			t.Errorf("LogFields missing %q: %s", k, suffix)
		}
	}
}

func TestLogFields_EmptyRunContextEmitsEveryKeyWithEmptyValue(t *testing.T) {
	// Design rule: missing values render as empty string, never drop the
	// field. This test guards the rule so grep/alert patterns never break.
	suffix := LogFields(RunContext{})
	for _, k := range LogFieldOrder {
		needle := " " + k + "="
		if !strings.Contains(suffix, needle) {
			t.Errorf("empty RunContext missing key %q: %q", k, suffix)
		}
	}
}

func TestLogFields_OrderIsStable(t *testing.T) {
	suffix := LogFields(fullRun())
	prev := -1
	for _, k := range LogFieldOrder {
		idx := strings.Index(suffix, " "+k+"=")
		if idx <= prev {
			t.Errorf("field %q at index %d, expected > %d (canonical order violated)", k, idx, prev)
		}
		prev = idx
	}
}

func TestLogKV_AlternatingKeyValue(t *testing.T) {
	kv := LogKV(fullRun())
	if got, want := len(kv), len(LogFieldOrder)*2; got != want {
		t.Fatalf("LogKV length = %d, want %d", got, want)
	}
	for i, k := range LogFieldOrder {
		gotKey, ok := kv[i*2].(string)
		if !ok || gotKey != k {
			t.Errorf("LogKV[%d] = %v, want %q", i*2, kv[i*2], k)
		}
		if _, ok := kv[i*2+1].(string); !ok {
			t.Errorf("LogKV[%d] = %v, want string value", i*2+1, kv[i*2+1])
		}
	}
}

func TestMetricLabels_ExcludesHighCardinalityFields(t *testing.T) {
	labels := MetricLabels(fullRun())
	// The three high-cardinality identifiers MUST NOT appear on metrics.
	for _, banned := range []string{LogFieldBeadID, LogFieldAttemptID, LogFieldRunID, LogFieldFormulaStep, LogFieldWorkspaceName, LogFieldWorkspaceOrigin} {
		if _, ok := labels[banned]; ok {
			t.Errorf("MetricLabels includes forbidden field %q: %v", banned, labels)
		}
	}
	// The six low-cardinality fields MUST be present.
	for _, want := range MetricLabelOrder {
		if _, ok := labels[want]; !ok {
			t.Errorf("MetricLabels missing required field %q: %v", want, labels)
		}
	}
}

func TestMetricLabelsString_StableOrder(t *testing.T) {
	s := MetricLabelsString(fullRun())
	if !strings.HasPrefix(s, "{") || !strings.HasSuffix(s, "}") {
		t.Fatalf("MetricLabelsString missing braces: %q", s)
	}
	prev := -1
	for _, k := range MetricLabelOrder {
		idx := strings.Index(s, k+"=")
		if idx <= prev {
			t.Errorf("metric label %q at %d, want > %d (order violated): %s", k, idx, prev, s)
		}
		prev = idx
	}
}

func TestRunContextFromEnv_PopulatesFromCanonicalVars(t *testing.T) {
	t.Setenv(EnvTower, "dev")
	t.Setenv(EnvPrefix, "spi")
	t.Setenv(EnvBeadID, "spi-abc")
	t.Setenv(EnvAttemptID, "spi-attempt-1")
	t.Setenv(EnvRunID, "run-42")
	t.Setenv(EnvAgentName, "apprentice-spi-abc-0")
	t.Setenv(EnvRole, "apprentice")
	t.Setenv(EnvFormulaStep, "implement")
	t.Setenv(EnvBackend, "process")
	t.Setenv(EnvWorkspaceKind, "owned_worktree")
	t.Setenv(EnvWorkspaceName, "feat")
	t.Setenv(EnvWorkspaceOrigin, "local-bind")
	t.Setenv(EnvHandoffMode, "bundle")

	run := RunContextFromEnv()
	if run.TowerName != "dev" {
		t.Errorf("TowerName = %q, want dev", run.TowerName)
	}
	if run.Prefix != "spi" {
		t.Errorf("Prefix = %q, want spi", run.Prefix)
	}
	if run.BeadID != "spi-abc" {
		t.Errorf("BeadID = %q, want spi-abc", run.BeadID)
	}
	if run.AttemptID != "spi-attempt-1" {
		t.Errorf("AttemptID = %q, want spi-attempt-1", run.AttemptID)
	}
	if run.RunID != "run-42" {
		t.Errorf("RunID = %q, want run-42", run.RunID)
	}
	if run.AgentName != "apprentice-spi-abc-0" {
		t.Errorf("AgentName = %q, want apprentice-spi-abc-0", run.AgentName)
	}
	if run.Role != RoleApprentice {
		t.Errorf("Role = %q, want %q", run.Role, RoleApprentice)
	}
	if run.FormulaStep != "implement" {
		t.Errorf("FormulaStep = %q, want implement", run.FormulaStep)
	}
	if run.Backend != "process" {
		t.Errorf("Backend = %q, want process", run.Backend)
	}
	if run.WorkspaceKind != WorkspaceKindOwnedWorktree {
		t.Errorf("WorkspaceKind = %q, want %q", run.WorkspaceKind, WorkspaceKindOwnedWorktree)
	}
	if run.WorkspaceName != "feat" {
		t.Errorf("WorkspaceName = %q, want feat", run.WorkspaceName)
	}
	if run.WorkspaceOrigin != WorkspaceOriginLocalBind {
		t.Errorf("WorkspaceOrigin = %q, want %q", run.WorkspaceOrigin, WorkspaceOriginLocalBind)
	}
	if run.HandoffMode != HandoffBundle {
		t.Errorf("HandoffMode = %q, want %q", run.HandoffMode, HandoffBundle)
	}
}

func TestRunContextFromEnv_MissingVarsProduceEmptyStrings(t *testing.T) {
	// Clear every canonical var.
	for _, k := range []string{
		EnvTower, EnvPrefix, EnvBeadID, EnvAttemptID, EnvRunID, EnvAgentName,
		EnvRole, EnvFormulaStep, EnvBackend, EnvWorkspaceKind, EnvWorkspaceName,
		EnvWorkspaceOrigin, EnvWorkspacePath, EnvHandoffMode,
	} {
		t.Setenv(k, "")
	}
	run := RunContextFromEnv()
	if run.TowerName != "" || run.BeadID != "" || run.Role != "" {
		t.Errorf("expected zero-value RunContext, got %+v", run)
	}
}

func TestWithRunContext_RoundTrip(t *testing.T) {
	want := fullRun()
	ctx := WithRunContext(context.Background(), want)
	got := FromContext(ctx)
	if got != want {
		t.Errorf("FromContext = %+v, want %+v", got, want)
	}
}

func TestFromContext_EmptyOnNil(t *testing.T) {
	got := FromContext(nil)
	if got != (RunContext{}) {
		t.Errorf("FromContext(nil) = %+v, want zero-value", got)
	}
}

func TestFromContext_EmptyOnMissingKey(t *testing.T) {
	got := FromContext(context.Background())
	if got != (RunContext{}) {
		t.Errorf("FromContext(Background) = %+v, want zero-value", got)
	}
}
