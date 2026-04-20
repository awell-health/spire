# pkg/scaffold

Role-scoped CLI command catalogs — the single source of truth for both
scaffolder hook output and `docs/cli-reference.md`.

## What this package owns

- **Role catalog data** (`catalog.go`): the per-role command list for each
  of the five agent roles (apprentice, wizard, sage, cleric, arbiter)
  plus the multi-role common commands available to every role.
- **Lookup** (`GetCatalog`): retrieves a role's catalog by `Role`.
- **Rendering**:
  - `RenderHookInstructions(role)` — bash-safe plaintext for
    `.claude/spire-hook.sh` to echo on `SubagentStart`.
  - `RenderMarkdown(role)` — role-section markdown for
    `docs/cli-reference.md`.
- **Cross-role isolation invariant**: each role's rendered output lists
  only that role's scoped commands plus the common verbs — never another
  role's role-scoped commands. Tests in `scaffold_test.go` enforce this.

## What this package does NOT own

- **Command execution.** Commands are static strings. `pkg/scaffold` does
  not inspect, dispatch, or execute them — `cmd/spire/<role>.go` owns
  cobra wiring and execution.
- **Hook installation or scaffolder file IO.** `cmd/spire/scaffolding.go`
  consumes the rendered output and writes the hook script.
- **Documentation file IO.** Doc-generation tasks consume `RenderMarkdown`
  output to update `docs/cli-reference.md`.
- **Role assignment.** The agent spawner sets `SPIRE_ROLE`; the hook
  reads it and looks up the catalog. This package only renders by role.

## Import boundary

This package MUST NOT import from `cmd/spire/`. Both
`cmd/spire/scaffolding.go` (hooks) and the docs-generation path import
from `pkg/scaffold` — never the other way. Doing so would create a cycle
risk and put CLI wiring concerns into a leaf data package.

## Key types

| Type | Purpose |
|------|---------|
| `Role` | Identifies an agent role. Constants: `RoleApprentice`, `RoleWizard`, `RoleSage`, `RoleCleric`, `RoleArbiter`. |
| `Command` | One CLI verb: `Name` (path after `spire`), `Args` (signature suffix), `Description` (one-line summary). |
| `Catalog` | A role's command set: role-scoped `Commands` plus the multi-role `Common` list. |

## Key entry points

| Function / value | Purpose |
|------------------|---------|
| `GetCatalog(role)` | Returns the catalog for a role; errors on unknown role. |
| `KnownRoles()` | Lists all supported roles in stable alphabetical order. |
| `RenderHookInstructions(role)` | Plaintext hook output for `SubagentStart`. |
| `RenderMarkdown(role)` | Markdown section for `docs/cli-reference.md` (role-scoped commands only). |
| `CommonCommands` | The multi-role common verbs (`focus`, `grok`, `send`, `collect`, `read`). |

## Practical rules

1. **The catalog is the source of truth.** If a CLI implementation in
   `cmd/spire/*.go` disagrees with the catalog, the implementation is
   wrong — fix the implementation, not the catalog.
2. **Cross-role isolation is non-negotiable.** A wizard's session must
   never see `sage accept` or `apprentice submit` in its hook output.
   Tests enforce this for every role pair.
3. **Keep the data declarative.** No reflection, no dynamic discovery.
   Adding a role means adding a `Role` constant and a `catalogs` entry.
4. **No imports from `cmd/spire`.** This package is a leaf for the CLI
   layer.

## Where new work usually belongs

- Add it to **`pkg/scaffold`** when adding a new role-scoped verb, a new
  role, or a change to how roles are rendered for hooks/docs.
- Add it to **`cmd/spire/scaffolding.go`** when changing how hook scripts
  are written or installed.
- Add it to **`cmd/spire/<role>.go`** when wiring a catalog entry into a
  cobra command.
- Add it to **`docs/cli-reference.md`** rendering when changing the
  markdown shape consumed for the CLI reference.
