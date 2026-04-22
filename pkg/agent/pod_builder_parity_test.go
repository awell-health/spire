// Parity fence for BuildApprenticePod. This file is the single place
// other consumers (the steward's cluster-native dispatch path, the
// operator reconciler) must go to see the complete pod-shape contract
// they are expected to satisfy byte-for-byte.
//
// Three angles are covered:
//
//  1. TestBuildApprenticePod_GoldenShape_Parity — a top-to-bottom
//     golden assertion on the canonical PodSpec: every required env
//     var, every required volume, restart policy, service account,
//     attempt/repo-keyed labels and annotations, and the absence of
//     deprecated/legacy env keys. Duplicating the assertions in the
//     parity surface (rather than relying solely on pod_builder_test.go)
//     keeps the contract legible from one file.
//
//  2. TestBuildApprenticePod_Parametric_NoFieldDrift — for every PodSpec
//     field that shapes pod output, mutate only that field and assert
//     the resulting pod differs from the baseline in EXACTLY the set of
//     env vars, labels, and annotations tied to that field. No other
//     surface may drift. This is the regression fence for refactors that
//     accidentally cross-wire two fields.
//
//  3. TestBuildApprenticePod_ForbiddenEnvKeys — a negative assertion
//     that forbidden env keys (anything containing "local_path" in any
//     case, plus specific retired keys) never appear on any container
//     (main + init) of the canonical pod.
package agent

import (
	"sort"
	"strings"
	"testing"

	"github.com/awell-health/spire/pkg/runtime"
	corev1 "k8s.io/api/core/v1"
)

// envSig returns a canonical comparison string for an EnvVar. Literal
// values and SecretKeyRef bindings are both captured so the parametric
// drift check detects rotating secret references as well as literal
// drift.
func envSig(e corev1.EnvVar) string {
	if e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil {
		ref := e.ValueFrom.SecretKeyRef
		opt := ""
		if ref.Optional != nil && *ref.Optional {
			opt = "?"
		}
		return "secret:" + ref.Name + "/" + ref.Key + opt
	}
	return "literal:" + e.Value
}

// envAsMap reduces a container's env slice to name -> envSig for
// set-difference operations.
func envAsMap(env []corev1.EnvVar) map[string]string {
	m := make(map[string]string, len(env))
	for _, e := range env {
		m[e.Name] = envSig(e)
	}
	return m
}

// diffStringMapKeys returns the sorted set of keys whose values differ
// between a and b (present in only one side counts as differing).
func diffStringMapKeys(a, b map[string]string) []string {
	seen := map[string]struct{}{}
	for k := range a {
		seen[k] = struct{}{}
	}
	for k := range b {
		seen[k] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		if a[k] != b[k] {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// sortedKeys returns sorted keys of m for deterministic comparison.
func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// parityFixture returns canonicalPodSpec() with OTLPEndpoint cleared.
// The parametric test mutates one field at a time and compares drift
// sets — when OTLP is on, changing any identity field also rewrites
// OTEL_RESOURCE_ATTRIBUTES, which obscures the "exactly this field
// changed" assertion. OTLP wiring is exercised by the golden-shape
// test (which keeps OTLPEndpoint set) and by the dedicated OTLP case
// in the parametric table.
func parityFixture() PodSpec {
	s := canonicalPodSpec()
	s.OTLPEndpoint = ""
	return s
}

// TestBuildApprenticePod_GoldenShape_Parity pins the full canonical
// pod shape in one place so downstream callers have a single fence to
// read when producing a byte-identical pod.
func TestBuildApprenticePod_GoldenShape_Parity(t *testing.T) {
	spec := canonicalPodSpec()
	pod, err := BuildApprenticePod(spec)
	if err != nil {
		t.Fatalf("BuildApprenticePod: %v", err)
	}

	// --- Required env vars (literal values). -----------------------
	wantLiteralEnv := map[string]string{
		// Canonical DOLT wiring.
		"DOLT_URL":               "spire-dolt.spire-test.svc:3306",
		"BEADS_DOLT_SERVER_HOST": "spire-dolt.spire-test.svc",
		"BEADS_DOLT_SERVER_PORT": "3306",
		"BEADS_DATABASE":         "test-tower",
		"BEADS_PREFIX":           "spi",
		"DOLT_DATA_DIR":          DataMountPath,
		"SPIRE_CONFIG_DIR":       DataMountPath + "/spire-config",
		"DOLTHUB_REMOTE":         "https://dolthub.test/example/repo",
		// Repo / bead identity.
		"SPIRE_REPO_URL":    "https://github.com/example/repo.git",
		"SPIRE_REPO_BRANCH": "main",
		"SPIRE_REPO_PREFIX": "spi",
		"SPIRE_TOWER":       "test-tower",
		"SPIRE_BEAD_ID":     "spi-abc",
		// RunContext vocabulary.
		"SPIRE_ROLE":           string(RoleApprentice),
		"SPIRE_ATTEMPT_ID":     "spi-att9",
		"SPIRE_RUN_ID":         "run-xyz",
		"SPIRE_APPRENTICE_IDX": "0",
		"SPIRE_FORMULA_STEP":   "implement",
		"SPIRE_BACKEND":        "k8s",
		"SPIRE_PROVIDER":       "claude",
		// Workspace materialization.
		"SPIRE_WORKSPACE_KIND":   string(runtime.WorkspaceKindBorrowedWorktree),
		"SPIRE_WORKSPACE_NAME":   "spi-abc-impl",
		"SPIRE_WORKSPACE_ORIGIN": string(runtime.WorkspaceOriginOriginClone),
		"SPIRE_WORKSPACE_PATH":   "/workspace/spi",
		// Handoff.
		"SPIRE_HANDOFF_MODE": string(runtime.HandoffBorrowed),
	}
	main := pod.Spec.Containers[0]
	env := envByName(main.Env)
	for k, want := range wantLiteralEnv {
		e, ok := env[k]
		if !ok {
			t.Errorf("missing required env %q", k)
			continue
		}
		if e.Value != want {
			t.Errorf("env %s = %q, want %q", k, e.Value, want)
		}
	}

	// --- Required SecretKeyRef env vars. ---------------------------
	// Literal values of these keys MUST NOT be inlined — they are
	// wired through the k8s Secret so credential rotation does not
	// require restarting the pod-builder.
	for _, name := range []string{"ANTHROPIC_API_KEY", "GITHUB_TOKEN"} {
		e, ok := env[name]
		if !ok {
			t.Errorf("missing secret env %q", name)
			continue
		}
		if e.ValueFrom == nil || e.ValueFrom.SecretKeyRef == nil {
			t.Errorf("env %s must use SecretKeyRef, got %+v", name, e.ValueFrom)
		}
	}

	// --- Required volumes. -----------------------------------------
	// /data holds the tower-attach substrate; /workspace holds the
	// cloned repo. Both MUST exist on every apprentice pod.
	vols := volumeByName(pod.Spec.Volumes)
	for _, name := range []string{"data", "workspace"} {
		v, ok := vols[name]
		if !ok {
			t.Errorf("missing volume %q", name)
			continue
		}
		// The canonical fixture does not set SharedWorkspacePVCName,
		// so both volumes must be EmptyDir.
		if v.EmptyDir == nil {
			t.Errorf("volume %q source = %+v, want EmptyDir", name, v.VolumeSource)
		}
	}

	// --- Restart policy / service account. -------------------------
	if pod.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("RestartPolicy = %q, want %q",
			pod.Spec.RestartPolicy, corev1.RestartPolicyNever)
	}
	if pod.Spec.ServiceAccountName != "spire-apprentice" {
		t.Errorf("ServiceAccountName = %q, want spire-apprentice",
			pod.Spec.ServiceAccountName)
	}

	// --- Labels keyed to attempt + repo. ---------------------------
	// Repo-identity lives in labels (bounded cardinality). Attempt +
	// run IDs live in annotations — see the cardinality guard at the
	// bottom of this test.
	wantLabels := map[string]string{
		LabelTower:         "test-tower",
		LabelPrefix:        "spi",
		LabelBead:          "spi-abc",
		LabelRole:          string(RoleApprentice),
		LabelBackend:       "k8s",
		LabelFormulaStep:   "implement",
		"spire.agent":      "true",
		"spire.agent.name": "apprentice-spi-abc-0",
		"spire.bead":       "spi-abc",
		"spire.tower":      "test-tower",
		"spire.role":       string(RoleApprentice),
	}
	for k, want := range wantLabels {
		got, ok := pod.Labels[k]
		if !ok {
			t.Errorf("missing label %q", k)
			continue
		}
		if got != want {
			t.Errorf("label %s = %q, want %q", k, got, want)
		}
	}
	// Attempt + run annotations carry the high-cardinality IDs.
	if got := pod.Annotations[AnnotationAttemptID]; got != "spi-att9" {
		t.Errorf("annotation %s = %q, want spi-att9", AnnotationAttemptID, got)
	}
	if got := pod.Annotations[AnnotationRunID]; got != "run-xyz" {
		t.Errorf("annotation %s = %q, want run-xyz", AnnotationRunID, got)
	}
	// And they MUST NOT appear as labels (cardinality guard).
	for _, k := range []string{AnnotationAttemptID, AnnotationRunID} {
		if _, ok := pod.Labels[k]; ok {
			t.Errorf("high-cardinality key %q leaked into labels", k)
		}
	}

	// --- Absence of deprecated / legacy env keys. ------------------
	// Quick smoke pass; the full case list lives in
	// TestBuildApprenticePod_ForbiddenEnvKeys.
	for _, c := range append(pod.Spec.Containers, pod.Spec.InitContainers...) {
		for _, e := range c.Env {
			if strings.Contains(strings.ToLower(e.Name), "local_path") {
				t.Errorf("container %q: forbidden env %q (contains local_path)",
					c.Name, e.Name)
			}
		}
	}
}

// parityCase is one row of TestBuildApprenticePod_Parametric_NoFieldDrift.
// The invariant is: mutating exactly this PodSpec field changes
// exactly the env/label/annotation keys listed below and nothing else.
type parityCase struct {
	name   string
	mutate func(*PodSpec)

	// wantEnv / wantLabels / wantAnnos list the keys that MUST differ
	// between baseline and mutated pod. Non-empty values also assert
	// the mutated pod has that literal value. Empty value means "this
	// key must be absent in the mutated pod" (removed).
	wantEnv    map[string]string
	wantLabels map[string]string
	wantAnnos  map[string]string

	// wantEnvKeysOnly lists additional env keys that MUST appear in
	// the drift set but whose value is deliberately not asserted
	// inline (e.g. OTEL_RESOURCE_ATTRIBUTES, whose formatting is
	// exercised in extraChecks). Kept separate from wantEnv so the
	// literal-value assertion stays unambiguous.
	wantEnvKeysOnly []string

	// extraChecks covers drift that is not captured by env/label/
	// annotation sets — most commonly the main container Command or
	// the tower-attach init container's CLI flags.
	extraChecks func(t *testing.T, base, mut *corev1.Pod)
}

// TestBuildApprenticePod_Parametric_NoFieldDrift is the regression
// fence against cross-wiring refactors. For each PodSpec field row, we
// build a baseline pod (parityFixture) and a mutated pod (baseline +
// the single mutation) and assert the env/label/annotation drift sets
// match EXACTLY the expected keys. If an unrelated field suddenly
// starts responding to an identity input, this test catches it.
func TestBuildApprenticePod_Parametric_NoFieldDrift(t *testing.T) {
	cases := []parityCase{
		{
			name:   "BeadID",
			mutate: func(s *PodSpec) { s.BeadID = "spi-new" },
			wantEnv: map[string]string{
				"SPIRE_BEAD_ID": "spi-new",
			},
			wantLabels: map[string]string{
				LabelBead:    "spi-new",
				"spire.bead": "spi-new",
			},
			extraChecks: func(t *testing.T, base, mut *corev1.Pod) {
				// Command: `spire apprentice run <bead> --name <agent>`.
				if got := mut.Spec.Containers[0].Command[3]; got != "spi-new" {
					t.Errorf("Command[3] = %q, want spi-new", got)
				}
			},
		},
		{
			name:   "AttemptID updated",
			mutate: func(s *PodSpec) { s.AttemptID = "spi-att-new" },
			wantEnv: map[string]string{
				"SPIRE_ATTEMPT_ID": "spi-att-new",
			},
			wantAnnos: map[string]string{
				AnnotationAttemptID: "spi-att-new",
			},
		},
		{
			name:   "AttemptID cleared",
			mutate: func(s *PodSpec) { s.AttemptID = "" },
			wantEnv: map[string]string{
				"SPIRE_ATTEMPT_ID": "", // absent in mutated
			},
			wantAnnos: map[string]string{
				AnnotationAttemptID: "", // absent in mutated
			},
		},
		{
			name:   "RunID updated",
			mutate: func(s *PodSpec) { s.RunID = "run-new" },
			wantEnv: map[string]string{
				"SPIRE_RUN_ID": "run-new",
			},
			wantAnnos: map[string]string{
				AnnotationRunID: "run-new",
			},
		},
		{
			name:   "ApprenticeIdx",
			mutate: func(s *PodSpec) { s.ApprenticeIdx = "2" },
			wantEnv: map[string]string{
				"SPIRE_APPRENTICE_IDX": "2",
			},
		},
		{
			name:   "FormulaStep",
			mutate: func(s *PodSpec) { s.FormulaStep = "sage-review" },
			wantEnv: map[string]string{
				"SPIRE_FORMULA_STEP": "sage-review",
			},
			wantLabels: map[string]string{
				LabelFormulaStep: "sage-review",
			},
		},
		{
			name:   "Identity.TowerName",
			mutate: func(s *PodSpec) { s.Identity.TowerName = "other-tower" },
			wantEnv: map[string]string{
				"SPIRE_TOWER":    "other-tower",
				"BEADS_DATABASE": "other-tower",
			},
			wantLabels: map[string]string{
				LabelTower:    "other-tower",
				"spire.tower": "other-tower",
			},
			extraChecks: func(t *testing.T, base, mut *corev1.Pod) {
				ta := mut.Spec.InitContainers[0]
				wantFlags := []string{
					"--database=other-tower",
					"--data-dir=" + DataMountPath + "/other-tower",
				}
				for _, f := range wantFlags {
					if !containsString(ta.Command, f) {
						t.Errorf("tower-attach missing %q; got %v", f, ta.Command)
					}
				}
			},
		},
		{
			name:   "Identity.Prefix",
			mutate: func(s *PodSpec) { s.Identity.Prefix = "other" },
			wantEnv: map[string]string{
				"SPIRE_REPO_PREFIX": "other",
				"BEADS_PREFIX":      "other",
			},
			wantLabels: map[string]string{
				LabelPrefix: "other",
			},
			extraChecks: func(t *testing.T, base, mut *corev1.Pod) {
				ta := mut.Spec.InitContainers[0]
				if !containsString(ta.Command, "--prefix=other") {
					t.Errorf("tower-attach missing --prefix=other; got %v", ta.Command)
				}
			},
		},
		{
			name:   "Identity.RepoURL",
			mutate: func(s *PodSpec) { s.Identity.RepoURL = "https://github.com/other/repo.git" },
			wantEnv: map[string]string{
				"SPIRE_REPO_URL": "https://github.com/other/repo.git",
			},
		},
		{
			name:   "Identity.BaseBranch",
			mutate: func(s *PodSpec) { s.Identity.BaseBranch = "develop" },
			wantEnv: map[string]string{
				"SPIRE_REPO_BRANCH": "develop",
			},
		},
		{
			name:   "Workspace.Kind",
			mutate: func(s *PodSpec) { s.Workspace.Kind = runtime.WorkspaceKindOwnedWorktree },
			wantEnv: map[string]string{
				"SPIRE_WORKSPACE_KIND": string(runtime.WorkspaceKindOwnedWorktree),
			},
			wantLabels: map[string]string{
				LabelWorkspaceKind: string(runtime.WorkspaceKindOwnedWorktree),
			},
		},
		{
			name:   "Workspace.Name",
			mutate: func(s *PodSpec) { s.Workspace.Name = "other-workspace" },
			wantEnv: map[string]string{
				"SPIRE_WORKSPACE_NAME": "other-workspace",
			},
			wantLabels: map[string]string{
				LabelWorkspaceName: "other-workspace",
			},
		},
		{
			name:   "Workspace.Origin",
			mutate: func(s *PodSpec) { s.Workspace.Origin = runtime.WorkspaceOriginLocalBind },
			wantEnv: map[string]string{
				"SPIRE_WORKSPACE_ORIGIN": string(runtime.WorkspaceOriginLocalBind),
			},
			wantLabels: map[string]string{
				LabelWorkspaceOrigin: string(runtime.WorkspaceOriginLocalBind),
			},
		},
		{
			name:   "Workspace.Path",
			mutate: func(s *PodSpec) { s.Workspace.Path = "/workspace/other" },
			wantEnv: map[string]string{
				"SPIRE_WORKSPACE_PATH": "/workspace/other",
			},
		},
		{
			name:   "HandoffMode",
			mutate: func(s *PodSpec) { s.HandoffMode = runtime.HandoffBundle },
			wantEnv: map[string]string{
				"SPIRE_HANDOFF_MODE": string(runtime.HandoffBundle),
			},
			wantLabels: map[string]string{
				LabelHandoffMode: string(runtime.HandoffBundle),
			},
		},
		{
			name:   "Backend",
			mutate: func(s *PodSpec) { s.Backend = "operator-k8s" },
			wantEnv: map[string]string{
				"SPIRE_BACKEND": "operator-k8s",
			},
			wantLabels: map[string]string{
				LabelBackend: "operator-k8s",
			},
		},
		{
			name:   "Provider",
			mutate: func(s *PodSpec) { s.Provider = "codex" },
			wantEnv: map[string]string{
				"SPIRE_PROVIDER": "codex",
			},
		},
		{
			name:   "DoltURL",
			mutate: func(s *PodSpec) { s.DoltURL = "other-dolt.svc:3307" },
			wantEnv: map[string]string{
				"DOLT_URL":               "other-dolt.svc:3307",
				"BEADS_DOLT_SERVER_HOST": "other-dolt.svc",
				"BEADS_DOLT_SERVER_PORT": "3307",
			},
		},
		{
			name:   "DolthubRemote",
			mutate: func(s *PodSpec) { s.DolthubRemote = "https://other.dolthub/repo" },
			wantEnv: map[string]string{
				"DOLTHUB_REMOTE": "https://other.dolthub/repo",
			},
			extraChecks: func(t *testing.T, base, mut *corev1.Pod) {
				ta := mut.Spec.InitContainers[0]
				if !containsString(ta.Command, "--dolthub-remote=https://other.dolthub/repo") {
					t.Errorf("tower-attach missing --dolthub-remote; got %v", ta.Command)
				}
			},
		},
		{
			name:   "CustomPrompt added",
			mutate: func(s *PodSpec) { s.CustomPrompt = "custom.md" },
			wantEnv: map[string]string{
				"SPIRE_CUSTOM_PROMPT": "custom.md",
			},
		},
		{
			name:   "OTLPEndpoint enables full OTLP env block",
			mutate: func(s *PodSpec) { s.OTLPEndpoint = "http://otel.svc:4317" },
			wantEnv: map[string]string{
				"OTEL_EXPORTER_OTLP_ENDPOINT":         "http://otel.svc:4317",
				"CLAUDE_CODE_ENABLE_TELEMETRY":        "1",
				"CLAUDE_CODE_ENHANCED_TELEMETRY_BETA": "1",
				"OTEL_TRACES_EXPORTER":                "otlp",
				"OTEL_LOGS_EXPORTER":                  "otlp",
				"OTEL_EXPORTER_OTLP_PROTOCOL":         "grpc",
			},
			// OTEL_RESOURCE_ATTRIBUTES's exact value is a comma-
			// separated attribute list whose formatting belongs to
			// the OTEL spec, not to this parity contract. Assert
			// the key is present in the drift set here and delegate
			// the content checks to extraChecks.
			wantEnvKeysOnly: []string{"OTEL_RESOURCE_ATTRIBUTES"},
			extraChecks: func(t *testing.T, base, mut *corev1.Pod) {
				env := envByName(mut.Spec.Containers[0].Env)
				attrs, ok := env["OTEL_RESOURCE_ATTRIBUTES"]
				if !ok {
					t.Fatal("OTEL_RESOURCE_ATTRIBUTES env missing")
				}
				for _, want := range []string{
					"bead_id=spi-abc",
					"tower=test-tower",
					"prefix=spi",
					"role=" + string(RoleApprentice),
				} {
					if !strings.Contains(attrs.Value, want) {
						t.Errorf("OTEL_RESOURCE_ATTRIBUTES = %q, missing %q",
							attrs.Value, want)
					}
				}
			},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			baseSpec := parityFixture()
			base, err := BuildApprenticePod(baseSpec)
			if err != nil {
				t.Fatalf("BuildApprenticePod(base): %v", err)
			}
			mutSpec := parityFixture()
			tc.mutate(&mutSpec)
			mut, err := BuildApprenticePod(mutSpec)
			if err != nil {
				t.Fatalf("BuildApprenticePod(mut): %v", err)
			}

			// --- env drift ----------------------------------------
			baseEnv := envAsMap(base.Spec.Containers[0].Env)
			mutEnv := envAsMap(mut.Spec.Containers[0].Env)
			gotChanged := diffStringMapKeys(baseEnv, mutEnv)
			wantChangedSet := map[string]struct{}{}
			for k := range tc.wantEnv {
				wantChangedSet[k] = struct{}{}
			}
			for _, k := range tc.wantEnvKeysOnly {
				wantChangedSet[k] = struct{}{}
			}
			wantChanged := make([]string, 0, len(wantChangedSet))
			for k := range wantChangedSet {
				wantChanged = append(wantChanged, k)
			}
			sort.Strings(wantChanged)
			if !stringSliceEq(gotChanged, wantChanged) {
				t.Errorf("env drift set = %v, want %v", gotChanged, wantChanged)
			}
			mutMain := envByName(mut.Spec.Containers[0].Env)
			for k, wantV := range tc.wantEnv {
				if wantV == "" {
					if e, ok := mutMain[k]; ok {
						t.Errorf("env %q should have been removed; got %q",
							k, e.Value)
					}
					continue
				}
				e, ok := mutMain[k]
				if !ok {
					t.Errorf("env %q missing in mutated pod", k)
					continue
				}
				if e.Value != wantV {
					t.Errorf("env %q = %q, want %q", k, e.Value, wantV)
				}
			}
			// Keys-only: just require presence.
			for _, k := range tc.wantEnvKeysOnly {
				if _, ok := mutMain[k]; !ok {
					t.Errorf("env %q missing in mutated pod", k)
				}
			}

			// --- label drift --------------------------------------
			gotLabelDiff := diffStringMapKeys(base.Labels, mut.Labels)
			wantLabelDiff := sortedKeys(tc.wantLabels)
			if !stringSliceEq(gotLabelDiff, wantLabelDiff) {
				t.Errorf("label drift set = %v, want %v",
					gotLabelDiff, wantLabelDiff)
			}
			for k, wantV := range tc.wantLabels {
				if wantV == "" {
					if v, ok := mut.Labels[k]; ok {
						t.Errorf("label %q should have been removed; got %q", k, v)
					}
					continue
				}
				if mut.Labels[k] != wantV {
					t.Errorf("label %q = %q, want %q", k, mut.Labels[k], wantV)
				}
			}

			// --- annotation drift ---------------------------------
			gotAnnoDiff := diffStringMapKeys(base.Annotations, mut.Annotations)
			wantAnnoDiff := sortedKeys(tc.wantAnnos)
			if !stringSliceEq(gotAnnoDiff, wantAnnoDiff) {
				t.Errorf("annotation drift set = %v, want %v",
					gotAnnoDiff, wantAnnoDiff)
			}
			for k, wantV := range tc.wantAnnos {
				if wantV == "" {
					if v, ok := mut.Annotations[k]; ok {
						t.Errorf("annotation %q should have been removed; got %q", k, v)
					}
					continue
				}
				if mut.Annotations[k] != wantV {
					t.Errorf("annotation %q = %q, want %q",
						k, mut.Annotations[k], wantV)
				}
			}

			if tc.extraChecks != nil {
				tc.extraChecks(t, base, mut)
			}
		})
	}
}

// TestBuildApprenticePod_ForbiddenEnvKeys pins the negative side of the
// contract: no env key on any container (main or init) may contain
// "local_path" in any casing, and a small allowlist of explicitly
// retired keys is also absent. The cluster apprentice materializes its
// workspace via SPIRE_WORKSPACE_PATH + init containers — reading any
// LOCAL_PATH variant would regress cluster semantics to process-mode
// ambient CWD.
func TestBuildApprenticePod_ForbiddenEnvKeys(t *testing.T) {
	// Build the canonical pod plus a few variants to make sure the
	// forbidden set does not leak in through optional wiring paths
	// (cache overlay, shared workspace PVC, custom prompt, extra args).
	variants := []struct {
		name   string
		mutate func(*PodSpec)
	}{
		{"canonical", func(*PodSpec) {}},
		{"cache overlay", func(s *PodSpec) { s.CachePVCName = "core-cache" }},
		{"shared workspace PVC", func(s *PodSpec) { s.SharedWorkspacePVCName = "wiz-ws" }},
		{"custom prompt", func(s *PodSpec) { s.CustomPrompt = "custom.md" }},
		{"extra args", func(s *PodSpec) { s.ExtraArgs = []string{"--review-fix"} }},
	}

	// Any key containing any of these substrings is forbidden (case-
	// insensitive). "local_path" is the contract's headline — cluster
	// apprentices resolve the workspace via SPIRE_WORKSPACE_PATH, never
	// via a LocalPath/LOCAL_PATH/local_path input.
	forbiddenSubstrings := []string{
		"local_path",
		"localpath",
	}

	// Explicit keys retired during the runtime-contract migration.
	forbiddenExact := map[string]struct{}{
		// Scheduler-side identity — leaking it to the worker was the
		// original dispatch-time anti-pattern.
		"SPIRE_INSTANCE_ID": {},
		// Local-bind path keys that a host-side process expects but
		// must NEVER appear on a cluster pod.
		"SPIRE_LOCAL_PATH":        {},
		"BEADS_LOCAL_PATH":        {},
		"SPIRE_WIZARD_LOCAL_PATH": {},
	}

	for _, v := range variants {
		v := v
		t.Run(v.name, func(t *testing.T) {
			spec := canonicalPodSpec()
			v.mutate(&spec)
			pod, err := BuildApprenticePod(spec)
			if err != nil {
				t.Fatalf("BuildApprenticePod: %v", err)
			}

			containers := append([]corev1.Container{}, pod.Spec.Containers...)
			containers = append(containers, pod.Spec.InitContainers...)
			for _, c := range containers {
				for _, e := range c.Env {
					lower := strings.ToLower(e.Name)
					for _, sub := range forbiddenSubstrings {
						if strings.Contains(lower, sub) {
							t.Errorf("container %q: forbidden env %q (contains %q)",
								c.Name, e.Name, sub)
						}
					}
					if _, bad := forbiddenExact[e.Name]; bad {
						t.Errorf("container %q: forbidden env %q", c.Name, e.Name)
					}
				}
			}
		})
	}
}
