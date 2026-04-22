package controllers

import (
	"strings"
	"testing"

	"github.com/go-logr/logr/testr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	spirev1 "github.com/awell-health/spire/operator/api/v1alpha1"
	"github.com/awell-health/spire/pkg/agent"
)

// TestBuildWorkloadPod_CacheOverlay_VolumesAndMounts pins the phase-2
// cluster repo-cache contract on the pod-builder side: when
// WizardGuild.Spec.Cache is populated, the generated PodSpec must
// reference the reconciler-managed PVC as a read-only volume mounted
// at pkg/agent.CacheMountPath, and expose the writable workspace
// emptyDir at pkg/agent.WorkspaceMountPath. The main container's
// working directory must land on the materialized workspace so
// resolveBeadsDir / ResolveBackend find spire.yaml.
func TestBuildWorkloadPod_CacheOverlay_VolumesAndMounts(t *testing.T) {
	ns := "spire"
	wg := makeCachePodGuild("core", ns)
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

	// --- Volumes: cache PVC (read-only) alongside the pre-existing
	// data + workspace emptyDirs.
	volsByName := make(map[string]corev1.Volume, len(pod.Spec.Volumes))
	for _, v := range pod.Spec.Volumes {
		volsByName[v.Name] = v
	}
	cacheVol, ok := volsByName["repo-cache"]
	if !ok {
		t.Fatalf("pod missing repo-cache volume; have: %+v", pod.Spec.Volumes)
	}
	if cacheVol.PersistentVolumeClaim == nil {
		t.Fatalf("repo-cache volume is not a PVC reference: %+v", cacheVol)
	}
	if got := cacheVol.PersistentVolumeClaim.ClaimName; got != "core-repo-cache" {
		t.Errorf("cache volume ClaimName = %q, want %q (matches cache reconciler naming)", got, "core-repo-cache")
	}
	if !cacheVol.PersistentVolumeClaim.ReadOnly {
		t.Errorf("cache volume must be ReadOnly; got %+v", cacheVol.PersistentVolumeClaim)
	}
	// data + workspace must still exist so tower-attach and the main
	// container's canonical mounts keep working.
	if _, ok := volsByName["data"]; !ok {
		t.Errorf("pod missing data volume after cache overlay; have: %+v", pod.Spec.Volumes)
	}
	if _, ok := volsByName["workspace"]; !ok {
		t.Errorf("pod missing workspace volume after cache overlay; have: %+v", pod.Spec.Volumes)
	}

	// --- Main container mounts: cache at CacheMountPath (RO),
	// workspace remapped to WorkspaceMountPath (RW).
	if len(pod.Spec.Containers) != 1 {
		t.Fatalf("want 1 main container; got %d", len(pod.Spec.Containers))
	}
	main := pod.Spec.Containers[0]
	mountByPath := make(map[string]corev1.VolumeMount, len(main.VolumeMounts))
	for _, vm := range main.VolumeMounts {
		mountByPath[vm.MountPath] = vm
	}
	cacheMount, ok := mountByPath[agent.CacheMountPath]
	if !ok {
		t.Fatalf("main container missing mount at %s; have: %+v", agent.CacheMountPath, main.VolumeMounts)
	}
	if cacheMount.Name != "repo-cache" {
		t.Errorf("cache mount name = %q, want repo-cache", cacheMount.Name)
	}
	if !cacheMount.ReadOnly {
		t.Errorf("cache mount at %s must be ReadOnly; got %+v", agent.CacheMountPath, cacheMount)
	}
	wsMount, ok := mountByPath[agent.WorkspaceMountPath]
	if !ok {
		t.Fatalf("main container missing writable workspace mount at %s; have: %+v",
			agent.WorkspaceMountPath, main.VolumeMounts)
	}
	if wsMount.Name != "workspace" {
		t.Errorf("workspace mount name = %q, want workspace", wsMount.Name)
	}
	if wsMount.ReadOnly {
		t.Errorf("workspace mount at %s must be writable; got %+v", agent.WorkspaceMountPath, wsMount)
	}

	// WorkingDir must point at the materialized workspace so cwd-sensitive
	// code (resolveBeadsDir, ResolveBackend("")) lands inside the tree
	// MaterializeWorkspaceFromCache produced.
	if main.WorkingDir != agent.WorkspaceMountPath {
		t.Errorf("main.WorkingDir = %q, want %q (MaterializeWorkspaceFromCache clones into WorkspaceMountPath)",
			main.WorkingDir, agent.WorkspaceMountPath)
	}

	// The pre-cache "/workspace" mount must be retired — leaving it in
	// place would make two mount points reference the same volume and
	// would also defeat the WorkingDir rewrite above.
	if _, stale := mountByPath["/workspace"]; stale {
		t.Errorf("stale /workspace mount must be replaced by %s when cache overlay is active",
			agent.WorkspaceMountPath)
	}
}

// TestBuildWorkloadPod_CacheOverlay_InitContainerInvokesBootstrap
// verifies the init container contract: a single container named
// `cache-bootstrap` replaces the shared builder's `repo-bootstrap`. It
// must call `spire cluster cache-bootstrap` with flags naming CacheMountPath,
// WorkspaceMountPath, and the guild's repo prefix — matching the
// `cmd/spire/cache_bootstrap.go` entrypoint, which in turn calls
// agent.MaterializeWorkspaceFromCache then agent.BindLocalRepo.
func TestBuildWorkloadPod_CacheOverlay_InitContainerInvokesBootstrap(t *testing.T) {
	ns := "spire"
	wg := makeCachePodGuild("core", ns)
	m := &AgentMonitor{
		Log:          testr.New(t),
		Namespace:    ns,
		StewardImage: "spire-agent:dev",
		Database:     "spire",
		Prefix:       "spi",
	}

	pod := m.buildWorkloadPod(wg, "spi-abc", nil)

	// tower-attach stays. cache-bootstrap replaces repo-bootstrap.
	var bootstrap *corev1.Container
	for i := range pod.Spec.InitContainers {
		if pod.Spec.InitContainers[i].Name == "cache-bootstrap" {
			bootstrap = &pod.Spec.InitContainers[i]
		}
		if pod.Spec.InitContainers[i].Name == "repo-bootstrap" {
			t.Errorf("cache overlay must replace repo-bootstrap; found both")
		}
	}
	if bootstrap == nil {
		t.Fatalf("cache-bootstrap init container missing; got %+v", pod.Spec.InitContainers)
	}

	// Command shape: `spire cluster cache-bootstrap --cache-path=<CacheMountPath>
	// --workspace-path=<WorkspaceMountPath> --prefix=<prefix>`. Expressed
	// as substring checks so cosmetic flag-order reshuffles don't break
	// the test.
	cmd := strings.Join(bootstrap.Command, " ")
	wantPieces := []string{
		"spire", "cluster", "cache-bootstrap",
		"--cache-path=" + agent.CacheMountPath,
		"--workspace-path=" + agent.WorkspaceMountPath,
		"--prefix=spi",
	}
	for _, w := range wantPieces {
		if !strings.Contains(cmd, w) {
			t.Errorf("cache-bootstrap Command missing %q; got: %s", w, cmd)
		}
	}

	// The init container must mount both the cache PVC (RO) and the
	// workspace emptyDir (RW) so MaterializeWorkspaceFromCache can read
	// from one and write to the other.
	mountByPath := make(map[string]corev1.VolumeMount, len(bootstrap.VolumeMounts))
	for _, vm := range bootstrap.VolumeMounts {
		mountByPath[vm.MountPath] = vm
	}
	if got, ok := mountByPath[agent.CacheMountPath]; !ok || !got.ReadOnly {
		t.Errorf("cache-bootstrap missing RO mount at %s; got %+v", agent.CacheMountPath, bootstrap.VolumeMounts)
	}
	if got, ok := mountByPath[agent.WorkspaceMountPath]; !ok || got.ReadOnly {
		t.Errorf("cache-bootstrap missing writable mount at %s; got %+v", agent.WorkspaceMountPath, bootstrap.VolumeMounts)
	}
	// Data mount carried over so the bootstrap helper can read
	// tower-attach-written config if it needs to.
	if _, ok := mountByPath["/data"]; !ok {
		t.Errorf("cache-bootstrap missing /data mount; got %+v", bootstrap.VolumeMounts)
	}

	// The bootstrap helper runs from the shared agent image (same
	// binary the main container uses), NOT from the pinned git-only
	// image the cache refresh Job uses. That's how it has access to
	// `spire cluster cache-bootstrap`.
	if bootstrap.Image != "spire-agent:dev" {
		t.Errorf("cache-bootstrap Image = %q, want to match the agent image so it ships the `spire` CLI", bootstrap.Image)
	}
}

// TestBuildWorkloadPod_CacheOverlay_RuntimeSurfaceUnchanged pins the
// canonical runtime-contract env surface (spi-xplwy §1) pkg/executor
// and pkg/wizard read. The overlay must not change DOLT_DATA_DIR,
// SPIRE_CONFIG_DIR, SPIRE_REPO_*, SPIRE_TOWER/BEAD/ROLE/BACKEND, or the
// workspace-identity triple between a guild that spells out CacheSpec
// explicitly and one that does not. Any drift here would push
// cache-vs-origin branching into wizard/executor — a boundary violation.
func TestBuildWorkloadPod_CacheOverlay_RuntimeSurfaceUnchanged(t *testing.T) {
	ns := "spire"
	m := &AgentMonitor{
		Log:          testr.New(t),
		Namespace:    ns,
		StewardImage: "spire-agent:dev",
		Database:     "spire",
		Prefix:       "spi",
	}

	// Bare guild (no explicit CacheSpec) vs. guild with explicit CacheSpec.
	// Both go through the unconditional overlay (spi-gvrfv); env surface
	// must be identical regardless of whether the CR declares the cache
	// or just inherits the operator default. Use the same guild fixture
	// on both sides so spurious Repo/Branch drift doesn't flag the test.
	bare := makeCachePodGuild("core", ns)
	bare.Spec.Cache = nil
	barePod := m.buildWorkloadPod(bare, "spi-abc", nil)
	if barePod == nil {
		t.Fatalf("bare pod build failed")
	}
	bareMain := barePod.Spec.Containers[0]
	bareEnv := envMap(bareMain.Env)

	withCache := makeCachePodGuild("core", ns)
	withCachePod := m.buildWorkloadPod(withCache, "spi-abc", nil)
	if withCachePod == nil {
		t.Fatalf("explicit cache pod build failed")
	}
	withCacheMain := withCachePod.Spec.Containers[0]
	withCacheEnv := envMap(withCacheMain.Env)

	canonical := []string{
		// Identity
		"DOLT_DATA_DIR",
		"SPIRE_CONFIG_DIR",
		"SPIRE_REPO_URL",
		"SPIRE_REPO_BRANCH",
		"SPIRE_REPO_PREFIX",
		"SPIRE_TOWER",
		"SPIRE_BEAD_ID",
		"SPIRE_ROLE",
		"SPIRE_BACKEND",
		// Workspace identity (labels-as-env)
		"SPIRE_WORKSPACE_KIND",
		"SPIRE_WORKSPACE_NAME",
		"SPIRE_WORKSPACE_ORIGIN",
	}
	for _, name := range canonical {
		want, wantOK := bareEnv[name]
		got, gotOK := withCacheEnv[name]
		if wantOK != gotOK {
			t.Errorf("env %s presence changed: bare=%v, with-cache=%v", name, wantOK, gotOK)
			continue
		}
		if wantOK && want.Value != got.Value {
			t.Errorf("env %s drifted under cache overlay: bare=%q, with-cache=%q — pkg/executor / pkg/wizard would see a different surface",
				name, want.Value, got.Value)
		}
	}

	// Command must still be `spire execute <bead> --name <guild>`. The
	// cache overlay only changes how the workspace is MATERIALIZED; the
	// main container's entrypoint is unchanged.
	if !stringSlicesEqual(bareMain.Command, withCacheMain.Command) {
		t.Errorf("main container Command changed under cache overlay:\n  bare=%v\n  with-cache=%v",
			bareMain.Command, withCacheMain.Command)
	}
}

// TestBuildWorkloadPod_CacheOverlay_Unconditional pins the spi-gvrfv
// contract: EVERY operator-managed wizard pod boots from the cache PVC,
// regardless of whether the guild CR has an explicit CacheSpec. The
// operator replaces the shared builder's repo-bootstrap with
// cache-bootstrap unconditionally; the CacheSpec itself is only the
// cache-reconciler's signal to provision the PVC.
func TestBuildWorkloadPod_CacheOverlay_Unconditional(t *testing.T) {
	ns := "spire"
	wg := makeAgent("core", ns, nil) // no explicit CacheSpec
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

	// Cache PVC volume must be present even without an explicit CacheSpec.
	// The CacheReconciler provisions the PVC when spec.cache is set on the
	// CR; the pod-builder wires the reference unconditionally so pods stay
	// consistent between guilds that spell out the cache and guilds that
	// rely on the operator default.
	var foundCache bool
	for _, v := range pod.Spec.Volumes {
		if v.Name == "repo-cache" {
			foundCache = true
			if v.PersistentVolumeClaim == nil || !strings.HasSuffix(v.PersistentVolumeClaim.ClaimName, "-repo-cache") {
				t.Errorf("repo-cache volume must reference the cache PVC; got %+v", v)
			}
		}
	}
	if !foundCache {
		t.Errorf("repo-cache volume must appear unconditionally; got %+v", pod.Spec.Volumes)
	}

	// Init containers: cache-bootstrap only; repo-bootstrap must be gone.
	hasCacheBootstrap := false
	for _, ic := range pod.Spec.InitContainers {
		if ic.Name == "cache-bootstrap" {
			hasCacheBootstrap = true
		}
		if ic.Name == "repo-bootstrap" {
			t.Errorf("repo-bootstrap must be retired from cluster-native path; got %+v", pod.Spec.InitContainers)
		}
	}
	if !hasCacheBootstrap {
		t.Errorf("cache-bootstrap must be present on every operator-managed wizard pod")
	}

	// WorkingDir lands on WorkspaceMountPath (cache-bootstrap materializes
	// the repo root there, no prefix subdir).
	if got := pod.Spec.Containers[0].WorkingDir; got != agent.WorkspaceMountPath {
		t.Errorf("WorkingDir = %q, want %q", got, agent.WorkspaceMountPath)
	}
}

// TestBuildWorkloadPod_CacheOverlay_PVCNameMatchesReconciler locks the
// cross-task invariant: the pod builder must reference the SAME PVC
// name that cache_reconciler.go's pvcName() produces for the same
// guild. A drift here means one side creates a PVC and the other tries
// to mount a different one — pods hang in ContainerCreating.
func TestBuildWorkloadPod_CacheOverlay_PVCNameMatchesReconciler(t *testing.T) {
	ns := "spire"
	// Use a mixed-case name to exercise sanitizeK8sName downcasing on
	// both sides.
	wg := makeCachePodGuild("CoreGuild", ns)
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

	expected := pvcName(wg.Name)
	for _, v := range pod.Spec.Volumes {
		if v.Name != "repo-cache" {
			continue
		}
		if v.PersistentVolumeClaim == nil {
			t.Fatalf("repo-cache not a PVC: %+v", v)
		}
		if got := v.PersistentVolumeClaim.ClaimName; got != expected {
			t.Errorf("cache volume ClaimName = %q, want %q (pvcName) — naming drift between pod builder and reconciler", got, expected)
		}
		return
	}
	t.Fatalf("no repo-cache volume found on pod")
}

// makeCachePodGuild returns a WizardGuild configured with a populated
// CacheSpec, ready for pod-builder overlay tests. Keeps the fixture
// separate from agent_monitor_test.go's makeAgent() so cache changes
// don't ripple through the non-cache parity suite.
func makeCachePodGuild(name, namespace string) *spirev1.WizardGuild {
	g := &spirev1.WizardGuild{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: spirev1.WizardGuildSpec{
			Mode:       "managed",
			Image:      "spire-agent:dev",
			Repo:       "git@example.com:awell-health/spire.git",
			RepoBranch: "main",
			Prefixes:   []string{"spi"},
			Cache: &spirev1.CacheSpec{
				Size:       resource.MustParse("10Gi"),
				AccessMode: corev1.ReadOnlyMany,
			},
		},
	}
	return g
}
