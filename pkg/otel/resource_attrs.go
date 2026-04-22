package otel

import (
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
)

// Canonical OTLP resource attribute keys that carry the worker runtime
// contract (see docs/design/spi-xplwy-runtime-contract.md and the
// pkg/agent spawn paths). Receivers must accept these as authoritative.
const (
	resAttrBeadID          = "bead_id"
	resAttrAttemptID       = "attempt_id"
	resAttrRunID           = "run_id"
	resAttrTower           = "tower"
	resAttrPrefix          = "prefix"
	resAttrRole            = "role"
	resAttrFormulaStep     = "formula_step"
	resAttrBackend         = "backend"
	resAttrWorkspaceKind   = "workspace_kind"
	resAttrWorkspaceName   = "workspace_name"
	resAttrWorkspaceOrigin = "workspace_origin"
	resAttrHandoffMode     = "handoff_mode"
	resAttrAgentName       = "agent.name"
	resAttrSessionID       = "session.id"
	resAttrServiceInstance = "service.instance.id"
)

// Legacy resource attribute keys that predate the canonical runtime
// contract. Accepted as a fallback only — canonical keys win when both
// are present. Producers have migrated off these (spi-xplwy / spi-zm3b1);
// the fallback exists so telemetry from older worker binaries (or manual
// OTLP fixtures) still correlates to a bead during the migration window.
const (
	legacyAttrBeadID = "bead.id"
	legacyAttrStep   = "step"
)

// RunContext is the identity set extracted from OTLP resource attributes.
// It is the typed-row surface the trace and log ingestion paths use to
// populate tool_spans / tool_events / api_events, factored out of the
// persistence path so the receiver's extract→typed-row stage can be
// exercised by tests without opening DuckDB.
//
// Field names follow the canonical runtime-contract vocabulary
// (FormulaStep, not Step) so schema drift between emitter and receiver
// is visible in one place.
type RunContext struct {
	BeadID          string
	AgentName       string
	FormulaStep     string
	Tower           string
	Prefix          string
	AttemptID       string
	RunID           string
	Role            string
	Backend         string
	WorkspaceKind   string
	WorkspaceName   string
	WorkspaceOrigin string
	HandoffMode     string
	SessionID       string
}

// ExtractRunContext reads the canonical runtime-contract keys from a
// Resource and returns a RunContext. Legacy keys (bead.id, step) are
// accepted only when the canonical key is absent from the same Resource —
// canonical always wins when both are present, silently, without logging
// on the hot ingestion path. A nil Resource returns the zero value so
// callers can pass through without a nil check.
func ExtractRunContext(res *resourcepb.Resource) RunContext {
	if res == nil {
		return RunContext{}
	}
	return extractRunContextFromAttrs(res.GetAttributes())
}

// extractRunContextFromAttrs is the pure attribute walk. Split from
// ExtractRunContext so tests can exercise the decoding without building
// a Resource wrapper.
func extractRunContextFromAttrs(attrs []*commonpb.KeyValue) RunContext {
	var rc RunContext
	// Legacy values are captured separately so a canonical key appearing
	// later in the slice cannot be clobbered by a legacy alias, and a
	// canonical key appearing earlier cannot be clobbered by a later
	// legacy alias. OTLP does not guarantee attribute order, so either
	// relative position is possible on the wire.
	var legacyBeadID, legacyStep string
	for _, kv := range attrs {
		val := kvStringValue(kv)
		switch kv.GetKey() {
		case resAttrBeadID:
			rc.BeadID = val
		case resAttrAgentName:
			rc.AgentName = val
		case resAttrFormulaStep:
			rc.FormulaStep = val
		case resAttrTower:
			rc.Tower = val
		case resAttrPrefix:
			rc.Prefix = val
		case resAttrAttemptID:
			rc.AttemptID = val
		case resAttrRunID:
			rc.RunID = val
		case resAttrRole:
			rc.Role = val
		case resAttrBackend:
			rc.Backend = val
		case resAttrWorkspaceKind:
			rc.WorkspaceKind = val
		case resAttrWorkspaceName:
			rc.WorkspaceName = val
		case resAttrWorkspaceOrigin:
			rc.WorkspaceOrigin = val
		case resAttrHandoffMode:
			rc.HandoffMode = val
		case resAttrSessionID, resAttrServiceInstance:
			rc.SessionID = val
		case legacyAttrBeadID:
			legacyBeadID = val
		case legacyAttrStep:
			legacyStep = val
		}
	}
	if rc.BeadID == "" {
		rc.BeadID = legacyBeadID
	}
	if rc.FormulaStep == "" {
		rc.FormulaStep = legacyStep
	}
	return rc
}
