package cluster_test

import (
	"context"
	"errors"
	"sort"
	"testing"
	"time"

	"github.com/awell-health/spire/pkg/wizardregistry"
	"github.com/awell-health/spire/pkg/wizardregistry/cluster"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

const testNamespace = "spire"

// wizardPod returns a pod stamped with the canonical wizard labels.
func wizardPod(name, wizardID, beadID string, phase corev1.PodPhase) *corev1.Pod {
	return wizardPodAt(name, wizardID, beadID, phase, time.Unix(1700000000, 0))
}

func wizardPodAt(name, wizardID, beadID string, phase corev1.PodPhase, created time.Time) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: testNamespace,
			Labels: map[string]string{
				cluster.DefaultRoleLabel:   cluster.DefaultRoleValue,
				cluster.DefaultIDLabel:     wizardID,
				cluster.DefaultBeadIDLabel: beadID,
			},
			CreationTimestamp: metav1.NewTime(created),
		},
		Status: corev1.PodStatus{
			Phase:     phase,
			StartTime: &metav1.Time{Time: created.Add(time.Second)},
		},
	}
}

func newRegistry(t *testing.T, objs ...*corev1.Pod) (*cluster.Registry, *fake.Clientset) {
	t.Helper()
	c := fake.NewSimpleClientset()
	for _, o := range objs {
		if _, err := c.CoreV1().Pods(o.Namespace).Create(context.Background(), o, metav1.CreateOptions{}); err != nil {
			t.Fatalf("seed pod %q: %v", o.Name, err)
		}
	}
	return cluster.New(c, cluster.Options{Namespace: testNamespace}), c
}

func TestNew_PanicsOnEmptyNamespace(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("New with empty namespace did not panic")
		}
	}()
	_ = cluster.New(fake.NewSimpleClientset(), cluster.Options{})
}

func TestIsAlive(t *testing.T) {
	cases := []struct {
		name      string
		phase     corev1.PodPhase
		deleting  bool
		wantAlive bool
	}{
		{"running", corev1.PodRunning, false, true},
		{"runningTerminating", corev1.PodRunning, true, false},
		{"pending", corev1.PodPending, false, false},
		{"failed", corev1.PodFailed, false, false},
		{"succeeded", corev1.PodSucceeded, false, false},
		{"unknown", corev1.PodUnknown, false, false},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			pod := wizardPod("wizard-pod-"+tc.name, "wizard-"+tc.name, "spi-"+tc.name, tc.phase)
			if tc.deleting {
				now := metav1.NewTime(time.Now())
				pod.DeletionTimestamp = &now
			}
			r, _ := newRegistry(t, pod)

			got, err := r.IsAlive(context.Background(), "wizard-"+tc.name)
			if err != nil {
				t.Fatalf("IsAlive: %v", err)
			}
			if got != tc.wantAlive {
				t.Errorf("IsAlive = %v, want %v", got, tc.wantAlive)
			}
		})
	}
}

func TestIsAlive_NotFound(t *testing.T) {
	r, _ := newRegistry(t)

	alive, err := r.IsAlive(context.Background(), "no-such-wizard")
	if !errors.Is(err, wizardregistry.ErrNotFound) {
		t.Errorf("IsAlive missing: error = %v, want ErrNotFound", err)
	}
	if alive {
		t.Errorf("IsAlive missing: alive = true, want false")
	}
}

func TestGet_PopulatesWizard(t *testing.T) {
	created := time.Unix(1700000200, 0)
	pod := wizardPodAt("wizard-spi-abc-w1-0", "wizard-spi-abc", "spi-abc", corev1.PodRunning, created)
	r, _ := newRegistry(t, pod)

	w, err := r.Get(context.Background(), "wizard-spi-abc")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if w.ID != "wizard-spi-abc" {
		t.Errorf("ID = %q, want wizard-spi-abc", w.ID)
	}
	if w.Mode != wizardregistry.ModeCluster {
		t.Errorf("Mode = %q, want ModeCluster", w.Mode)
	}
	if w.PodName != "wizard-spi-abc-w1-0" {
		t.Errorf("PodName = %q, want wizard-spi-abc-w1-0", w.PodName)
	}
	if w.Namespace != testNamespace {
		t.Errorf("Namespace = %q, want %q", w.Namespace, testNamespace)
	}
	if w.BeadID != "spi-abc" {
		t.Errorf("BeadID = %q, want spi-abc", w.BeadID)
	}
	if w.PID != 0 {
		t.Errorf("PID = %d, want 0 (cluster mode)", w.PID)
	}
	if w.StartedAt.IsZero() {
		t.Errorf("StartedAt is zero")
	}
}

func TestGet_NotFound(t *testing.T) {
	r, _ := newRegistry(t)

	_, err := r.Get(context.Background(), "no-such-wizard")
	if !errors.Is(err, wizardregistry.ErrNotFound) {
		t.Errorf("Get missing: error = %v, want ErrNotFound", err)
	}
}

func TestGet_IgnoresNonWizardPods(t *testing.T) {
	// Pod in the same namespace without the wizard role label.
	other := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "apprentice-foo",
			Namespace: testNamespace,
			Labels: map[string]string{
				cluster.DefaultRoleLabel: "apprentice",
				cluster.DefaultIDLabel:   "wizard-spi-abc", // collides on ID label only
			},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	r, _ := newRegistry(t, other)

	if _, err := r.Get(context.Background(), "wizard-spi-abc"); !errors.Is(err, wizardregistry.ErrNotFound) {
		t.Errorf("Get on apprentice-only pod: error = %v, want ErrNotFound", err)
	}
}

func TestList_OnlyWizardPods(t *testing.T) {
	wizardOne := wizardPod("wizard-pod-one", "wizard-one", "spi-one", corev1.PodRunning)
	wizardTwo := wizardPod("wizard-pod-two", "wizard-two", "spi-two", corev1.PodFailed)
	apprentice := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "apprentice-pod",
			Namespace: testNamespace,
			Labels: map[string]string{
				cluster.DefaultRoleLabel: "apprentice",
				cluster.DefaultIDLabel:   "apprentice-spi-x-0",
			},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}

	r, _ := newRegistry(t, wizardOne, wizardTwo, apprentice)

	got, err := r.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("List len = %d, want 2 (apprentice excluded)", len(got))
	}

	ids := []string{got[0].ID, got[1].ID}
	sort.Strings(ids)
	if ids[0] != "wizard-one" || ids[1] != "wizard-two" {
		t.Errorf("List ids = %v, want [wizard-one wizard-two]", ids)
	}
}

func TestList_RespectsNamespace(t *testing.T) {
	inNS := wizardPod("wizard-in-ns", "wizard-in", "spi-in", corev1.PodRunning)
	outOfNS := wizardPod("wizard-out-of-ns", "wizard-out", "spi-out", corev1.PodRunning)
	outOfNS.Namespace = "other"
	outOfNS.ObjectMeta.Namespace = "other"

	r, _ := newRegistry(t, inNS, outOfNS)

	got, err := r.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 || got[0].ID != "wizard-in" {
		ids := make([]string, len(got))
		for i, w := range got {
			ids[i] = w.ID
		}
		t.Errorf("List ids = %v, want [wizard-in]", ids)
	}
}

func TestSweep_ReturnsOnlyDead(t *testing.T) {
	alive1 := wizardPod("pod-alive-1", "alive-1", "spi-a1", corev1.PodRunning)
	alive2 := wizardPod("pod-alive-2", "alive-2", "spi-a2", corev1.PodRunning)
	dead1 := wizardPod("pod-dead-1", "dead-1", "spi-d1", corev1.PodFailed)
	dead2 := wizardPod("pod-dead-2", "dead-2", "spi-d2", corev1.PodSucceeded)
	pending := wizardPod("pod-pending", "pending-1", "spi-p1", corev1.PodPending)

	r, _ := newRegistry(t, alive1, alive2, dead1, dead2, pending)

	got, err := r.Sweep(context.Background())
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	wantIDs := map[string]bool{"dead-1": true, "dead-2": true, "pending-1": true}
	if len(got) != len(wantIDs) {
		ids := make([]string, len(got))
		for i, w := range got {
			ids[i] = w.ID
		}
		t.Fatalf("Sweep len = %d (%v), want 3 (%v)", len(got), ids, wantIDs)
	}
	for _, w := range got {
		if !wantIDs[w.ID] {
			t.Errorf("Sweep returned unexpected wizard %q", w.ID)
		}
	}
}

func TestSweep_DoesNotMutate(t *testing.T) {
	alive := wizardPod("pod-alive", "alive", "spi-a", corev1.PodRunning)
	dead := wizardPod("pod-dead", "dead", "spi-d", corev1.PodFailed)

	r, c := newRegistry(t, alive, dead)

	if _, err := r.Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	pods, err := c.CoreV1().Pods(testNamespace).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("List pods after Sweep: %v", err)
	}
	if len(pods.Items) != 2 {
		t.Errorf("pod count after Sweep = %d, want 2 (Sweep MUST NOT delete)", len(pods.Items))
	}
}

func TestUpsert_ReadOnly(t *testing.T) {
	r, c := newRegistry(t)

	w := wizardregistry.Wizard{
		ID:        "wizard-new",
		Mode:      wizardregistry.ModeCluster,
		PodName:   "pod-new",
		Namespace: testNamespace,
		BeadID:    "spi-new",
	}
	err := r.Upsert(context.Background(), w)
	if !errors.Is(err, wizardregistry.ErrReadOnly) {
		t.Errorf("Upsert err = %v, want ErrReadOnly", err)
	}

	pods, lerr := c.CoreV1().Pods(testNamespace).List(context.Background(), metav1.ListOptions{})
	if lerr != nil {
		t.Fatalf("post-Upsert List: %v", lerr)
	}
	if len(pods.Items) != 0 {
		t.Errorf("Upsert created %d pods, want 0 (must be no-op)", len(pods.Items))
	}
}

func TestRemove_ReadOnly(t *testing.T) {
	pod := wizardPod("pod-keep", "wizard-keep", "spi-keep", corev1.PodRunning)
	r, c := newRegistry(t, pod)

	err := r.Remove(context.Background(), "wizard-keep")
	if !errors.Is(err, wizardregistry.ErrReadOnly) {
		t.Errorf("Remove err = %v, want ErrReadOnly", err)
	}

	pods, lerr := c.CoreV1().Pods(testNamespace).List(context.Background(), metav1.ListOptions{})
	if lerr != nil {
		t.Fatalf("post-Remove List: %v", lerr)
	}
	if len(pods.Items) != 1 {
		t.Errorf("Remove deleted pods (count = %d, want 1, must be no-op)", len(pods.Items))
	}
}

func TestGet_RollingRestart_PrefersMostRecent(t *testing.T) {
	older := wizardPodAt(
		"wizard-old", "wizard-spi-rolling", "spi-rolling",
		corev1.PodRunning, time.Unix(1700000000, 0),
	)
	newer := wizardPodAt(
		"wizard-new", "wizard-spi-rolling", "spi-rolling",
		corev1.PodRunning, time.Unix(1700000999, 0),
	)

	r, _ := newRegistry(t, older, newer)

	w, err := r.Get(context.Background(), "wizard-spi-rolling")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if w.PodName != "wizard-new" {
		t.Errorf("Get PodName = %q, want wizard-new (most recent CreationTimestamp)", w.PodName)
	}
}

func TestList_EmptyClient(t *testing.T) {
	r, _ := newRegistry(t)
	got, err := r.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("List len = %d, want 0", len(got))
	}
}

func TestSweep_EmptyClient(t *testing.T) {
	r, _ := newRegistry(t)
	got, err := r.Sweep(context.Background())
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("Sweep len = %d, want 0", len(got))
	}
}

func TestOptions_OverrideLabels(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "custom-pod",
			Namespace: testNamespace,
			Labels: map[string]string{
				"my.role":    "wizard-custom",
				"my.id":      "custom-id",
				"my.bead-id": "spi-custom",
			},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	c := fake.NewSimpleClientset(pod)
	r := cluster.New(c, cluster.Options{
		Namespace:   testNamespace,
		RoleLabel:   "my.role",
		RoleValue:   "wizard-custom",
		IDLabel:     "my.id",
		BeadIDLabel: "my.bead-id",
	})

	w, err := r.Get(context.Background(), "custom-id")
	if err != nil {
		t.Fatalf("Get with custom labels: %v", err)
	}
	if w.ID != "custom-id" || w.BeadID != "spi-custom" {
		t.Errorf("Get = %+v, want ID=custom-id BeadID=spi-custom", w)
	}
}
