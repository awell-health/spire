package scaffold

import (
	"fmt"
	"sort"
	"strings"
)

// GetCatalog returns the catalog for a given role. Returns an error if
// the role is not one of the known constants.
func GetCatalog(role Role) (*Catalog, error) {
	cat, ok := catalogs[role]
	if !ok {
		return nil, fmt.Errorf("unknown role %q", role)
	}
	return cat, nil
}

// KnownRoles returns the supported roles in stable alphabetical order.
func KnownRoles() []Role {
	out := make([]Role, 0, len(catalogs))
	for r := range catalogs {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// RenderHookInstructions emits bash-safe plaintext suitable for
// .claude/spire-hook.sh to echo on SubagentStart. Output includes:
//   - the role name
//   - the role-scoped commands with signatures + 1-line descriptions
//   - the multi-role common commands
//   - the commit-message convention reminder
//
// Other roles' role-scoped commands are NEVER listed — the cross-role
// isolation invariant is enforced by tests in scaffold_test.go.
func RenderHookInstructions(role Role) (string, error) {
	cat, err := GetCatalog(role)
	if err != nil {
		return "", err
	}

	var b strings.Builder
	fmt.Fprintf(&b, "# Spire role: %s\n\n", cat.Role)
	fmt.Fprintf(&b, "You are operating as a %s. Use only the commands listed below.\n\n", cat.Role)

	fmt.Fprintf(&b, "## %s commands\n\n", titleCase(string(cat.Role)))
	for _, c := range cat.Commands {
		writeHookCommand(&b, c)
	}

	fmt.Fprintf(&b, "\n## Common commands (available to every role)\n\n")
	for _, c := range cat.Common {
		writeHookCommand(&b, c)
	}

	fmt.Fprintf(&b, "\n## Commit format\n\n")
	fmt.Fprintf(&b, "Always reference the bead in commit messages:\n\n")
	fmt.Fprintf(&b, "    <type>(<bead-id>): <msg>\n\n")
	fmt.Fprintf(&b, "Types: feat, fix, chore, docs, refactor, test\n")

	return b.String(), nil
}

// RenderMarkdown emits docs-ready markdown for the role's section in
// docs/cli-reference.md. The output covers only the role-scoped commands;
// the common multi-role commands are rendered in their own section of
// the cli-reference and callers iterate CommonCommands directly for
// that purpose.
func RenderMarkdown(role Role) (string, error) {
	cat, err := GetCatalog(role)
	if err != nil {
		return "", err
	}

	var b strings.Builder
	fmt.Fprintf(&b, "## %s\n\n", titleCase(string(cat.Role)))
	for _, c := range cat.Commands {
		writeMarkdownCommand(&b, c)
	}
	return b.String(), nil
}

// writeHookCommand renders a single command into the hook output.
// Format:
//
//	  spire <Name> <Args>
//	      <Description>
func writeHookCommand(b *strings.Builder, c Command) {
	if c.Args != "" {
		fmt.Fprintf(b, "  spire %s %s\n", c.Name, c.Args)
	} else {
		fmt.Fprintf(b, "  spire %s\n", c.Name)
	}
	fmt.Fprintf(b, "      %s\n", c.Description)
}

// writeMarkdownCommand renders one command as a markdown subsection: a
// fenced code block for the signature line, followed by the description
// paragraph.
func writeMarkdownCommand(b *strings.Builder, c Command) {
	if c.Args != "" {
		fmt.Fprintf(b, "### `spire %s %s`\n\n", c.Name, c.Args)
	} else {
		fmt.Fprintf(b, "### `spire %s`\n\n", c.Name)
	}
	fmt.Fprintf(b, "%s\n\n", c.Description)
}

// titleCase capitalizes the first ASCII byte of s. The role identifiers
// in this package are all ASCII, so byte slicing is safe.
func titleCase(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
