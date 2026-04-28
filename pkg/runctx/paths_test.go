package runctx

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/awell-health/spire/pkg/logartifact"
	"github.com/awell-health/spire/pkg/runtime"
)

// fullRC is the canonical fully-populated RunContext shared by every
// path-derivation test in this file. The set mirrors the one in
// pkg/runtime/obs_test.go so a future cross-package refactor that
// changes the field set fails one place at a time, not nine.
func fullRC() runtime.RunContext {
	return runtime.RunContext{
		TowerName:       "dev",
		Prefix:          "spi",
		BeadID:          "spi-abc",
		AttemptID:       "spi-att-1",
		RunID:           "run-42",
		AgentName:       "wizard-spi-abc",
		Role:            runtime.RoleWizard,
		FormulaStep:     "implement",
		Backend:         "process",
		WorkspaceKind:   runtime.WorkspaceKindOwnedWorktree,
		WorkspaceName:   "feat",
		WorkspaceOrigin: runtime.WorkspaceOriginLocalBind,
		HandoffMode:     runtime.HandoffBundle,
	}
}

func TestTranscriptFile_DerivedFromIdentityNotEnv(t *testing.T) {
	// Claude AND Codex transcript paths must be fully identity-derived
	// (acceptance criterion of spi-egw26j). Each provider gets the same
	// canonical eight-segment-plus-leaf shape, differing only in the
	// per-write provider segment.
	root := t.TempDir()
	rc := fullRC()
	p := New(rc, root)

	for _, provider := range []string{"claude", "codex"} {
		t.Run(provider, func(t *testing.T) {
			got, err := p.TranscriptFile(provider, logartifact.StreamStdout)
			if err != nil {
				t.Fatalf("TranscriptFile: %v", err)
			}

			want := filepath.Join(root,
				rc.TowerName,
				rc.BeadID,
				rc.AttemptID,
				rc.RunID,
				rc.AgentName,
				string(rc.Role),
				rc.FormulaStep,
				provider,
				"stdout.jsonl",
			)
			if got != want {
				t.Fatalf("TranscriptFile = %q\n          want %q", got, want)
			}
		})
	}
}

func TestTranscriptFile_IndependentOfCWDAndHostname(t *testing.T) {
	// Same identity → same path regardless of which directory the
	// process sits in or which hostname env var the runtime exposes.
	// This is the core acceptance test for spi-egw26j.
	root := t.TempDir()
	rc := fullRC()

	original, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(original) })

	dir1 := t.TempDir()
	if err := os.Chdir(dir1); err != nil {
		t.Fatalf("chdir %s: %v", dir1, err)
	}
	t.Setenv("HOSTNAME", "node-A")
	t.Setenv("POD_NAME", "wizard-pod-A")
	got1, err := New(rc, root).TranscriptFile("claude", logartifact.StreamStdout)
	if err != nil {
		t.Fatalf("TranscriptFile [pass1]: %v", err)
	}

	dir2 := t.TempDir()
	if err := os.Chdir(dir2); err != nil {
		t.Fatalf("chdir %s: %v", dir2, err)
	}
	t.Setenv("HOSTNAME", "node-B")
	t.Setenv("POD_NAME", "wizard-pod-B-restart-7")
	got2, err := New(rc, root).TranscriptFile("claude", logartifact.StreamStdout)
	if err != nil {
		t.Fatalf("TranscriptFile [pass2]: %v", err)
	}

	if got1 != got2 {
		t.Fatalf("transcript path drifted with CWD/hostname:\n  pass1=%s\n  pass2=%s", got1, got2)
	}
}

func TestTranscriptFile_MatchesLogArtifactObjectKey(t *testing.T) {
	// runctx and logartifact must agree on the path schema byte-for-byte:
	// the local-store reconciler walks files at the same paths runctx
	// writes to, and the cluster exporter uploads them to GCS at the
	// same key BuildObjectKey produces. Drifting the two would silently
	// orphan artifacts from their manifest rows.
	root := t.TempDir()
	rc := fullRC()
	p := New(rc, root)

	id, err := p.Identity("claude", logartifact.StreamStdout)
	if err != nil {
		t.Fatalf("Identity: %v", err)
	}
	gcsKey, err := logartifact.BuildObjectKey("", id, 0)
	if err != nil {
		t.Fatalf("BuildObjectKey: %v", err)
	}
	want := filepath.Join(root, filepath.FromSlash(gcsKey))

	got, err := p.TranscriptFile("claude", logartifact.StreamStdout)
	if err != nil {
		t.Fatalf("TranscriptFile: %v", err)
	}
	if got != want {
		t.Fatalf("paths diverged:\n  TranscriptFile = %s\n  BuildObjectKey = %s", got, want)
	}
}

func TestOperationalAndTranscript_ShareBeadAttemptRunPrefix(t *testing.T) {
	// Acceptance criterion of spi-egw26j: "wizard operational logs and
	// provider transcripts can be associated with the same
	// bead/attempt/run."
	//
	// Build a single RunContext, derive the wizard operational log
	// path, and derive a Claude transcript path from the same identity.
	// Both must share the per-bead/attempt/run prefix so a downstream
	// joiner (gateway, spi-j3r694) can list every artifact for a single
	// run by walking that prefix.
	root := t.TempDir()
	rc := fullRC()
	p := New(rc, root)

	op, err := p.OperationalLog()
	if err != nil {
		t.Fatalf("OperationalLog: %v", err)
	}
	tr, err := p.TranscriptFile("claude", logartifact.StreamStdout)
	if err != nil {
		t.Fatalf("TranscriptFile: %v", err)
	}

	wantPrefix := filepath.Join(root,
		rc.TowerName,
		rc.BeadID,
		rc.AttemptID,
		rc.RunID,
	) + string(filepath.Separator)
	if !strings.HasPrefix(op, wantPrefix) {
		t.Errorf("operational log %q lacks bead/attempt/run prefix %q", op, wantPrefix)
	}
	if !strings.HasPrefix(tr, wantPrefix) {
		t.Errorf("transcript %q lacks bead/attempt/run prefix %q", tr, wantPrefix)
	}
}

func TestOperationalLog_SiblingOfTranscriptDir(t *testing.T) {
	root := t.TempDir()
	p := New(fullRC(), root)

	op, err := p.OperationalLog()
	if err != nil {
		t.Fatalf("OperationalLog: %v", err)
	}
	transcript, err := p.TranscriptFile("claude", logartifact.StreamStdout)
	if err != nil {
		t.Fatalf("TranscriptFile: %v", err)
	}

	// Both files must share the per-run directory so a single mkdir
	// covers the wizard's operational log and every provider transcript
	// it produces in the same phase.
	opDir := filepath.Dir(op)
	transcriptDir := filepath.Dir(filepath.Dir(transcript)) // strip <provider>/<stream>
	if opDir != transcriptDir {
		t.Fatalf("operational and transcript dirs diverge:\n  op=%s\n  tr=%s", op, transcript)
	}
}

func TestPaths_RejectsMissingIdentityFields(t *testing.T) {
	root := t.TempDir()
	cases := map[string]runtime.RunContext{
		"missing tower":     mutateRC(fullRC(), func(r *runtime.RunContext) { r.TowerName = "" }),
		"missing bead":      mutateRC(fullRC(), func(r *runtime.RunContext) { r.BeadID = "" }),
		"missing attempt":   mutateRC(fullRC(), func(r *runtime.RunContext) { r.AttemptID = "" }),
		"missing run":       mutateRC(fullRC(), func(r *runtime.RunContext) { r.RunID = "" }),
		"missing agent":     mutateRC(fullRC(), func(r *runtime.RunContext) { r.AgentName = "" }),
		"missing role":      mutateRC(fullRC(), func(r *runtime.RunContext) { r.Role = "" }),
		"missing phase":     mutateRC(fullRC(), func(r *runtime.RunContext) { r.FormulaStep = "" }),
	}
	for name, rc := range cases {
		t.Run(name, func(t *testing.T) {
			p := New(rc, root)
			if _, err := p.OperationalLog(); err == nil {
				t.Errorf("OperationalLog accepted %s", name)
			}
			if _, err := p.TranscriptFile("claude", logartifact.StreamStdout); err == nil {
				t.Errorf("TranscriptFile accepted %s", name)
			}
		})
	}
}

func TestTranscriptFile_RejectsMissingStream(t *testing.T) {
	root := t.TempDir()
	p := New(fullRC(), root)
	if _, err := p.TranscriptFile("claude", ""); err == nil {
		t.Fatalf("expected error for empty stream")
	}
}

func TestTranscriptFile_AllowsEmptyProvider(t *testing.T) {
	// Wizard operational stdout uses an empty provider — the path
	// schema in spi-7wzwk2 explicitly omits the provider segment when
	// it is unset. This test guards that flexibility against a future
	// "every field required" tightening.
	root := t.TempDir()
	rc := fullRC()
	p := New(rc, root)

	got, err := p.TranscriptFile("", logartifact.StreamStdout)
	if err != nil {
		t.Fatalf("TranscriptFile: %v", err)
	}
	want := filepath.Join(root,
		rc.TowerName, rc.BeadID, rc.AttemptID, rc.RunID,
		rc.AgentName, string(rc.Role), rc.FormulaStep,
		"stdout.jsonl",
	)
	if got != want {
		t.Fatalf("TranscriptFile = %q\n          want %q", got, want)
	}
}

func TestPaths_DistinctRolesPhasesProvidersDoNotCollide(t *testing.T) {
	root := t.TempDir()
	base := fullRC()

	// Two paths derived from RunContexts that differ only by role +
	// formula_step + agent_name must point at different files. This is
	// the multi-role / multi-phase coexistence case for one bead.
	a := base
	a.Role = runtime.RoleApprentice
	a.AgentName = "apprentice-spi-abc-w1-0"
	a.FormulaStep = "implement"

	b := base
	b.Role = runtime.RoleSage
	b.AgentName = "sage-spi-abc-r1"
	b.FormulaStep = "review"

	pathA, errA := New(a, root).TranscriptFile("claude", logartifact.StreamStdout)
	pathB, errB := New(b, root).TranscriptFile("claude", logartifact.StreamStdout)
	if errA != nil || errB != nil {
		t.Fatalf("TranscriptFile errs: a=%v b=%v", errA, errB)
	}
	if pathA == pathB {
		t.Fatalf("collision across role/phase/agent: %s", pathA)
	}

	// Provider segregation: claude vs codex on the same RunContext.
	pClaude, _ := New(base, root).TranscriptFile("claude", logartifact.StreamStdout)
	pCodex, _ := New(base, root).TranscriptFile("codex", logartifact.StreamStdout)
	if pClaude == pCodex {
		t.Fatalf("collision across providers: %s", pClaude)
	}
}

func TestResolveLogRoot_EnvPrecedence(t *testing.T) {
	t.Setenv(EnvLogRoot, "/tmp/spire-cluster-logs")
	if got := ResolveLogRoot("/local/default"); got != "/tmp/spire-cluster-logs" {
		t.Errorf("ResolveLogRoot = %q, want /tmp/spire-cluster-logs", got)
	}

	t.Setenv(EnvLogRoot, "")
	if got := ResolveLogRoot("/local/default"); got != "/local/default" {
		t.Errorf("ResolveLogRoot = %q, want /local/default", got)
	}
}

func TestDefaultLocalRoot(t *testing.T) {
	if got := DefaultLocalRoot(""); got != "" {
		t.Errorf("DefaultLocalRoot(\"\") = %q, want \"\"", got)
	}
	if got := DefaultLocalRoot("/var/data"); !strings.HasSuffix(got, "logs") {
		t.Errorf("DefaultLocalRoot did not include /logs suffix: %q", got)
	}
}

func TestMkdirAll_CreatesPerRunDir(t *testing.T) {
	root := t.TempDir()
	p := New(fullRC(), root)

	if err := p.MkdirAll(); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	dir, err := p.Dir()
	if err != nil {
		t.Fatalf("Dir: %v", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("Dir %s not created: %v", dir, err)
	}
}

func mutateRC(rc runtime.RunContext, mut func(*runtime.RunContext)) runtime.RunContext {
	mut(&rc)
	return rc
}
