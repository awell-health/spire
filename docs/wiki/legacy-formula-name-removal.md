# Legacy v2/v3 Formula Name Alias Removal

## What happened (spi-np2su9)

`pkg/formula/formula.go` carried a `legacyV2NameMap` (9 entries) plus a `translateLegacyName` wrapper that translated old formula names — `spire-agent-work`, `spire-bugfix`, `spire-epic`, plus `*-v3` variants and three sub-graph aliases (`spire-recovery-v3`, `review-phase`, `epic-implement-phase`) — into their canonical v3 equivalents (`task-default`, `bug-default`, `epic-default`, `cleric-default`, `subgraph-review`, `subgraph-implement`).

A live-bead audit (`bd list --json | jq -r '.[].labels[]?' | grep ^formula:`) confirmed zero beads referenced any of the 9 legacy names. The shim was dead.

Removed and updated across three layers:

- **Code:** Deleted `legacyV2NameMap` and `translateLegacyName` from `pkg/formula/formula.go`. Both call sites inside `ResolveV3Name` (label resolution and repo-config resolution) now return the value verbatim. The doc comment dropped the legacy-translation sentence.
- **Recovery guard:** Removed `"spire-recovery-v3"` from the recursion-guard switch in `pkg/executor/recovery_dispatch.go` and its corresponding test in `recovery_retry_merge_test.go`. With the alias map gone, `graphState.Formula` can never equal `"spire-recovery-v3"` — it resolves to `"cleric-default"`. The case was unreachable, not defense-in-depth.
- **TUI:** `cmd/spire/grok.go:88` previously printed a hardcoded `--- Workflow (spire-agent-work) ---` for every bead regardless of formula. Now uses `resolveFormulaName(target)` to display the actual resolved name.
- **Tests:** Swapped `spire-agent-work` → `task-default` across 6 test fixtures (`pkg/bd/`, `pkg/executor/`, `pkg/store/`, `cmd/spire/`). Deleted `TestResolveV3_LegacyNameTranslation` outright — with the alias map gone, swapping its label would have made it a duplicate of `TestResolveV3_ExplicitFormula` directly above.
- **User-facing strings/docs:** Updated `cmd/spire/embedded/SPIRE.md.tmpl` (shipped via `spire init`), `pkg/formula/README.md`, the comment in `pkg/formula/embedded/formulas/chore-default.formula.toml`, and 8 doc files (`CONTRIBUTING.md`, `PLAYBOOK.md`, `docs/agent-development.md`, `docs/getting-started.md`, `docs/spire-yaml.md`, `docs/epic-formula.md`, `docs/v3-formula.md`, `docs/review-v0.20.md`).

`docs/plans/` and `docs/superpowers/` were left untouched — they are historical archaeology that pre-dates the v3 rename, and rewriting them corrupts the archive.

## Why it matters

A backward-compat shim outlives its purpose the moment the last legacy name disappears from live data. Keeping it around:

- Lets new beads or repo configs sneak in legacy names that "just work" — perpetuating the old vocabulary instead of forcing a clean migration.
- Misleads readers into thinking those names are still semantically meaningful.
- Costs every reader of `ResolveV3Name` a wrapper hop to chase translation logic that no longer translates anything in practice.

User-facing docs are worse: every reference to `spire-agent-work` in a tutorial or template teaches new users the wrong name, even after all internal code has migrated.

## How to spot similar issues

1. **Audit live data before assuming a shim is in use.** A query like `bd list --json | jq -r '.[].labels[]?' | grep ^<prefix>:` confirms whether the legacy values still flow through the system. Zero hits = dead shim.
2. **Trace the wrapper to its callers.** If `translateLegacyName` is the only place a map is read and it's a pass-through for the canonical case, deleting both is mechanical.
3. **Grep beyond code.** Templates shipped to user repos (`cmd/spire/embedded/*.tmpl`), embedded formulas (`pkg/formula/embedded/`), READMEs, and tutorials are common places where legacy names persist after the code itself has moved on.
4. **Check downstream guards for alias-only branches.** A switch case like `case "cleric-default", "recovery", "spire-recovery-v3":` that only existed to catch alias-resolved values becomes unreachable when the aliases go. Distinguish dead defensive code (kept-but-noted) from dead unreachable code (removed); ask whether the value can still arrive via any path.

## Handling this kind of chore

- **Type:** `chore`. No behavior change for any input that wasn't already broken.
- **Scope discipline:** Stay narrow. Don't rename embedded formula files or drop deprecated APIs in the same pass — that widens into refactor territory and inflates the review surface. The bead's `Out of scope` section is the contract.
- **Acceptance grep:** A `grep` across `pkg/`, `cmd/`, `docs/`, and top-level docs for the legacy strings is the canonical "is it really gone?" check. Decide upfront which directories to exclude (here: `docs/plans/` and `docs/superpowers/` for archaeology) and document the exclusion.
- **Testing:** Compiler-driven — deleting the alias map breaks any tests that asserted translation behavior, which is a feature, not a bug. Run `go vet ./... && go build ./... && go test ./...`. No new tests needed; the chore is removal-only.
- **Review bar:** Low — verdict-only sage review is sufficient. The risk surface is limited to the audit grep returning clean and the test suite still passing.
