# Spire: The Product Engineer's Operating System

**Date**: 2026-03-19
**Status**: Vision document

## The shift

You're a product engineer. Six months ago, you wrote code all day. Now you
write specs, review PRs, and make decisions. Your agents write the code.

You didn't become a manager. You became a **technical director**. You still
understand every line of code. You still catch bugs in review. You still
make the architecture calls. But your hands aren't on the keyboard for
implementation anymore — they're on the steering wheel.

Your day looks like this:

```
9:00  Open spire. See what happened overnight.
      Three PRs landed. Two are green. One has a failing test.
      The failing one: agent misunderstood the spec. Send it back
      with a one-line correction.

9:15  Check the queue. Five beads ready. Skim them.
      Reorder two — the auth fix is more urgent than the UI polish.
      An agent claims the auth fix within a minute.

9:30  Review the two green PRs. One is clean. Merge.
      The other: correct but over-engineered. Add a comment:
      "simpler — just use the existing helper."
      Agent revises in 4 minutes. Merge.

10:00 Write a spec for the new onboarding flow. Takes an hour.
      File it as an epic. Break it into three beads.
      Agents claim them as you file them.

11:00 Check progress. Two of three are in PR already.
      The third is stuck — agent is going in circles on a
      test fixture. Intervene: "use the factory from test/helpers."
      Unstuck in 2 minutes.

11:30 All three PRs up. Review. One needs a design tweak.
      Send back with a sketch. The other two: merge.

12:00 Lunch. Agents keep working on the backlog.

1:00  Four new PRs. Review cycle. Merge three, send one back.

2:00  Write specs for next week's work. File beads.
      Triage incoming bugs from Linear — two are real, one is noise.
      Close the noise. Agents claim the real ones.

3:00  Deep review of a complex PR. This one matters.
      Read every line. Leave detailed comments on the tricky parts.
      Agent addresses all of them. Merge.

4:00  Check the dashboard. 11 PRs merged today. 2 still in review.
      Zero incidents. Test coverage up 3%.
      Write a quick note about tomorrow's priorities. Done.
```

You shipped 11 changes today. You wrote zero implementation code.
You wrote two specs and one sketch. The rest was review, prioritization,
and occasional course correction.

This is 10x, but it's not magic. It's the same engineering judgment
you've always had, applied at a different level.

## What you need

### 1. A queue you can see and shape

Not a kanban board. Not a spreadsheet. A **living queue** that shows you:

- What's ready to work on, in priority order
- What's in progress and who's working on it
- What's blocked and why
- What's in review, waiting for you

You should be able to reorder, reprioritize, and reassign from the CLI.
The queue is the heartbeat of your day.

```bash
spire board                    # the full picture
spire board --mine             # just things waiting for my review
spire board --agents           # what are agents doing right now
```

### 2. Specs as the primary artifact

Your most important output is no longer code — it's **specs**.

A good spec is the difference between an agent that nails it on the first
try and one that goes in circles for an hour. The spec is your leverage.

Spire should make spec-writing fast and first-class:

```bash
spire spec "New onboarding flow"       # scaffold a spec from a template
spire spec --from-linear LIN-123       # pull context from Linear
spire spec --break spi-xyz             # break a spec into beads
```

When you file an epic with a spec attached, agents should be able to
read the spec, understand the intent, and execute without asking
clarifying questions.

### 3. Review as the bottleneck you optimize for

Review is the human's primary job. Everything else should be designed
to make review faster and more effective.

What slows review down:
- Large PRs with no context
- Not knowing what the agent was trying to do
- Having to re-read the spec to understand the PR
- Agents that don't address review comments well

What spire should do:
- PRs link back to the bead, which links to the spec
- Agent includes a summary of what it did and why
- Review comments go back to the agent as actionable corrections
- Agent revisions are fast because context is preserved

```bash
spire review                   # show PRs waiting for review
spire review spi-xyz           # review the PR for this bead
spire revise spi-xyz "use the existing helper, don't create a new one"
```

### 4. Visibility without micromanagement

You need to know what's happening without having to check constantly.

**Dashboard** — a terminal UI or web view showing:
- Agent activity (who's working on what, for how long)
- PR pipeline (filed → in review → merged)
- Quality signals (test pass rate, review rejection rate)
- Velocity (beads closed per day, time from filed to merged)

**Alerts** — push notifications for things that need you:
- PR ready for review
- Agent stuck (in_progress too long with no commits)
- Test failure on a PR
- Agent asking a question

```bash
spire status                   # quick check
spire dashboard                # full terminal UI
spire watch                    # live stream of events
```

### 5. The ability to steer

Sometimes agents go wrong. You need to intervene cheaply.

- **Course correct**: "not that approach, try this instead"
- **Unblock**: "use the factory in test/helpers"
- **Abort**: "stop, this isn't what I wanted, let me rewrite the spec"
- **Redirect**: "deprioritize this, work on the auth fix first"

These should be one-liners. The cost of intervention should be
30 seconds, not 30 minutes.

```bash
spire steer spi-xyz "use the REST API, not GraphQL"
spire stop spi-xyz
spire reprioritize spi-xyz -p 0
```

### 6. Trust through transparency

You're delegating execution to machines. Trust is earned through:

- **Audit trail**: every bead has a full history of what happened
- **Diffs you can read**: agents produce clean, focused PRs
- **Test results**: automated quality gates before review
- **Patterns over time**: which agents are reliable, which need guidance

The system should make it easy to spot when things are going well
and when they're not.

## What spire is NOT

- **Not an IDE**. Agents use whatever tools they need. Spire coordinates.
- **Not a CI system**. GitHub Actions / whatever runs tests. Spire reads results.
- **Not a code review tool**. GitHub PRs are fine. Spire connects them to beads.
- **Not a chat interface**. You're not chatting with agents. You're directing work.

Spire is the **coordination layer** between you and your agents.
It's where work is defined, assigned, tracked, and reviewed.

## The architecture, simplified

```
You (product engineer)
  │
  ├── write specs
  ├── file beads
  ├── review PRs
  └── steer agents
  │
  ▼
┌─────────────────────┐
│  spire               │
│  (CLI + daemon +     │
│   optional k8s)      │
│                      │
│  - work queue        │
│  - agent registry    │
│  - assignment engine  │
│  - review pipeline   │
│  - visibility layer  │
└──────────┬──────────┘
           │
    ┌──────┼──────┐
    ▼      ▼      ▼
  agent   agent   agent
  (local, CI, or managed)
```

All state lives in two places:
- **Beads (Dolt)** — work items, assignments, messages, audit trail
- **Git** — code, specs, PRs, reviews

No web app required. No SaaS dependency. Everything works from the terminal.
Everything is version controlled. Everything is portable.

## Open source strategy

Spire should be useful to anyone who works with AI coding agents,
regardless of which agent they use or which platform they're on.

**Core (MIT or Apache 2.0):**
- `spire` CLI (init, up, down, status, file, claim, focus, collect, send)
- `spire mayor` (standalone work coordinator)
- Beads integration (bd commands for work tracking)
- Claude Code hooks (SessionStart, PostCompact, SubagentStart)
- Spec templates and workflow molecules

**Integrations (same license):**
- Linear sync (daemon)
- GitHub PR linking
- Cursor rules and MCP server

**Operator (same license):**
- CRDs (SpireAgent, SpireWorkload, SpireConfig)
- Kubernetes operator
- Helm chart

**What's NOT open source:**
- Nothing. All of it is open.

The bet: the coordination layer is more valuable when it's ubiquitous
than when it's proprietary. If every team using AI agents adopts spire,
the ecosystem effects (shared workflows, community specs, agent
interoperability) are worth more than any licensing revenue.

## Design principles

1. **CLI-first**. If it can't be done from the terminal, it's not a feature.
   Web UIs are optional views, not primary interfaces.

2. **Git-native**. Work state lives in Dolt (which is git for databases).
   Specs and code live in git. Nothing is locked in a SaaS.

3. **Agent-agnostic**. Spire works with Claude, GPT, Gemini, local models.
   The hooks and MCP server are Claude-specific but the core protocol isn't.

4. **Human-in-the-loop by default**. Agents propose, humans approve.
   The review step is not optional — it's the product.

5. **Small, composable tools**. `spire` is a single binary. It doesn't replace
   git, GitHub, or your CI system. It connects them.

6. **Transparency over magic**. You can always see what the system is doing
   and why. No black boxes. State is inspectable. History is auditable.
