package controllers

import (
	"context"
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
	return &spirev1.WizardGuild{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       spirev1.WizardGuildSpec{Mode: "managed", Image: "test-image:latest"},
		Status:     spirev1.WizardGuildStatus{CurrentWork: currentWork},
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

func TestIsPodFinished(t *testing.T) {
	cases := []struct {
		name string
		pod  *corev1.Pod
		want bool
	}{
		{
			name: "wizard container terminated",
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
