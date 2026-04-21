package controllers

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr/testr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spirev1 "github.com/awell-health/spire/operator/api/v1alpha1"
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
// contract produced by the shared pkg/agent builder (spi-fjt2t):
//
//   - Two init containers: "tower-attach" (stages tower data onto /data)
//     and "repo-bootstrap" (clones the repo into /workspace/<prefix>).
//     Both match the pkg/agent wizard-pod shape.
//   - One "agent" main container running `spire execute`.
//   - Two emptyDir volumes: /data and /workspace.
//   - No Model A artifacts (sidecar, /comms, beads-seed ConfigMap).
//
// Operator-specific overlay (labels, MaxApprentices, secret refs) is
// tested separately in the parity test.
func TestBuildWorkloadPod_CanonicalShape(t *testing.T) {
	ns := "spire"
	agent := makeAgent("core", ns, nil)
	m := &AgentMonitor{
		Log:          testr.New(t),
		Namespace:    ns,
		StewardImage: "spire-agent:dev",
		Database:     "spire",
		Prefix:       "spi",
	}

	pod := m.buildWorkloadPod(agent, "spi-abc", nil)
	if pod == nil {
		t.Fatalf("buildWorkloadPod returned nil (SpawnConfig validation failed?)")
	}

	// Init containers: tower-attach + repo-bootstrap, matching the shared
	// pkg/agent wizard-pod shape.
	if len(pod.Spec.InitContainers) != 2 {
		t.Fatalf("len(InitContainers) = %d, want 2 (tower-attach + repo-bootstrap)", len(pod.Spec.InitContainers))
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
	if _, ok := icByName["repo-bootstrap"]; !ok {
		t.Errorf("init container %q not found; got %v", "repo-bootstrap", pod.Spec.InitContainers)
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

	// Volumes: exactly data + workspace emptyDir, no comms / beads-seed.
	if len(pod.Spec.Volumes) != 2 {
		t.Fatalf("len(Volumes) = %d, want 2; got %+v", len(pod.Spec.Volumes), pod.Spec.Volumes)
	}
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

	// Main container volume mounts: /data + /workspace, no /comms.
	mainMounts := make(map[string]string, len(main.VolumeMounts))
	for _, vm := range main.VolumeMounts {
		mainMounts[vm.MountPath] = vm.Name
	}
	if mainMounts["/data"] != "data" {
		t.Errorf("main container /data mount volume = %q, want data", mainMounts["/data"])
	}
	if mainMounts["/workspace"] != "workspace" {
		t.Errorf("main container /workspace mount volume = %q, want workspace", mainMounts["/workspace"])
	}
	if _, ok := mainMounts["/comms"]; ok {
		t.Error("main container must not mount /comms on the canonical workload pod")
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
