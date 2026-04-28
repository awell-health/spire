package cleric

import "encoding/json"

// ActionEntry describes one verb in the cleric's action manifest.
// The manifest is the cleric's vocabulary: cleric.publish validates
// proposed verbs against it; cleric.execute looks up the gateway
// endpoint via Verb.
//
// Adding a new action: append an entry to Manifest(), add a gateway
// handler under pkg/gateway, and update the cleric prompt-builder so
// the cleric knows the verb is available.
type ActionEntry struct {
	// Verb is the canonical name (e.g. "reset --to <step>").
	Verb string

	// Description is human-readable; surfaces in the cleric's prompt
	// so the agent knows what each verb does.
	Description string

	// ArgsSchema declares the per-arg shape. Empty means the verb
	// takes no args. Validators reject unknown args and required-but-
	// missing args.
	ArgsSchema map[string]ArgSchema

	// DefaultDestructive is the manifest's default for the proposal's
	// Destructive flag — applied when the cleric does not set
	// Destructive explicitly. The cleric may override.
	DefaultDestructive bool

	// GatewayPath is the relative path the gateway client uses to
	// invoke the verb. Empty for verbs that have no gateway endpoint
	// yet (the gateway client returns ErrUnimplemented in that case).
	GatewayPath string
}

// ArgSchema describes a single argument in an ActionEntry's ArgsSchema.
type ArgSchema struct {
	// Required, when true, means cleric.publish rejects the proposal
	// if the arg is missing or empty.
	Required bool

	// Description surfaces in the cleric's prompt to guide arg choice.
	Description string
}

// Manifest returns the action vocabulary the cleric proposes from.
// Returned as a copy so callers can mutate without affecting the
// shared catalog. The catalog is fixed at build time — there is no
// runtime registration today (cleric runtime v1, spi-hhkozk).
//
// Verbs:
//   - reset --to <step>: rewind formula state to a named step on the
//     existing branch, dropping graph state past it. The most common
//     repair (e.g. wizard's review found post-implement bugs and we
//     want to retry implement). Gateway endpoint shipped under
//     spi-kntoe1.
//   - reset --hard: workspace + branch + graph-state nuke. Used when
//     the workspace is unrecoverable. Destructive.
//   - resummon: retry from the current step on the existing branch.
//     Equivalent to "spire summon <bead>" while the bead is hooked.
//   - dismiss: close the source bead with no merge. Used when the
//     work is no longer wanted (or proven impossible).
//   - update: rare; sets a non-status bead field. Args carry the
//     field+value pair.
//   - comment-request-input: cleric asks the human a question rather
//     than proposing an action. Symmetric counterpart to takeover.
func Manifest() map[string]ActionEntry {
	return map[string]ActionEntry{
		"reset --to <step>": {
			Verb:        "reset --to <step>",
			Description: "Rewind formula state to a named step on the existing branch.",
			ArgsSchema: map[string]ArgSchema{
				"step": {Required: true, Description: "Step name to rewind to (e.g. \"implement\")."},
			},
			GatewayPath: "/cleric/actions/reset-to-step",
		},
		"reset --hard": {
			Verb:               "reset --hard",
			Description:        "Workspace + branch + graph-state nuke. Use only when workspace is unrecoverable.",
			DefaultDestructive: true,
			GatewayPath:        "/cleric/actions/reset-hard",
		},
		"resummon": {
			Verb:        "resummon",
			Description: "Retry from current step on existing branch (the default repair).",
			GatewayPath: "/cleric/actions/resummon",
		},
		"dismiss": {
			Verb:               "dismiss",
			Description:        "Cancel work entirely; close source bead with no merge.",
			DefaultDestructive: true,
			GatewayPath:        "/cleric/actions/dismiss",
		},
		"update": {
			Verb:        "update",
			Description: "Set a non-status field on the source bead (rare).",
			ArgsSchema: map[string]ArgSchema{
				"field": {Required: true, Description: "Bead field to update."},
				"value": {Required: true, Description: "New value."},
			},
			GatewayPath: "/cleric/actions/update",
		},
		"comment-request-input": {
			Verb:        "comment-request-input",
			Description: "Ask the human a question rather than proposing an action.",
			ArgsSchema: map[string]ArgSchema{
				"question": {Required: true, Description: "The question to surface to the human."},
			},
			GatewayPath: "/cleric/actions/request-input",
		},
	}
}

// ActionManifest describes a single verb in the v1 action catalog. The
// JSON shape is the wire format served by GET /api/v1/actions; the
// desktop's HITL dropdown consumes it directly.
type ActionManifest struct {
	// Name is the verb identifier (e.g. "resummon", "dismiss"). Stable
	// across versions — desktops match on this string.
	Name string `json:"name"`

	// ArgsSchema is a JSON schema describing the verb's required and
	// optional arguments. RawMessage preserves the literal bytes so we
	// don't double-marshal.
	ArgsSchema json.RawMessage `json:"args_schema"`

	// Destructive marks verbs whose effect is hard to undo (closes beads,
	// nukes worktrees, etc.). The desktop uses this to gate the action
	// behind a confirm dialog by default.
	Destructive bool `json:"destructive"`

	// EndpointPath is the gateway path the desktop POSTs to fire this
	// verb. Includes the leading slash; the {id} placeholder is replaced
	// client-side with the bead/recovery ID.
	EndpointPath string `json:"endpoint_path"`

	// Description is human-readable. Surfaced in the desktop tooltip /
	// help text.
	Description string `json:"description"`
}

// V1Actions is the v1 action catalog. Order is presentational — the
// desktop renders the dropdown in this order. Adding a new verb means
// appending an entry here AND landing the matching gateway handler +
// gatewayclient method.
//
// The catalog deliberately omits `reset --to <step>`: that verb shipped
// in spi-kntoe1 as part of the existing /reset endpoint, and the desktop
// already has a dedicated affordance for it. If we later want it in the
// dropdown alongside the others, add an entry pointing at the existing
// /reset endpoint with a `to` arg.
var V1Actions = []ActionManifest{
	{
		Name:         "resummon",
		ArgsSchema:   json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
		Destructive:  false,
		EndpointPath: "/api/v1/beads/{id}/resummon",
		Description:  "Re-summon the wizard at the current step on the existing branch. Bead must be in `hooked` state — otherwise the wizard is presumed alive.",
	},
	{
		Name:         "dismiss",
		ArgsSchema:   json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
		Destructive:  true,
		EndpointPath: "/api/v1/beads/{id}/dismiss",
		Description:  "Cancel the bead's work entirely. Closes the bead with no merge and cleans up the worktree and branch.",
	},
	{
		Name: "update_status",
		ArgsSchema: json.RawMessage(`{
			"type":"object",
			"required":["to"],
			"properties":{
				"to":{"type":"string","enum":["open","ready","in_progress","hooked","awaiting_review","closed"]}
			},
			"additionalProperties":false
		}`),
		Destructive:  false,
		EndpointPath: "/api/v1/beads/{id}/update_status",
		Description:  "Transition the bead to a different status. Server-side whitelist of valid transitions; rare — for non-execution interventions.",
	},
	{
		Name: "comment_request",
		ArgsSchema: json.RawMessage(`{
			"type":"object",
			"required":["question"],
			"properties":{
				"question":{"type":"string","minLength":1}
			},
			"additionalProperties":false
		}`),
		Destructive:  false,
		EndpointPath: "/api/v1/recoveries/{id}/comment_request",
		Description:  "Cleric-side: write a labeled question comment on the recovery bead, signaling the cleric needs human input. Status stays `awaiting_review`. Humans respond via comment reply.",
	},
	{
		Name:         "reset_hard",
		ArgsSchema:   json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
		Destructive:  true,
		EndpointPath: "/api/v1/beads/{id}/reset_hard",
		Description:  "Nuke worktree, branch, and graph state. Closes attempt/review beads with the reset-cycle tag so logs survive.",
	},
}

// FindAction returns the manifest entry for the given verb name, or nil
// if the verb is not in the catalog. Useful for handlers that want to
// double-check that the route they're serving is registered in the
// manifest.
func FindAction(name string) *ActionManifest {
	for i := range V1Actions {
		if V1Actions[i].Name == name {
			return &V1Actions[i]
		}
	}
	return nil
}

// UpdateStatusTransitions is the server-side whitelist of valid
// {from, to} transitions for the update_status action. Any combination
// not present here is rejected with 400 by the handler. Conservative for
// v1: covers the "human fixed it manually, take the bead back" cases
// that the design bead (spi-1s5w0o) calls out, plus a small set of
// adjacent moves the desktop is likely to need.
//
// Lookup shape: AllowedFromStatuses[from] returns the set of valid
// targets. Empty value means "no transitions allowed from this status".
var UpdateStatusTransitions = map[string]map[string]bool{
	"hooked": {
		"open":            true,
		"in_progress":     true,
		"awaiting_review": true,
		"closed":          true,
	},
	"awaiting_review": {
		"in_progress": true,
		"closed":      true,
	},
	"in_progress": {
		"open":            true,
		"hooked":          true,
		"awaiting_review": true,
	},
	"open": {
		"ready":    true,
		"deferred": true,
	},
	"ready": {
		"open":     true,
		"deferred": true,
	},
	"deferred": {
		"open":  true,
		"ready": true,
	},
}

// IsValidStatusTransition reports whether a from→to status flip is in
// the whitelist. Idempotent self-transitions (from == to) are always
// considered valid so re-firing the same verb is a 200 no-op.
func IsValidStatusTransition(from, to string) bool {
	if from == to {
		return true
	}
	if allowed, ok := UpdateStatusTransitions[from]; ok {
		return allowed[to]
	}
	return false
}
