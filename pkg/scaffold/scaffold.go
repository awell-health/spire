// Package scaffold defines the role-scoped CLI command catalogs that drive
// both scaffolder hooks and CLI documentation.
//
// This package is the single source of truth for the role-scoped CLI
// taxonomy (apprentice, wizard, sage, cleric, arbiter). Both
// .claude/spire-hook.sh (via cmd/spire/scaffolding.go) and
// docs/cli-reference.md pull from here so that the rendered command lists
// never drift from each other.
//
// scaffold MUST NOT import from cmd/spire/. Commands are listed as static
// strings — this package never inspects, dispatches, or executes them.
package scaffold

// Role identifies a Spire agent role with its own scoped command set.
type Role string

const (
	RoleApprentice Role = "apprentice"
	RoleWizard     Role = "wizard"
	RoleSage       Role = "sage"
	RoleCleric     Role = "cleric"
	RoleArbiter    Role = "arbiter"
)

// String returns the role's lower-case identifier.
func (r Role) String() string { return string(r) }

// Command is one CLI verb in a role's catalog.
//
// Name is the path after "spire" — e.g., "wizard claim", "apprentice
// submit", or "focus" for common verbs that have no role prefix. Args is
// the argument signature suffix (e.g., "<bead>", "[--bead <id>]"). When
// Args is empty the command takes no arguments. Description is the
// one-line summary used in hook output and markdown rendering.
type Command struct {
	Name        string
	Args        string
	Description string
}

// Catalog is one role's full command set: the role-scoped Commands plus
// the multi-role Common commands available to every role.
type Catalog struct {
	Role     Role
	Commands []Command
	Common   []Command
}
