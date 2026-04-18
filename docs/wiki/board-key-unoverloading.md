# Board Key Unoverloading & Label-Aware Action Menu

## What happened (spi-0dfl4)

The `y` key in the board TUI had three context-dependent meanings:

- **Inspector mode** on a design bead with `needs-human` → opened
  confirm for `ActionApproveDesign`.
- **Board mode** on a hooked bead → dispatched `ActionResume`.
- **Otherwise** → copied the selected bead ID to the clipboard (yank).

That overloading made `y` unpredictable — pressing it to yank could
silently trigger an approve-confirm on a bead that happened to match
the hidden label conditions. The fix reverts `y` to a single meaning
(yank, always) and moves the branched behaviors into the action menu
(`a` opens the menu, then `y` invokes the context-appropriate approve).

Key changes:

- **`pkg/board/tui.go`** — deleted the `isReviewableDesign` branch from
  the inspector `y` handler and the `hooked → ActionResume` branch from
  the board-mode `y` handler. Both paths now fall straight through to
  yank.
- **`pkg/board/action_menu.go`** — appended a context-sensitive approve
  entry bound to `y` after the status-based switch. Precedence:
  1. `awaiting-approval` → `ActionApproveGate` (gate wins over plain
     needs-human because the steward sets both labels together).
  2. design bead + `needs-human` → `ActionApproveDesign`.
  3. `needs-human` (non-design) → `ActionApprove`.
- **`pkg/board/render.go`** — footer hints at `ViewLower` and
  `ViewBoard` now read `y yank` instead of `y approve`.
- **`pkg/board/board_test.go`** — added per-label-combo coverage and
  updated the prior "labels do not affect action menu" test, which
  directly contradicted the new behavior.

Resume (previously overloaded onto `y`) stays reachable via `S` in the
action menu for hooked beads — no capability lost.

## Why it matters

One key per action is the floor of TUI predictability. When a key's
meaning depends on invisible state (labels, mode, bead type), muscle
memory becomes a liability: the user presses `y` expecting yank and
gets a confirm dialog on a destructive action. Moving disambiguation
into the action menu keeps the hot keys safe and shifts discovery to
the menu, which is the layer users already consult when they don't
know what's possible.

The action menu was already the right home — `dangerForAction` and
`confirmPromptForAction` (tui.go) already knew about `ActionApprove`,
`ActionApproveDesign`, and `ActionApproveGate`, and
`cmd/spire/board.go` already wired their handlers. The only missing
piece was the menu entry itself.

## The label-aware menu entry pattern

`BuildActionMenu` (pkg/board/action_menu.go) routes purely by
`bead.Status` in its main switch. Label-aware entries belong **after**
the switch, appended in the same style as the "Always available"
grok/trace entries. This keeps the status switch readable and makes
the label rules easy to find in one block:

```go
// After the status switch, before the "Always available" append.
if hasLabel(bead, "awaiting-approval") {
    actions = append(actions, MenuAction{
        Key: "'y'", Label: "Approve gate",
        Danger: DangerConfirm, ActionType: ActionApproveGate,
    })
} else if hasLabel(bead, "needs-human") {
    if bead.IssueType == "design" {
        actions = append(actions, MenuAction{
            Key: "'y'", Label: "Approve design",
            Danger: DangerConfirm, ActionType: ActionApproveDesign,
        })
    } else {
        actions = append(actions, MenuAction{
            Key: "'y'", Label: "Approve",
            Danger: DangerConfirm, ActionType: ActionApprove,
        })
    }
}
```

Precedence matters: check `awaiting-approval` first, because the
steward's `human.approve` hook sets both `awaiting-approval` and
`needs-human` together. Checking `needs-human` first would misroute
gate approvals to `ActionApprove`.

## How to spot similar keybinding smells

1. **Grep for `case 'x':` with nested `if` branches on bead state.**
   A top-level key handler that branches on labels, issue type, or
   status is usually one key doing three jobs. Good candidates for
   menu migration.
2. **Read the footer hints.** If the hint text reads `y approve` but
   `y` also yanks, the key is overloaded. The hint is the user's
   mental model — keep it honest or fix the code.
3. **Check `isInlineAction` allowlists.** Actions that require
   confirmation (`DangerConfirm`) should almost never be bound to a
   single unprefixed key, especially a commonly-pressed one like `y`.
   The action menu's `a → <key>` path makes the guard explicit.

## Handling this kind of chore

- **Type:** `chore`. Pure behavior unoverloading — no new features,
  no new wiring (handlers already existed).
- **Test-location pitfall:** Task descriptions sometimes reference
  test files that don't exist (`action_menu_test.go`,
  `tui_test.go` in this case). Check first — `BuildActionMenu`
  coverage actually lives in `pkg/board/board_test.go`. Don't create
  new test files when the existing one is the canonical home.
- **Break pre-existing negative tests.** A test like "labels do not
  affect action menu" **must** be updated as part of a change that
  starts making labels affect the action menu. Leaving the old
  assertion as a TODO produces a compilation pass with a semantic
  regression.
- **Footer hints count as part of the change.** The hint is code-
  adjacent documentation; if it disagrees with the new behavior after
  the patch lands, the patch is incomplete.
- **Review bar:** Verdict-only sage review is typical. The risk is
  low (removing branches, adding menu entries) and the menu-side
  tests cover the new surface. A direct TUI `Update()` test for the
  removed branches is a nice-to-have but non-blocking — the branches
  are gone, so regression would require someone re-adding them
  deliberately.
