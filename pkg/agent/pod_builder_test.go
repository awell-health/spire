package agent

import (
	"errors"
	"strings"
	"testing"

	"github.com/awell-health/spire/pkg/config"
	"github.com/awell-health/spire/pkg/runtime"
	"github.com/awell-health/spire/pkg/steward/intent"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// TestSelectBuilder_AllAllowedPairsResolveToBuilder asserts that every
// pair in intent.Allowed has a registered PodBuilder in builderTable,
// and that each one produces a valid (non-nil) pod from the canonical
// fixture. A new entry in intent.Allowed without a corresponding
// builderTable entry surfaces as an explicit failure here.
func TestSelectBuilder_AllAllowedPairsResolveToBuilder(t *testing.T) {
	for role, phases := range intent.Allowed {
		for phase := range phases {
			t.Run(string(role)+"/"+string(phase), func(t *testing.T) {
				b, err := SelectBuilder(role, phase)
				if err != nil {
					t.Fatalf("SelectBuilder(%s, %s) = err %v, want nil", role, phase, err)
				}
				if b == nil {
					t.Fatalf("SelectBuilder(%s, %s) returned nil builder", role, phase)
				}

				pod, err := b(canonicalPodSpec())
				if err != nil {
					t.Fatalf("builder(%s, %s) on canonical spec: %v", role, phase, err)
				}
				if pod == nil {
					t.Fatalf("builder(%s, %s) returned nil pod", role, phase)
				}
				if len(pod.Spec.Containers) != 1 {
					t.Fatalf("builder(%s, %s) produced %d containers, want 1",
						role, phase, len(pod.Spec.Containers))
				}
			})
		}
	}
}

// TestSelectBuilder_FailsClosedForDisallowedPair pins the fail-closed
// invariant: any (Role, Phase) pair not in intent.Allowed must return
// an error rather than silently defaulting to apprentice. Covers the
// classic misroute (sage/implement) plus an unknown-role case.
func TestSelectBuilder_FailsClosedForDisallowedPair(t *testing.T) {
	cases := []struct {
		name       string
		role       intent.Role
		phase      intent.Phase
		wantSubstr string
	}{
		{"unknown role", "necromancer", intent.PhaseImplement, `unknown role "necromancer"`},
		{"sage on implement", intent.RoleSage, intent.PhaseImplement, "no pod builder for sage/implement"},
		{"cleric on review", intent.RoleCleric, intent.PhaseReview, "no pod builder for cleric/review"},
		{"wizard on recovery", intent.RoleWizard, intent.PhaseRecovery, "no pod builder for wizard/recovery"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, err := SelectBuilder(tc.role, tc.phase)
			if err == nil {
				t.Fatalf("SelectBuilder(%s, %s) returned builder %v, want error",
					tc.role, tc.phase, b)
			}
			if !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Errorf("SelectBuilder(%s, %s) error = %q, want substring %q",
					tc.role, tc.phase, err.Error(), tc.wantSubstr)
			}
			if b != nil {
				t.Errorf("SelectBuilder(%s, %s) returned non-nil builder alongside error",
					tc.role, tc.phase)
			}
		})
	}
}

// TestSelectBuilder_BuilderTableMatchesIntentAllowed enforces the
// invariant that builderTable's keys exactly mirror intent.Allowed.
// A divergence (extra builderTable entry, missing one) is a contract
// drift that must surface immediately, not at runtime when a real
// intent shows up unrouted.
func TestSelectBuilder_BuilderTableMatchesIntentAllowed(t *testing.T) {
	for role, phases := range intent.Allowed {
		got, ok := builderTable[role]
		if !ok {
			t.Errorf("builderTable missing role %q from intent.Allowed", role)
			continue
		}
		for phase := range phases {
			if _, ok := got[phase]; !ok {
				t.Errorf("builderTable[%s] missing phase %q", role, phase)
			}
		}
	}
	for role, phases := range builderTable {
		if _, ok := intent.Allowed[role]; !ok {
			t.Errorf("builderTable has role %q absent from intent.Allowed", role)
			continue
		}
		for phase := range phases {
			if _, ok := intent.Allowed[role][phase]; !ok {
				t.Errorf("builderTable[%s] has phase %q absent from intent.Allowed[%s]",
					role, phase, role)
			}
		}
	}
}

// TestBuildClericPod_Command pins the cleric pod's main container
// command. The operator routes cleric work via Role=cleric/Phase=
// recovery (no formula_phase=recovery) and the produced pod must
// invoke the cleric verb so the recovery driver actually runs.
func TestBuildClericPod_Command(t *testing.T) {
	spec := canonicalPodSpec()
	spec.Name = "cleric-spi-rec-0"
	spec.AgentName = "cleric-spi-rec-0"
	spec.BeadID = "spi-rec"
	spec.FormulaStep = string(intent.PhaseRecovery)

	pod, err := BuildClericPod(spec)
	if err != nil {
		t.Fatalf("BuildClericPod: %v", err)
	}
	if len(pod.Spec.Containers) != 1 {
		t.Fatalf("Containers = %d, want 1", len(pod.Spec.Containers))
	}
	wantCmd := []string{"spire", "cleric", "diagnose", "spi-rec", "--name", "cleric-spi-rec-0"}
	if !stringSliceEq(pod.Spec.Containers[0].Command, wantCmd) {
		t.Errorf("cleric Command = %v, want %v", pod.Spec.Containers[0].Command, wantCmd)
	}
}

// canonicalPodSpec returns a PodSpec populated with every required
// field plus every optional field that flows into the golden shape.
// Shared fixture so every BuildApprenticePod test asserts against the
// same canonical input — if a new field lands on PodSpec, the fixture
// is the one place that needs to grow.
func canonicalPodSpec() PodSpec {
	return PodSpec{
		Name:               "apprentice-spi-abc-0",
		Namespace:          "spire-test",
		Image:              "spire-agent:dev",
		ServiceAccountName: "spire-apprentice",
		AgentName:          "apprentice-spi-abc-0",
		BeadID:             "spi-abc",
		AttemptID:          "spi-att9",
		RunID:              "run-xyz",
		ApprenticeIdx:      "0",
		FormulaStep:        "implement",
		Identity: runtime.RepoIdentity{
			TowerName:  "test-tower",
			Prefix:     "spi",
			RepoURL:    "https://github.com/example/repo.git",
			BaseBranch: "main",
		},
		DolthubRemote: "https://dolthub.test/example/repo",
		Workspace: runtime.WorkspaceHandle{
			Name:       "spi-abc-impl",
			Kind:       runtime.WorkspaceKindBorrowedWorktree,
			Branch:     "spi-abc/implement",
			BaseBranch: "main",
			Path:       "/workspace/spi",
			Origin:     runtime.WorkspaceOriginOriginClone,
			Borrowed:   true,
		},
		HandoffMode:  runtime.HandoffBorrowed,
		Backend:      "k8s",
		Provider:     "claude",
		DoltURL:      "spire-dolt.spire-test.svc:3306",
		OTLPEndpoint: "http://spire-steward.spire-test.svc:4317",
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceMemory: resource.MustParse("1Gi"),
				corev1.ResourceCPU:    resource.MustParse("500m"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceMemory: resource.MustParse("4Gi"),
				corev1.ResourceCPU:    resource.MustParse("2000m"),
			},
		},
	}
}

// TestBuildApprenticePod_GoldenShape pins the canonical apprentice pod
// structure: pod-level fields (name, namespace, restart policy,
// priority class, service account), volumes, init container order, and
// the main container's command / working set.
func TestBuildApprenticePod_GoldenShape(t *testing.T) {
	pod, err := BuildApprenticePod(canonicalPodSpec())
	if err != nil {
		t.Fatalf("BuildApprenticePod: %v", err)
	}

	if pod.Name != "apprentice-spi-abc-0" {
		t.Errorf("pod.Name = %q, want apprentice-spi-abc-0", pod.Name)
	}
	if pod.Namespace != "spire-test" {
		t.Errorf("pod.Namespace = %q, want spire-test", pod.Namespace)
	}
	if pod.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("RestartPolicy = %q, want %q", pod.Spec.RestartPolicy, corev1.RestartPolicyNever)
	}
	if pod.Spec.PriorityClassName != DefaultPriorityClassName {
		t.Errorf("PriorityClassName = %q, want %q", pod.Spec.PriorityClassName, DefaultPriorityClassName)
	}
	if pod.Spec.ServiceAccountName != "spire-apprentice" {
		t.Errorf("ServiceAccountName = %q, want spire-apprentice", pod.Spec.ServiceAccountName)
	}

	// Init container order matters: tower-attach stages /data first,
	// repo-bootstrap then clones into /workspace. Swapping the order
	// would break the bind-local write target.
	if len(pod.Spec.InitContainers) != 2 {
		t.Fatalf("InitContainers count = %d, want 2", len(pod.Spec.InitContainers))
	}
	if pod.Spec.InitContainers[0].Name != "tower-attach" {
		t.Errorf("InitContainers[0].Name = %q, want tower-attach", pod.Spec.InitContainers[0].Name)
	}
	if pod.Spec.InitContainers[1].Name != "repo-bootstrap" {
		t.Errorf("InitContainers[1].Name = %q, want repo-bootstrap", pod.Spec.InitContainers[1].Name)
	}

	// Main container shape.
	if len(pod.Spec.Containers) != 1 {
		t.Fatalf("Containers count = %d, want 1", len(pod.Spec.Containers))
	}
	main := pod.Spec.Containers[0]
	if main.Name != "agent" {
		t.Errorf("main.Name = %q, want agent", main.Name)
	}
	if main.Image != "spire-agent:dev" {
		t.Errorf("main.Image = %q, want spire-agent:dev", main.Image)
	}
	// Command: `spire apprentice run <bead> --name <agent>`.
	wantCmd := []string{"spire", "apprentice", "run", "spi-abc", "--name", "apprentice-spi-abc-0"}
	if !stringSliceEq(main.Command, wantCmd) {
		t.Errorf("main.Command = %v, want %v", main.Command, wantCmd)
	}
}

// TestBuildApprenticePod_RequiredEnv pins the canonical env set the
// apprentice pod MUST carry: DOLT_URL, the bead/attempt/repo identity
// vocabulary, handoff mode, and the full RunContext field set. Missing
// any of these is a contract violation — wizard/sage/handoff callers
// downstream assume they are set.
func TestBuildApprenticePod_RequiredEnv(t *testing.T) {
	pod, err := BuildApprenticePod(canonicalPodSpec())
	if err != nil {
		t.Fatalf("BuildApprenticePod: %v", err)
	}

	env := envByName(pod.Spec.Containers[0].Env)

	wantLiteral := map[string]string{
		// DOLT connection (explicit task requirement).
		"DOLT_URL": "spire-dolt.spire-test.svc:3306",
		// Bead / repo identity.
		"SPIRE_BEAD_ID":     "spi-abc",
		"SPIRE_REPO_PREFIX": "spi",
		"SPIRE_REPO_URL":    "https://github.com/example/repo.git",
		"SPIRE_REPO_BRANCH": "main",
		// Handoff mode.
		"SPIRE_HANDOFF_MODE": string(runtime.HandoffBorrowed),
		// RunContext vocabulary.
		"SPIRE_TOWER":            "test-tower",
		"SPIRE_ROLE":             string(RoleApprentice),
		"SPIRE_ATTEMPT_ID":       "spi-att9",
		"SPIRE_RUN_ID":           "run-xyz",
		"SPIRE_APPRENTICE_IDX":   "0",
		"SPIRE_FORMULA_STEP":     "implement",
		"SPIRE_BACKEND":          "k8s",
		"SPIRE_WORKSPACE_KIND":   string(runtime.WorkspaceKindBorrowedWorktree),
		"SPIRE_WORKSPACE_NAME":   "spi-abc-impl",
		"SPIRE_WORKSPACE_ORIGIN": string(runtime.WorkspaceOriginOriginClone),
		"SPIRE_WORKSPACE_PATH":   "/workspace/spi",
		// Provider + substrate.
		"SPIRE_PROVIDER":   "claude",
		"DOLT_DATA_DIR":    DataMountPath,
		"SPIRE_CONFIG_DIR": DataMountPath + "/spire-config",
		"BEADS_DATABASE":   "test-tower",
		"BEADS_PREFIX":     "spi",
		"DOLTHUB_REMOTE":   "https://dolthub.test/example/repo",
		// OTLP.
		"OTEL_EXPORTER_OTLP_ENDPOINT": "http://spire-steward.spire-test.svc:4317",
	}
	for k, want := range wantLiteral {
		got, ok := env[k]
		if !ok {
			t.Errorf("missing required env %s", k)
			continue
		}
		if got.Value != want {
			t.Errorf("env %s = %q, want %q", k, got.Value, want)
		}
	}

	// ANTHROPIC_API_KEY must be wired as a SecretKeyRef, not a literal.
	apiKey, ok := env["ANTHROPIC_API_KEY"]
	if !ok {
		t.Fatal("missing ANTHROPIC_API_KEY env var")
	}
	if apiKey.ValueFrom == nil || apiKey.ValueFrom.SecretKeyRef == nil {
		t.Fatal("ANTHROPIC_API_KEY must use SecretKeyRef")
	}
	if apiKey.ValueFrom.SecretKeyRef.Name != DefaultCredentialsSecret {
		t.Errorf("ANTHROPIC_API_KEY secret name = %q, want %q",
			apiKey.ValueFrom.SecretKeyRef.Name, DefaultCredentialsSecret)
	}
	if apiKey.ValueFrom.SecretKeyRef.Key != "ANTHROPIC_API_KEY_DEFAULT" {
		t.Errorf("ANTHROPIC_API_KEY secret key = %q, want ANTHROPIC_API_KEY_DEFAULT",
			apiKey.ValueFrom.SecretKeyRef.Key)
	}

	// GITHUB_TOKEN must be wired as an optional SecretKeyRef.
	ghToken, ok := env["GITHUB_TOKEN"]
	if !ok {
		t.Fatal("missing GITHUB_TOKEN env var")
	}
	if ghToken.ValueFrom == nil || ghToken.ValueFrom.SecretKeyRef == nil {
		t.Fatal("GITHUB_TOKEN must use SecretKeyRef")
	}
	if ghToken.ValueFrom.SecretKeyRef.Optional == nil || !*ghToken.ValueFrom.SecretKeyRef.Optional {
		t.Error("GITHUB_TOKEN must be Optional=true")
	}
}

// TestBuildApprenticePod_NoStaleEnv guards against leaking deprecated
// env keys into the canonical shape. Matches the guidance in the
// sibling parity task (spi-v8l62) that forbids any key containing
// "local_path", plus explicit keys retired during the runtime-contract
// migration.
func TestBuildApprenticePod_NoStaleEnv(t *testing.T) {
	pod, err := BuildApprenticePod(canonicalPodSpec())
	if err != nil {
		t.Fatalf("BuildApprenticePod: %v", err)
	}

	forbiddenSubstrings := []string{
		"local_path",
		"LOCAL_PATH",
		"LocalPath",
	}

	// Explicit stale keys from older dispatch sites. Keeping the list
	// small and specific so the test remains legible; extend here when
	// a key is formally retired.
	forbiddenExact := map[string]struct{}{
		// Cluster apprentice pods must not read the scheduler's own
		// instance identity — that was a dispatch-time anti-pattern.
		"SPIRE_INSTANCE_ID": {},
	}

	for _, c := range append(pod.Spec.Containers, pod.Spec.InitContainers...) {
		for _, e := range c.Env {
			for _, sub := range forbiddenSubstrings {
				if strings.Contains(e.Name, sub) {
					t.Errorf("container %q: forbidden env %q (contains %q)",
						c.Name, e.Name, sub)
				}
			}
			if _, bad := forbiddenExact[e.Name]; bad {
				t.Errorf("container %q: forbidden env %q", c.Name, e.Name)
			}
		}
	}
}

// TestBuildApprenticePod_RequiredVolumes pins the canonical volume set:
// /data + /workspace on every apprentice pod; /spire/cache only when
// the caller opts in via CachePVCName.
func TestBuildApprenticePod_RequiredVolumes(t *testing.T) {
	pod, err := BuildApprenticePod(canonicalPodSpec())
	if err != nil {
		t.Fatalf("BuildApprenticePod: %v", err)
	}

	volumes := volumeByName(pod.Spec.Volumes)
	for _, name := range []string{"data", "workspace"} {
		v, ok := volumes[name]
		if !ok {
			t.Errorf("missing volume %q", name)
			continue
		}
		if v.EmptyDir == nil {
			t.Errorf("volume %q: want EmptyDir source, got %+v", name, v.VolumeSource)
		}
	}
	// Cache volume must be absent by default (no CachePVCName set).
	if _, ok := volumes["repo-cache"]; ok {
		t.Error("unexpected repo-cache volume when CachePVCName is unset")
	}

	// Main container mounts: /data + /workspace.
	main := pod.Spec.Containers[0]
	mainMounts := mountByPath(main.VolumeMounts)
	if mainMounts[DataMountPath].Name != "data" {
		t.Errorf("main container %s mount name = %q, want data",
			DataMountPath, mainMounts[DataMountPath].Name)
	}
	if mainMounts[DefaultWorkspaceMountPath].Name != "workspace" {
		t.Errorf("main container %s mount name = %q, want workspace",
			DefaultWorkspaceMountPath, mainMounts[DefaultWorkspaceMountPath].Name)
	}

	// tower-attach mounts /data only; repo-bootstrap mounts both.
	ta := pod.Spec.InitContainers[0]
	taMounts := mountByPath(ta.VolumeMounts)
	if len(taMounts) != 1 || taMounts[DataMountPath].Name != "data" {
		t.Errorf("tower-attach mounts = %+v, want {/data:data}", ta.VolumeMounts)
	}
	rb := pod.Spec.InitContainers[1]
	rbMounts := mountByPath(rb.VolumeMounts)
	if rbMounts[DataMountPath].Name != "data" {
		t.Errorf("repo-bootstrap /data mount = %q, want data", rbMounts[DataMountPath].Name)
	}
	if rbMounts[DefaultWorkspaceMountPath].Name != "workspace" {
		t.Errorf("repo-bootstrap /workspace mount = %q, want workspace",
			rbMounts[DefaultWorkspaceMountPath].Name)
	}
}

// TestBuildApprenticePod_CachePVCOverlay verifies the optional cache
// PVC path: non-empty CachePVCName produces a read-only repo-cache
// volume at CacheMountPath with the correct ClaimName.
func TestBuildApprenticePod_CachePVCOverlay(t *testing.T) {
	spec := canonicalPodSpec()
	spec.CachePVCName = "core-repo-cache"

	pod, err := BuildApprenticePod(spec)
	if err != nil {
		t.Fatalf("BuildApprenticePod: %v", err)
	}

	volumes := volumeByName(pod.Spec.Volumes)
	cache, ok := volumes["repo-cache"]
	if !ok {
		t.Fatal("missing repo-cache volume")
	}
	if cache.PersistentVolumeClaim == nil {
		t.Fatalf("repo-cache VolumeSource = %+v, want PVC", cache.VolumeSource)
	}
	if cache.PersistentVolumeClaim.ClaimName != "core-repo-cache" {
		t.Errorf("repo-cache ClaimName = %q, want core-repo-cache",
			cache.PersistentVolumeClaim.ClaimName)
	}
	if !cache.PersistentVolumeClaim.ReadOnly {
		t.Error("repo-cache ReadOnly = false, want true")
	}

	// Main container should have a ReadOnly mount at CacheMountPath.
	mainMounts := mountByPath(pod.Spec.Containers[0].VolumeMounts)
	mount, ok := mainMounts[CacheMountPath]
	if !ok {
		t.Fatalf("missing main container mount at %s", CacheMountPath)
	}
	if mount.Name != "repo-cache" {
		t.Errorf("main cache mount name = %q, want repo-cache", mount.Name)
	}
	if !mount.ReadOnly {
		t.Error("main cache mount ReadOnly = false, want true")
	}
}

// TestBuildApprenticePod_SharedWorkspacePVC verifies that a non-empty
// SharedWorkspacePVCName routes /workspace to a PVC instead of emptyDir.
func TestBuildApprenticePod_SharedWorkspacePVC(t *testing.T) {
	spec := canonicalPodSpec()
	spec.SharedWorkspacePVCName = "wizard-spi-abc-workspace"

	pod, err := BuildApprenticePod(spec)
	if err != nil {
		t.Fatalf("BuildApprenticePod: %v", err)
	}

	workspace := volumeByName(pod.Spec.Volumes)["workspace"]
	if workspace.EmptyDir != nil {
		t.Error("workspace unexpectedly backed by EmptyDir when SharedWorkspacePVCName is set")
	}
	if workspace.PersistentVolumeClaim == nil {
		t.Fatalf("workspace VolumeSource = %+v, want PVC", workspace.VolumeSource)
	}
	if got := workspace.PersistentVolumeClaim.ClaimName; got != "wizard-spi-abc-workspace" {
		t.Errorf("workspace PVC ClaimName = %q, want wizard-spi-abc-workspace", got)
	}
}

// TestBuildApprenticePod_CanonicalLabels pins the spire.io/* label
// vocabulary and the legacy spire.* labels. Attempt/run IDs MUST live
// on annotations, not labels, so metric cardinality stays bounded.
func TestBuildApprenticePod_CanonicalLabels(t *testing.T) {
	pod, err := BuildApprenticePod(canonicalPodSpec())
	if err != nil {
		t.Fatalf("BuildApprenticePod: %v", err)
	}

	wantLabels := map[string]string{
		// Canonical spire.io/* vocabulary.
		LabelBackend:         "k8s",
		LabelTower:           "test-tower",
		LabelPrefix:          "spi",
		LabelBead:            "spi-abc",
		LabelRole:            string(RoleApprentice),
		LabelFormulaStep:     "implement",
		LabelWorkspaceKind:   string(runtime.WorkspaceKindBorrowedWorktree),
		LabelWorkspaceName:   "spi-abc-impl",
		LabelWorkspaceOrigin: string(runtime.WorkspaceOriginOriginClone),
		LabelHandoffMode:     string(runtime.HandoffBorrowed),
		// Legacy labels preserved byte-for-byte.
		"spire.agent":      "true",
		"spire.agent.name": "apprentice-spi-abc-0",
		"spire.bead":       "spi-abc",
		"spire.role":       string(RoleApprentice),
		"spire.tower":      "test-tower",
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

	// Annotations carry high-cardinality identifiers.
	if got := pod.Annotations[AnnotationAttemptID]; got != "spi-att9" {
		t.Errorf("annotation %s = %q, want spi-att9", AnnotationAttemptID, got)
	}
	if got := pod.Annotations[AnnotationRunID]; got != "run-xyz" {
		t.Errorf("annotation %s = %q, want run-xyz", AnnotationRunID, got)
	}
	// Attempt / run IDs MUST NOT leak into labels (unbounded cardinality).
	for _, k := range []string{AnnotationAttemptID, AnnotationRunID} {
		if _, ok := pod.Labels[k]; ok {
			t.Errorf("high-cardinality key %q leaked into labels", k)
		}
	}
}

// TestBuildApprenticePod_Resources verifies the main container carries
// whatever the caller plumbed through in PodSpec.Resources — no env
// override lookup, no silent default.
func TestBuildApprenticePod_Resources(t *testing.T) {
	pod, err := BuildApprenticePod(canonicalPodSpec())
	if err != nil {
		t.Fatalf("BuildApprenticePod: %v", err)
	}

	r := pod.Spec.Containers[0].Resources
	checks := []struct {
		label string
		got   resource.Quantity
		want  string
	}{
		{"Requests[memory]", r.Requests[corev1.ResourceMemory], "1Gi"},
		{"Requests[cpu]", r.Requests[corev1.ResourceCPU], "500m"},
		{"Limits[memory]", r.Limits[corev1.ResourceMemory], "4Gi"},
		{"Limits[cpu]", r.Limits[corev1.ResourceCPU], "2000m"},
	}
	for _, c := range checks {
		w := resource.MustParse(c.want)
		if !c.got.Equal(w) {
			t.Errorf("%s = %s, want %s", c.label, c.got.String(), w.String())
		}
	}
}

// TestBuildApprenticePod_RejectsMissingRequired exercises every
// required-input failure path so future refactors cannot silently
// default missing fields to empty strings.
func TestBuildApprenticePod_RejectsMissingRequired(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*PodSpec)
		want error
	}{
		{"missing Name", func(s *PodSpec) { s.Name = "" }, ErrPodSpecName},
		{"missing Namespace", func(s *PodSpec) { s.Namespace = "" }, ErrPodSpecNamespace},
		{"missing Image", func(s *PodSpec) { s.Image = "" }, ErrPodSpecImage},
		{"missing BeadID", func(s *PodSpec) { s.BeadID = "" }, ErrPodSpecBeadID},
		{"missing TowerName", func(s *PodSpec) { s.Identity.TowerName = "" }, ErrPodSpecTower},
		{"missing RepoURL", func(s *PodSpec) { s.Identity.RepoURL = "" }, ErrPodSpecIdentity},
		{"missing BaseBranch", func(s *PodSpec) { s.Identity.BaseBranch = "" }, ErrPodSpecIdentity},
		{"missing Prefix", func(s *PodSpec) { s.Identity.Prefix = "" }, ErrPodSpecIdentity},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := canonicalPodSpec()
			tc.mut(&spec)
			_, err := BuildApprenticePod(spec)
			if err == nil {
				t.Fatalf("want error for %s, got nil", tc.name)
			}
			if !errors.Is(err, tc.want) {
				t.Errorf("error = %v, want errors.Is(%v)", err, tc.want)
			}
		})
	}
}

// TestBuildApprenticePod_Defaults pins the optional-field defaulting
// behavior so callers can rely on the documented defaults without
// specifying every knob.
func TestBuildApprenticePod_Defaults(t *testing.T) {
	spec := canonicalPodSpec()
	spec.CredentialsSecret = ""
	spec.RestartPolicy = ""
	spec.PriorityClassName = ""
	spec.Backend = ""

	pod, err := BuildApprenticePod(spec)
	if err != nil {
		t.Fatalf("BuildApprenticePod: %v", err)
	}

	if pod.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("RestartPolicy default = %q, want %q",
			pod.Spec.RestartPolicy, corev1.RestartPolicyNever)
	}
	if pod.Spec.PriorityClassName != DefaultPriorityClassName {
		t.Errorf("PriorityClassName default = %q, want %q",
			pod.Spec.PriorityClassName, DefaultPriorityClassName)
	}

	env := envByName(pod.Spec.Containers[0].Env)
	if got := env["SPIRE_BACKEND"].Value; got != DefaultApprenticeBackend {
		t.Errorf("SPIRE_BACKEND default = %q, want %q", got, DefaultApprenticeBackend)
	}
	apiKey := env["ANTHROPIC_API_KEY"]
	if apiKey.ValueFrom == nil || apiKey.ValueFrom.SecretKeyRef == nil {
		t.Fatal("ANTHROPIC_API_KEY missing SecretKeyRef")
	}
	if got := apiKey.ValueFrom.SecretKeyRef.Name; got != DefaultCredentialsSecret {
		t.Errorf("CredentialsSecret default = %q, want %q", got, DefaultCredentialsSecret)
	}
}

// TestBuildApprenticePod_SplitsDoltURL verifies the DoltURL is emitted
// verbatim as DOLT_URL and also split into BEADS_DOLT_SERVER_HOST /
// BEADS_DOLT_SERVER_PORT so legacy in-pod clients continue to function.
func TestBuildApprenticePod_SplitsDoltURL(t *testing.T) {
	cases := []struct {
		url      string
		wantHost string
		wantPort string
	}{
		{"spire-dolt.spire.svc:3306", "spire-dolt.spire.svc", "3306"},
		{"mysql://spire-dolt.spire.svc:3306", "spire-dolt.spire.svc", "3306"},
	}
	for _, tc := range cases {
		t.Run(tc.url, func(t *testing.T) {
			spec := canonicalPodSpec()
			spec.DoltURL = tc.url

			pod, err := BuildApprenticePod(spec)
			if err != nil {
				t.Fatalf("BuildApprenticePod: %v", err)
			}
			env := envByName(pod.Spec.Containers[0].Env)
			if got := env["DOLT_URL"].Value; got != tc.url {
				t.Errorf("DOLT_URL = %q, want %q", got, tc.url)
			}
			if got := env["BEADS_DOLT_SERVER_HOST"].Value; got != tc.wantHost {
				t.Errorf("BEADS_DOLT_SERVER_HOST = %q, want %q", got, tc.wantHost)
			}
			if got := env["BEADS_DOLT_SERVER_PORT"].Value; got != tc.wantPort {
				t.Errorf("BEADS_DOLT_SERVER_PORT = %q, want %q", got, tc.wantPort)
			}
		})
	}
}

// TestBuildApprenticePod_InitContainerCommands pins the argv tokens of
// the two init containers so refactors that rename a CLI flag surface
// immediately rather than at cluster-startup time.
func TestBuildApprenticePod_InitContainerCommands(t *testing.T) {
	pod, err := BuildApprenticePod(canonicalPodSpec())
	if err != nil {
		t.Fatalf("BuildApprenticePod: %v", err)
	}

	ta := pod.Spec.InitContainers[0]
	wantPrefix := []string{"spire", "tower", "attach-cluster"}
	for i, w := range wantPrefix {
		if ta.Command[i] != w {
			t.Errorf("tower-attach Command[%d] = %q, want %q", i, ta.Command[i], w)
		}
	}
	wantFlags := []string{
		"--data-dir=" + DataMountPath + "/test-tower",
		"--database=test-tower",
		"--prefix=spi",
		"--dolthub-remote=https://dolthub.test/example/repo",
	}
	for _, want := range wantFlags {
		if !containsString(ta.Command, want) {
			t.Errorf("tower-attach Command missing %q; got %v", want, ta.Command)
		}
	}

	rb := pod.Spec.InitContainers[1]
	if len(rb.Command) < 3 || rb.Command[0] != "sh" || rb.Command[1] != "-c" {
		t.Fatalf("repo-bootstrap Command = %v, want [sh -c ...]", rb.Command)
	}
	for _, substr := range []string{"SPIRE_REPO_URL", "SPIRE_REPO_BRANCH", "SPIRE_REPO_PREFIX",
		"git clone", "spire repo bind-local"} {
		if !strings.Contains(rb.Command[2], substr) {
			t.Errorf("repo-bootstrap script missing %q", substr)
		}
	}
}

// TestBuildApprenticePod_ExtraArgs verifies formula-supplied args flow
// through to the main container unchanged (no mutation, no reordering).
func TestBuildApprenticePod_ExtraArgs(t *testing.T) {
	spec := canonicalPodSpec()
	spec.ExtraArgs = []string{"--review-fix", "--worktree-dir=/workspace/spi"}

	pod, err := BuildApprenticePod(spec)
	if err != nil {
		t.Fatalf("BuildApprenticePod: %v", err)
	}
	cmd := pod.Spec.Containers[0].Command
	// Command must end with the ExtraArgs in the order supplied.
	if len(cmd) < len(spec.ExtraArgs) {
		t.Fatalf("Command too short: %v", cmd)
	}
	tail := cmd[len(cmd)-len(spec.ExtraArgs):]
	if !stringSliceEq(tail, spec.ExtraArgs) {
		t.Errorf("Command tail = %v, want %v", tail, spec.ExtraArgs)
	}
}

// TestBuildApprenticePod_AuthSlot_Subscription verifies that AuthSlot
// = subscription routes the Anthropic credential to Secret key
// ANTHROPIC_SUBSCRIPTION_TOKEN under env var CLAUDE_CODE_OAUTH_TOKEN
// (the env name the `claude` CLI reads for Max/Team OAuth tokens) and
// that the SecretKeyRef is Optional so a partially-populated cluster
// secret does not block pod scheduling.
func TestBuildApprenticePod_AuthSlot_Subscription(t *testing.T) {
	spec := canonicalPodSpec()
	spec.AuthSlot = config.AuthSlotSubscription

	pod, err := BuildApprenticePod(spec)
	if err != nil {
		t.Fatalf("BuildApprenticePod: %v", err)
	}

	env := envByName(pod.Spec.Containers[0].Env)

	// API-key env var must be absent — the slot routing replaced it.
	if _, ok := env[config.EnvAnthropicAPIKey]; ok {
		t.Errorf("subscription slot must not emit %s env var; got %+v",
			config.EnvAnthropicAPIKey, env[config.EnvAnthropicAPIKey])
	}

	cred, ok := env[config.EnvClaudeCodeOAuthToken]
	if !ok {
		t.Fatalf("missing %s env var for subscription slot", config.EnvClaudeCodeOAuthToken)
	}
	if cred.ValueFrom == nil || cred.ValueFrom.SecretKeyRef == nil {
		t.Fatalf("%s must use SecretKeyRef, got %+v", config.EnvClaudeCodeOAuthToken, cred)
	}
	if got := cred.ValueFrom.SecretKeyRef.Key; got != SecretKeyAnthropicSubscriptionToken {
		t.Errorf("subscription SecretKeyRef Key = %q, want %q",
			got, SecretKeyAnthropicSubscriptionToken)
	}
	if got := cred.ValueFrom.SecretKeyRef.Name; got != DefaultCredentialsSecret {
		t.Errorf("subscription SecretKeyRef Name = %q, want %q",
			got, DefaultCredentialsSecret)
	}
	if cred.ValueFrom.SecretKeyRef.Optional == nil || !*cred.ValueFrom.SecretKeyRef.Optional {
		t.Error("subscription SecretKeyRef must be Optional=true so missing-token does not block scheduling")
	}
}

// TestBuildApprenticePod_AuthSlot_APIKey verifies the api-key slot
// routes to Secret key ANTHROPIC_API_KEY_DEFAULT under env var
// ANTHROPIC_API_KEY (existing default behavior, asserted explicitly so
// the routing does not silently regress when the slot is set).
func TestBuildApprenticePod_AuthSlot_APIKey(t *testing.T) {
	spec := canonicalPodSpec()
	spec.AuthSlot = config.AuthSlotAPIKey

	pod, err := BuildApprenticePod(spec)
	if err != nil {
		t.Fatalf("BuildApprenticePod: %v", err)
	}

	env := envByName(pod.Spec.Containers[0].Env)

	// Subscription env var must be absent.
	if _, ok := env[config.EnvClaudeCodeOAuthToken]; ok {
		t.Errorf("api-key slot must not emit %s env var", config.EnvClaudeCodeOAuthToken)
	}

	cred, ok := env[config.EnvAnthropicAPIKey]
	if !ok {
		t.Fatalf("missing %s env var for api-key slot", config.EnvAnthropicAPIKey)
	}
	if cred.ValueFrom == nil || cred.ValueFrom.SecretKeyRef == nil {
		t.Fatalf("%s must use SecretKeyRef", config.EnvAnthropicAPIKey)
	}
	if got := cred.ValueFrom.SecretKeyRef.Key; got != SecretKeyAnthropicAPIKey {
		t.Errorf("api-key SecretKeyRef Key = %q, want %q", got, SecretKeyAnthropicAPIKey)
	}
	// Back-compat: api-key SecretKeyRef must remain non-Optional so
	// installs that always populate apiKey fail fast on a missing key
	// rather than silently launching with no credential.
	if cred.ValueFrom.SecretKeyRef.Optional != nil {
		t.Errorf("api-key SecretKeyRef Optional = %v, want nil (back-compat)",
			*cred.ValueFrom.SecretKeyRef.Optional)
	}
}

// TestBuildApprenticePod_AuthSlot_EmptyDefault pins the back-compat
// path: an unset AuthSlot must produce the same env shape as before
// the two-slot routing landed (ANTHROPIC_API_KEY_DEFAULT key, env var
// ANTHROPIC_API_KEY, non-Optional).
func TestBuildApprenticePod_AuthSlot_EmptyDefault(t *testing.T) {
	spec := canonicalPodSpec()
	spec.AuthSlot = ""
	spec.AuthEnv = ""

	pod, err := BuildApprenticePod(spec)
	if err != nil {
		t.Fatalf("BuildApprenticePod: %v", err)
	}

	env := envByName(pod.Spec.Containers[0].Env)

	cred, ok := env["ANTHROPIC_API_KEY"]
	if !ok {
		t.Fatal("missing ANTHROPIC_API_KEY env var for empty AuthSlot (back-compat)")
	}
	if cred.ValueFrom == nil || cred.ValueFrom.SecretKeyRef == nil {
		t.Fatal("ANTHROPIC_API_KEY must use SecretKeyRef")
	}
	if got := cred.ValueFrom.SecretKeyRef.Key; got != "ANTHROPIC_API_KEY_DEFAULT" {
		t.Errorf("empty-slot SecretKeyRef Key = %q, want ANTHROPIC_API_KEY_DEFAULT (back-compat)", got)
	}
	if cred.ValueFrom.SecretKeyRef.Optional != nil {
		t.Errorf("empty-slot SecretKeyRef Optional = %v, want nil (back-compat)",
			*cred.ValueFrom.SecretKeyRef.Optional)
	}
	// Subscription env var must be absent.
	if _, ok := env[config.EnvClaudeCodeOAuthToken]; ok {
		t.Errorf("empty AuthSlot must not emit %s", config.EnvClaudeCodeOAuthToken)
	}
}

// TestBuildApprenticePod_AuthEnv_Override verifies that a non-empty
// AuthEnv replaces the slot's default env var name without changing
// the secret-key routing. Exercised on both slots so the override
// honours the slot-specific Optional flag.
func TestBuildApprenticePod_AuthEnv_Override(t *testing.T) {
	cases := []struct {
		name        string
		slot        string
		envOverride string
		wantKey     string
		wantOpt     bool
	}{
		{
			name:        "subscription with custom env",
			slot:        config.AuthSlotSubscription,
			envOverride: "MY_CUSTOM_OAUTH_TOKEN",
			wantKey:     SecretKeyAnthropicSubscriptionToken,
			wantOpt:     true,
		},
		{
			name:        "api-key with custom env",
			slot:        config.AuthSlotAPIKey,
			envOverride: "MY_CUSTOM_API_KEY",
			wantKey:     SecretKeyAnthropicAPIKey,
			wantOpt:     false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := canonicalPodSpec()
			spec.AuthSlot = tc.slot
			spec.AuthEnv = tc.envOverride

			pod, err := BuildApprenticePod(spec)
			if err != nil {
				t.Fatalf("BuildApprenticePod: %v", err)
			}

			env := envByName(pod.Spec.Containers[0].Env)

			cred, ok := env[tc.envOverride]
			if !ok {
				t.Fatalf("missing override env var %q; got %+v",
					tc.envOverride, mapKeys(env))
			}
			if cred.ValueFrom == nil || cred.ValueFrom.SecretKeyRef == nil {
				t.Fatalf("%s must use SecretKeyRef", tc.envOverride)
			}
			if got := cred.ValueFrom.SecretKeyRef.Key; got != tc.wantKey {
				t.Errorf("override SecretKeyRef Key = %q, want %q (slot routing must be unchanged)",
					got, tc.wantKey)
			}

			gotOpt := cred.ValueFrom.SecretKeyRef.Optional != nil &&
				*cred.ValueFrom.SecretKeyRef.Optional
			if gotOpt != tc.wantOpt {
				t.Errorf("override SecretKeyRef Optional = %v, want %v (slot must drive Optional)",
					gotOpt, tc.wantOpt)
			}

			// The slot's canonical env name must not also appear; the
			// override is a replacement, not an addition.
			canonical := config.EnvAnthropicAPIKey
			if tc.slot == config.AuthSlotSubscription {
				canonical = config.EnvClaudeCodeOAuthToken
			}
			if canonical == tc.envOverride {
				return
			}
			if _, leaked := env[canonical]; leaked {
				t.Errorf("override leaked canonical env var %q alongside override %q",
					canonical, tc.envOverride)
			}
		})
	}
}

// --- helpers ------------------------------------------------------------

func envByName(list []corev1.EnvVar) map[string]corev1.EnvVar {
	m := make(map[string]corev1.EnvVar, len(list))
	for _, e := range list {
		m[e.Name] = e
	}
	return m
}

func mapKeys(m map[string]corev1.EnvVar) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

func volumeByName(list []corev1.Volume) map[string]corev1.Volume {
	m := make(map[string]corev1.Volume, len(list))
	for _, v := range list {
		m[v.Name] = v
	}
	return m
}

func mountByPath(list []corev1.VolumeMount) map[string]corev1.VolumeMount {
	m := make(map[string]corev1.VolumeMount, len(list))
	for _, v := range list {
		m[v.MountPath] = v
	}
	return m
}

func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func stringSliceEq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
