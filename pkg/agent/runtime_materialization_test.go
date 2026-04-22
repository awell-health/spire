// Runtime-contract materialization tests. This file pins the
// spi-xplwy runtime contract (docs/design/spi-xplwy-runtime-contract.md
// §1 & §2) against BuildApprenticePod's output:
//
//   - RepoIdentity: every field (TowerName, Prefix, RepoURL, BaseBranch)
//     is reachable from either the main container's env or an init
//     container's CLI args before the apprentice process starts.
//   - WorkspaceHandle: the referenced path's mount volume exists on
//     the pod, and each handle field flows into SPIRE_WORKSPACE_* env
//     so in-pod tooling can materialize without re-parsing the name.
//   - HandoffMode: the executor-selected mode flows into env AND label
//     for every defined mode value.
//   - RunContext: every canonical RunContext field lands as a SPIRE_*
//     env var the apprentice process reads at startup.
//
// Tests are table-driven and backend-neutral. They use
// BuildApprenticePod + corev1 types directly — no cluster, no fakes.
package agent

import (
	"strings"
	"testing"

	"github.com/awell-health/spire/pkg/runtime"
)

// TestRuntimeMaterialization_RepoIdentity pins §2.1 of the contract:
// every required RepoIdentity field MUST flow into the pod's env or
// init container args. Regressing this leaves the apprentice
// re-deriving identity from ambient state.
func TestRuntimeMaterialization_RepoIdentity(t *testing.T) {
	spec := canonicalPodSpec()
	// Use distinct values so a source that accidentally reads from the
	// wrong field fails the assertion. The canonical fixture values
	// already differ, but we re-stamp them here to make the assertion
	// self-documenting.
	spec.Identity = runtime.RepoIdentity{
		TowerName:  "pin-tower",
		TowerID:    "tower-id-pin",
		Prefix:     "pin",
		RepoURL:    "https://github.com/pin/repo.git",
		BaseBranch: "trunk",
	}
	spec.DolthubRemote = "https://dolthub.test/pin/repo"

	pod, err := BuildApprenticePod(spec)
	if err != nil {
		t.Fatalf("BuildApprenticePod: %v", err)
	}
	mainEnv := envByName(pod.Spec.Containers[0].Env)

	// Main container env + labels. Table-driven so a missing field
	// surfaces as a single failing row, not a noisy multi-line dump.
	envCases := []struct {
		what string // logical field being tested
		key  string // env key or label key
		want string
		got  string
	}{
		// TowerName is the dolt database name — lives in three env
		// vars plus the spire.io/tower label.
		{"RepoIdentity.TowerName → SPIRE_TOWER", "SPIRE_TOWER", spec.Identity.TowerName, mainEnv["SPIRE_TOWER"].Value},
		{"RepoIdentity.TowerName → BEADS_DATABASE", "BEADS_DATABASE", spec.Identity.TowerName, mainEnv["BEADS_DATABASE"].Value},
		{"RepoIdentity.TowerName → LabelTower", LabelTower, spec.Identity.TowerName, pod.Labels[LabelTower]},
		{"RepoIdentity.TowerName → spire.tower (legacy)", "spire.tower", spec.Identity.TowerName, pod.Labels["spire.tower"]},

		// Prefix → two env vars + prefix label.
		{"RepoIdentity.Prefix → SPIRE_REPO_PREFIX", "SPIRE_REPO_PREFIX", spec.Identity.Prefix, mainEnv["SPIRE_REPO_PREFIX"].Value},
		{"RepoIdentity.Prefix → BEADS_PREFIX", "BEADS_PREFIX", spec.Identity.Prefix, mainEnv["BEADS_PREFIX"].Value},
		{"RepoIdentity.Prefix → LabelPrefix", LabelPrefix, spec.Identity.Prefix, pod.Labels[LabelPrefix]},

		// RepoURL + BaseBranch power the repo-bootstrap init container.
		{"RepoIdentity.RepoURL → SPIRE_REPO_URL", "SPIRE_REPO_URL", spec.Identity.RepoURL, mainEnv["SPIRE_REPO_URL"].Value},
		{"RepoIdentity.BaseBranch → SPIRE_REPO_BRANCH", "SPIRE_REPO_BRANCH", spec.Identity.BaseBranch, mainEnv["SPIRE_REPO_BRANCH"].Value},
	}
	for _, tc := range envCases {
		tc := tc
		t.Run(tc.what, func(t *testing.T) {
			if tc.got != tc.want {
				t.Errorf("%s = %q, want %q", tc.key, tc.got, tc.want)
			}
		})
	}

	// Init container args. tower-attach runs BEFORE the main container
	// so the contract requires the dolt identity to be on its argv,
	// not just in its env. Validate each CLI flag the apprentice
	// substrate depends on.
	ta := pod.Spec.InitContainers[0]
	if ta.Name != "tower-attach" {
		t.Fatalf("InitContainers[0].Name = %q, want tower-attach", ta.Name)
	}
	initFlagCases := []struct {
		what, flag string
	}{
		{"RepoIdentity.TowerName → tower-attach --database", "--database=" + spec.Identity.TowerName},
		{"RepoIdentity.TowerName → tower-attach --data-dir", "--data-dir=" + DataMountPath + "/" + spec.Identity.TowerName},
		{"RepoIdentity.Prefix → tower-attach --prefix", "--prefix=" + spec.Identity.Prefix},
	}
	for _, tc := range initFlagCases {
		tc := tc
		t.Run(tc.what, func(t *testing.T) {
			if !containsString(ta.Command, tc.flag) {
				t.Errorf("tower-attach missing %q; got %v", tc.flag, ta.Command)
			}
		})
	}

	// repo-bootstrap is `sh -c <script>`; it consumes the identity
	// from env (SPIRE_REPO_URL / SPIRE_REPO_BRANCH / SPIRE_REPO_PREFIX)
	// that the main container shares. Validate the script references
	// those keys so env and script stay in lockstep.
	rb := pod.Spec.InitContainers[1]
	if rb.Name != "repo-bootstrap" {
		t.Fatalf("InitContainers[1].Name = %q, want repo-bootstrap", rb.Name)
	}
	if len(rb.Command) < 3 || rb.Command[0] != "sh" || rb.Command[1] != "-c" {
		t.Fatalf("repo-bootstrap Command = %v, want [sh -c ...]", rb.Command)
	}
	script := rb.Command[2]
	for _, want := range []string{
		"SPIRE_REPO_URL",
		"SPIRE_REPO_BRANCH",
		"SPIRE_REPO_PREFIX",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("repo-bootstrap script missing env reference %q; got %q", want, script)
		}
	}
}

// TestRuntimeMaterialization_WorkspaceHandle pins §2.2 of the contract:
// the pod MUST expose the workspace as a mounted volume whose mount
// path is a prefix of WorkspaceHandle.Path, and every handle field
// (Kind, Name, Origin, Path) MUST be visible to the apprentice via
// SPIRE_WORKSPACE_* env.
func TestRuntimeMaterialization_WorkspaceHandle(t *testing.T) {
	spec := canonicalPodSpec()
	spec.Workspace = runtime.WorkspaceHandle{
		Name:       "pin-workspace",
		Kind:       runtime.WorkspaceKindOwnedWorktree,
		Branch:     "pin/impl",
		BaseBranch: "main",
		Path:       "/workspace/pin",
		Origin:     runtime.WorkspaceOriginOriginClone,
		Borrowed:   false,
	}
	pod, err := BuildApprenticePod(spec)
	if err != nil {
		t.Fatalf("BuildApprenticePod: %v", err)
	}
	mainEnv := envByName(pod.Spec.Containers[0].Env)

	// 1. SPIRE_WORKSPACE_* env is the canonical carrier of the handle.
	envCases := []struct {
		what, want, got string
	}{
		{"WorkspaceHandle.Kind → SPIRE_WORKSPACE_KIND",
			string(spec.Workspace.Kind), mainEnv["SPIRE_WORKSPACE_KIND"].Value},
		{"WorkspaceHandle.Name → SPIRE_WORKSPACE_NAME",
			spec.Workspace.Name, mainEnv["SPIRE_WORKSPACE_NAME"].Value},
		{"WorkspaceHandle.Origin → SPIRE_WORKSPACE_ORIGIN",
			string(spec.Workspace.Origin), mainEnv["SPIRE_WORKSPACE_ORIGIN"].Value},
		{"WorkspaceHandle.Path → SPIRE_WORKSPACE_PATH",
			spec.Workspace.Path, mainEnv["SPIRE_WORKSPACE_PATH"].Value},
	}
	for _, tc := range envCases {
		tc := tc
		t.Run(tc.what, func(t *testing.T) {
			if tc.got != tc.want {
				t.Errorf("got %q, want %q", tc.got, tc.want)
			}
		})
	}

	// 2. Labels carry the low-cardinality subset for metric slicing.
	labelCases := []struct {
		what, want, got string
	}{
		{"WorkspaceHandle.Kind → LabelWorkspaceKind",
			string(spec.Workspace.Kind), pod.Labels[LabelWorkspaceKind]},
		{"WorkspaceHandle.Name → LabelWorkspaceName",
			spec.Workspace.Name, pod.Labels[LabelWorkspaceName]},
		{"WorkspaceHandle.Origin → LabelWorkspaceOrigin",
			string(spec.Workspace.Origin), pod.Labels[LabelWorkspaceOrigin]},
	}
	for _, tc := range labelCases {
		tc := tc
		t.Run(tc.what, func(t *testing.T) {
			if tc.got != tc.want {
				t.Errorf("got %q, want %q", tc.got, tc.want)
			}
		})
	}

	// 3. The workspace volume MUST exist on the pod. The mount path
	// MUST be a prefix of WorkspaceHandle.Path — otherwise the path
	// points at a directory the main container cannot read, and the
	// apprentice reports a substrate error at startup.
	vols := volumeByName(pod.Spec.Volumes)
	ws, ok := vols["workspace"]
	if !ok {
		t.Fatal("workspace volume missing")
	}
	if ws.EmptyDir == nil && ws.PersistentVolumeClaim == nil {
		t.Errorf("workspace VolumeSource = %+v, want EmptyDir or PVC", ws.VolumeSource)
	}

	mainMounts := mountByPath(pod.Spec.Containers[0].VolumeMounts)
	wsMount, ok := mainMounts[DefaultWorkspaceMountPath]
	if !ok {
		t.Fatalf("main container missing workspace mount at %s", DefaultWorkspaceMountPath)
	}
	if wsMount.Name != "workspace" {
		t.Errorf("workspace mount name = %q, want workspace", wsMount.Name)
	}
	if !strings.HasPrefix(spec.Workspace.Path, DefaultWorkspaceMountPath) {
		t.Errorf("WorkspaceHandle.Path %q must be inside mount %q",
			spec.Workspace.Path, DefaultWorkspaceMountPath)
	}

	// The repo-bootstrap init container also needs the workspace
	// mount so `git clone ${dest}` lands on the shared volume.
	rb := pod.Spec.InitContainers[1]
	rbMounts := mountByPath(rb.VolumeMounts)
	if m, ok := rbMounts[DefaultWorkspaceMountPath]; !ok || m.Name != "workspace" {
		t.Errorf("repo-bootstrap missing workspace mount; got %+v", rb.VolumeMounts)
	}
}

// TestRuntimeMaterialization_WorkspaceHandle_SharedPVC verifies that
// SharedWorkspacePVCName materializes the workspace as a PVC-backed
// volume (the borrowed-worktree path) while leaving SPIRE_WORKSPACE_*
// env invariant — the apprentice resolves the workspace the same way
// regardless of the backing volume type.
func TestRuntimeMaterialization_WorkspaceHandle_SharedPVC(t *testing.T) {
	spec := canonicalPodSpec()
	spec.SharedWorkspacePVCName = "wizard-spi-abc-workspace"

	pod, err := BuildApprenticePod(spec)
	if err != nil {
		t.Fatalf("BuildApprenticePod: %v", err)
	}
	ws := volumeByName(pod.Spec.Volumes)["workspace"]
	if ws.PersistentVolumeClaim == nil {
		t.Fatalf("workspace VolumeSource = %+v, want PVC", ws.VolumeSource)
	}
	if got := ws.PersistentVolumeClaim.ClaimName; got != "wizard-spi-abc-workspace" {
		t.Errorf("workspace PVC ClaimName = %q, want wizard-spi-abc-workspace", got)
	}
	// SPIRE_WORKSPACE_PATH must still carry the handle's Path value.
	env := envByName(pod.Spec.Containers[0].Env)
	if got := env["SPIRE_WORKSPACE_PATH"].Value; got != spec.Workspace.Path {
		t.Errorf("SPIRE_WORKSPACE_PATH = %q, want %q", got, spec.Workspace.Path)
	}
}

// TestRuntimeMaterialization_HandoffMode pins §2.3 of the contract:
// the executor-selected HandoffMode MUST flow into pod env AND the
// spire.io/handoff-mode label for every defined mode value. Table-
// driven so regressions in a single mode show up with a minimal diff.
func TestRuntimeMaterialization_HandoffMode(t *testing.T) {
	cases := []struct {
		name string
		mode runtime.HandoffMode
	}{
		{"none", runtime.HandoffNone},
		{"borrowed", runtime.HandoffBorrowed},
		{"bundle", runtime.HandoffBundle},
		{"transitional", runtime.HandoffTransitional},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			spec := canonicalPodSpec()
			spec.HandoffMode = tc.mode
			pod, err := BuildApprenticePod(spec)
			if err != nil {
				t.Fatalf("BuildApprenticePod: %v", err)
			}

			env := envByName(pod.Spec.Containers[0].Env)
			got, ok := env["SPIRE_HANDOFF_MODE"]
			if !ok {
				t.Fatalf("SPIRE_HANDOFF_MODE env missing for mode %q", tc.mode)
			}
			if got.Value != string(tc.mode) {
				t.Errorf("SPIRE_HANDOFF_MODE = %q, want %q", got.Value, tc.mode)
			}
			if pod.Labels[LabelHandoffMode] != string(tc.mode) {
				t.Errorf("label %s = %q, want %q", LabelHandoffMode,
					pod.Labels[LabelHandoffMode], tc.mode)
			}
		})
	}
}

// TestRuntimeMaterialization_RunContext pins §2.3 of the contract: the
// canonical RunContext identity set MUST be reachable from the pod's
// main container env as SPIRE_* env vars so the apprentice emits
// consistent logs and traces across process, docker, and cluster
// backends. Every canonical field is checked — new fields added to
// runtime.RunContext must extend the assertion set here.
func TestRuntimeMaterialization_RunContext(t *testing.T) {
	// Use distinct sentinel values for every field so cross-wiring
	// (e.g. SPIRE_BEAD_ID reading from AttemptID) surfaces as a
	// specific mismatch rather than a silent pass.
	spec := canonicalPodSpec()
	spec.Identity.TowerName = "rc-tower"
	spec.Identity.Prefix = "rc"
	spec.BeadID = "rc-bead"
	spec.AttemptID = "rc-att"
	spec.RunID = "rc-run"
	spec.FormulaStep = "rc-step"
	spec.Backend = "rc-backend"
	spec.Workspace.Kind = runtime.WorkspaceKindStaging
	spec.Workspace.Name = "rc-workspace"
	spec.Workspace.Origin = runtime.WorkspaceOriginOriginClone
	spec.HandoffMode = runtime.HandoffBundle

	pod, err := BuildApprenticePod(spec)
	if err != nil {
		t.Fatalf("BuildApprenticePod: %v", err)
	}
	env := envByName(pod.Spec.Containers[0].Env)

	// The canonical RunContext the apprentice should see on startup.
	expected := runtime.RunContext{
		TowerName:       spec.Identity.TowerName,
		Prefix:          spec.Identity.Prefix,
		BeadID:          spec.BeadID,
		AttemptID:       spec.AttemptID,
		RunID:           spec.RunID,
		Role:            runtime.RoleApprentice,
		FormulaStep:     spec.FormulaStep,
		Backend:         spec.Backend,
		WorkspaceKind:   spec.Workspace.Kind,
		WorkspaceName:   spec.Workspace.Name,
		WorkspaceOrigin: spec.Workspace.Origin,
		HandoffMode:     spec.HandoffMode,
	}

	cases := []struct {
		what   string
		envKey string
		want   string
	}{
		{"RunContext.TowerName", "SPIRE_TOWER", expected.TowerName},
		{"RunContext.Prefix", "SPIRE_REPO_PREFIX", expected.Prefix},
		{"RunContext.BeadID", "SPIRE_BEAD_ID", expected.BeadID},
		{"RunContext.AttemptID", "SPIRE_ATTEMPT_ID", expected.AttemptID},
		{"RunContext.RunID", "SPIRE_RUN_ID", expected.RunID},
		{"RunContext.Role", "SPIRE_ROLE", string(expected.Role)},
		{"RunContext.FormulaStep", "SPIRE_FORMULA_STEP", expected.FormulaStep},
		{"RunContext.Backend", "SPIRE_BACKEND", expected.Backend},
		{"RunContext.WorkspaceKind", "SPIRE_WORKSPACE_KIND", string(expected.WorkspaceKind)},
		{"RunContext.WorkspaceName", "SPIRE_WORKSPACE_NAME", expected.WorkspaceName},
		{"RunContext.WorkspaceOrigin", "SPIRE_WORKSPACE_ORIGIN", string(expected.WorkspaceOrigin)},
		{"RunContext.HandoffMode", "SPIRE_HANDOFF_MODE", string(expected.HandoffMode)},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.what, func(t *testing.T) {
			got, ok := env[tc.envKey]
			if !ok {
				t.Fatalf("%s env missing", tc.envKey)
			}
			if got.Value != tc.want {
				t.Errorf("%s = %q, want %q", tc.envKey, got.Value, tc.want)
			}
		})
	}
}

// TestRuntimeMaterialization_RunContext_OptionalFields verifies that
// optional RunContext fields (AttemptID, RunID, ApprenticeIdx,
// FormulaStep) are ABSENT from env entirely when the spec leaves them
// empty — rather than being emitted as empty literals. Absent env
// distinguishes "not set" from "explicitly empty" in observability.
func TestRuntimeMaterialization_RunContext_OptionalFields(t *testing.T) {
	spec := canonicalPodSpec()
	spec.AttemptID = ""
	spec.RunID = ""
	spec.ApprenticeIdx = ""
	spec.FormulaStep = ""
	spec.Provider = ""
	spec.CustomPrompt = ""

	pod, err := BuildApprenticePod(spec)
	if err != nil {
		t.Fatalf("BuildApprenticePod: %v", err)
	}
	env := envByName(pod.Spec.Containers[0].Env)

	for _, key := range []string{
		"SPIRE_ATTEMPT_ID",
		"SPIRE_RUN_ID",
		"SPIRE_APPRENTICE_IDX",
		"SPIRE_FORMULA_STEP",
		"SPIRE_PROVIDER",
		"SPIRE_CUSTOM_PROMPT",
	} {
		if _, present := env[key]; present {
			t.Errorf("optional env %q present when spec field is empty", key)
		}
	}

	// Annotations must also drop — when BOTH AttemptID and RunID are
	// empty, buildAnnotations returns nil entirely.
	if pod.Annotations != nil {
		if _, ok := pod.Annotations[AnnotationAttemptID]; ok {
			t.Errorf("annotation %s present when AttemptID is empty", AnnotationAttemptID)
		}
		if _, ok := pod.Annotations[AnnotationRunID]; ok {
			t.Errorf("annotation %s present when RunID is empty", AnnotationRunID)
		}
	}
}

// TestRuntimeMaterialization_WorkspaceOrigin_AllVariants locks the
// origin enum to its string wire form on both env and label surfaces.
// The origin value feeds metric filters (e.g. "how many guild-cache
// apprentices this week"), so drift between code constant and label
// value breaks dashboards silently.
func TestRuntimeMaterialization_WorkspaceOrigin_AllVariants(t *testing.T) {
	cases := []struct {
		name   string
		origin runtime.WorkspaceOrigin
		wire   string
	}{
		{"local-bind", runtime.WorkspaceOriginLocalBind, "local-bind"},
		{"origin-clone", runtime.WorkspaceOriginOriginClone, "origin-clone"},
		{"guild-cache", runtime.WorkspaceOriginGuildCache, "guild-cache"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			spec := canonicalPodSpec()
			spec.Workspace.Origin = tc.origin
			pod, err := BuildApprenticePod(spec)
			if err != nil {
				t.Fatalf("BuildApprenticePod: %v", err)
			}
			env := envByName(pod.Spec.Containers[0].Env)
			if got := env["SPIRE_WORKSPACE_ORIGIN"].Value; got != tc.wire {
				t.Errorf("SPIRE_WORKSPACE_ORIGIN = %q, want %q", got, tc.wire)
			}
			if got := pod.Labels[LabelWorkspaceOrigin]; got != tc.wire {
				t.Errorf("label %s = %q, want %q", LabelWorkspaceOrigin, got, tc.wire)
			}
		})
	}
}

// TestRuntimeMaterialization_WorkspaceKind_AllVariants locks the kind
// enum to its string wire form. Same rationale as
// TestRuntimeMaterialization_WorkspaceOrigin_AllVariants.
func TestRuntimeMaterialization_WorkspaceKind_AllVariants(t *testing.T) {
	cases := []struct {
		name string
		kind runtime.WorkspaceKind
		wire string
	}{
		{"repo", runtime.WorkspaceKindRepo, "repo"},
		{"owned_worktree", runtime.WorkspaceKindOwnedWorktree, "owned_worktree"},
		{"borrowed_worktree", runtime.WorkspaceKindBorrowedWorktree, "borrowed_worktree"},
		{"staging", runtime.WorkspaceKindStaging, "staging"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			spec := canonicalPodSpec()
			spec.Workspace.Kind = tc.kind
			pod, err := BuildApprenticePod(spec)
			if err != nil {
				t.Fatalf("BuildApprenticePod: %v", err)
			}
			env := envByName(pod.Spec.Containers[0].Env)
			if got := env["SPIRE_WORKSPACE_KIND"].Value; got != tc.wire {
				t.Errorf("SPIRE_WORKSPACE_KIND = %q, want %q", got, tc.wire)
			}
			if got := pod.Labels[LabelWorkspaceKind]; got != tc.wire {
				t.Errorf("label %s = %q, want %q", LabelWorkspaceKind, got, tc.wire)
			}
		})
	}
}
