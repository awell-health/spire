package controllers

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr/testr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spirev1 "github.com/awell-health/spire/operator/api/v1alpha1"
	"github.com/awell-health/spire/pkg/agent"
)

func newTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	sch := runtime.NewScheme()
	if err := corev1.AddToScheme(sch); err != nil {
		t.Fatalf("add corev1 to scheme: %v", err)
	}
	if err := spirev1.AddToScheme(sch); err != nil {
		t.Fatalf("add spirev1 to scheme: %v", err)
	}
	return sch
}

func makeAgent(name, namespace string, currentWork []string) *spirev1.WizardGuild {
	// Populate Repo and RepoBranch so SpawnConfig validation in the
	// shared pkg/agent builder succeeds. A real managed-mode guild
	// always has these set; an empty Repo is an admin misconfiguration
	// that buildWorkloadPod now catches explicitly (spi-fjt2t).
	return &spirev1.WizardGuild{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: spirev1.WizardGuildSpec{
			Mode:       "managed",
			Image:      "test-image:latest",
			Repo:       "git@example.com:spire-test/repo.git",
			RepoBranch: "main",
			Prefixes:   []string{"spi"},
		},
		Status: spirev1.WizardGuildStatus{CurrentWork: currentWork},
	}
}

func makeAgentPod(name, namespace, agentName, beadID string, phase corev1.PodPhase, mods ...func(*corev1.Pod)) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				"spire.awell.io/agent":   agentName,
				"spire.awell.io/bead":    beadID,
				"spire.awell.io/managed": "true",
			},
		},
		Status: corev1.PodStatus{Phase: phase},
	}
	for _, mod := range mods {
		mod(pod)
	}
	return pod
}

// TestReconcileManagedAgent_ReapLogic covers the four states of signal-based
// reaping: (signal present × pod present/absent) and (no signal × pod
// terminated/active). Each case asserts CurrentWork shape, pod existence, and
// whether the failure-branch remote-branch cleanup ran.
func TestReconcileManagedAgent_ReapLogic(t *testing.T) {
	ns := "spire"

	type sigMap map[string]map[string]string // beadID → metadata

	cases := []struct {
		name            string
		currentWork     []string
		pods            []*corev1.Pod
		signals         sigMap
		wantCurrentWork []string
		wantPodDeleted  map[string]bool // pod name → expected deletion
	}{
		{
			name:        "signal present with running pod: success, reap pod",
			currentWork: []string{"spi-ok"},
			pods: []*corev1.Pod{
				makeAgentPod("pod-ok", ns, "test-agent", "spi-ok", corev1.PodRunning),
			},
			signals: sigMap{
				"spi-ok": {"apprentice_signal_apprentice-spi-ok-0": `{"kind":"bundle"}`},
			},
			wantCurrentWork: nil,
			wantPodDeleted:  map[string]bool{"pod-ok": true},
		},
		{
			name:        "signal present with no pod: success, only CurrentWork cleared",
			currentWork: []string{"spi-already-gone"},
			pods:        nil,
			signals: sigMap{
				"spi-already-gone": {"apprentice_signal_apprentice-spi-already-gone-0": `{"kind":"no-op"}`},
			},
			wantCurrentWork: nil,
			wantPodDeleted:  nil,
		},
		{
			name:        "no signal, active pod: in progress, nothing changes",
			currentWork: []string{"spi-running"},
			pods: []*corev1.Pod{
				makeAgentPod("pod-running", ns, "test-agent", "spi-running", corev1.PodRunning),
			},
			signals:         sigMap{},
			wantCurrentWork: []string{"spi-running"},
			wantPodDeleted:  map[string]bool{"pod-running": false},
		},
		{
			name:        "no signal, succeeded pod: failure path, reap + clear",
			currentWork: []string{"spi-crashed"},
			pods: []*corev1.Pod{
				makeAgentPod("pod-crashed", ns, "test-agent", "spi-crashed", corev1.PodSucceeded),
			},
			signals:         sigMap{},
			wantCurrentWork: nil,
			wantPodDeleted:  map[string]bool{"pod-crashed": true},
		},
		{
			name:        "no signal, failed pod: failure path, reap + clear",
			currentWork: []string{"spi-failed"},
			pods: []*corev1.Pod{
				makeAgentPod("pod-failed", ns, "test-agent", "spi-failed", corev1.PodFailed),
			},
			signals:         sigMap{},
			wantCurrentWork: nil,
			wantPodDeleted:  map[string]bool{"pod-failed": true},
		},
		{
			name:        "no signal, no pod: leave CurrentWork alone (re-provision next)",
			currentWork: []string{"spi-will-respawn"},
			pods:        nil,
			signals:     sigMap{},
			// Pod creation is attempted by the create-pod loop; ignore that here
			// by asserting CurrentWork is preserved for re-provisioning.
			wantCurrentWork: []string{"spi-will-respawn"},
			wantPodDeleted:  nil,
		},
		{
			name:        "wizard container terminated but pod Running: failure path",
			currentWork: []string{"spi-term"},
			pods: []*corev1.Pod{
				makeAgentPod("pod-term", ns, "test-agent", "spi-term", corev1.PodRunning, func(p *corev1.Pod) {
					p.Status.ContainerStatuses = []corev1.ContainerStatus{
						{Name: "wizard", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1}}},
						{Name: "sidecar", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
					}
				}),
			},
			signals:         sigMap{},
			wantCurrentWork: nil,
			wantPodDeleted:  map[string]bool{"pod-term": true},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sch := newTestScheme(t)
			agent := makeAgent("test-agent", ns, tc.currentWork)

			objs := []client.Object{agent}
			for _, pod := range tc.pods {
				objs = append(objs, pod)
			}
			c := fake.NewClientBuilder().
				WithScheme(sch).
				WithObjects(objs...).
				WithStatusSubresource(&spirev1.WizardGuild{}).
				Build()

			// Inject bead metadata via the seam.
			orig := getBeadMetadataFn
			getBeadMetadataFn = func(id string) (map[string]string, error) {
				return tc.signals[id], nil
			}
			defer func() { getBeadMetadataFn = orig }()

			m := &AgentMonitor{
				Client:    c,
				Log:       testr.New(t),
				Namespace: ns,
				Interval:  time.Minute,
			}

			ctx := context.Background()
			m.reconcileManagedAgent(ctx, agent, nil)

			var gotAgent spirev1.WizardGuild
			if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: agent.Name}, &gotAgent); err != nil {
				t.Fatalf("get agent: %v", err)
			}
			if !stringSlicesEqual(gotAgent.Status.CurrentWork, tc.wantCurrentWork) {
				t.Fatalf("CurrentWork = %v, want %v", gotAgent.Status.CurrentWork, tc.wantCurrentWork)
			}

			for podName, shouldBeDeleted := range tc.wantPodDeleted {
				var got corev1.Pod
				err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: podName}, &got)
				gone := errors.IsNotFound(err)
				if shouldBeDeleted && !gone {
					t.Fatalf("pod %s: expected deleted, still exists (err=%v)", podName, err)
				}
				if !shouldBeDeleted && gone {
					t.Fatalf("pod %s: expected alive, was deleted", podName)
				}
			}
		})
	}
}

func stringSlicesEqual(a, b []string) bool {
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

// TestBuildWorkloadPod_NamingAndLabels locks in the pod-name template and
// label contract so future renames are deliberate. Per spi-kh2em: name is
// "<guild>-wizard-<bead>" (no "spire-" prefix) and the pod carries both
// "spire.awell.io/agent" (today's selector) and "spire.awell.io/guild"
// (forward-compat for the WizardGuild CRD rename).
func TestBuildWorkloadPod_NamingAndLabels(t *testing.T) {
	ns := "spire"
	agent := makeAgent("core", ns, nil)
	m := &AgentMonitor{Log: testr.New(t), Namespace: ns}

	pod := m.buildWorkloadPod(agent, "spi-abc", nil)

	if pod.Name != "core-wizard-spi-abc" {
		t.Fatalf("pod.Name = %q, want %q", pod.Name, "core-wizard-spi-abc")
	}

	wantLabels := map[string]string{
		"spire.awell.io/agent":   "core",
		"spire.awell.io/guild":   "core",
		"spire.awell.io/bead":    "spi-abc",
		"spire.awell.io/managed": "true",
		"spire.awell.io/role":    "wizard",
		"app.kubernetes.io/name": "spire-wizard",
	}
	for k, want := range wantLabels {
		if got := pod.Labels[k]; got != want {
			t.Errorf("labels[%q] = %q, want %q", k, got, want)
		}
	}
}

// TestBuildWorkloadPod_NameTruncatedTo63 ensures we stay within the k8s
// pod-name limit even when agent/bead names are unusually long.
func TestBuildWorkloadPod_NameTruncatedTo63(t *testing.T) {
	ns := "spire"
	longAgent := "very-long-guild-name-that-pushes-the-limit"
	longBead := "spi-this-is-also-quite-long-to-overflow"
	agent := makeAgent(longAgent, ns, nil)
	m := &AgentMonitor{Log: testr.New(t), Namespace: ns}

	pod := m.buildWorkloadPod(agent, longBead, nil)

	if len(pod.Name) > 63 {
		t.Fatalf("pod.Name length = %d, want <= 63 (got %q)", len(pod.Name), pod.Name)
	}
}

// TestBuildWorkloadPod_CanonicalShape pins the canonical wizard pod
// contract produced by the operator after the unconditional cache overlay
// (spi-gvrfv): the shared pkg/agent builder still emits a repo-bootstrap
// init container, but the operator overlay always replaces it with
// cache-bootstrap and wires the guild-owned cache PVC. Every
// operator-managed wizard pod therefore has:
//
//   - Two init containers: "tower-attach" (stages tower data onto /data)
//     and "cache-bootstrap" (materializes /spire/workspace from the cache
//     PVC). repo-bootstrap is retired from the cluster-native path.
//   - One "agent" main container running `spire execute`.
//   - Three volumes: /data + /workspace emptyDirs plus the repo-cache PVC.
//   - No Model A artifacts (sidecar, /comms, beads-seed ConfigMap).
//
// Operator-specific overlay (labels, MaxApprentices, secret refs) is
// tested separately in the parity test.
func TestBuildWorkloadPod_CanonicalShape(t *testing.T) {
	ns := "spire"
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
		t.Fatalf("buildWorkloadPod returned nil (SpawnConfig validation failed?)")
	}

	// Init containers: tower-attach + cache-bootstrap. repo-bootstrap must
	// not survive the operator overlay.
	if len(pod.Spec.InitContainers) != 2 {
		t.Fatalf("len(InitContainers) = %d, want 2 (tower-attach + cache-bootstrap)", len(pod.Spec.InitContainers))
	}
	icByName := make(map[string]corev1.Container, len(pod.Spec.InitContainers))
	for _, ic := range pod.Spec.InitContainers {
		icByName[ic.Name] = ic
	}
	ic, ok := icByName["tower-attach"]
	if !ok {
		t.Fatalf("init container %q not found; got %v", "tower-attach", pod.Spec.InitContainers)
	}
	wantPrefix := []string{"spire", "tower", "attach-cluster"}
	if len(ic.Command) < len(wantPrefix) {
		t.Fatalf("init container Command too short: %v", ic.Command)
	}
	for i, w := range wantPrefix {
		if ic.Command[i] != w {
			t.Errorf("init container Command[%d] = %q, want %q", i, ic.Command[i], w)
		}
	}
	for _, flag := range []string{"--data-dir=/data/", "--database=", "--prefix=", "--dolthub-remote="} {
		if !anyHasPrefix(ic.Command, flag) {
			t.Errorf("init container Command missing flag with prefix %q; got %v", flag, ic.Command)
		}
	}
	if _, ok := icByName["cache-bootstrap"]; !ok {
		t.Errorf("init container %q not found; got %v", "cache-bootstrap", pod.Spec.InitContainers)
	}
	if _, stale := icByName["repo-bootstrap"]; stale {
		t.Errorf("repo-bootstrap must be retired from operator-managed pods; got %v", pod.Spec.InitContainers)
	}

	// Main container: exactly one named "agent" running `spire execute`.
	if len(pod.Spec.Containers) != 1 {
		t.Fatalf("len(Containers) = %d, want 1", len(pod.Spec.Containers))
	}
	main := pod.Spec.Containers[0]
	if main.Name != "agent" {
		t.Errorf("main container Name = %q, want agent", main.Name)
	}
	wantCmd := []string{"spire", "execute", "spi-abc", "--name", "core"}
	if !stringSlicesEqual(main.Command, wantCmd) {
		t.Errorf("main container Command = %v, want %v", main.Command, wantCmd)
	}

	// Volumes: data + workspace emptyDirs plus the repo-cache PVC, no
	// Model A artifacts.
	vols := make(map[string]corev1.Volume, len(pod.Spec.Volumes))
	for _, v := range pod.Spec.Volumes {
		vols[v.Name] = v
	}
	for _, name := range []string{"data", "workspace"} {
		v, ok := vols[name]
		if !ok {
			t.Errorf("missing volume %q", name)
			continue
		}
		if v.EmptyDir == nil {
			t.Errorf("volume %q: EmptyDir is nil, want EmptyDir source", name)
		}
	}
	cache, ok := vols["repo-cache"]
	if !ok {
		t.Errorf("missing repo-cache volume; cache overlay must add it unconditionally")
	} else if cache.PersistentVolumeClaim == nil {
		t.Errorf("repo-cache must be a PVC reference; got %+v", cache)
	}
	for _, forbidden := range []string{"comms", "beads-seed"} {
		if _, ok := vols[forbidden]; ok {
			t.Errorf("Model A volume %q must not exist on the canonical workload pod", forbidden)
		}
	}

	// Main container env must include DOLT_DATA_DIR and SPIRE_CONFIG_DIR so
	// resolveBeadsDir() finds the store staged by tower-attach.
	envMap := make(map[string]corev1.EnvVar, len(main.Env))
	for _, e := range main.Env {
		envMap[e.Name] = e
	}
	wantEnv := map[string]string{
		"DOLT_DATA_DIR":    "/data",
		"SPIRE_CONFIG_DIR": "/data/spire-config",
		"SPIRE_AGENT_NAME": "core",
		"SPIRE_BEAD_ID":    "spi-abc",
		"SPIRE_ROLE":       "wizard",
	}
	for k, want := range wantEnv {
		got, ok := envMap[k]
		if !ok {
			t.Errorf("missing env var %s", k)
			continue
		}
		if got.Value != want {
			t.Errorf("env %s = %q, want %q", k, got.Value, want)
		}
	}

	// Pod-level invariants from the canonical contract.
	if pod.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("RestartPolicy = %q, want Never", pod.Spec.RestartPolicy)
	}
	if pod.Spec.PriorityClassName != "spire-agent-default" {
		t.Errorf("PriorityClassName = %q, want spire-agent-default", pod.Spec.PriorityClassName)
	}

	// Main container volume mounts: /data + /spire/workspace (remapped by
	// cache overlay) + /spire/cache RO. /comms must not appear.
	mainMounts := make(map[string]string, len(main.VolumeMounts))
	for _, vm := range main.VolumeMounts {
		mainMounts[vm.MountPath] = vm.Name
	}
	if mainMounts["/data"] != "data" {
		t.Errorf("main container /data mount volume = %q, want data", mainMounts["/data"])
	}
	if mainMounts[agent.WorkspaceMountPath] != "workspace" {
		t.Errorf("main container %s mount volume = %q, want workspace",
			agent.WorkspaceMountPath, mainMounts[agent.WorkspaceMountPath])
	}
	if mainMounts[agent.CacheMountPath] != "repo-cache" {
		t.Errorf("main container %s mount volume = %q, want repo-cache",
			agent.CacheMountPath, mainMounts[agent.CacheMountPath])
	}
	if _, ok := mainMounts["/comms"]; ok {
		t.Error("main container must not mount /comms on the canonical workload pod")
	}
}

// TestBuildWorkloadPod_WorkingDirInsideClone is the spi-vrzhf regression
// test, updated for the cache-bootstrap substrate (spi-gvrfv).
// cache-bootstrap materializes the workspace at pkg/agent.WorkspaceMountPath
// (no per-prefix subdirectory — the repo root IS the mount path), so the
// main container's WorkingDir must land on WorkspaceMountPath for
// spire.yaml lookups in ResolveBackend("") and resolveMaxApprentices()
// to succeed.
func TestBuildWorkloadPod_WorkingDirInsideClone(t *testing.T) {
	ns := "spire"
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
	if len(pod.Spec.Containers) == 0 {
		t.Fatalf("pod has no containers")
	}
	main := pod.Spec.Containers[0]

	// cache-bootstrap clones the cache tree into WorkspaceMountPath;
	// WorkingDir must match so repoconfig.Load(".") finds spire.yaml.
	if main.WorkingDir != agent.WorkspaceMountPath {
		t.Errorf("main.WorkingDir = %q, want %q — cache-bootstrap materializes repo root at WorkspaceMountPath",
			main.WorkingDir, agent.WorkspaceMountPath)
	}

	// Double-check: the SPIRE_REPO_PREFIX env var remains on every
	// container so downstream code that reads it (not for clone-path
	// choice anymore, but for identity logging/routing) sees the right
	// value.
	var gotPrefix string
	for _, e := range main.Env {
		if e.Name == "SPIRE_REPO_PREFIX" {
			gotPrefix = e.Value
			break
		}
	}
	if gotPrefix != "spi" {
		t.Errorf("SPIRE_REPO_PREFIX env on main container = %q, want spi — init container would clone to a different path", gotPrefix)
	}
}

// TestBuildWorkloadPod_NoSidecar explicitly guards against regressing to the
// sidecar model: a single main container, never a container named "sidecar".
func TestBuildWorkloadPod_NoSidecar(t *testing.T) {
	ns := "spire"
	agent := makeAgent("core", ns, nil)
	m := &AgentMonitor{Log: testr.New(t), Namespace: ns}

	pod := m.buildWorkloadPod(agent, "spi-abc", nil)

	if len(pod.Spec.Containers) != 1 {
		t.Fatalf("len(Containers) = %d, want 1 (no sidecar)", len(pod.Spec.Containers))
	}
	for _, c := range pod.Spec.Containers {
		if c.Name == "sidecar" {
			t.Errorf("container named %q must not exist on the canonical workload pod", c.Name)
		}
		if strings.Contains(strings.Join(c.Command, " "), "agent-entrypoint.sh") {
			t.Errorf("container %q runs agent-entrypoint.sh; that path was removed in spi-d4ku6", c.Name)
		}
	}
}

// TestBuildWorkloadPod_ResourceOverride verifies guild-level overrides win
// and that the wizard-tier default is applied when the guild is unset. This
// is the contract spi-9wo3a preserves on purpose: teams may already rely on
// guild-level Resources, so the switch to the canonical pod must not drop
// that override path.
func TestBuildWorkloadPod_ResourceOverride(t *testing.T) {
	ns := "spire"

	t.Run("guild override wins", func(t *testing.T) {
		agent := makeAgent("core", ns, nil)
		agent.Spec.Resources = &spirev1.GuildResourceRequirements{
			Requests: map[string]string{"memory": "512Mi", "cpu": "100m"},
			Limits:   map[string]string{"memory": "1Gi", "cpu": "500m"},
		}
		m := &AgentMonitor{Log: testr.New(t), Namespace: ns}
		pod := m.buildWorkloadPod(agent, "spi-abc", nil)

		res := pod.Spec.Containers[0].Resources
		if got := res.Requests[corev1.ResourceMemory]; got.String() != "512Mi" {
			t.Errorf("memory request = %s, want 512Mi", got.String())
		}
		if got := res.Limits[corev1.ResourceMemory]; got.String() != "1Gi" {
			t.Errorf("memory limit = %s, want 1Gi", got.String())
		}
	})

	t.Run("default wizard tier applies when guild is unset", func(t *testing.T) {
		agent := makeAgent("core", ns, nil)
		m := &AgentMonitor{Log: testr.New(t), Namespace: ns}
		pod := m.buildWorkloadPod(agent, "spi-abc", nil)

		res := pod.Spec.Containers[0].Resources
		// Canonical defaults from pkg/agent/resources.go: wizard tier gets
		// 1Gi/2Gi memory and 250m/1000m cpu. Resources aren't nil because
		// of the fallback — assert they match.
		if got := res.Requests[corev1.ResourceMemory]; got.String() != "1Gi" {
			t.Errorf("default memory request = %s, want 1Gi", got.String())
		}
		if got := res.Limits[corev1.ResourceMemory]; got.String() != "2Gi" {
			t.Errorf("default memory limit = %s, want 2Gi", got.String())
		}
	})
}

func TestIsPodFinished(t *testing.T) {
	cases := []struct {
		name string
		pod  *corev1.Pod
		want bool
	}{
		{
			// Canonical workload pod: single "agent" container, terminated.
			// This is the spi-9wo3a shape — assert reap still works.
			name: "single agent container terminated",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
					ContainerStatuses: []corev1.ContainerStatus{
						{Name: "agent", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
					},
				},
			},
			want: true,
		},
		{
			// Epic/review (Model A) pod: wizard terminated, sidecar still
			// running keeps the pod in Running phase. Reap must still fire.
			name: "wizard container terminated (epic/review path)",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
					ContainerStatuses: []corev1.ContainerStatus{
						{Name: "wizard", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
						{Name: "sidecar", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
					},
				},
			},
			want: true,
		},
		{
			name: "all containers running",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
					ContainerStatuses: []corev1.ContainerStatus{
						{Name: "wizard", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
						{Name: "sidecar", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
					},
				},
			},
			want: false,
		},
		{
			name: "only sidecar terminated",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
					ContainerStatuses: []corev1.ContainerStatus{
						{Name: "wizard", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
						{Name: "sidecar", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
					},
				},
			},
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isPodFinished(tc.pod); got != tc.want {
				t.Fatalf("isPodFinished=%v want %v", got, tc.want)
			}
		})
	}
}

// anyHasPrefix reports whether any element of cmd starts with prefix.
func anyHasPrefix(cmd []string, prefix string) bool {
	for _, arg := range cmd {
		if strings.HasPrefix(arg, prefix) {
			return true
		}
	}
	return false
}

// TestReconcileManagedAgent_ProvisionsOwningWizardPVC asserts that when a
// guild opts into sharedWorkspace, the reconcile cycle creates a PVC next
// to each wizard pod with the canonical owning-wizard label and an
// ownerReference that pins the PVC's lifetime to the pod. This closes
// the gap spi-cslm8 left open (gate was opt-in, but nothing actually
// provisioned the PVC).
func TestReconcileManagedAgent_ProvisionsOwningWizardPVC(t *testing.T) {
	ns := "spire"
	sch := newTestScheme(t)

	on := true
	agent := makeAgent("test-agent", ns, []string{"spi-abc"})
	agent.Spec.SharedWorkspace = &on

	c := fake.NewClientBuilder().
		WithScheme(sch).
		WithObjects(agent).
		WithStatusSubresource(&spirev1.WizardGuild{}).
		Build()

	orig := getBeadMetadataFn
	getBeadMetadataFn = func(id string) (map[string]string, error) { return nil, nil }
	defer func() { getBeadMetadataFn = orig }()

	m := &AgentMonitor{
		Client:       c,
		Log:          testr.New(t),
		Namespace:    ns,
		Interval:     time.Minute,
		StewardImage: "spire-agent:dev",
		Database:     "spire",
		Prefix:       "spi",
	}

	ctx := context.Background()
	m.reconcileManagedAgent(ctx, agent, nil)

	// The reconciler must have created both the wizard pod and the
	// backing PVC.
	var pod corev1.Pod
	if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: "test-agent-wizard-spi-abc"}, &pod); err != nil {
		t.Fatalf("get pod: %v", err)
	}

	wantPVC := pod.Name + "-workspace"
	var pvc corev1.PersistentVolumeClaim
	if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: wantPVC}, &pvc); err != nil {
		t.Fatalf("get PVC %q: %v", wantPVC, err)
	}

	// Canonical label that child apprentice/sage pods select on.
	if got := pvc.Labels["spire.io/owning-wizard-pod"]; got != pod.Name {
		t.Errorf("PVC label spire.io/owning-wizard-pod = %q, want %q", got, pod.Name)
	}

	// OwnerReference: Controller=true so k8s GC cascades the delete;
	// BlockOwnerDeletion=true so the PVC survives long enough for writes
	// to flush. UID must match the pod's UID.
	if len(pvc.OwnerReferences) != 1 {
		t.Fatalf("PVC OwnerReferences = %+v, want exactly 1", pvc.OwnerReferences)
	}
	or := pvc.OwnerReferences[0]
	if or.Kind != "Pod" {
		t.Errorf("OwnerReference Kind = %q, want Pod", or.Kind)
	}
	if or.Name != pod.Name {
		t.Errorf("OwnerReference Name = %q, want %q", or.Name, pod.Name)
	}
	if or.UID != pod.UID {
		t.Errorf("OwnerReference UID = %q, want %q (pod UID)", or.UID, pod.UID)
	}
	if or.Controller == nil || !*or.Controller {
		t.Errorf("OwnerReference Controller = %v, want true", or.Controller)
	}
	if or.BlockOwnerDeletion == nil || !*or.BlockOwnerDeletion {
		t.Errorf("OwnerReference BlockOwnerDeletion = %v, want true", or.BlockOwnerDeletion)
	}

	// Pod's workspace volume must reference the provisioned PVC rather
	// than an emptyDir — otherwise children mount a PVC whose contents
	// differ from what the wizard sees.
	var workspace *corev1.Volume
	for i := range pod.Spec.Volumes {
		if pod.Spec.Volumes[i].Name == "workspace" {
			workspace = &pod.Spec.Volumes[i]
			break
		}
	}
	if workspace == nil {
		t.Fatalf("pod missing 'workspace' volume; volumes=%+v", pod.Spec.Volumes)
	}
	if workspace.PersistentVolumeClaim == nil {
		t.Fatalf("workspace volume not backed by PVC (emptyDir=%v)", workspace.EmptyDir != nil)
	}
	if workspace.PersistentVolumeClaim.ClaimName != wantPVC {
		t.Errorf("workspace ClaimName = %q, want %q", workspace.PersistentVolumeClaim.ClaimName, wantPVC)
	}
}

// TestReconcileManagedAgent_DoesNotProvisionPVCWhenOptedOut asserts the
// default path: without spec.sharedWorkspace=true the reconciler leaves
// the workspace as an emptyDir and does not create a PVC.
func TestReconcileManagedAgent_DoesNotProvisionPVCWhenOptedOut(t *testing.T) {
	ns := "spire"
	sch := newTestScheme(t)

	agent := makeAgent("test-agent", ns, []string{"spi-abc"})
	// SharedWorkspace deliberately left nil — the default-off case.

	c := fake.NewClientBuilder().
		WithScheme(sch).
		WithObjects(agent).
		WithStatusSubresource(&spirev1.WizardGuild{}).
		Build()

	orig := getBeadMetadataFn
	getBeadMetadataFn = func(id string) (map[string]string, error) { return nil, nil }
	defer func() { getBeadMetadataFn = orig }()

	m := &AgentMonitor{
		Client:       c,
		Log:          testr.New(t),
		Namespace:    ns,
		Interval:     time.Minute,
		StewardImage: "spire-agent:dev",
		Database:     "spire",
		Prefix:       "spi",
	}

	m.reconcileManagedAgent(context.Background(), agent, nil)

	var pvcs corev1.PersistentVolumeClaimList
	if err := c.List(context.Background(), &pvcs, client.InNamespace(ns)); err != nil {
		t.Fatalf("list PVCs: %v", err)
	}
	if len(pvcs.Items) != 0 {
		t.Fatalf("unexpected PVCs created with sharedWorkspace off: %+v", pvcs.Items)
	}
}

// TestReconcileManagedAgent_OwningWizardPVC_Idempotent confirms a second
// reconcile cycle does NOT create a duplicate PVC or error out when the
// PVC already exists. The reconciler must converge on a single PVC per
// wizard pod even when the cycle fires repeatedly.
func TestReconcileManagedAgent_OwningWizardPVC_Idempotent(t *testing.T) {
	ns := "spire"
	sch := newTestScheme(t)

	on := true
	agent := makeAgent("test-agent", ns, []string{"spi-abc"})
	agent.Spec.SharedWorkspace = &on

	c := fake.NewClientBuilder().
		WithScheme(sch).
		WithObjects(agent).
		WithStatusSubresource(&spirev1.WizardGuild{}).
		Build()

	orig := getBeadMetadataFn
	getBeadMetadataFn = func(id string) (map[string]string, error) { return nil, nil }
	defer func() { getBeadMetadataFn = orig }()

	m := &AgentMonitor{
		Client:       c,
		Log:          testr.New(t),
		Namespace:    ns,
		Interval:     time.Minute,
		StewardImage: "spire-agent:dev",
		Database:     "spire",
		Prefix:       "spi",
	}

	ctx := context.Background()
	m.reconcileManagedAgent(ctx, agent, nil)
	// Second reconcile cycle — should be a no-op for PVC creation.
	m.reconcileManagedAgent(ctx, agent, nil)

	var pvcs corev1.PersistentVolumeClaimList
	if err := c.List(ctx, &pvcs, client.InNamespace(ns)); err != nil {
		t.Fatalf("list PVCs: %v", err)
	}
	if len(pvcs.Items) != 1 {
		t.Fatalf("len(PVCs) after two reconciles = %d, want 1 (idempotent)", len(pvcs.Items))
	}
}

// TestReconcileManagedAgent_OwningWizardPVC_SizeAndStorageClass covers
// the size and storage-class overrides on WizardGuildSpec: both fall
// through to the PVC spec verbatim, and an unset size uses the operator
// default (5Gi).
func TestReconcileManagedAgent_OwningWizardPVC_SizeAndStorageClass(t *testing.T) {
	ns := "spire"
	sch := newTestScheme(t)

	on := true
	agent := makeAgent("test-agent", ns, []string{"spi-abc"})
	agent.Spec.SharedWorkspace = &on
	sc := "fast-ssd"
	agent.Spec.SharedWorkspaceStorageClass = &sc
	size := resource.MustParse("12Gi")
	agent.Spec.SharedWorkspaceSize = &size

	c := fake.NewClientBuilder().
		WithScheme(sch).
		WithObjects(agent).
		WithStatusSubresource(&spirev1.WizardGuild{}).
		Build()

	orig := getBeadMetadataFn
	getBeadMetadataFn = func(id string) (map[string]string, error) { return nil, nil }
	defer func() { getBeadMetadataFn = orig }()

	m := &AgentMonitor{
		Client:       c,
		Log:          testr.New(t),
		Namespace:    ns,
		Interval:     time.Minute,
		StewardImage: "spire-agent:dev",
		Database:     "spire",
		Prefix:       "spi",
	}

	m.reconcileManagedAgent(context.Background(), agent, nil)

	var pvcs corev1.PersistentVolumeClaimList
	if err := c.List(context.Background(), &pvcs, client.InNamespace(ns)); err != nil {
		t.Fatalf("list PVCs: %v", err)
	}
	if len(pvcs.Items) != 1 {
		t.Fatalf("want exactly 1 PVC, got %d", len(pvcs.Items))
	}
	pvc := pvcs.Items[0]

	if pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName != "fast-ssd" {
		t.Errorf("StorageClassName = %v, want fast-ssd", pvc.Spec.StorageClassName)
	}
	got := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
	if got.Cmp(size) != 0 {
		t.Errorf("storage request = %v, want %v", got.String(), size.String())
	}
	if len(pvc.Spec.AccessModes) != 1 || pvc.Spec.AccessModes[0] != corev1.ReadWriteOnce {
		t.Errorf("AccessModes = %v, want [ReadWriteOnce]", pvc.Spec.AccessModes)
	}
}

// TestReconcileManagedAgent_OwningWizardPVC_UsesDefaultSize covers the
// fall-through when the guild does not override size: the PVC is
// provisioned with the operator default (5Gi).
func TestReconcileManagedAgent_OwningWizardPVC_UsesDefaultSize(t *testing.T) {
	ns := "spire"
	sch := newTestScheme(t)

	on := true
	agent := makeAgent("test-agent", ns, []string{"spi-abc"})
	agent.Spec.SharedWorkspace = &on
	// SharedWorkspaceSize intentionally unset

	c := fake.NewClientBuilder().
		WithScheme(sch).
		WithObjects(agent).
		WithStatusSubresource(&spirev1.WizardGuild{}).
		Build()

	orig := getBeadMetadataFn
	getBeadMetadataFn = func(id string) (map[string]string, error) { return nil, nil }
	defer func() { getBeadMetadataFn = orig }()

	m := &AgentMonitor{
		Client:       c,
		Log:          testr.New(t),
		Namespace:    ns,
		Interval:     time.Minute,
		StewardImage: "spire-agent:dev",
		Database:     "spire",
		Prefix:       "spi",
	}

	m.reconcileManagedAgent(context.Background(), agent, nil)

	var pvcs corev1.PersistentVolumeClaimList
	if err := c.List(context.Background(), &pvcs, client.InNamespace(ns)); err != nil {
		t.Fatalf("list PVCs: %v", err)
	}
	if len(pvcs.Items) != 1 {
		t.Fatalf("want exactly 1 PVC, got %d", len(pvcs.Items))
	}
	got := pvcs.Items[0].Spec.Resources.Requests[corev1.ResourceStorage]
	want := resource.MustParse("5Gi")
	if got.Cmp(want) != 0 {
		t.Errorf("default storage request = %v, want %v", got.String(), want.String())
	}
}

// envLookup reduces an env slice to a name → value map for assertions.
func envLookup(list []corev1.EnvVar) map[string]corev1.EnvVar {
	m := make(map[string]corev1.EnvVar, len(list))
	for _, e := range list {
		m[e.Name] = e
	}
	return m
}

// TestAgentMonitor_buildOverlayEnv_LogStore covers the four log-store
// branching states emitted onto every wizard container by the operator
// overlay: empty backend (no env), backend=local (only LOGSTORE_BACKEND),
// backend=gcs with all fields (all four env vars), and backend=gcs with
// empty bucket/retention (bucket and retention omitted, prefix always
// emitted).
//
// The wizard pod ships these env vars so apprentice/sage subprocesses
// it spawns inherit the same log-substrate target — keeping cluster
// pods on the cloud-native artifact store instead of the in-binary
// local default. The branching matches pkg/agent/pod_builder.go's
// buildEnv shape (apprentice path) so wizards and apprentices stay in
// lockstep on the LOGSTORE_* contract.
func TestAgentMonitor_buildOverlayEnv_LogStore(t *testing.T) {
	cases := []struct {
		name              string
		backend           string
		bucket            string
		prefix            string
		retentionDays     string
		wantBackend       string // "" means env must be absent
		wantBucket        string // "" means env must be absent
		wantPrefix        string // empty + wantPrefixPresent=true asserts presence with empty value
		wantPrefixPresent bool
		wantRetentionDays string // "" means env must be absent
	}{
		{
			name:    "empty backend emits no LOGSTORE_* env",
			backend: "",
		},
		{
			name:        "backend=local emits only LOGSTORE_BACKEND",
			backend:     "local",
			wantBackend: "local",
		},
		{
			name:              "backend=gcs with all fields emits all four env vars",
			backend:           "gcs",
			bucket:            "spire-logs-prod",
			prefix:            "spire/agent-logs",
			retentionDays:     "30",
			wantBackend:       "gcs",
			wantBucket:        "spire-logs-prod",
			wantPrefix:        "spire/agent-logs",
			wantPrefixPresent: true,
			wantRetentionDays: "30",
		},
		{
			name:              "backend=gcs with empty bucket/retention omits both, prefix still emitted",
			backend:           "gcs",
			bucket:            "",
			prefix:            "",
			retentionDays:     "",
			wantBackend:       "gcs",
			wantBucket:        "",
			wantPrefix:        "",
			wantPrefixPresent: true,
			wantRetentionDays: "",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			m := &AgentMonitor{
				LogStoreBackend:       tc.backend,
				LogStoreGCSBucket:     tc.bucket,
				LogStoreGCSPrefix:     tc.prefix,
				LogStoreRetentionDays: tc.retentionDays,
			}
			wg := makeAgent("test-agent", "spire", nil)

			env := envLookup(m.buildOverlayEnv(wg, nil))

			gotBackend, hasBackend := env["LOGSTORE_BACKEND"]
			if tc.wantBackend == "" {
				if hasBackend {
					t.Errorf("LOGSTORE_BACKEND = %q, want absent", gotBackend.Value)
				}
			} else {
				if !hasBackend {
					t.Errorf("LOGSTORE_BACKEND missing, want %q", tc.wantBackend)
				} else if gotBackend.Value != tc.wantBackend {
					t.Errorf("LOGSTORE_BACKEND = %q, want %q", gotBackend.Value, tc.wantBackend)
				}
			}

			gotBucket, hasBucket := env["LOGSTORE_GCS_BUCKET"]
			if tc.wantBucket == "" {
				if hasBucket {
					t.Errorf("LOGSTORE_GCS_BUCKET = %q, want absent", gotBucket.Value)
				}
			} else {
				if !hasBucket {
					t.Errorf("LOGSTORE_GCS_BUCKET missing, want %q", tc.wantBucket)
				} else if gotBucket.Value != tc.wantBucket {
					t.Errorf("LOGSTORE_GCS_BUCKET = %q, want %q", gotBucket.Value, tc.wantBucket)
				}
			}

			gotPrefix, hasPrefix := env["LOGSTORE_GCS_PREFIX"]
			if !tc.wantPrefixPresent {
				if hasPrefix {
					t.Errorf("LOGSTORE_GCS_PREFIX = %q, want absent", gotPrefix.Value)
				}
			} else {
				if !hasPrefix {
					t.Errorf("LOGSTORE_GCS_PREFIX missing, want present with value %q", tc.wantPrefix)
				} else if gotPrefix.Value != tc.wantPrefix {
					t.Errorf("LOGSTORE_GCS_PREFIX = %q, want %q", gotPrefix.Value, tc.wantPrefix)
				}
			}

			gotRetention, hasRetention := env["LOGSTORE_RETENTION_DAYS"]
			if tc.wantRetentionDays == "" {
				if hasRetention {
					t.Errorf("LOGSTORE_RETENTION_DAYS = %q, want absent", gotRetention.Value)
				}
			} else {
				if !hasRetention {
					t.Errorf("LOGSTORE_RETENTION_DAYS missing, want %q", tc.wantRetentionDays)
				} else if gotRetention.Value != tc.wantRetentionDays {
					t.Errorf("LOGSTORE_RETENTION_DAYS = %q, want %q", gotRetention.Value, tc.wantRetentionDays)
				}
			}
		})
	}
}
