package controllers

import (
	"context"
	"testing"
	"time"

	"github.com/go-logr/logr/testr"
	corev1 "k8s.io/api/core/v1"
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

func makeAgent(name, namespace string, currentWork []string) *spirev1.SpireAgent {
	return &spirev1.SpireAgent{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       spirev1.SpireAgentSpec{Mode: "managed", Image: "test-image:latest"},
		Status:     spirev1.SpireAgentStatus{CurrentWork: currentWork},
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

// TestReconcileManagedAgent_SelfHealsCurrentWork proves that after an operator
// restart (CurrentWork empty but a wizard pod is still Running), the monitor
// re-attaches the pod's bead to CurrentWork instead of reaping the pod.
func TestReconcileManagedAgent_SelfHealsCurrentWork(t *testing.T) {
	ns := "spire"
	sch := newTestScheme(t)
	agent := makeAgent("test-agent", ns, nil) // simulate lost state
	pod := makeAgentPod("test-agent-pod", ns, agent.Name, "spi-test", corev1.PodRunning)

	c := fake.NewClientBuilder().
		WithScheme(sch).
		WithObjects(agent, pod).
		WithStatusSubresource(&spirev1.SpireAgent{}).
		Build()

	m := &AgentMonitor{
		Client:    c,
		Log:       testr.New(t),
		Namespace: ns,
		Interval:  time.Minute,
	}

	ctx := context.Background()
	m.reconcileManagedAgent(ctx, agent, nil)

	// Pod must still exist — it must NOT have been reaped.
	var got corev1.Pod
	if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: pod.Name}, &got); err != nil {
		t.Fatalf("expected pod to still exist, got error: %v", err)
	}
	if got.DeletionTimestamp != nil {
		t.Fatalf("expected pod to be intact, but it has a DeletionTimestamp")
	}

	// Agent CurrentWork must contain the bead.
	var gotAgent spirev1.SpireAgent
	if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: agent.Name}, &gotAgent); err != nil {
		t.Fatalf("get agent: %v", err)
	}
	if len(gotAgent.Status.CurrentWork) != 1 || gotAgent.Status.CurrentWork[0] != "spi-test" {
		t.Fatalf("expected CurrentWork=[spi-test], got %v", gotAgent.Status.CurrentWork)
	}
}

// TestReconcileManagedAgent_DoesNotHealTerminalPods makes sure terminal or
// deleting pods are not re-added to CurrentWork — they're still reaped.
func TestReconcileManagedAgent_DoesNotHealTerminalPods(t *testing.T) {
	ns := "spire"
	sch := newTestScheme(t)
	agent := makeAgent("test-agent", ns, nil)
	succeeded := makeAgentPod("pod-succeeded", ns, agent.Name, "spi-done", corev1.PodSucceeded)
	failed := makeAgentPod("pod-failed", ns, agent.Name, "spi-failed", corev1.PodFailed)
	deleting := makeAgentPod("pod-deleting", ns, agent.Name, "spi-deleting", corev1.PodRunning, func(p *corev1.Pod) {
		now := metav1.Now()
		p.DeletionTimestamp = &now
		p.Finalizers = []string{"spire.awell.io/test"}
	})

	c := fake.NewClientBuilder().
		WithScheme(sch).
		WithObjects(agent, succeeded, failed, deleting).
		WithStatusSubresource(&spirev1.SpireAgent{}).
		Build()

	m := &AgentMonitor{
		Client:    c,
		Log:       testr.New(t),
		Namespace: ns,
		Interval:  time.Minute,
	}

	ctx := context.Background()
	m.reconcileManagedAgent(ctx, agent, nil)

	var gotAgent spirev1.SpireAgent
	if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: agent.Name}, &gotAgent); err != nil {
		t.Fatalf("get agent: %v", err)
	}
	if len(gotAgent.Status.CurrentWork) != 0 {
		t.Fatalf("expected CurrentWork empty after reconcile with only terminal/deleting pods, got %v", gotAgent.Status.CurrentWork)
	}
}

func TestIsPodActive(t *testing.T) {
	now := metav1.Now()
	cases := []struct {
		name string
		pod  *corev1.Pod
		want bool
	}{
		{
			name: "running",
			pod:  &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodRunning}},
			want: true,
		},
		{
			name: "pending",
			pod:  &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodPending}},
			want: true,
		},
		{
			name: "succeeded",
			pod:  &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodSucceeded}},
			want: false,
		},
		{
			name: "failed",
			pod:  &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodFailed}},
			want: false,
		},
		{
			name: "deleting",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{DeletionTimestamp: &now, Finalizers: []string{"x"}},
				Status:     corev1.PodStatus{Phase: corev1.PodRunning},
			},
			want: false,
		},
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
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isPodActive(tc.pod); got != tc.want {
				t.Fatalf("isPodActive=%v want %v", got, tc.want)
			}
		})
	}
}
