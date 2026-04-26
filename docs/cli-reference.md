# Spire CLI Reference

Complete reference for all `spire` commands, organized by agent role.

For the `bd` (beads) CLI reference, see the [beads documentation](https://github.com/steveyegge/beads).

> **Role-scoped CLI taxonomy.** As of v0.44.0 the CLI is organized by
> agent role: each of the five agent roles (apprentice, wizard, sage,
> cleric, arbiter) has its own parent command and scoped subcommands.
> Multi-role and top-level commands live under their own sections. A
> role-aware scaffolder hook reads the `SPIRE_ROLE` env var and only
> surfaces that role's catalog in the agent's session — see CLAUDE.md
> for the full scaffolder contract.
>
> **Source of truth.** Role sections below (Apprentice, Wizard, Sage,
> Cleric, Arbiter) mirror `pkg/scaffold/catalog.go`. If a signature here
> drifts from that catalog, the catalog wins.

---

## Global flags

```
--tower <name>    Override active tower for this command
```

---

## Apprentice

Role-scoped commands for the **apprentice** — the agent dispatched by a
wizard to implement a single task bead in an isolated worktree.

### `spire apprentice submit`

Bundle apprentice work and signal the wizard via the BundleStore.

```bash
spire apprentice submit [--bead <id>] [--no-changes]
```

| Flag | Description |
|------|-------------|
| `--bead <id>` | Task bead ID (overrides `SPIRE_BEAD_ID`) |
| `--no-changes` | Signal the bead with a no-op payload; skips bundle creation |

Bundles every commit between the base branch and HEAD into a git bundle,
uploads the bundle to the tower's BundleStore, and writes a signal on the
task bead so the wizard knows the bundle is ready to consume. The command
never pushes to a git remote — the bundle IS the delivery.

---

## Wizard

Role-scoped commands for the **wizard** — the per-bead orchestrator that
drives the formula lifecycle, creates attempt beads, dispatches
apprentices, consults sages, and seals work on merge.

### `spire wizard claim <bead>`

Atomic claim: create the attempt bead and set the task to `in_progress`.

```bash
spire wizard claim <bead-id>
```

Creates a child attempt bead under `<bead-id>` and flips the task bead's
status to `in_progress`. Fails if an open attempt already exists under
the task (the error names the existing attempt ID).

### `spire wizard seal <bead> [--merge-commit <sha>]`

Record `merge_commit` + `sealed_at` on the task and close the attempt
bead.

```bash
spire wizard seal <bead-id> [--merge-commit <sha>]
```

| Flag | Description |
|------|-------------|
| `--merge-commit <sha>` | Merge commit SHA (defaults to current git HEAD) |

Writes `merge_commit` + `sealed_at` fields to the task bead's metadata
and closes the bead's current open attempt. Called after the feature
branch has merged into main.

---

## Sage

Role-scoped commands for the **sage** — the agent dispatched to review
an apprentice's work and record a verdict on the open review round.

The user-facing verbs are `accept` / `reject`. The CLI translates them to
the canonical review-round verdicts `approve` / `request_changes` and
writes through the single review-round store helper used by the wizard
review loop. Steward routing, feedback re-dispatch, and review history
read from the review-round bead's `review_verdict` metadata, so sage-CLI
verdicts and wizard-driven verdicts flow through the same pipeline.

Verdict metadata is stored on the **review-round bead** (the child of
the task), not on the task itself. This is the single authoritative
storage boundary an arbiter shares with the sage — keeping all verdict
state on one bead makes arbiter decisions binding. No parallel verdict
field is written to the parent bead.

### `spire sage accept <bead> [comment]`

Close the open review round with the canonical `approve` verdict.

```bash
spire sage accept <bead-id> [comment]
```

Writes `review_verdict=approve` to the open review-round child bead
(via `CloseReviewBead`), adds the `review-approved` label to the task
bead so the merge queue picks it up, and appends a review-approved
comment. The optional positional `comment` is carried in the review-round
summary and parent comment.

Errors out if the most-recent review-round on `<bead-id>` already
carries an `arbiter_verdict` — arbiter decisions are binding and the
sage CLI refuses to overwrite them.

### `spire sage reject <bead> --feedback <text>`

Close the open review round with the canonical `request_changes` verdict.

```bash
spire sage reject <bead-id> --feedback <text>
```

| Flag | Description |
|------|-------------|
| `--feedback <text>` | Feedback text stored as the review-round summary (required) |

Writes `review_verdict=request_changes` to the open review-round child
bead (via `CloseReviewBead`) with the feedback as the summary, and
appends a `request_changes` comment to the task bead. The steward's
feedback detector observes the review-round verdict and re-dispatches the
apprentice, identical to the wizard-driven path. `--feedback` is
required — rejections without actionable feedback are not accepted.

Errors out if the most-recent review-round on `<bead-id>` already
carries an `arbiter_verdict` — see `spire arbiter decide`.

---

## Cleric

Role-scoped commands for the **cleric** — the role that drives the
failure-recovery state machine. The three subcommands map ~1:1 to the
cleric formula actions: `diagnose` → `cleric.decide`, `execute` →
`cleric.execute`, `learn` → `cleric.learn`.

### `spire cleric diagnose <bead>`

Diagnose a stuck or failing bead and propose a recovery action.

```bash
spire cleric diagnose <bead-id> [--decision <text>]
```

| Flag | Description |
|------|-------------|
| `--decision <text>` | Next-action decision text attached to the diagnosis |

Writes `cleric_state=diagnosed` (and the optional decision text) to the
bead's metadata.

### `spire cleric execute --action <name>`

Run the recovery action chosen during diagnosis.

```bash
spire cleric execute --action <name> [--bead <id>]
```

| Flag | Description |
|------|-------------|
| `--action <name>` | Recovery action to run (required) |
| `--bead <id>` | Bead ID (overrides `SPIRE_BEAD` / cwd auto-detect) |

If `--bead` is absent, the bead is resolved from `SPIRE_BEAD` or by
walking the working directory for a basename that matches the bead-ID
pattern.

### `spire cleric learn <bead>`

Persist the diagnosis and outcome into the cleric's knowledge base.

```bash
spire cleric learn <bead-id> [--notes <text>]
```

| Flag | Description |
|------|-------------|
| `--notes <text>` | Learning notes to record on the bead |

Records the cleric's learning notes on the bead and marks the cleric
state `finished`.

---

## Arbiter

Role-scoped commands for the **arbiter** — the role that resolves
disputes between sages and apprentices by issuing a binding verdict.

### `spire arbiter decide <bead> [--verdict accept|reject|custom]`

Record a binding arbiter verdict for a contested review round.

```bash
spire arbiter decide <bead-id> --verdict accept|reject|custom [--note <text>]
```

| Flag | Description |
|------|-------------|
| `--verdict` | Arbiter verdict: `accept`, `reject`, or `custom` (required) |
| `--note <text>` | Optional verdict reasoning/note |

The authoritative storage boundary for review verdicts is the
**review-round bead** — the same child bead a sage writes to. Arbiter
decisions are binding because they land on that round and close it; they
are not a parallel metadata channel on the task.

Writes an `arbiter_verdict` JSON payload (with `source="arbiter"` marker,
plus `verdict`, optional `note`, and `decided_at`) to the most recent
review-round child of `<bead-id>`. The matching plain `review_verdict`
key is mirrored on the same review-round so existing readers see the
arbiter's call without parsing the JSON. If the review-round is still
open, it is closed.

If the task has an active attempt bead (the current dispute round), the
attempt is closed with result `arbiter-resolved`. The sage CLI refuses
to write `accept`/`reject` on a round whose most-recent review carries
an `arbiter_verdict`, and downstream readers (board, steward) prefer the
arbiter verdict over any sage-written `review_verdict` on the same
review-round.

Errors out cleanly if `<bead-id>` has no review-round child — the
arbiter only resolves rounds a sage has already opened.

---

## Artificer

Role-scoped commands for the **artificer** — the role that authors,
validates, inspects, and publishes formulas. The artificer operates on
formula TOML files; it never drives live bead execution.

### `spire workshop`

Drop into the interactive formula workshop.

```bash
spire workshop
```

### `spire workshop list`

List available formulas.

```bash
spire workshop list [--custom] [--embedded] [--all] [--json]
```

| Flag | Description |
|------|-------------|
| `--custom` | Show only tower-published (disk) formulas |
| `--embedded` | Show only binary-embedded defaults |
| `--all` | Show every formula (default if no filter set) |
| `--json` | JSON output |

### `spire workshop show <name>`

Display formula details with the ASCII step-graph diagram.

```bash
spire workshop show <formula-name>
```

### `spire workshop validate <name>`

Validate a formula's syntax, structure, and semantics.

```bash
spire workshop validate <formula-name>
```

Exits non-zero if any errors are found. Warnings are reported but
non-fatal.

### `spire workshop compose`

Interactive formula builder for authoring v3 step-graph formulas.

```bash
spire workshop compose
```

### `spire workshop dry-run <name>`

Simulate formula execution without live side effects.

```bash
spire workshop dry-run <formula-name> [--json] [--bead <id>]
```

| Flag | Description |
|------|-------------|
| `--json` | JSON output |
| `--bead <id>` | Bead ID for context (accepted for backward compat) |

### `spire workshop test <name>`

Dry-run a formula against a real bead's full context.

```bash
spire workshop test <formula-name> --bead <id> [--json]
```

| Flag | Description |
|------|-------------|
| `--bead <id>` | Bead ID (required) |
| `--json` | JSON output |

Logs a note to stderr if the bead's resolved formula does not match
`<formula-name>` (you asked for one formula, the bead would normally use
another).

### `spire workshop publish <name>`

Copy a formula to the tower's `.beads/formulas/` so layered resolution
picks it up.

```bash
spire workshop publish <formula-name>
```

### `spire workshop unpublish <name>`

Remove a published formula; beads fall back to the embedded default.

```bash
spire workshop unpublish <formula-name>
```

---

## Common (multi-role reads / messaging)

These commands are available to every role. They are not role-scoped —
the scaffolder hook emits them alongside the role's own catalog. A
wizard, sage, cleric, arbiter, and apprentice can all run these.

### `spire focus <bead>`

Assemble full context for a bead (deps, messages, comments, workflow
molecule).

```bash
spire focus <bead-id>
```

Outputs: bead details, workflow progress, referenced design beads,
recent messages, comments. Pours the workflow molecule on first focus.

### `spire grok <bead>`

Deep focus that also pulls live integration context (e.g., Linear).

```bash
spire grok <bead-id>
```

Like `spire focus` but also fetches the linked Linear issue (requires
Linear integration).

### `spire send <agent> "msg" --ref <bead>`

Send a message to another agent, optionally referencing a bead.

```bash
spire send <to> "<message>" [--ref <bead-id>] [--thread <msg-id>] [--priority <0-4>]
```

`<to>` can be an agent name or bead ID. Messages are stored in the bead
graph and routed via labels (`to:<agent>`, `from:<agent>`).

### `spire collect`

Check the inbox for messages addressed to this agent.

```bash
spire collect [name]
```

Prints new messages addressed to `name` (or your registered identity if
omitted).

### `spire read <bead>`

Mark a message thread on a bead as read.

```bash
spire read <bead-id>
```

---

## Archmage (user / top-level)

These are top-level commands invoked by the human archmage (or by tools
acting on the archmage's behalf). They are not role-scoped: they set up
infrastructure, file work, operate on the tower, and observe the system.

### Setup

#### `spire tower create`

Create a new tower (shared workspace backed by Dolt).

```bash
spire tower create --name my-team [--dolthub org/repo] [--prefix spi]
```

#### `spire tower attach`

Join an existing tower from DoltHub.

```bash
spire tower attach <dolthub-url> [--name local-name]
```

#### `spire tower list`

List configured towers.

```bash
spire tower list
```

#### `spire tower use`

Set the active tower for subsequent commands.

```bash
spire tower use <name>
```

#### `spire tower remove`

Remove a tower.

```bash
spire tower remove <name> [--force]
```

#### `spire repo add`

Register a repo under the active tower.

```bash
spire repo add [path] [--prefix web] [--repo-url <url>] [--branch main]
```

#### `spire repo list`

List repos registered in the active tower.

```bash
spire repo list [--json]
```

#### `spire repo remove`

Remove a repo from the tower.

```bash
spire repo remove <prefix>
```

#### `spire config`

Read and write config values and credentials.

```bash
spire config set <key> <value>
spire config get <key> [--unmask]
spire config list
```

#### `spire doctor`

Health checks and auto-repair.

```bash
spire doctor [--fix]
```

### Sync

#### `spire push`

Push local database to DoltHub.

```bash
spire push [url]
```

#### `spire pull`

Pull from DoltHub (fast-forward by default, `--force` to overwrite).

```bash
spire pull [url] [--force]
```

### Lifecycle

#### `spire up`

Start the local control plane: dolt server, sync daemon, and steward.

```bash
spire up [--interval 2m] [--steward-interval 10s] [--no-steward] [--backend process|docker|k8s]
```

The daemon and steward have independent intervals:

- `--interval` (default `2m`) — daemon cadence; controls dolt push/pull, Linear sync, webhook processing, and OLAP ETL. Heavy work; raising it is fine, lowering it is wasteful.
- `--steward-interval` (default `10s`) — steward cadence; controls ready-bead dispatch, hooked sweep, orphan cleanup, and stale detection. Cheap local work; the default keeps ready→spawn latency under ~15s.

Pass `--no-steward` to start dolt + daemon only (sync-only / debug mode). The deprecated `--steward` flag is still accepted as a no-op for back-compat — the steward starts by default now. Scripts that pass only `--interval` keep working: that flag now affects the daemon alone, and the steward gets its 10s default.

#### `spire down`

Stop the sync daemon (dolt keeps running).

```bash
spire down
```

#### `spire shutdown`

Stop the sync daemon and dolt server.

```bash
spire shutdown
```

#### `spire status`

Show running services, agents, and work queue.

```bash
spire status
```

#### `spire board`

Interactive Kanban board TUI.

```bash
spire board [--mine] [--ready] [--epic <id>] [--json]
```

#### `spire logs`

Tail agent or system logs.

```bash
spire logs [wizard-name] [--daemon] [--dolt]
```

#### `spire metrics`

Agent run metrics and DORA metrics.

```bash
spire metrics [--bead <id>] [--model] [--json]
```

#### `spire watch`

Live-updating activity view.

```bash
spire watch [bead-id]
```

### Work filing and orchestration

#### `spire file`

Create a bead (work item).

```bash
spire file "<title>" [--prefix <p>] -t <type> -p <priority> [--parent <id>] [--design <id>] [--label <label>]
```

#### `spire design`

Create a design bead (brainstorming/exploration artifact).

```bash
spire design "<title>" [-p <priority>]
```

#### `spire summon`

Summon wizard agents to claim and work ready beads.

```bash
spire summon [n] [--targets <ids>] [--auto]
```

Each wizard runs as a local process in an isolated git worktree, driven
by the bead's formula.

#### `spire resummon`

Clear `needs-human` / timer labels and re-summon a wizard for a bead.

```bash
spire resummon <bead-id>
```

#### `spire ready`

Publish a bead so the cluster can pick it up.

```bash
spire ready <bead-id>
```

#### `spire reset`

Reset a bead's workflow state without destroying the bead.

```bash
spire reset <bead-id>
```

#### `spire update`

Update bead fields.

```bash
spire update <bead-id> [--title ...] [--status ...] [--priority <n>]
```

Prefer `spire update` over `bd update` for the same reason `spire file`
is preferred over `bd create`.

#### `spire close`

Force-close a bead (removes phase labels, closes molecule steps).

```bash
spire close <bead-id>
```

#### `spire approve`

Record an archmage-level approval on a bead.

```bash
spire approve <bead-id>
```

#### `spire resolve`

Resolve a bead decision point (e.g., surfaced by the cleric flow).

```bash
spire resolve <bead-id>
```

#### `spire review`

Manually enter the review flow for a bead.

```bash
spire review <bead-id>
```

### Messaging

#### `spire register`

Register an agent identity.

```bash
spire register <name>
```

#### `spire unregister`

Unregister an agent identity.

```bash
spire unregister <name>
```

#### `spire send`

Top-level message send (also available as a Common command; see that
section for the full signature).

```bash
spire send <to> "<message>" [--ref <bead-id>] [--thread <msg-id>] [--priority <0-4>]
```

#### `spire alert`

Alert on bead state changes (priority-tagged notification).

```bash
spire alert [bead-id] [--type <type>] [-p <priority>]
```

### Formulas (archmage surface)

#### `spire formula`

Formula convenience surface for the archmage. See `spire workshop` under
the Artificer section for the authoring/validation surface.

```bash
spire formula <subcommand> [args]
```

### Observability / debugging

#### `spire sql`

Open an interactive SQL prompt against the tower's Dolt database.

```bash
spire sql
```

#### `spire version`

Print version information.

```bash
spire version
```

### Infrastructure daemons

#### `spire daemon`

Run the sync daemon directly (without `spire up`).

```bash
spire daemon [--interval 2m] [--once]
```

#### `spire gateway`

Run the gateway service (webhook receiver and HTTP surface).

```bash
spire gateway [--port 8080]
```

### Internal (hidden) commands

#### `spire execute`

Run the full formula executor (spawned by `summon`).

```bash
spire execute <bead-id>
```

Internal — spawned by `summon` and the operator. Not part of the normal
archmage workflow; documented here for completeness.

#### `spire debug`

Hidden parent for archmage-only debugging tooling. Subcommands refuse
to operate against a non-debug tower: the active tower must have a
`debug-` name prefix, or its name must appear in the comma-separated
`SPIRE_DEBUG_TOWER` allowlist.

```bash
spire debug recovery <subcommand> [flags]
```

##### `spire debug recovery new`

Author a synthetic recovery bead that mirrors the shape a real
wizard/cleric escalation produces. Intended for exercising the cleric
end-to-end without reproducing a real failure. Prints the new bead's
ID to stdout so it composes with shell pipelines.

```bash
spire debug recovery new --origin <bead> --failure-class <class> \
  [--failed-step <step>] [--labels k=v,...] [--wisp]
```

| Flag | Description |
|------|-------------|
| `--origin <bead>` | Bead the synthetic recovery points at via a `caused-by` edge (required). Parent bead for a pour, or a pre-existing pinned identity bead when combined with `--wisp`. |
| `--failure-class <class>` | Simulated recovery `FailureClass` (required). One of: `empty-implement`, `merge-failure`, `build-failure`, `review-fix`, `repo-resolution`, `arbiter`, `step-failure`, `unknown`. |
| `--failed-step <step>` | Simulated failed-step hint; included in the `interrupted:failed-step=<step>` label and the `source_step` metadata. |
| `--labels k=v,...` | Extra labels, comma-separated, merged into `interrupted:<k>=<v>` unless the key already starts with `interrupted:` (in which case the prefix is preserved verbatim). |
| `--wisp` | Mark the bead as wisp-routed; records a `synthetic:wisp` provenance label. |

##### `spire debug recovery dispatch`

Run the `cleric-default` formula synchronously in the foreground
against an existing recovery bead. Emits one human-readable status
line per phase (`collect_context`, `decide`, `execute`, `verify`,
`learn`, `finish`) as they complete, followed by an `OUTCOME` summary
line. Refuses to run against a bead that is neither labeled
`recovery-bead` nor carries a `caused-by` / `recovery-for` edge.

```bash
spire debug recovery dispatch --bead <recovery-bead>
```

| Flag | Description |
|------|-------------|
| `--bead <recovery-bead>` | Recovery bead ID to dispatch (required). Typically the ID printed by `spire debug recovery new`. |

Exit codes:

| Code | Meaning |
|------|---------|
| `0` | Cleric finished with `decision=resume` (repair applied, source resumed). |
| `2` | Cleric finished with `decision=escalate` (durably persisted on the bead). |
| `1` | Infrastructure error — bead could not be loaded, guard rejected, or the cleric crashed before writing an outcome. |

##### `spire debug recovery trace`

Read the durable trace written by a completed cleric run: the decide
branch, repair mode/action, verify verdict, final decision, and any
related learnings. Works whether the recovery was driven by a real
wizard escalation or `spire debug recovery dispatch`.

```bash
spire debug recovery trace <recovery-bead> [--json]
```

| Flag | Description |
|------|-------------|
| `--json` | Emit the full trace as indented JSON instead of the text rendering. |

Positional argument: the recovery bead ID (exactly one). In text mode,
the rendering includes the source bead, failure class, reconstructed
decide branch, repair mode/action, any recipe reference, verify kind
and verdict, final decision, the learning-summary metadata, and up to
five related learning bead IDs.

---

## Cluster

Cluster-only commands invoked from inside cluster workloads (wizard pod
init containers, reconciler-managed Jobs, and other cluster-internal
entrypoints). These verbs are grouped under `spire cluster` to keep
cluster-internal surface out of the laptop/agent CLI — they do not
appear in any `SPIRE_ROLE` catalog and are not intended for interactive
use.

### `spire cluster cache-bootstrap`

Materialize a writable workspace from the guild repo cache and bind it
locally. Invoked by the wizard pod's `cache-bootstrap` init container
(see `operator/controllers/agent_monitor.go` `applyCacheOverlay`).

```bash
spire cluster cache-bootstrap [--cache-path <path>] [--workspace-path <path>] [--prefix <prefix>]
```

| Flag | Description |
|------|-------------|
| `--cache-path <path>` | Read-only guild cache mount path (default `pkg/agent.CacheMountPath`) |
| `--workspace-path <path>` | Writable workspace mount path (default `pkg/agent.WorkspaceMountPath`) |
| `--prefix <prefix>` | Canonical repo prefix (defaults to `$SPIRE_REPO_PREFIX`) |

Runs `agent.MaterializeWorkspaceFromCache(cache-path, workspace-path, prefix)`
to clone the read-only cache into a writable workspace, then
`agent.BindLocalRepo(workspace-path, prefix)` to register the checkout
in the tower's LocalBindings so `wizard.ResolveRepo` succeeds when the
main container starts.

---

## Deprecated

The verbs in this section continue to work but print a stderr
deprecation warning and will be hard-removed in **v1.0**. Prefer the
replacements listed below.

### `spire wizard-merge`

`[deprecated since v0.44.0, hard removal v1.0]`

```bash
spire wizard-merge
```

Use the new review flow instead. The wizard seals beads after merge via
`spire wizard seal <bead>`; merge orchestration itself runs inside the
wizard's formula — no separate `wizard-merge` verb is required.

### `spire wizard-epic`

`[deprecated since v0.44.0, hard removal v1.0]`

```bash
spire wizard-epic <epic-id>
```

Use the new review flow instead. Epic orchestration is now driven by
`spire summon` pulling from the ready queue, with per-attempt lifecycle
managed via `spire wizard claim` / `spire wizard seal`.
