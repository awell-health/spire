package scaffold

// CommonCommands are the multi-role verbs available to every role. They
// live under "spire" with no role prefix.
//
// Keep this list EXACT — it is the source of truth for hook output and
// the Common section of docs/cli-reference.md.
var CommonCommands = []Command{
	{
		Name:        "focus",
		Args:        "<bead>",
		Description: "Assemble full context for a bead (deps, messages, comments, workflow molecule).",
	},
	{
		Name:        "grok",
		Args:        "<bead>",
		Description: "Deep focus that also pulls live integration context (e.g., Linear).",
	},
	{
		Name:        "graph",
		Args:        "<bead> [--depth N] [--rel ...] [--with-changes] [--with-diffs]",
		Description: "Walk the bead graph and render neighbors' bodies + comments (and optionally commits + diffs).",
	},
	{
		Name:        "send",
		Args:        `<agent> "msg" --ref <bead>`,
		Description: "Send a message to another agent, optionally referencing a bead.",
	},
	{
		Name:        "collect",
		Args:        "",
		Description: "Check the inbox for messages addressed to this agent.",
	},
	{
		Name:        "read",
		Args:        "<bead>",
		Description: "Mark a message thread on a bead as read.",
	},
}

// catalogs is the per-role catalog table. Each entry's Commands list MUST
// match the accepted taxonomy in spi-2fmix exactly — it is the contract
// that scaffolder hooks and docs/cli-reference.md both render against.
var catalogs = map[Role]*Catalog{
	RoleApprentice: {
		Role: RoleApprentice,
		Commands: []Command{
			{
				Name:        "apprentice submit",
				Args:        "[--bead <id>] [--no-changes]",
				Description: "Bundle apprentice work and signal the wizard via the BundleStore.",
			},
		},
		Common: CommonCommands,
	},
	RoleWizard: {
		Role: RoleWizard,
		Commands: []Command{
			{
				Name:        "wizard claim",
				Args:        "<bead>",
				Description: "Atomic claim: create the attempt bead and set the task to in_progress.",
			},
			{
				Name:        "wizard seal",
				Args:        "<bead> [--merge-commit <sha>]",
				Description: "Record merge_commit + sealed_at on the task and close the attempt bead.",
			},
		},
		Common: CommonCommands,
	},
	RoleSage: {
		Role: RoleSage,
		Commands: []Command{
			{
				Name:        "sage accept",
				Args:        "<bead> [comment]",
				Description: "Close the open review round with verdict=approve and label the task review-approved.",
			},
			{
				Name:        "sage reject",
				Args:        "<bead> --feedback <text>",
				Description: "Close the open review round with verdict=request_changes and the required feedback.",
			},
		},
		Common: CommonCommands,
	},
	RoleCleric: {
		Role: RoleCleric,
		Commands: []Command{
			{
				Name:        "cleric diagnose",
				Args:        "<bead>",
				Description: "Diagnose a stuck or failing bead and propose a recovery action.",
			},
			{
				Name:        "cleric execute",
				Args:        "--action <name>",
				Description: "Run the recovery action chosen during diagnosis.",
			},
			{
				Name:        "cleric learn",
				Args:        "<bead>",
				Description: "Persist the diagnosis and outcome into the cleric's knowledge base.",
			},
		},
		Common: CommonCommands,
	},
	RoleArbiter: {
		Role: RoleArbiter,
		Commands: []Command{
			{
				Name:        "arbiter decide",
				Args:        "<bead> [--verdict accept|reject|custom]",
				Description: "Record a binding arbiter verdict for a contested review round.",
			},
		},
		Common: CommonCommands,
	},
}
