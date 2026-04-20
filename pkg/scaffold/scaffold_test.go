package scaffold

import (
	"strings"
	"testing"
)

func TestGetCatalog_AllKnownRoles(t *testing.T) {
	tests := []struct {
		role         Role
		commandCount int
		commandNames []string
	}{
		{RoleApprentice, 1, []string{"apprentice submit"}},
		{RoleWizard, 2, []string{"wizard claim", "wizard seal"}},
		{RoleSage, 2, []string{"sage accept", "sage reject"}},
		{RoleCleric, 3, []string{"cleric diagnose", "cleric execute", "cleric learn"}},
		{RoleArbiter, 1, []string{"arbiter decide"}},
	}
	for _, tt := range tests {
		t.Run(string(tt.role), func(t *testing.T) {
			cat, err := GetCatalog(tt.role)
			if err != nil {
				t.Fatalf("GetCatalog(%q): %v", tt.role, err)
			}
			if cat.Role != tt.role {
				t.Errorf("Role = %q, want %q", cat.Role, tt.role)
			}
			if len(cat.Commands) != tt.commandCount {
				t.Errorf("len(Commands) = %d, want %d (got %v)", len(cat.Commands), tt.commandCount, cat.Commands)
			}
			gotNames := commandNames(cat.Commands)
			for _, want := range tt.commandNames {
				if !contains(gotNames, want) {
					t.Errorf("Commands missing %q, got %v", want, gotNames)
				}
			}
			if len(cat.Common) != len(CommonCommands) {
				t.Errorf("len(Common) = %d, want %d", len(cat.Common), len(CommonCommands))
			}
		})
	}
}

func TestGetCatalog_UnknownRoleReturnsError(t *testing.T) {
	if _, err := GetCatalog(Role("ghost")); err == nil {
		t.Fatal("expected error for unknown role, got nil")
	}
}

func TestRenderHookInstructions_WizardListsWizardVerbsOnly(t *testing.T) {
	out := mustRender(t, RoleWizard)
	mustContain(t, out, "wizard claim")
	mustContain(t, out, "wizard seal")
	mustNotContain(t, out, "sage accept")
	mustNotContain(t, out, "sage reject")
	mustNotContain(t, out, "apprentice submit")
	mustNotContain(t, out, "cleric diagnose")
	mustNotContain(t, out, "arbiter decide")
}

func TestRenderHookInstructions_ApprenticeIsolation(t *testing.T) {
	out := mustRender(t, RoleApprentice)
	mustContain(t, out, "apprentice submit")
	mustNotContain(t, out, "sage accept")
	mustNotContain(t, out, "sage reject")
	mustNotContain(t, out, "wizard claim")
	mustNotContain(t, out, "wizard seal")
	mustNotContain(t, out, "cleric diagnose")
	mustNotContain(t, out, "arbiter decide")
}

func TestRenderHookInstructions_SageIsolation(t *testing.T) {
	out := mustRender(t, RoleSage)
	mustContain(t, out, "sage accept")
	mustContain(t, out, "sage reject")
	mustNotContain(t, out, "wizard claim")
	mustNotContain(t, out, "wizard seal")
	mustNotContain(t, out, "apprentice submit")
	mustNotContain(t, out, "cleric diagnose")
	mustNotContain(t, out, "arbiter decide")
}

func TestRenderHookInstructions_ClericIsolation(t *testing.T) {
	out := mustRender(t, RoleCleric)
	mustContain(t, out, "cleric diagnose")
	mustContain(t, out, "cleric execute")
	mustContain(t, out, "cleric learn")
	mustNotContain(t, out, "wizard claim")
	mustNotContain(t, out, "sage accept")
	mustNotContain(t, out, "apprentice submit")
	mustNotContain(t, out, "arbiter decide")
}

func TestRenderHookInstructions_ArbiterIsolation(t *testing.T) {
	out := mustRender(t, RoleArbiter)
	mustContain(t, out, "arbiter decide")
	mustNotContain(t, out, "wizard claim")
	mustNotContain(t, out, "wizard seal")
	mustNotContain(t, out, "sage accept")
	mustNotContain(t, out, "apprentice submit")
	mustNotContain(t, out, "cleric diagnose")
}

func TestRenderHookInstructions_IncludesCommonCommands(t *testing.T) {
	for _, r := range KnownRoles() {
		out := mustRender(t, r)
		for _, c := range CommonCommands {
			if !strings.Contains(out, "spire "+c.Name) {
				t.Errorf("role %q hook output missing common command %q", r, c.Name)
			}
		}
	}
}

func TestRenderHookInstructions_IncludesCommitConvention(t *testing.T) {
	out := mustRender(t, RoleApprentice)
	mustContain(t, out, "<type>(<bead-id>): <msg>")
}

func TestRenderHookInstructions_IncludesRoleHeader(t *testing.T) {
	for _, r := range KnownRoles() {
		out := mustRender(t, r)
		if !strings.Contains(out, "Spire role: "+string(r)) {
			t.Errorf("role %q hook output missing role header", r)
		}
	}
}

func TestRenderHookInstructions_UnknownRoleReturnsError(t *testing.T) {
	if _, err := RenderHookInstructions(Role("ghost")); err == nil {
		t.Fatal("expected error for unknown role, got nil")
	}
}

func TestRenderMarkdown_WizardSection(t *testing.T) {
	out, err := RenderMarkdown(RoleWizard)
	if err != nil {
		t.Fatalf("RenderMarkdown(wizard): %v", err)
	}
	mustContain(t, out, "## Wizard")
	mustContain(t, out, "`spire wizard claim <bead>`")
	mustContain(t, out, "`spire wizard seal <bead> [--merge-commit <sha>]`")
	mustNotContain(t, out, "sage accept")
	mustNotContain(t, out, "apprentice submit")
}

func TestRenderMarkdown_AllRolesProduceSectionHeader(t *testing.T) {
	for _, r := range KnownRoles() {
		out, err := RenderMarkdown(r)
		if err != nil {
			t.Errorf("RenderMarkdown(%q): %v", r, err)
			continue
		}
		if !strings.HasPrefix(out, "## ") {
			t.Errorf("role %q markdown missing leading section header: %q", r, firstLine(out))
		}
	}
}

func TestRenderMarkdown_UnknownRoleReturnsError(t *testing.T) {
	if _, err := RenderMarkdown(Role("ghost")); err == nil {
		t.Fatal("expected error for unknown role, got nil")
	}
}

func TestKnownRoles_StableAlphabeticalOrder(t *testing.T) {
	got := KnownRoles()
	want := []Role{RoleApprentice, RoleArbiter, RoleCleric, RoleSage, RoleWizard}
	if len(got) != len(want) {
		t.Fatalf("len(KnownRoles) = %d, want %d", len(got), len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("KnownRoles[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestCatalog_RoleFieldMatchesMapKey(t *testing.T) {
	for r, cat := range catalogs {
		if cat.Role != r {
			t.Errorf("catalogs[%q].Role = %q, mismatch", r, cat.Role)
		}
	}
}

func TestCommonCommands_HasExpectedVerbs(t *testing.T) {
	want := []string{"focus", "grok", "send", "collect", "read"}
	got := commandNames(CommonCommands)
	if len(got) != len(want) {
		t.Fatalf("len(CommonCommands) = %d, want %d (got %v)", len(got), len(want), got)
	}
	for _, w := range want {
		if !contains(got, w) {
			t.Errorf("CommonCommands missing %q (got %v)", w, got)
		}
	}
}

// helpers

func mustRender(t *testing.T, r Role) string {
	t.Helper()
	out, err := RenderHookInstructions(r)
	if err != nil {
		t.Fatalf("RenderHookInstructions(%q): %v", r, err)
	}
	return out
}

func mustContain(t *testing.T, s, sub string) {
	t.Helper()
	if !strings.Contains(s, sub) {
		t.Errorf("output missing %q\n--- output ---\n%s", sub, s)
	}
}

func mustNotContain(t *testing.T, s, sub string) {
	t.Helper()
	if strings.Contains(s, sub) {
		t.Errorf("output unexpectedly contains %q\n--- output ---\n%s", sub, s)
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

func commandNames(cs []Command) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.Name
	}
	return out
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
