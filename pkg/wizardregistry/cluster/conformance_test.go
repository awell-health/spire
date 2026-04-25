package cluster_test

import (
	"context"
	"testing"

	"github.com/awell-health/spire/pkg/wizardregistry"
	"github.com/awell-health/spire/pkg/wizardregistry/cluster"
	"github.com/awell-health/spire/pkg/wizardregistry/conformance"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// fakeControl flips pod phase in the fake k8s clientset to drive the
// conformance suite's authoritative-source view.
//
// Most cases in the suite first call Registry.Upsert and Skip on
// ErrReadOnly, so the cluster registry exercises only the read-only
// surface (testIsAliveMissing). fakeControl still implements full
// SetAlive semantics so the few cases that reach it behave correctly.
type fakeControl struct {
	c  *fake.Clientset
	ns string
}

func (f *fakeControl) SetAlive(id string, alive bool) {
	ctx := context.Background()
	podName := "wizard-pod-" + id

	existing, err := f.c.CoreV1().Pods(f.ns).Get(ctx, podName, metav1.GetOptions{})
	switch {
	case err == nil:
		desired := corev1.PodFailed
		if alive {
			desired = corev1.PodRunning
		}
		if existing.Status.Phase != desired {
			existing.Status.Phase = desired
			if _, err := f.c.CoreV1().Pods(f.ns).UpdateStatus(ctx, existing, metav1.UpdateOptions{}); err != nil {
				panic("fakeControl: update status: " + err.Error())
			}
		}
	case apierrors.IsNotFound(err):
		if !alive {
			return
		}
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      podName,
				Namespace: f.ns,
				Labels: map[string]string{
					cluster.DefaultRoleLabel:   cluster.DefaultRoleValue,
					cluster.DefaultIDLabel:     id,
					cluster.DefaultBeadIDLabel: "spi-" + id,
				},
			},
			Status: corev1.PodStatus{Phase: corev1.PodRunning},
		}
		if _, err := f.c.CoreV1().Pods(f.ns).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
			panic("fakeControl: create pod: " + err.Error())
		}
	default:
		panic("fakeControl: get pod: " + err.Error())
	}
}

func TestConformance(t *testing.T) {
	conformance.Run(t, func(t *testing.T) (wizardregistry.Registry, conformance.Control) {
		c := fake.NewSimpleClientset()
		r := cluster.New(c, cluster.Options{Namespace: testNamespace})
		ctl := &fakeControl{c: c, ns: testNamespace}
		return r, ctl
	})
}

// Compile-time assertion that fakeControl satisfies conformance.Control.
var _ conformance.Control = (*fakeControl)(nil)

// TestUpsertReturnsReadOnly_Sentinel locks in the cluster contract:
// Upsert MUST return wizardregistry.ErrReadOnly so the conformance
// suite's write-skip paths fire correctly.
func TestUpsertReturnsReadOnly_Sentinel(t *testing.T) {
	c := fake.NewSimpleClientset()
	r := cluster.New(c, cluster.Options{Namespace: testNamespace})

	w := wizardregistry.Wizard{ID: "x", Mode: wizardregistry.ModeCluster}
	if err := r.Upsert(context.Background(), w); err == nil {
		t.Fatalf("Upsert err = nil, want ErrReadOnly")
	}
	if err := r.Remove(context.Background(), "x"); err == nil {
		t.Fatalf("Remove err = nil, want ErrReadOnly")
	}
}
