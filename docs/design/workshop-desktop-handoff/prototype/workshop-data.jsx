// Spire Desktop — Workshop view: formula data
// Modeled from real .formula.toml files; treated as the gateway response shape
// the spec proposes (FormulaInfo[] for list view, FormulaDetail for detail view).

const FORMULAS = [
  // ──────────────────────────────────────────────────────────────────
  {
    name: "task-default",
    description: "Standard agent work: plan → implement → review → merge",
    version: 3,
    source: "embedded",
    default_for: ["task", "feature"],
    entry: "plan",
    vars: [
      { name: "bead_id",          type: "bead_id", required: true,  description: "The bead being worked on" },
      { name: "base_branch",      type: "string",  required: false, default: "main", description: "Base branch to merge into" },
      { name: "max_review_rounds",type: "string",  required: false, default: "3",    description: "Maximum review rounds before arbiter escalation" },
    ],
    workspaces: [
      { name: "feature", kind: "owned_worktree", branch: "feat/{vars.bead_id}", base: "{vars.base_branch}", scope: "run", cleanup: "terminal" },
    ],
    steps: [
      { name: "plan",      kind: "op",   action: "wizard.run",       flow: "task-plan",    title: "Plan implementation",  needs: [],            terminal: false },
      { name: "implement", kind: "op",   action: "wizard.run",       flow: "implement",    title: "Implement changes",    needs: ["plan"],      workspace: "feature", terminal: false },
      { name: "review",    kind: "call", action: "graph.run",        graph: "subgraph-review", title: "Review changes",   needs: ["implement"], workspace: "feature", terminal: false },
      { name: "merge",     kind: "op",   action: "git.merge_to_main", title: "Merge to main", needs: ["review"], workspace: "feature", terminal: false,
        with: { strategy: "squash" },
        when: "steps.review.outputs.outcome == merge" },
      { name: "close",     kind: "op",   action: "bead.finish",      title: "Close bead",        needs: ["merge"],     terminal: true,  with: { status: "closed" } },
      { name: "discard",   kind: "op",   action: "bead.finish",      title: "Discard branch",    needs: ["review"],    terminal: true,  with: { status: "discard" },
        when: "steps.review.outputs.outcome == discard" },
    ],
    edges: [
      { from: "plan",      to: "implement" },
      { from: "implement", to: "review" },
      { from: "review",    to: "merge",   when: "outcome == merge" },
      { from: "review",    to: "discard", when: "outcome == discard" },
      { from: "merge",     to: "close" },
    ],
    paths: [
      ["plan", "implement", "review", "merge", "close"],
      ["plan", "implement", "review", "discard"],
    ],
    outputs: [],
    issues: [],
    stats: { runs: 142, success: 0.91, avg_cost: 1.84, p50_duration: "8m 12s" },
  },

  // ──────────────────────────────────────────────────────────────────
  {
    name: "epic-default",
    description: "Epic lifecycle: design-check → plan → materialize → implement → review → merge",
    version: 3,
    source: "embedded",
    default_for: ["epic"],
    entry: "design-check",
    vars: [
      { name: "bead_id",          type: "bead_id", required: true, description: "The epic bead being worked on" },
      { name: "base_branch",      type: "string",  default: "main" },
      { name: "max_review_rounds",type: "string",  default: "3" },
    ],
    workspaces: [
      { name: "staging", kind: "staging", branch: "epic/{vars.bead_id}", base: "{vars.base_branch}", scope: "run", cleanup: "terminal" },
    ],
    steps: [
      { name: "design-check",     kind: "op",   action: "check.design-linked",  title: "Validate design linkage",         needs: [],                with: { auto_create: "true" } },
      { name: "plan",             kind: "op",   action: "wizard.run", flow: "epic-plan",  title: "Plan epic subtasks",      needs: ["design-check"] },
      { name: "materialize",      kind: "op",   action: "beads.materialize_plan",         title: "Verify subtask materialization", needs: ["plan"] },
      { name: "implement",        kind: "call", action: "graph.run", graph: "subgraph-implement", title: "Implement epic subtasks", needs: ["materialize"], workspace: "staging" },
      { name: "implement-failed", kind: "op",   action: "bead.finish", title: "Implementation failed — escalate",          needs: ["implement"],   terminal: true,
        with: { status: "escalate" }, when: "steps.implement.outputs.outcome != verified" },
      { name: "review",           kind: "call", action: "graph.run", graph: "subgraph-review",  title: "Review epic changes",      needs: ["implement"],   workspace: "staging",
        when: "steps.implement.outputs.outcome == verified" },
      { name: "merge",            kind: "op",   action: "git.merge_to_main", title: "Merge to main", needs: ["review"], workspace: "staging",
        with: { strategy: "squash" }, when: "steps.review.outputs.outcome == merge" },
      { name: "close",            kind: "op",   action: "bead.finish", title: "Close epic",   needs: ["merge"],   terminal: true, with: { status: "closed" } },
      { name: "discard",          kind: "op",   action: "bead.finish", title: "Discard epic", needs: ["review"],  terminal: true, with: { status: "discard" },
        when: "steps.review.outputs.outcome == discard" },
    ],
    edges: [
      { from: "design-check", to: "plan" },
      { from: "plan",         to: "materialize" },
      { from: "materialize",  to: "implement" },
      { from: "implement",    to: "implement-failed", when: "outcome != verified" },
      { from: "implement",    to: "review",           when: "outcome == verified" },
      { from: "review",       to: "merge",   when: "outcome == merge" },
      { from: "review",       to: "discard", when: "outcome == discard" },
      { from: "merge",        to: "close" },
    ],
    paths: [
      ["design-check", "plan", "materialize", "implement", "review", "merge", "close"],
      ["design-check", "plan", "materialize", "implement", "review", "discard"],
      ["design-check", "plan", "materialize", "implement", "implement-failed"],
    ],
    outputs: [],
    issues: [
      { level: "warning", phase: "implement-failed",
        message: "step has no observed runs in the last 30 days; consider whether the escalation path is still reachable" },
    ],
    stats: { runs: 28, success: 0.79, avg_cost: 14.6, p50_duration: "1h 22m" },
  },

  // ──────────────────────────────────────────────────────────────────
  {
    name: "bug-default",
    description: "Quick bugfix: plan → implement → review → merge",
    version: 3,
    source: "embedded",
    default_for: ["bug"],
    entry: "plan",
    vars: [
      { name: "bead_id",          type: "bead_id", required: true },
      { name: "base_branch",      type: "string",  default: "main" },
      { name: "max_review_rounds",type: "string",  default: "2", description: "Bugfix budget — stricter than task-default" },
    ],
    workspaces: [
      { name: "feature", kind: "owned_worktree", branch: "feat/{vars.bead_id}", base: "{vars.base_branch}", scope: "run", cleanup: "terminal" },
    ],
    steps: [
      { name: "plan",      kind: "op",   action: "wizard.run", flow: "task-plan",  title: "Plan bugfix",       needs: [],
        with: { extra_instructions: "IMPORTANT: identify the introducing commit/bead and create a caused-by dep." } },
      { name: "implement", kind: "op",   action: "wizard.run", flow: "implement",  title: "Implement bugfix",  needs: ["plan"], workspace: "feature" },
      { name: "review",    kind: "call", action: "graph.run",  graph: "subgraph-review", title: "Review bugfix", needs: ["implement"], workspace: "feature" },
      { name: "merge",     kind: "op",   action: "git.merge_to_main", title: "Merge to main", needs: ["review"], workspace: "feature",
        with: { strategy: "squash" }, when: "steps.review.outputs.outcome == merge" },
      { name: "close",     kind: "op",   action: "bead.finish", title: "Close bead",      needs: ["merge"],   terminal: true, with: { status: "closed" } },
      { name: "discard",   kind: "op",   action: "bead.finish", title: "Discard branch",  needs: ["review"],  terminal: true, with: { status: "discard" },
        when: "steps.review.outputs.outcome == discard" },
    ],
    edges: [
      { from: "plan",      to: "implement" },
      { from: "implement", to: "review" },
      { from: "review",    to: "merge",   when: "outcome == merge" },
      { from: "review",    to: "discard", when: "outcome == discard" },
      { from: "merge",     to: "close" },
    ],
    paths: [
      ["plan", "implement", "review", "merge", "close"],
      ["plan", "implement", "review", "discard"],
    ],
    outputs: [],
    issues: [],
    stats: { runs: 67, success: 0.88, avg_cost: 1.12, p50_duration: "5m 41s" },
  },

  // ──────────────────────────────────────────────────────────────────
  {
    name: "chore-default",
    description: "Chore lifecycle: research → implement → review → document → merge → close",
    version: 3,
    source: "embedded",
    default_for: ["chore"],
    entry: "research",
    vars: [
      { name: "bead_id",          type: "bead_id", required: true },
      { name: "base_branch",      type: "string",  default: "main" },
      { name: "max_review_rounds",type: "string",  default: "3" },
    ],
    workspaces: [
      { name: "feature", kind: "owned_worktree", branch: "feat/{vars.bead_id}", base: "{vars.base_branch}", scope: "run", cleanup: "terminal" },
    ],
    steps: [
      { name: "research",  kind: "op",   action: "wizard.run", flow: "implement", title: "Research problem and existing docs", needs: [],
        with: { prompt: "Read docs/wiki/, related code, and the bead's linked beads. Write findings as a comment. No code changes." } },
      { name: "implement", kind: "op",   action: "wizard.run", flow: "implement", title: "Implement changes", needs: ["research"], workspace: "feature" },
      { name: "review",    kind: "call", action: "graph.run",  graph: "subgraph-review", title: "Review changes", needs: ["implement"], workspace: "feature" },
      { name: "document",  kind: "op",   action: "wizard.run", flow: "implement", title: "Update documentation", needs: ["review"], workspace: "feature",
        with: { prompt: "Create or update a docs/wiki/ page documenting what changed and how to handle similar work." },
        when: "steps.review.outputs.outcome == merge" },
      { name: "merge",     kind: "op",   action: "git.merge_to_main", title: "Merge to main", needs: ["document"], workspace: "feature", with: { strategy: "squash" } },
      { name: "close",     kind: "op",   action: "bead.finish", title: "Close chore", needs: ["merge"], terminal: true, with: { status: "closed" } },
      { name: "discard",   kind: "op",   action: "bead.finish", title: "Discard chore", needs: ["review"], terminal: true, with: { status: "discard" },
        when: "steps.review.outputs.outcome == discard" },
    ],
    edges: [
      { from: "research",  to: "implement" },
      { from: "implement", to: "review" },
      { from: "review",    to: "document", when: "outcome == merge" },
      { from: "review",    to: "discard",  when: "outcome == discard" },
      { from: "document",  to: "merge" },
      { from: "merge",     to: "close" },
    ],
    paths: [
      ["research", "implement", "review", "document", "merge", "close"],
      ["research", "implement", "review", "discard"],
    ],
    outputs: [],
    issues: [],
    stats: { runs: 11, success: 0.95, avg_cost: 2.41, p50_duration: "16m 03s" },
  },

  // ──────────────────────────────────────────────────────────────────
  {
    name: "subgraph-review",
    description: "Review DAG: sage-review → fix/arbiter → merge/discard with terminal branch invariants",
    version: 3,
    source: "embedded",
    default_for: [],
    entry: "sage-review",
    vars: [
      { name: "bead_id", required: true, description: "The bead being reviewed" },
      { name: "branch",  required: true, description: "Staging branch (feat/<bead-id> or epic/<bead-id>)" },
      { name: "max_review_rounds", default: "3" },
    ],
    workspaces: [],
    steps: [
      { name: "sage-review", kind: "op", action: "wizard.run", flow: "sage-review",
        role: "sage", title: "Sage review", model: "claude-opus-4-7", timeout: "10m",
        verdict_only: true, needs: [], terminal: false },
      { name: "fix", kind: "op", action: "wizard.run", flow: "review-fix",
        role: "apprentice", title: "Fix: address sage review feedback",
        model: "claude-opus-4-7", timeout: "15m",
        needs: ["sage-review"], resets: ["sage-review", "fix"],
        when: "sage-review.verdict == request_changes && completed_count < max_review_rounds",
        terminal: false },
      { name: "arbiter", kind: "op", action: "wizard.run", flow: "arbiter",
        role: "arbiter", title: "Arbiter: break review deadlock",
        model: "claude-opus-4-7", timeout: "10m",
        needs: ["sage-review"],
        when: "sage-review.verdict == request_changes && completed_count >= max_review_rounds",
        terminal: false },
      { name: "merge",   kind: "op", action: "noop", title: "Merge to main",     needs: ["sage-review", "arbiter"], terminal: true,
        when: "sage.verdict == approve || arbiter.decision IN (merge, split)" },
      { name: "discard", kind: "op", action: "noop", title: "Discard branch",    needs: ["arbiter"],                terminal: true,
        when: "arbiter.decision == discard" },
    ],
    edges: [
      { from: "sage-review", to: "fix",     when: "request_changes && rounds < max" },
      { from: "sage-review", to: "arbiter", when: "request_changes && rounds >= max" },
      { from: "sage-review", to: "merge",   when: "approve" },
      { from: "fix",         to: "sage-review", kind: "reset" },
      { from: "arbiter",     to: "merge",   when: "merge | split" },
      { from: "arbiter",     to: "discard", when: "discard" },
    ],
    paths: [
      ["sage-review", "merge"],
      ["sage-review", "fix", "sage-review"],
      ["sage-review", "arbiter", "merge"],
      ["sage-review", "arbiter", "discard"],
    ],
    outputs: [
      { name: "outcome",          type: "enum",   values: ["merge", "discard"], description: "Terminal review outcome" },
      { name: "verdict",          type: "string", description: "Last sage verdict" },
      { name: "arbiter_decision", type: "string", description: "Arbiter decision if escalated" },
      { name: "rounds_used",      type: "int",    description: "Number of review rounds completed" },
    ],
    issues: [],
    stats: { runs: 198, success: 0.84, avg_cost: 0.92, p50_duration: "3m 48s" },
  },

  // ──────────────────────────────────────────────────────────────────
  {
    name: "subgraph-implement",
    description: "Declarative epic implementation: dispatch children in waves, verify build, signal readiness for review",
    version: 3,
    source: "embedded",
    default_for: [],
    entry: "dispatch-children",
    vars: [
      { name: "bead_id", type: "bead_id", required: true },
      { name: "max_build_fix_rounds", type: "int", default: "2" },
    ],
    workspaces: [
      { name: "staging", kind: "staging", branch: "staging/{vars.bead_id}", base: "main", scope: "run", cleanup: "terminal" },
    ],
    steps: [
      { name: "dispatch-children", kind: "dispatch", action: "dispatch.children", workspace: "staging",
        title: "Dispatch child beads", needs: [],
        with: { strategy: "dependency-wave" } },
      { name: "verify-build",      kind: "op",       action: "verify.run", workspace: "staging",
        title: "Verify build", needs: ["dispatch-children"], produces: ["status"] },
      { name: "verified",          kind: "op",       action: "noop", title: "Build verified — ready for review",
        needs: ["verify-build"], terminal: true,
        when: "steps.verify-build.outputs.status == pass" },
      { name: "build-failed",      kind: "op",       action: "noop", title: "Build verification failed",
        needs: ["verify-build"], terminal: true, with: { status: "escalate" },
        when: "steps.verify-build.outputs.status == fail" },
    ],
    edges: [
      { from: "dispatch-children", to: "verify-build" },
      { from: "verify-build",      to: "verified",     when: "status == pass" },
      { from: "verify-build",      to: "build-failed", when: "status == fail" },
    ],
    paths: [
      ["dispatch-children", "verify-build", "verified"],
      ["dispatch-children", "verify-build", "build-failed"],
    ],
    outputs: [
      { name: "dispatch_status", type: "string" },
      { name: "build_status",    type: "enum", values: ["pass", "fail"] },
      { name: "outcome",         type: "enum", values: ["verified", "build-failed"] },
    ],
    issues: [],
    stats: { runs: 31, success: 0.71, avg_cost: 9.42, p50_duration: "42m 18s" },
  },

  // ──────────────────────────────────────────────────────────────────
  {
    name: "cleric-default",
    description: "Agentic recovery: Claude-driven decide + learn steps, always-close lifecycle",
    version: 3,
    source: "embedded",
    default_for: ["recovery"],
    entry: "collect_context",
    vars: [
      { name: "bead_id",       type: "bead_id", required: true },
      { name: "parent_bead",   type: "bead_id", required: true, description: "The bead that encountered the failure" },
      { name: "failure_class", type: "string",  required: true },
      { name: "base_branch",   type: "string",  default: "main" },
      { name: "max_retries",   type: "string",  default: "4", description: "Verify-fail budget" },
      { name: "max_execute_error_retries", type: "string", default: "2", description: "Execute-error budget (independent)" },
    ],
    workspaces: [],
    steps: [
      { name: "collect_context", kind: "op", action: "cleric.execute", title: "Collect diagnosis context and prior learnings", needs: [], with: { action: "collect_context" } },
      { name: "decide",          kind: "op", action: "cleric.execute", title: "Claude-driven recovery decision",            needs: ["collect_context"], with: { action: "decide" } },
      { name: "execute",         kind: "op", action: "cleric.execute", title: "Execute chosen recovery action",
        needs: ["decide"], on_error: "record",
        with: { action: "execute" },
        when: "steps.decide.outputs.needs_human != true" },
      { name: "verify",          kind: "op", action: "cleric.execute", title: "Verify source bead health",
        needs: ["execute"], produces: ["verification_status"], with: { action: "verify" },
        when: "steps.execute.outputs.status == success" },
      { name: "retry",           kind: "op", action: "noop", title: "Retry recovery loop",
        needs: ["verify"], resets: ["decide", "execute", "verify"],
        when: "verify.status == fail && decide.completed_count < max_retries" },
      { name: "retry_on_error",  kind: "op", action: "cleric.execute", title: "Retry after execute error",
        needs: ["execute"], resets: ["retry_on_error", "decide", "execute", "verify"],
        with: { action: "record_error" },
        when: "execute.status == failed && retry_on_error.completed_count < max_execute_error_retries" },
      { name: "learn",           kind: "op", action: "cleric.learn", title: "Claude-driven learning extraction",
        needs: ["verify"], with: { action: "learn" },
        when: "verify.status == pass || decide.completed_count >= max_retries" },
      { name: "finish",                       kind: "op", action: "cleric.execute", title: "Close recovery bead",
        needs: ["learn"],   terminal: true, with: { action: "finish" } },
      { name: "finish_needs_human",           kind: "op", action: "cleric.execute", title: "Close recovery bead (needs human)",
        needs: ["decide"],  terminal: true, with: { action: "finish" },
        when: "decide.outputs.needs_human == true" },
      { name: "finish_needs_human_on_error",  kind: "op", action: "cleric.execute", title: "Close recovery bead (execute errors exhausted)",
        needs: ["execute"], terminal: true, with: { action: "finish", needs_human: "true" },
        when: "execute.status == failed && retry_on_error.completed_count >= max_execute_error_retries" },
    ],
    edges: [
      { from: "collect_context", to: "decide" },
      { from: "decide",          to: "execute",                when: "!needs_human" },
      { from: "decide",          to: "finish_needs_human",     when: "needs_human" },
      { from: "execute",         to: "verify",                 when: "status == success" },
      { from: "execute",         to: "retry_on_error",         when: "status == failed && budget" },
      { from: "execute",         to: "finish_needs_human_on_error", when: "status == failed && exhausted" },
      { from: "verify",          to: "retry",                  when: "fail && budget" },
      { from: "verify",          to: "learn",                  when: "pass || exhausted" },
      { from: "retry",           to: "decide", kind: "reset" },
      { from: "retry_on_error",  to: "decide", kind: "reset" },
      { from: "learn",           to: "finish" },
    ],
    paths: [
      ["collect_context", "decide", "execute", "verify", "learn", "finish"],
      ["collect_context", "decide", "finish_needs_human"],
      ["collect_context", "decide", "execute", "verify", "retry", "decide", "execute", "verify", "learn", "finish"],
      ["collect_context", "decide", "execute", "retry_on_error", "decide", "execute", "verify", "learn", "finish"],
      ["collect_context", "decide", "execute", "finish_needs_human_on_error"],
    ],
    outputs: [],
    issues: [
      { level: "warning", phase: "retry_on_error",
        message: "self-reset step — verify reset list includes the step itself or it will only fire once" },
    ],
    stats: { runs: 14, success: 0.64, avg_cost: 3.18, p50_duration: "11m 22s" },
  },

  // ──────────────────────────────────────────────────────────────────
  // Custom (user-authored) examples
  {
    name: "design-review-only",
    description: "Custom: skip implementation; design beads → sage review only → close",
    version: 3,
    source: "custom",
    default_for: [],
    entry: "design-review",
    vars: [
      { name: "bead_id", required: true },
    ],
    workspaces: [],
    steps: [
      { name: "design-review", kind: "op",   action: "wizard.run", flow: "sage-review", role: "sage", title: "Design review", needs: [], terminal: false },
      { name: "approve",       kind: "op",   action: "bead.finish", title: "Approve design", needs: ["design-review"], terminal: true,
        with: { status: "closed" }, when: "steps.design-review.outputs.verdict == approve" },
      { name: "reject",        kind: "op",   action: "bead.finish", title: "Reject design",  needs: ["design-review"], terminal: true,
        with: { status: "discard" }, when: "steps.design-review.outputs.verdict == request_changes" },
    ],
    edges: [
      { from: "design-review", to: "approve", when: "approve" },
      { from: "design-review", to: "reject",  when: "request_changes" },
    ],
    paths: [
      ["design-review", "approve"],
      ["design-review", "reject"],
    ],
    outputs: [],
    issues: [],
    stats: { runs: 4, success: 1.0, avg_cost: 0.31, p50_duration: "1m 12s" },
  },

  {
    name: "hotfix-fastpath",
    description: "Custom: skip review for emergency hotfixes; only senior engineers can run.",
    version: 3,
    source: "custom",
    default_for: [],
    entry: "implement",
    vars: [
      { name: "bead_id",     required: true },
      { name: "base_branch", default: "main" },
    ],
    workspaces: [
      { name: "feature", kind: "owned_worktree", branch: "hotfix/{vars.bead_id}", base: "{vars.base_branch}" },
    ],
    steps: [
      { name: "implement", kind: "op", action: "wizard.run", flow: "implement",  title: "Implement hotfix",  needs: [], workspace: "feature" },
      { name: "merge",     kind: "op", action: "git.merge_to_main", title: "Merge to main", needs: ["implement"], workspace: "feature", with: { strategy: "squash" } },
      { name: "close",     kind: "op", action: "bead.finish", title: "Close hotfix", needs: ["merge"], terminal: true, with: { status: "closed" } },
    ],
    edges: [
      { from: "implement", to: "merge" },
      { from: "merge",     to: "close" },
    ],
    paths: [["implement", "merge", "close"]],
    outputs: [],
    issues: [
      { level: "error",   phase: "implement", message: "no review step — hotfix-fastpath bypasses sage/arbiter review. Verify policy with archmage before publishing." },
      { level: "warning", phase: "merge",     message: "no test verification before merge — high blast radius" },
    ],
    stats: { runs: 2, success: 1.0, avg_cost: 0.81, p50_duration: "4m 02s" },
  },
];

// ── Step "kind" metadata ───────────────────────────────────────────
const KIND_META = {
  op:       { label: "OP",       fg: "#86b9ff", bg: "rgba(90,165,255,0.10)",  ring: "rgba(90,165,255,0.30)",  glyph: "▭",  description: "Single in-process action" },
  call:     { label: "CALL",     fg: "#c8adff", bg: "rgba(179,140,255,0.10)", ring: "rgba(179,140,255,0.30)", glyph: "◇",  description: "Invoke a sub-graph (graph.run)" },
  dispatch: { label: "DISPATCH", fg: "#ffba5a", bg: "rgba(247,201,72,0.10)",  ring: "rgba(247,201,72,0.30)",  glyph: "⟁",  description: "Spawn child beads in waves" },
};

// Map bead.id → formula name (for per-bead pill in BeadDetail).
// In the real desktop this would come from the gateway; here we infer by type.
function formulaForBead(bead) {
  const byType = {
    task: "task-default", feature: "task-default",
    bug: "bug-default",
    epic: "epic-default",
    chore: "chore-default",
    design: "design-review-only",
    recovery: "cleric-default",
  };
  return byType[bead?.type] || "task-default";
}

function getFormula(name) {
  return FORMULAS.find(f => f.name === name) || FORMULAS[0];
}

Object.assign(window, { FORMULAS, KIND_META, formulaForBead, getFormula });
