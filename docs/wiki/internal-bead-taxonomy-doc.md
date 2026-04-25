# Documenting the Internal Bead Taxonomy

## What happened (spi-oxe8rd)

Spire has four internal-only bead types — `message`, `step`, `attempt`,
`review` — defined in `pkg/store/internal_types.go` and gated by
`InternalTypes` / `IsWorkBead` / `IsInternalBead`. They are hidden from
the board, the steward queue, and the inspector, but the taxonomy was
nowhere in the docs. Agents and contributors had to read the predicate
code to learn what the four types modeled, who created them, and where
they got filtered out.

This chore added a canonical reference and three cross-links:

- **`docs/INTERNAL-BEADS.md`** (new) — taxonomy table, runtime bead
  graph, `IsWorkBead` invariant, every filter site, the legacy label
  fallback, and explicit agent guidance.
- **`CLAUDE.md`** — callout under "Filing work" pointing at the new doc,
  so agents reading project orientation hit the taxonomy before they
  consider hand-filing an internal type.
- **`docs/ARCHITECTURE.md`** — short paragraph in the "Beads / bd"
  section linking to the new doc, so readers walking the data model
  land there too.
- **`pkg/store/internal_types.go`** — doc comment on `InternalTypes`
  pointing at the doc, so readers tracing the type definition find the
  full picture without leaving their editor.

## Why it matters

The four types are programmatic-only by **convention**, not by any hard
guard in `spire file`. `parseIssueType` accepts `-t attempt` /
`-t step` / `-t review` / `-t message` — nothing prevents a confused
agent from filing one. The hiding behavior makes that confusion silent:
a hand-filed internal bead executes (the steward might dispatch it,
because top-level + non-internal predicate breaks down at the boundary),
but is invisible on the board, so the bug surfaces as "agents acting
weird" rather than "I see a stray bead."

Documenting the contract turns "convention" into something an agent can
read and follow. It also captures the filter map in one place, so a
future change that adds a new filter site has a checklist.

## When to create a new doc vs append

ARCHITECTURE.md was already 1120 lines. The taxonomy topic is bounded
(four types, one invariant, ~8 filter sites, one migration path), so it
fits cleanly as its own file.

Rule of thumb: **a topic that has its own predicate function or its own
table belongs in its own doc.** Topics that are one paragraph of context
on an existing system belong appended to the relevant existing doc.
ARCHITECTURE.md gets the short paragraph + link; INTERNAL-BEADS.md gets
the full treatment.

## Cross-linking strategy

Three landing points cover the realistic ways a reader hits this topic:

1. **`CLAUDE.md`** — agents reading project orientation. The "Filing
   work" section is where they decide what to file; that's the right
   moment to surface "these types exist but aren't for you."
2. **`docs/ARCHITECTURE.md`** — readers walking the data model. The
   "Beads / bd" section already covers the work-bead types, so the
   internal-type paragraph fits as a peer.
3. **Source-code doc comment** on `InternalTypes` — readers tracing the
   predicate from a code site. They are already in the file; a comment
   pointing at the doc costs nothing and saves them a search.

Don't add cross-links from every filter site. The doc lists the filter
sites; that direction is enough. Forward links from each filter site
back to the doc would be churn with no payoff.

## What goes in a taxonomy doc like this

Structure that worked here, in order:

1. **Header table** — one row per type, columns for "what it models",
   creator function, source file, label set. The table is the doc's
   reason for existing; everything else is supporting material.
2. **Runtime diagram** — show the bead graph during a wizard run.
   Abstract types become concrete when you see them as children of a
   parent work bead. ASCII tree is enough; no fancy rendering needed.
3. **The invariant** — quote the predicate verbatim. Readers who came
   from a `IsWorkBead(...)` call site need to confirm the doc matches
   the code.
4. **Filter sites table** — every place the invariant is applied, with
   a one-line "what it filters." Make this exhaustive: it's the
   checklist a future change uses.
5. **Legacy fallback** — if the system has a migration path
   (`labelToType` + `MigrateInternalTypes`), document it. Old data is
   real and the predicates handle it; the doc must too.
6. **The contract** — explicitly state what's enforced and what's
   convention. "There is no hard guard in `spire file`" is the kind of
   detail readers can't infer from the predicate alone.
7. **Agent guidance** — the "what to do" section. Short. Three
   imperatives are enough: don't file these, treat top-level instances
   as bug indicators, use the predicates not the labels.

## File paths, not line numbers

The research summary on the bead used `pkg/store/internal_types.go:11-16`
style references. The final doc uses **file paths only** (rendered as
markdown links). Line numbers rot the moment someone adds a comment or
reorders a function; file-path links keep working as long as the file
exists. The research comment is for the implementer and is allowed to
rot; the published doc is for readers and shouldn't.

If a function is hard to find inside its file, name it (`See
labelToType in ...`) — the reader can grep. Don't pin to a line.

## Handling this kind of chore

- **Type:** `chore`. Pure documentation; no code behavior changes.
- **Skip the research phase if you wrote the comment yourself.** This
  chore had a complete research comment from the archmage with file
  paths and the recommended structure. The wizard's research phase
  (`wizard-spi-oxe8rd-research-1 finished without changes`) was a no-op
  — the work was done in the bead comment. Don't duplicate it.
- **Scope discipline:** Don't sneak in code changes ("while I'm here, I
  noticed `IsWorkBead` could also drop hooked beads..."). The chore is
  documentation. Code refactors get their own bead.
- **Testing:** None. `go build` confirms the source comment compiles;
  the rest is markdown.
- **Review bar:** Verdict-only sage review is sufficient. Reviewer
  checks: (a) the doc matches the predicate code, (b) every claimed
  filter site exists, (c) cross-links are placed in sections readers
  will actually visit.
- **Commit:** A single `docs(<bead>): ...` commit covering the new doc
  + cross-links + source comment. One unit, one commit.
