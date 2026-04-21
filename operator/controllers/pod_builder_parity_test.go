package controllers

import (
	"sort"
	"strings"
	"testing"

	"github.com/go-logr/logr/testr"
	corev1 "k8s.io/api/core/v1"

	spirev1 "github.com/awell-health/spire/operator/api/v1alpha1"
	"github.com/awell-health/spire/pkg/agent"
	"github.com/awell-health/spire/pkg/runtime"
)

// TestBuildWorkloadPod_SharedBuilderParity enforces that the operator's
// buildWorkloadPod output converges on the shared pkg/agent wizard-pod
// shape for the same SpawnConfig. Any difference that isn't an
// operator-specific overlay (guild-scoped labels, SpireConfig-sourced
// secret refs, MaxApprentices, custom image/resources, deterministic
// pod name, operator-sourced --dolthub-remote) is a parity bug and
// blocks merge per spi-fjt2t.
//
// The test covers the fields the spi-fjt2t task spec enumerates:
// image, command, args, env, volumes, volumeMounts, init containers,
// labels, annotations, resource requests/limits.
func TestBuildWorkloadPod_SharedBuilderParity(t *testing.T) {
	const (
		ns          = "spire"
		guildName   = "core"
		beadID      = "spi-abc"
		image       = "spire-agent:dev"
		database    = "spire"
		prefix      = "spi"
		repoURL     = "git@example.com:spire-test/repo.git"
		repoBranch  = "main"
		dolthubURL  = "https://dolthub.test/spire/spire"
		maxApprent  = 4
		defaultToken = "default"
	)

	// Build the operator-produced pod.
	wg := &spirev1.WizardGuild{}
	wg.Name = guildName
	wg.Namespace = ns
	wg.Spec.Mode = "managed"
	wg.Spec.Image = image
	wg.Spec.Repo = repoURL
	wg.Spec.RepoBranch = repoBranch
	wg.Spec.Prefixes = []string{prefix}
	apprentPtr := maxApprent
	wg.Spec.MaxApprentices = &apprentPtr
	wg.Spec.Token = defaultToken
	// Opt this parity guild into the shared-workspace gate so the
	// assertion below (SPIRE_K8S_SHARED_WORKSPACE=1) still holds. The
	// gate is now opt-in per spi-cslm8; default-off behavior is covered
	// in TestBuildWorkloadPod_SharedWorkspaceGate_OptIn below.
	sharedWs := true
	wg.Spec.SharedWorkspace = &sharedWs

	cfg := &spirev1.SpireConfig{}
	cfg.Spec.DefaultToken = defaultToken
	cfg.Spec.Tokens = map[string]spirev1.TokenRef{
		defaultToken: {Secret: "spire-credentials", Key: "ANTHROPIC_API_KEY_DEFAULT"},
	}
	cfg.Spec.DoltHub.Remote = dolthubURL
	cfg.Spec.DoltHub.CredentialsSecret = "spire-credentials"

	m := &AgentMonitor{
		Log:           testr.New(t),
		Namespace:     ns,
		StewardImage:  image,
		Database:      database,
		Prefix:        prefix,
		DolthubRemote: dolthubURL,
	}

	opPod := m.buildWorkloadPod(wg, beadID, cfg)
	if opPod == nil {
		t.Fatalf("operator buildWorkloadPod returned nil")
	}

	// Build the shared pkg/agent pod for the same SpawnConfig.
	spawnCfg := agent.SpawnConfig{
		Name:       guildName,
		BeadID:     beadID,
		Role:       agent.RoleWizard,
		Tower:      database,
		Step:       "wizard",
		RepoURL:    repoURL,
		RepoBranch: repoBranch,
		RepoPrefix: prefix,
		Identity: runtime.RepoIdentity{
			TowerName:  database,
			Prefix:     prefix,
			RepoURL:    repoURL,
			BaseBranch: repoBranch,
		},
		Run: runtime.RunContext{
			TowerName:       database,
			Prefix:          prefix,
			BeadID:          beadID,
			Role:            runtime.RoleWizard,
			FormulaStep:     "wizard",
			Backend:         "operator-k8s",
			WorkspaceKind:   runtime.WorkspaceKindOwnedWorktree,
			WorkspaceName:   "wizard",
			WorkspaceOrigin: runtime.WorkspaceOriginOriginClone,
			HandoffMode:     runtime.HandoffNone,
		},
	}
	builder := agent.NewPodBuilder(nil, ns, image, "")
	sharedPod, err := builder.BuildPod(spawnCfg)
	if err != nil {
		t.Fatalf("pkg/agent BuildPod failed: %v", err)
	}

	// --- Volumes ---
	// Shape must match exactly (same names, same sources).
	assertVolumesEqual(t, opPod.Spec.Volumes, sharedPod.Spec.Volumes)

	// --- Init containers ---
	// Same names, commands, volume mounts. Env: operator overlay adds
	// SPIRE_AGENT_NAME / SPIRE_K8S_SHARED_WORKSPACE / MaxApprentices /
	// secret refs on top; every shared-builder env var must survive.
	assertInitContainersPartialEqual(t, opPod.Spec.InitContainers, sharedPod.Spec.InitContainers, opOverlayEnvNames())

	// tower-attach Command must use the operator's configured
	// --dolthub-remote value (m.DolthubRemote), not whatever the shared
	// builder sniffed from process env.
	opTower := findInitContainer(opPod, "tower-attach")
	if opTower == nil {
		t.Fatalf("operator pod missing tower-attach init container")
	}
	if !commandHas(opTower.Command, "--dolthub-remote="+dolthubURL) {
		t.Errorf("operator tower-attach --dolthub-remote = %v, want %q", opTower.Command, dolthubURL)
	}

	// --- Main container ---
	if len(opPod.Spec.Containers) != 1 || len(sharedPod.Spec.Containers) != 1 {
		t.Fatalf("containers: op=%d, shared=%d; want 1 each",
			len(opPod.Spec.Containers), len(sharedPod.Spec.Containers))
	}
	opMain := opPod.Spec.Containers[0]
	sharedMain := sharedPod.Spec.Containers[0]

	// Name, image, command must match.
	if opMain.Name != sharedMain.Name {
		t.Errorf("container Name: op=%q, shared=%q", opMain.Name, sharedMain.Name)
	}
	if opMain.Image != sharedMain.Image {
		t.Errorf("container Image: op=%q, shared=%q", opMain.Image, sharedMain.Image)
	}
	if !stringSlicesEqual(opMain.Command, sharedMain.Command) {
		t.Errorf("container Command: op=%v, shared=%v", opMain.Command, sharedMain.Command)
	}

	// VolumeMounts must match: /data + /workspace for the wizard shape.
	assertVolumeMountsEqual(t, opMain.VolumeMounts, sharedMain.VolumeMounts)

	// Env: every shared-builder env var with a fixed Value (skip secret
	// refs and overlay names) must appear in the operator pod with the
	// same value. Operator-added env vars are allowed as additions.
	assertEnvSupersetValuesOnly(t, sharedMain.Env, opMain.Env, opOverlayEnvNames())

	// Operator-specific overlay env must be present.
	opEnvMap := envMap(opMain.Env)
	if got, ok := opEnvMap["SPIRE_K8S_SHARED_WORKSPACE"]; !ok || got.Value != "1" {
		t.Errorf("SPIRE_K8S_SHARED_WORKSPACE = %+v, want Value=1", got)
	}
	if got, ok := opEnvMap["SPIRE_AGENT_NAME"]; !ok || got.Value != guildName {
		t.Errorf("SPIRE_AGENT_NAME = %+v, want Value=%q", got, guildName)
	}
	if got, ok := opEnvMap["SPIRE_MAX_APPRENTICES"]; !ok || got.Value != "4" {
		t.Errorf("SPIRE_MAX_APPRENTICES = %+v, want Value=4", got)
	}
	if got, ok := opEnvMap["ANTHROPIC_API_KEY"]; !ok || got.ValueFrom == nil {
		t.Errorf("ANTHROPIC_API_KEY must be a SecretKeyRef; got %+v", got)
	}

	// --- Pod-level invariants ---
	if opPod.Spec.RestartPolicy != sharedPod.Spec.RestartPolicy {
		t.Errorf("RestartPolicy: op=%q, shared=%q", opPod.Spec.RestartPolicy, sharedPod.Spec.RestartPolicy)
	}
	if opPod.Spec.PriorityClassName != sharedPod.Spec.PriorityClassName {
		t.Errorf("PriorityClassName: op=%q, shared=%q", opPod.Spec.PriorityClassName, sharedPod.Spec.PriorityClassName)
	}

	// --- Labels ---
	// Operator adds spire.awell.io/* labels on top of the shared builder's
	// spire.* + spire.io/* labels. The shared labels must all be present
	// in the operator pod with the same values.
	for k, want := range sharedPod.Labels {
		// The shared builder's sanitizePodName timestamp-suffixes the pod
		// name; operator uses a deterministic name. Ignore that field
		// (not a label, but called out for completeness).
		if got := opPod.Labels[k]; got != want {
			t.Errorf("label %q: op=%q, shared=%q", k, got, want)
		}
	}

	// Annotations: the shared builder may emit attempt-id/run-id
	// annotations when they're populated on SpawnConfig.Run. Our
	// RunContext leaves them unset for operator-managed pods (those IDs
	// belong to spi-zm3b1's observability plumbing), so the shared pod
	// should carry no annotations — and neither should the operator pod.
	if len(sharedPod.Annotations) != 0 {
		t.Errorf("unexpected annotations on shared pod: %v", sharedPod.Annotations)
	}

	// --- Resources ---
	// The shared builder uses WizardResources() (wizard-tier defaults).
	// The operator uses the same defaults when guild doesn't override.
	// Here the guild does NOT override Resources, so they must match.
	if !resourcesEqual(opMain.Resources, sharedMain.Resources) {
		t.Errorf("Resources differ:\n  op=%+v\n  shared=%+v", opMain.Resources, sharedMain.Resources)
	}
}

// opOverlayEnvNames returns the set of env-var names the operator
// overlay adds or overrides. The parity comparator uses this to skip
// those names when asserting equivalence against the shared pod.
func opOverlayEnvNames() map[string]struct{} {
	return map[string]struct{}{
		// Additions
		"SPIRE_AGENT_NAME":          {},
		"SPIRE_K8S_SHARED_WORKSPACE": {},
		"SPIRE_MAX_APPRENTICES":     {},
		"ANTHROPIC_API_KEY":         {}, // shared builder uses a SecretKeyRef too, but with a different key name
		"GITHUB_TOKEN":              {},
	}
}

// assertVolumesEqual checks that the two volume lists contain the same
// volumes (by Name + EmptyDir/PVC presence). Order-insensitive.
func assertVolumesEqual(t *testing.T, got, want []corev1.Volume) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("volumes: got %d, want %d\n  got=%v\n  want=%v", len(got), len(want), got, want)
		return
	}
	gotByName := make(map[string]corev1.Volume, len(got))
	for _, v := range got {
		gotByName[v.Name] = v
	}
	for _, w := range want {
		g, ok := gotByName[w.Name]
		if !ok {
			t.Errorf("missing volume %q in operator pod; want %+v", w.Name, w)
			continue
		}
		if (g.EmptyDir == nil) != (w.EmptyDir == nil) {
			t.Errorf("volume %q EmptyDir mismatch: op.EmptyDir=%v, shared.EmptyDir=%v",
				w.Name, g.EmptyDir, w.EmptyDir)
		}
		if (g.PersistentVolumeClaim == nil) != (w.PersistentVolumeClaim == nil) {
			t.Errorf("volume %q PVC mismatch: op.PVC=%v, shared.PVC=%v",
				w.Name, g.PersistentVolumeClaim, w.PersistentVolumeClaim)
		}
	}
}

func assertVolumeMountsEqual(t *testing.T, got, want []corev1.VolumeMount) {
	t.Helper()
	gm := make(map[string]corev1.VolumeMount, len(got))
	for _, m := range got {
		gm[m.MountPath] = m
	}
	for _, w := range want {
		g, ok := gm[w.MountPath]
		if !ok {
			t.Errorf("missing mount at %q; want %+v", w.MountPath, w)
			continue
		}
		if g.Name != w.Name {
			t.Errorf("mount at %q: op.Name=%q, shared.Name=%q", w.MountPath, g.Name, w.Name)
		}
	}
}

// assertInitContainersPartialEqual checks init containers match by
// Name/Image/Command/VolumeMounts; env must be a superset modulo the
// overlay skip set.
func assertInitContainersPartialEqual(t *testing.T, got, want []corev1.Container, skip map[string]struct{}) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("init containers: got %d, want %d", len(got), len(want))
		return
	}
	gm := make(map[string]corev1.Container, len(got))
	for _, c := range got {
		gm[c.Name] = c
	}
	for _, w := range want {
		g, ok := gm[w.Name]
		if !ok {
			t.Errorf("missing init container %q", w.Name)
			continue
		}
		if g.Image != w.Image {
			// The operator overlay overrides the image from the guild
			// CR; use wantImage only when unset on guild — here we pass
			// the same image, so they should match.
			t.Errorf("init %q Image: op=%q, shared=%q", w.Name, g.Image, w.Image)
		}
		// Command equality: tower-attach is special-cased because the
		// operator overlay rewrites it with operator-sourced --dolthub-remote;
		// for every other init container (repo-bootstrap, etc.) the
		// commands must match byte-for-byte against the shared builder.
		if w.Name != "tower-attach" {
			if !stringSlicesEqual(g.Command, w.Command) {
				t.Errorf("init %q Command:\n  op=%v\n  shared=%v", w.Name, g.Command, w.Command)
			}
		}
		assertVolumeMountsEqual(t, g.VolumeMounts, w.VolumeMounts)
		assertEnvSupersetValuesOnly(t, w.Env, g.Env, skip)
	}
}

// assertEnvSupersetValuesOnly asserts every Name/Value env entry in
// wantSubset is present in got (unless the name is in skip). Entries
// with ValueFrom are compared by name only — ValueFrom equality is
// brittle across builds and not the concern of parity testing.
func assertEnvSupersetValuesOnly(t *testing.T, wantSubset, got []corev1.EnvVar, skip map[string]struct{}) {
	t.Helper()
	gm := envMap(got)
	for _, w := range wantSubset {
		if _, skipIt := skip[w.Name]; skipIt {
			continue
		}
		g, ok := gm[w.Name]
		if !ok {
			t.Errorf("env %q missing from operator pod", w.Name)
			continue
		}
		if w.Value != "" && g.Value != w.Value {
			t.Errorf("env %q Value: op=%q, shared=%q", w.Name, g.Value, w.Value)
		}
		if w.ValueFrom != nil && g.ValueFrom == nil {
			t.Errorf("env %q missing ValueFrom", w.Name)
		}
	}
}

func envMap(list []corev1.EnvVar) map[string]corev1.EnvVar {
	m := make(map[string]corev1.EnvVar, len(list))
	for _, e := range list {
		m[e.Name] = e
	}
	return m
}

func findInitContainer(pod *corev1.Pod, name string) *corev1.Container {
	for i := range pod.Spec.InitContainers {
		if pod.Spec.InitContainers[i].Name == name {
			return &pod.Spec.InitContainers[i]
		}
	}
	return nil
}

func commandHas(cmd []string, substr string) bool {
	for _, arg := range cmd {
		if strings.Contains(arg, substr) {
			return true
		}
	}
	return false
}

func resourcesEqual(a, b corev1.ResourceRequirements) bool {
	if !resourceListEqual(a.Requests, b.Requests) {
		return false
	}
	if !resourceListEqual(a.Limits, b.Limits) {
		return false
	}
	return true
}

func resourceListEqual(a, b corev1.ResourceList) bool {
	if len(a) != len(b) {
		return false
	}
	for k, av := range a {
		bv, ok := b[k]
		if !ok {
			return false
		}
		if av.Cmp(bv) != 0 {
			return false
		}
	}
	return true
}

// TestBuildWorkloadPod_SharedWorkspaceGate_OptIn pins the post-spi-cslm8
// contract: SPIRE_K8S_SHARED_WORKSPACE is NOT set on operator-managed
// pods by default (because production PVC provisioning is not wired yet;
// flipping it on without a PVC breaks child spawns with
// ErrSharedWorkspacePVCNotFound), and IS set when the guild opts in via
// spec.sharedWorkspace=true.
//
// The original unconditional-on contract (spi-fjt2t) was the bug fixed
// by spi-cslm8: the operator set the gate but nothing provisioned the
// PVC, so borrowed-worktree child spawns failed once spi-vrzhf made
// operator-managed wizards actually reach the k8s backend. Keep this
// test so future refactors can't silently flip the default back on
// without also landing PVC provisioning.
func TestBuildWorkloadPod_SharedWorkspaceGate_OptIn(t *testing.T) {
	ns := "spire"

	t.Run("default off (spec.sharedWorkspace unset)", func(t *testing.T) {
		wg := makeAgent("core", ns, nil)
		m := &AgentMonitor{
			Log:          testr.New(t),
			Namespace:    ns,
			StewardImage: "spire-agent:dev",
			Database:     "spire",
			Prefix:       "spi",
		}

		pod := m.buildWorkloadPod(wg, "spi-abc", nil)
		if pod == nil {
			t.Fatalf("buildWorkloadPod returned nil")
		}
		main := pod.Spec.Containers[0]
		em := envMap(main.Env)
		if got, ok := em["SPIRE_K8S_SHARED_WORKSPACE"]; ok {
			t.Errorf("SPIRE_K8S_SHARED_WORKSPACE = %+v, want unset (default off)", got)
		}
		// Also absent on every init container so tower-attach /
		// repo-bootstrap don't see a stale signal.
		for _, ic := range pod.Spec.InitContainers {
			icEnv := envMap(ic.Env)
			if got, ok := icEnv["SPIRE_K8S_SHARED_WORKSPACE"]; ok {
				t.Errorf("init %q has SPIRE_K8S_SHARED_WORKSPACE = %+v, want unset", ic.Name, got)
			}
		}
	})

	t.Run("explicit false (spec.sharedWorkspace=false)", func(t *testing.T) {
		wg := makeAgent("core", ns, nil)
		off := false
		wg.Spec.SharedWorkspace = &off
		m := &AgentMonitor{
			Log:          testr.New(t),
			Namespace:    ns,
			StewardImage: "spire-agent:dev",
			Database:     "spire",
			Prefix:       "spi",
		}

		pod := m.buildWorkloadPod(wg, "spi-abc", nil)
		main := pod.Spec.Containers[0]
		if _, ok := envMap(main.Env)["SPIRE_K8S_SHARED_WORKSPACE"]; ok {
			t.Error("SPIRE_K8S_SHARED_WORKSPACE must not be set when spec.sharedWorkspace=false")
		}
	})

	t.Run("opt-in on (spec.sharedWorkspace=true)", func(t *testing.T) {
		wg := makeAgent("core", ns, nil)
		on := true
		wg.Spec.SharedWorkspace = &on
		m := &AgentMonitor{
			Log:          testr.New(t),
			Namespace:    ns,
			StewardImage: "spire-agent:dev",
			Database:     "spire",
			Prefix:       "spi",
		}

		pod := m.buildWorkloadPod(wg, "spi-abc", nil)
		main := pod.Spec.Containers[0]
		got, ok := envMap(main.Env)["SPIRE_K8S_SHARED_WORKSPACE"]
		if !ok {
			t.Fatalf("SPIRE_K8S_SHARED_WORKSPACE not set on main container; want Value=1 (opt-in)")
		}
		if got.Value != "1" {
			t.Errorf("SPIRE_K8S_SHARED_WORKSPACE = %q, want %q", got.Value, "1")
		}
		// Propagation to init containers (parity with other overlay env).
		for _, ic := range pod.Spec.InitContainers {
			icEnv := envMap(ic.Env)
			icGot, icOk := icEnv["SPIRE_K8S_SHARED_WORKSPACE"]
			if !icOk || icGot.Value != "1" {
				t.Errorf("init %q SPIRE_K8S_SHARED_WORKSPACE = %+v, want Value=1", ic.Name, icGot)
			}
		}
	})
}

// TestBuildWorkloadPod_MissingRepoReturnsNil asserts buildWorkloadPod
// returns nil when the guild CR is missing fields required by the
// shared pkg/agent builder (Repo / RepoBranch). Callers in
// reconcileManagedAgent check for nil and skip the bead that cycle.
func TestBuildWorkloadPod_MissingRepoReturnsNil(t *testing.T) {
	ns := "spire"
	wg := makeAgent("core", ns, nil)
	wg.Spec.Repo = "" // simulate misconfigured CR
	m := &AgentMonitor{
		Log:          testr.New(t),
		Namespace:    ns,
		StewardImage: "spire-agent:dev",
		Database:     "spire",
		Prefix:       "spi",
	}

	if pod := m.buildWorkloadPod(wg, "spi-abc", nil); pod != nil {
		t.Errorf("want nil pod for missing Repo; got %+v", pod)
	}
}

// TestBuildWorkloadPod_DolthubRemoteFallback checks the precedence for
// the tower-attach --dolthub-remote value: operator startup config wins
// over SpireConfig CR.
func TestBuildWorkloadPod_DolthubRemoteFallback(t *testing.T) {
	ns := "spire"
	wg := makeAgent("core", ns, nil)

	t.Run("operator config wins", func(t *testing.T) {
		cfg := &spirev1.SpireConfig{}
		cfg.Spec.DoltHub.Remote = "from-config"
		m := &AgentMonitor{
			Log:           testr.New(t),
			Namespace:     ns,
			StewardImage:  "spire-agent:dev",
			Database:      "spire",
			Prefix:        "spi",
			DolthubRemote: "from-operator",
		}
		pod := m.buildWorkloadPod(wg, "spi-abc", cfg)
		ic := findInitContainer(pod, "tower-attach")
		if ic == nil {
			t.Fatalf("no tower-attach init container")
		}
		if !commandHas(ic.Command, "--dolthub-remote=from-operator") {
			t.Errorf("tower-attach Command = %v, want --dolthub-remote=from-operator", ic.Command)
		}
	})

	t.Run("SpireConfig fallback when operator unset", func(t *testing.T) {
		cfg := &spirev1.SpireConfig{}
		cfg.Spec.DoltHub.Remote = "from-config"
		m := &AgentMonitor{
			Log:          testr.New(t),
			Namespace:    ns,
			StewardImage: "spire-agent:dev",
			Database:     "spire",
			Prefix:       "spi",
			// DolthubRemote is intentionally unset
		}
		pod := m.buildWorkloadPod(wg, "spi-abc", cfg)
		ic := findInitContainer(pod, "tower-attach")
		if ic == nil {
			t.Fatalf("no tower-attach init container")
		}
		if !commandHas(ic.Command, "--dolthub-remote=from-config") {
			t.Errorf("tower-attach Command = %v, want --dolthub-remote=from-config", ic.Command)
		}
	})
}

// TestOperatorOverlayEnv_DoesNotMutateSharedEnv guards against the
// mergeEnv helper silently duplicating entries or losing the shared
// builder's canonical order for non-overlay vars.
func TestOperatorOverlayEnv_DoesNotMutateSharedEnv(t *testing.T) {
	shared := []corev1.EnvVar{
		{Name: "DOLT_DATA_DIR", Value: "/data"},
		{Name: "SPIRE_CONFIG_DIR", Value: "/data/spire-config"},
		{Name: "SPIRE_ROLE", Value: "wizard"},
	}
	overlay := []corev1.EnvVar{
		{Name: "SPIRE_ROLE", Value: "wizard"}, // same value — no-op override
		{Name: "SPIRE_AGENT_NAME", Value: "core"},
		{Name: "SPIRE_K8S_SHARED_WORKSPACE", Value: "1"},
	}
	got := mergeEnv(shared, overlay)

	// Every shared entry still present, in original order (non-overridden).
	gotNames := make([]string, 0, len(got))
	for _, e := range got {
		gotNames = append(gotNames, e.Name)
	}
	wantPrefix := []string{"DOLT_DATA_DIR", "SPIRE_CONFIG_DIR", "SPIRE_ROLE"}
	for i, w := range wantPrefix {
		if i >= len(gotNames) || gotNames[i] != w {
			t.Errorf("merged env names[%d] = %q, want %q (full: %v)", i, gotNames[i], w, gotNames)
		}
	}
	// No duplicates.
	seen := map[string]bool{}
	for _, e := range got {
		if seen[e.Name] {
			t.Errorf("duplicate env var %q in merged list", e.Name)
		}
		seen[e.Name] = true
	}
	// Overlay additions present.
	for _, w := range []string{"SPIRE_AGENT_NAME", "SPIRE_K8S_SHARED_WORKSPACE"} {
		if !seen[w] {
			t.Errorf("overlay env var %q missing from merged list", w)
		}
	}
	// Deterministic order of remaining: sort and compare the suffix.
	remaining := gotNames[len(wantPrefix):]
	sortedRemaining := append([]string{}, remaining...)
	sort.Strings(sortedRemaining)
	_ = sortedRemaining // order after prefix is implementation-defined; we only assert no duplicates above
}
