package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/awell-health/spire/pkg/scaffold"
)

func TestInstallSpireSkills_InstallsBundledSkillEverywhere(t *testing.T) {
	home := t.TempDir()
	codexHome := filepath.Join(home, "codex-home")
	repo := filepath.Join(home, "repo")
	claudeDir := filepath.Join(repo, ".claude")

	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", codexHome)

	if err := os.MkdirAll(filepath.Join(home, ".claude", "skills", "spire-work"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".claude", "skills", "spire-work", "SKILL.md"), []byte("legacy"), 0644); err != nil {
		t.Fatal(err)
	}

	installSpireSkills(claudeDir)

	assertFileContains(t, filepath.Join(home, ".claude", "skills", "spire-conflicts", "SKILL.md"), "dolt sql")
	assertFileContains(t, filepath.Join(claudeDir, "skills", "spire-conflicts", "SKILL.md"), "dolt sql")
	assertFileContains(t, filepath.Join(claudeDir, "skills", "spire-conflicts", "agents", "openai.yaml"), "display_name: \"Spire Conflicts\"")
	assertFileContains(t, filepath.Join(codexHome, "skills", "spire-conflicts", "SKILL.md"), "dolt sql")

	assertFileContains(t, filepath.Join(home, ".claude", "skills", "spire-design", "SKILL.md"), "design bead")
	assertFileContains(t, filepath.Join(claudeDir, "skills", "spire-design", "SKILL.md"), "design bead")
	assertFileContains(t, filepath.Join(codexHome, "skills", "spire-design", "SKILL.md"), "design bead")
}

func TestCheckSpireSkills_FixInstallsBundledConflictSkill(t *testing.T) {
	home := t.TempDir()
	repo := filepath.Join(home, "repo")
	claudeSkills := filepath.Join(repo, ".claude", "skills")

	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, "codex-home"))

	if err := os.MkdirAll(filepath.Join(claudeSkills, "spire-work"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(claudeSkills, "spire-work", "SKILL.md"), []byte("legacy"), 0644); err != nil {
		t.Fatal(err)
	}

	r := checkSpireSkills(repo)
	if r.Status != statusMissing {
		t.Fatalf("expected missing status, got %s (%s)", r.Status, r.Detail)
	}
	if !strings.Contains(r.Detail, "spire-conflicts") {
		t.Fatalf("expected detail to mention spire-conflicts, got %q", r.Detail)
	}
	if r.FixFunc == nil {
		t.Fatal("expected fix function")
	}

	r.FixFunc()

	assertFileContains(t, filepath.Join(claudeSkills, "spire-conflicts", "SKILL.md"), "dolt sql")
	assertFileContains(t, filepath.Join(claudeSkills, "spire-design", "SKILL.md"), "design bead")
}

// TestScaffolderHook_Apprentice verifies the apprentice case branch of the
// generated spire-hook.sh embeds the apprentice catalog (+ common cmds) and
// contains none of the other roles' role-scoped verbs.
func TestScaffolderHook_Apprentice(t *testing.T) {
	assertRoleHookIsolation(t, scaffold.RoleApprentice,
		[]string{"apprentice submit", "spire focus", "spire collect", "spire send"},
		[]string{"sage accept", "sage reject", "wizard claim", "wizard seal", "cleric diagnose", "cleric execute", "cleric learn", "arbiter decide"},
	)
}

// TestScaffolderHook_Wizard — wizard branch has wizard verbs only.
func TestScaffolderHook_Wizard(t *testing.T) {
	assertRoleHookIsolation(t, scaffold.RoleWizard,
		[]string{"wizard claim", "wizard seal", "spire focus", "spire collect"},
		[]string{"apprentice submit", "sage accept", "sage reject", "cleric diagnose", "arbiter decide"},
	)
}

// TestScaffolderHook_Sage — sage branch has sage verbs only.
func TestScaffolderHook_Sage(t *testing.T) {
	assertRoleHookIsolation(t, scaffold.RoleSage,
		[]string{"sage accept", "sage reject", "spire focus", "spire collect"},
		[]string{"apprentice submit", "wizard claim", "wizard seal", "cleric diagnose", "arbiter decide"},
	)
}

// TestScaffolderHook_Cleric — cleric branch has cleric verbs only.
func TestScaffolderHook_Cleric(t *testing.T) {
	assertRoleHookIsolation(t, scaffold.RoleCleric,
		[]string{"cleric diagnose", "cleric execute", "cleric learn", "spire focus", "spire collect"},
		[]string{"apprentice submit", "wizard claim", "wizard seal", "sage accept", "arbiter decide"},
	)
}

// TestScaffolderHook_Arbiter — arbiter branch has the arbiter verb only.
func TestScaffolderHook_Arbiter(t *testing.T) {
	assertRoleHookIsolation(t, scaffold.RoleArbiter,
		[]string{"arbiter decide", "spire focus", "spire collect"},
		[]string{"apprentice submit", "wizard claim", "wizard seal", "sage accept", "cleric diagnose"},
	)
}

// TestSpireHookScript_DispatchesOnSpireRole verifies the generated hook
// script reads $SPIRE_ROLE under the SubagentStart event and embeds one
// case branch per known scaffold role.
func TestSpireHookScript_DispatchesOnSpireRole(t *testing.T) {
	script := renderSpireHookScript("/repo", "test")

	if !strings.Contains(script, `case "${SPIRE_ROLE:-}"`) {
		t.Error("script missing $SPIRE_ROLE dispatch")
	}
	for _, role := range scaffold.KnownRoles() {
		if !strings.Contains(script, "            "+string(role)+")\n") {
			t.Errorf("script missing case branch for role %q", role)
		}
	}
}

// TestSpireHookScript_NoLegacyInlineProtocol verifies the script no longer
// inlines the role-protocol string that used to live in scaffolding.go
// ("You are a subagent in a Spire-managed repo") as the *role*-scoped
// output — that generic fallback now only fires when SPIRE_ROLE is unset.
func TestSpireHookScript_NoLegacyInlineProtocol(t *testing.T) {
	script := renderSpireHookScript("/repo", "test")

	// The case branches must not contain the legacy generic protocol text.
	for _, role := range scaffold.KnownRoles() {
		branch := extractRoleCaseBranch(t, script, role)
		if strings.Contains(branch, "You are a subagent in a Spire-managed repo") {
			t.Errorf("role %q case branch leaks the generic legacy protocol text", role)
		}
	}
}

// assertRoleHookIsolation asserts the case branch for a given role
// contains every `want` substring and none of the `mustNotContain`
// substrings. The branch under test is the bash fragment assigned to
// ROLE_CATALOG when `SPIRE_ROLE=<role>` matches — i.e. exactly the
// text the subagent will see.
func assertRoleHookIsolation(t *testing.T, role scaffold.Role, want, mustNotContain []string) {
	t.Helper()

	script := renderSpireHookScript("/repo", "test")
	branch := extractRoleCaseBranch(t, script, role)

	for _, w := range want {
		if !strings.Contains(branch, w) {
			t.Errorf("role %q hook branch missing %q\n--- branch ---\n%s", role, w, branch)
		}
	}
	for _, bad := range mustNotContain {
		if strings.Contains(branch, bad) {
			t.Errorf("role %q hook branch unexpectedly contains %q\n--- branch ---\n%s", role, bad, branch)
		}
	}
}

// extractRoleCaseBranch pulls the bash fragment between the `<role>)`
// label and the matching `;;` terminator out of the generated script so
// per-role content can be asserted in isolation.
func extractRoleCaseBranch(t *testing.T, script string, role scaffold.Role) string {
	t.Helper()

	label := "            " + string(role) + ")\n"
	start := strings.Index(script, label)
	if start < 0 {
		t.Fatalf("could not find %q case label in script", role)
	}
	remainder := script[start+len(label):]
	end := strings.Index(remainder, ";;")
	if end < 0 {
		t.Fatalf("could not find `;;` terminator for role %q case branch", role)
	}
	return remainder[:end]
}

func assertFileContains(t *testing.T, path, needle string) {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if !strings.Contains(string(data), needle) {
		t.Fatalf("%s does not contain %q", path, needle)
	}
}
