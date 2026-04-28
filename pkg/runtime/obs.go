package runtime

// Observability helpers for the canonical runtime contract.
//
// This file is the single source for the log/trace/metric field vocabulary
// declared in docs/design/spi-xplwy-runtime-contract.md §1.4. Every
// structured log emission from pkg/executor, pkg/wizard, pkg/apprentice,
// pkg/agent, and operator/controllers flows its identity through one of
// the helpers here so the surface stays uniform.
//
// Rules (chunk 6 of epic spi-xplwy):
//   - Log/trace fields: tower, prefix, bead_id, attempt_id, run_id, role,
//     formula_step, backend, workspace_kind, workspace_name,
//     workspace_origin, handoff_mode. Missing values render as empty
//     string — never drop the field.
//   - Metric labels: {tower, prefix, role, backend, workspace_kind,
//     handoff_mode}. bead_id/attempt_id/run_id stay OFF metric labels
//     (high-cardinality — logs/traces only).

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
)

// Canonical log/trace field names. Kept as constants so tests, dashboards,
// and alert rules can grep for the literal strings instead of drifting.
const (
	LogFieldTower           = "tower"
	LogFieldPrefix          = "prefix"
	LogFieldBeadID          = "bead_id"
	LogFieldAttemptID       = "attempt_id"
	LogFieldRunID           = "run_id"
	LogFieldAgentName       = "agent_name"
	LogFieldRole            = "role"
	LogFieldFormulaStep     = "formula_step"
	LogFieldBackend         = "backend"
	LogFieldWorkspaceKind   = "workspace_kind"
	LogFieldWorkspaceName   = "workspace_name"
	LogFieldWorkspaceOrigin = "workspace_origin"
	LogFieldHandoffMode     = "handoff_mode"
)

// LogFieldOrder is the canonical iteration order for log emission. Kept
// stable so grep/alert rules can rely on positional occurrence.
var LogFieldOrder = []string{
	LogFieldTower,
	LogFieldPrefix,
	LogFieldBeadID,
	LogFieldAttemptID,
	LogFieldRunID,
	LogFieldAgentName,
	LogFieldRole,
	LogFieldFormulaStep,
	LogFieldBackend,
	LogFieldWorkspaceKind,
	LogFieldWorkspaceName,
	LogFieldWorkspaceOrigin,
	LogFieldHandoffMode,
}

// MetricLabelOrder is the canonical label order for low-cardinality
// metric emission. The three high-cardinality identity fields (bead_id,
// attempt_id, run_id) are intentionally absent — see design §1.4.
var MetricLabelOrder = []string{
	LogFieldTower,
	LogFieldPrefix,
	LogFieldRole,
	LogFieldBackend,
	LogFieldWorkspaceKind,
	LogFieldHandoffMode,
}

// LogFields returns the canonical field suffix for a structured log line:
// " tower=<v> prefix=<v> bead_id=<v> ..." (with a leading space). The
// caller appends the suffix to their format string; empty values render
// as "" rather than being dropped so downstream log parsers see a
// stable schema. The emission order is fixed by LogFieldOrder.
func LogFields(run RunContext) string {
	kv := logFieldValues(run)
	var b strings.Builder
	for _, k := range LogFieldOrder {
		b.WriteByte(' ')
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(kv[k])
	}
	return b.String()
}

// LogKV returns the canonical key/value slice suitable for logr.WithValues
// (controller-runtime operator logger) or any structured-logger helper that
// consumes alternating string keys and any-typed values. Values are always
// strings; missing fields are emitted as "" (same contract as LogFields).
func LogKV(run RunContext) []interface{} {
	kv := logFieldValues(run)
	out := make([]interface{}, 0, len(LogFieldOrder)*2)
	for _, k := range LogFieldOrder {
		out = append(out, k, kv[k])
	}
	return out
}

// MetricLabels returns the low-cardinality label map for metric emission.
// bead_id, attempt_id, and run_id are omitted by design. Callers that want
// to emit metrics with these labels should iterate MetricLabelOrder to keep
// output stable.
func MetricLabels(run RunContext) map[string]string {
	return map[string]string{
		LogFieldTower:         run.TowerName,
		LogFieldPrefix:        run.Prefix,
		LogFieldRole:          string(run.Role),
		LogFieldBackend:       run.Backend,
		LogFieldWorkspaceKind: string(run.WorkspaceKind),
		LogFieldHandoffMode:   string(run.HandoffMode),
	}
}

// MetricLabelsString renders the metric-label set as a deterministic
// Prometheus-style label fragment: `{tower="...",prefix="...",...}`. The
// order matches MetricLabelOrder. Used by hand-rolled /metrics emitters
// (pkg/steward/metrics_server.go pattern) to stay consistent with the
// canonical label contract.
func MetricLabelsString(run RunContext) string {
	m := MetricLabels(run)
	keys := make([]string, 0, len(m))
	keys = append(keys, MetricLabelOrder...)
	// Defensive: if a caller later extends the map, surface any extras in
	// a stable order rather than dropping them.
	for k := range m {
		found := false
		for _, x := range keys {
			if x == k {
				found = true
				break
			}
		}
		if !found {
			keys = append(keys, k)
		}
	}
	sort.SliceStable(keys[len(MetricLabelOrder):], func(i, j int) bool {
		return keys[len(MetricLabelOrder)+i] < keys[len(MetricLabelOrder)+j]
	})
	var b strings.Builder
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%s=%q", k, m[k])
	}
	b.WriteByte('}')
	return b.String()
}

// logFieldValues materializes the canonical field map from a RunContext.
// Kept private — callers use LogFields / LogKV / MetricLabels.
func logFieldValues(run RunContext) map[string]string {
	return map[string]string{
		LogFieldTower:           run.TowerName,
		LogFieldPrefix:          run.Prefix,
		LogFieldBeadID:          run.BeadID,
		LogFieldAttemptID:       run.AttemptID,
		LogFieldRunID:           run.RunID,
		LogFieldAgentName:       run.AgentName,
		LogFieldRole:            string(run.Role),
		LogFieldFormulaStep:     run.FormulaStep,
		LogFieldBackend:         run.Backend,
		LogFieldWorkspaceKind:   string(run.WorkspaceKind),
		LogFieldWorkspaceName:   run.WorkspaceName,
		LogFieldWorkspaceOrigin: string(run.WorkspaceOrigin),
		LogFieldHandoffMode:     string(run.HandoffMode),
	}
}

// Env var names used by backends to propagate RunContext fields into
// spawned workers. The existing SPIRE_TOWER / SPIRE_BEAD_ID / SPIRE_ROLE /
// SPIRE_ATTEMPT_ID / SPIRE_RUN_ID / SPIRE_REPO_PREFIX / SPIRE_WORKSPACE_PATH
// vars predate this contract; these constants make the full canonical set
// a single grep target.
const (
	EnvTower           = "SPIRE_TOWER"
	EnvPrefix          = "SPIRE_REPO_PREFIX"
	EnvBeadID          = "SPIRE_BEAD_ID"
	EnvAttemptID       = "SPIRE_ATTEMPT_ID"
	EnvRunID           = "SPIRE_RUN_ID"
	EnvAgentName       = "SPIRE_AGENT_NAME"
	EnvRole            = "SPIRE_ROLE"
	EnvFormulaStep     = "SPIRE_FORMULA_STEP"
	EnvBackend         = "SPIRE_BACKEND"
	EnvWorkspaceKind   = "SPIRE_WORKSPACE_KIND"
	EnvWorkspaceName   = "SPIRE_WORKSPACE_NAME"
	EnvWorkspaceOrigin = "SPIRE_WORKSPACE_ORIGIN"
	EnvWorkspacePath   = "SPIRE_WORKSPACE_PATH"
	EnvHandoffMode     = "SPIRE_HANDOFF_MODE"
)

// RunContextFromEnv rebuilds a RunContext from the canonical SPIRE_* env
// vars set by backends at spawn time. Missing vars produce empty-string
// fields — never errors — so a worker spawned from a legacy path (no
// backend set the canonical vars) still gets a usable RunContext for its
// log prefix.
func RunContextFromEnv() RunContext {
	return RunContext{
		TowerName:       os.Getenv(EnvTower),
		Prefix:          os.Getenv(EnvPrefix),
		BeadID:          os.Getenv(EnvBeadID),
		AttemptID:       os.Getenv(EnvAttemptID),
		RunID:           os.Getenv(EnvRunID),
		AgentName:       os.Getenv(EnvAgentName),
		Role:            SpawnRole(os.Getenv(EnvRole)),
		FormulaStep:     os.Getenv(EnvFormulaStep),
		Backend:         os.Getenv(EnvBackend),
		WorkspaceKind:   WorkspaceKind(os.Getenv(EnvWorkspaceKind)),
		WorkspaceName:   os.Getenv(EnvWorkspaceName),
		WorkspaceOrigin: WorkspaceOrigin(os.Getenv(EnvWorkspaceOrigin)),
		HandoffMode:     HandoffMode(os.Getenv(EnvHandoffMode)),
	}
}

// Context propagation. RunContext rides on context.Context so passing it
// through a chain of helpers doesn't require threading an explicit param
// through every signature. Packages that prefer explicit params are
// welcome — this is additive.
type runContextKey struct{}

// WithRunContext returns a derived context carrying the RunContext value.
func WithRunContext(ctx context.Context, run RunContext) context.Context {
	return context.WithValue(ctx, runContextKey{}, run)
}

// FromContext returns the RunContext stored on ctx, or the zero value
// when none is present. Never returns ok=false — callers treat absence as
// an empty identity set (matches the "render as empty string" rule).
func FromContext(ctx context.Context) RunContext {
	if ctx == nil {
		return RunContext{}
	}
	if v, ok := ctx.Value(runContextKey{}).(RunContext); ok {
		return v
	}
	return RunContext{}
}
