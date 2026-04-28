package cleric

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
