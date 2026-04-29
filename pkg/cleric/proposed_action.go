// Package cleric holds the runtime for the open-loop recovery cycle:
// the ProposedAction schema the cleric Claude agent emits, the action
// manifest, and the four mechanical action handlers (publish, execute,
// takeover, finish) that drive the cleric-default formula. Dispatch
// itself lives in pkg/steward; the formula engine resolves action names
// to the handlers exported here via init() registration in pkg/executor.
//
// pkg/cleric depends only on pkg/store (for bead reads/writes) and the
// gateway client interface defined here. It MUST NOT import pkg/wizard
// or pkg/executor — both are dispatchers that route through the formula
// engine, not callers of cleric internals.
package cleric

import (
	"encoding/json"
	"fmt"
	"strings"
)

// MetadataKeyProposal is the bead-metadata key under which cleric.publish
// stores the most-recent ProposedAction (JSON string). cleric.execute
// reads it back. The key is stable so the desktop Review surface can
// fetch the proposal via the standard store metadata API.
const MetadataKeyProposal = "cleric_proposal"

// MetadataKeyOutcome is the bead-metadata key under which cleric.finish
// stores the outcome record consumed by the promotion/demotion learning
// loop (separate feature spi-kl8x5y).
const MetadataKeyOutcome = "cleric_outcome"

// MetadataKeyExecuteResult is the bead-metadata key under which
// cleric.execute records the gateway's execution outcome (success or
// error message) so the desktop surface can show what happened.
const MetadataKeyExecuteResult = "cleric_execute_result"

// MetadataKeyExecuteSuccess is the strict, machine-checkable success
// marker cleric.execute writes when the gateway returned a real
// success (Success=true, no error). cleric.finish refuses to stamp
// `cleric_outcome=approve+executed` unless this key reads "true";
// pkg/steward.recoveryShouldResume keys on this marker to decide
// whether to unhook + re-summon the source bead.
//
// This is the post-fix contract for spi-skfsia: a stub gateway
// (ErrGatewayUnimplemented) or any gateway error must NOT produce a
// successful resume. The marker is only set by cleric.Execute on real
// success — no audit/listing path writes it.
const MetadataKeyExecuteSuccess = "cleric_execute_success"

// MetadataKeyGate, MetadataKeyGateSetAt, and MetadataKeyGateComment are
// written by the gateway's POST /api/v1/recoveries/{id}/gate handler
// (pkg/gateway, spi-sn0qg3) to record the human reviewer's decision. The
// keys live on the cleric package because they are part of the recovery
// bead's durable contract: the desktop's listing reads them back to
// render rejection counts and audit trails. MetadataKeyGate's value is
// one of GateApprove / GateReject / GateTakeover.
//
// MetadataKeyOutcome (above) covers post-execute outcomes only — a
// rejected proposal never reaches cleric.finish, so the gate keys are
// the only durable record the desktop has of a rejection.
const (
	MetadataKeyGate        = "cleric_gate"
	MetadataKeyGateSetAt   = "cleric_gate_set_at"
	MetadataKeyGateComment = "cleric_gate_comment"
)

// LabelNeedsManual is the label cleric.takeover applies to the source
// bead (NOT the recovery bead) when the human chooses to take over
// manually. The source stays `hooked`; the label is the only signal.
const LabelNeedsManual = "needs-manual"

// ProposedAction is the structured proposal a cleric Claude agent emits
// as JSON on stdout for cleric.publish to parse and persist. The shape
// is fixed by the design bead (spi-1s5w0o) — adding fields is fine,
// removing or renaming is not.
type ProposedAction struct {
	// Verb names the recovery action — one of the verbs in the action
	// manifest (see Manifest()). Required.
	Verb string `json:"verb"`

	// Args is the verb-specific argument bag. Schema is per-verb; see
	// the manifest entry's ArgsSchema. May be nil for arg-less verbs.
	Args map[string]string `json:"args,omitempty"`

	// Reasoning is the cleric's free-text justification for the
	// proposal. Surfaces on the human-review card. Required.
	Reasoning string `json:"reasoning"`

	// Confidence is the cleric's self-rated probability the action
	// will resolve the failure. 0.0..1.0. Optional — zero is allowed
	// and means "not stated". Used by the promotion/demotion loop to
	// weight outcomes; the gate doesn't reject low confidence.
	Confidence float64 `json:"confidence,omitempty"`

	// Destructive is true when the action mutates state in a way that
	// is hard to reverse (e.g. reset --hard). Mirrors the manifest
	// entry's Destructive flag; the cleric MAY override the manifest
	// default if the specific args make a normally-safe action
	// destructive (or vice versa).
	Destructive bool `json:"destructive,omitempty"`

	// FailureClass identifies the failure shape the cleric is
	// recovering from (e.g. "step-failure:implement", "merge-conflict",
	// "compile-error"). Used by the promotion/demotion loop to key the
	// learning tally. Required — empty FailureClass blocks promotion.
	FailureClass string `json:"failure_class"`
}

// ParseProposedAction parses raw stdout bytes into a ProposedAction.
// Tolerates leading/trailing whitespace and Markdown code fences (```json
// ... ```), since Claude often wraps JSON output that way. Returns the
// first JSON object it can parse — any prose around it is dropped.
//
// Validation rejects:
//   - empty stdout
//   - JSON that doesn't decode into ProposedAction shape
//   - missing Verb
//   - missing Reasoning
//   - missing FailureClass
//   - Verb not present in the action manifest
//   - Confidence outside [0.0, 1.0]
//   - Args containing unknown keys for the resolved verb (when the
//     manifest declares an args schema)
//
// On success, the returned ProposedAction's Destructive field is
// reconciled against the manifest: if the cleric did not set
// Destructive but the manifest defaults it true, the returned value
// carries the manifest default. The cleric may still set Destructive
// true even when the manifest defaults false (for verb+args combos the
// cleric judges destructive).
func ParseProposedAction(stdout []byte) (ProposedAction, error) {
	body := stripFenceAndPrefix(stdout)
	if len(body) == 0 {
		return ProposedAction{}, fmt.Errorf("cleric stdout is empty")
	}
	var pa ProposedAction
	if err := json.Unmarshal(body, &pa); err != nil {
		return ProposedAction{}, fmt.Errorf("decode ProposedAction JSON: %w", err)
	}
	if err := pa.Validate(); err != nil {
		return ProposedAction{}, err
	}
	// Reconcile Destructive default from manifest when cleric left it false.
	if !pa.Destructive {
		if entry, ok := Manifest()[pa.Verb]; ok && entry.DefaultDestructive {
			pa.Destructive = true
		}
	}
	return pa, nil
}

// Validate enforces the structural invariants on a ProposedAction.
// Separate from ParseProposedAction so callers that build proposals in
// memory (tests, gateway re-validation) get the same checks.
func (pa ProposedAction) Validate() error {
	if strings.TrimSpace(pa.Verb) == "" {
		return fmt.Errorf("ProposedAction.Verb is required")
	}
	if strings.TrimSpace(pa.Reasoning) == "" {
		return fmt.Errorf("ProposedAction.Reasoning is required")
	}
	if strings.TrimSpace(pa.FailureClass) == "" {
		return fmt.Errorf("ProposedAction.FailureClass is required")
	}
	if pa.Confidence < 0.0 || pa.Confidence > 1.0 {
		return fmt.Errorf("ProposedAction.Confidence %f out of [0.0, 1.0]", pa.Confidence)
	}
	entry, ok := Manifest()[pa.Verb]
	if !ok {
		return fmt.Errorf("unknown verb %q (not in cleric action manifest)", pa.Verb)
	}
	if len(entry.ArgsSchema) > 0 {
		for k := range pa.Args {
			if _, allowed := entry.ArgsSchema[k]; !allowed {
				return fmt.Errorf("verb %q rejects unknown arg %q (allowed: %v)", pa.Verb, k, sortedKeys(entry.ArgsSchema))
			}
		}
		for k, schema := range entry.ArgsSchema {
			if schema.Required {
				if v, ok := pa.Args[k]; !ok || strings.TrimSpace(v) == "" {
					return fmt.Errorf("verb %q requires arg %q", pa.Verb, k)
				}
			}
		}
	}
	return nil
}

// Marshal returns the canonical JSON encoding of the proposal — used by
// cleric.publish to write to bead metadata and by tests for round-trip
// assertions.
func (pa ProposedAction) Marshal() ([]byte, error) {
	return json.Marshal(pa)
}

// stripFenceAndPrefix trims leading/trailing whitespace and Markdown
// code fences. Tolerates ```json, ```, and bare-language fences.
// Returns the trimmed bytes; the original is not modified.
func stripFenceAndPrefix(b []byte) []byte {
	s := strings.TrimSpace(string(b))
	if strings.HasPrefix(s, "```") {
		// Strip first line (```json or ``` or ```anything) and trailing fence.
		if nl := strings.Index(s, "\n"); nl >= 0 {
			s = s[nl+1:]
		} else {
			s = strings.TrimPrefix(s, "```")
		}
		s = strings.TrimSpace(s)
		s = strings.TrimSuffix(s, "```")
		s = strings.TrimSpace(s)
	}
	// Some Claude variants emit a "Here is the proposal:" preface; trim
	// any leading non-JSON text up to the first '{'. We only do this
	// when the text doesn't already start with '{' or '[' so legitimate
	// JSON arrays still parse.
	if !strings.HasPrefix(s, "{") && !strings.HasPrefix(s, "[") {
		if idx := strings.Index(s, "{"); idx >= 0 {
			s = s[idx:]
		}
	}
	return []byte(s)
}

func sortedKeys(m map[string]ArgSchema) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	// Stable order for error messages — keys are short, sort manually.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
