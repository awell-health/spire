package controllers

// Boundary tests for IntentWorkloadReconciler (spi-pg632).
//
// These tests pin the operator's cluster-native dispatch boundary:
//
//   1. Given an IntentConsumer that emits a WorkloadIntent, the
//      reconciler creates exactly one apprentice pod whose shape is
//      byte-for-byte equivalent to what pkg/agent.BuildApprenticePod
//      would produce for the same PodSpec (golden comparison). If the
//      operator ever grows a second pod-construction path, this test
//      fires.
//
//   2. Given an empty intent stream (no values sent on the channel),
//      the reconciler creates zero pods — it does NOT speculatively
//      scan beads, poll workloads, or invent work. The pod shape is a
//      pure function of received intents.

import (
	"context"
	"testing"
	"time"

	"github.com/go-logr/logr/testr"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/awell-health/spire/pkg/agent"
	"github.com/awell-health/spire/pkg/runtime"
	"github.com/awell-health/spire/pkg/steward/identity"
	"github.com/awell-health/spire/pkg/steward/intent"
)

// fakeIntentConsumer is a test double for intent.IntentConsumer that
// returns a caller-controlled channel. Closing the channel signals
// transport termination (the reconciler returns from Start).
type fakeIntentConsumer struct {
	ch chan intent.WorkloadIntent
}

func (f *fakeIntentConsumer) Consume(_ context.Context) (<-chan intent.WorkloadIntent, error) {
	return f.ch, nil
}

// fakeIdentityResolver is a test double for
// identity.ClusterIdentityResolver. It returns a fixed
// ClusterRepoIdentity regardless of the prefix — the reconciler tests
// do not exercise lookup misses; those are covered in pkg/steward/identity.
type fakeIdentityResolver struct {
	out identity.ClusterRepoIdentity
	err error
}

func (f *fakeIdentityResolver) Resolve(_ context.Context, _ string) (identity.ClusterRepoIdentity, error) {
	return f.out, f.err
}

// TestIntentReconciler_PodShapeMatchesSharedBuilder asserts that the
// reconciler routes pod construction through pkg/agent.BuildApprenticePod
// for the same logical PodSpec. Operator-specific overlays (the
// spire.awell.io/* reconciler labels) are applied on top; everything
// else must match the shared builder's output byte-for-byte.
func TestIntentReconciler_PodShapeMatchesSharedBuilder(t *testing.T) {
	const (
		ns                = "spire"
		attempt           = "spi-abc-0"
		prefix            = "spi"
		repoURL           = "git@example.com:spire-test/repo.git"
		branch            = "main"
		tower             = "spire"
		image             = "spire-agent:dev"
		dolthubRemote     = "https://dolthub.test/spire/spire"
		credentialsSecret = "spire-credentials"
		phase             = "implement"
		handoff           = "bundle"
	)

	scheme := newTestScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()

	ch := make(chan intent.WorkloadIntent, 1)
	consumer := &fakeIntentConsumer{ch: ch}
	resolver := &fakeIdentityResolver{out: identity.ClusterRepoIdentity{
		URL:        repoURL,
		BaseBranch: branch,
		Prefix:     prefix,
	}}

	r := &IntentWorkloadReconciler{
		Client:            c,
		Log:               testr.New(t),
		Namespace:         ns,
		Image:             image,
		Tower:             tower,
		DolthubRemote:     dolthubRemote,
		CredentialsSecret: credentialsSecret,
		Consumer:          consumer,
		Resolver:          resolver,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = r.Start(ctx)
	}()

	wi := intent.WorkloadIntent{
		AttemptID: attempt,
		RepoIdentity: intent.RepoIdentity{
			URL:        repoURL,
			BaseBranch: branch,
			Prefix:     prefix,
		},
		FormulaPhase: phase,
		HandoffMode:  handoff,
		Resources: intent.Resources{
			CPURequest:    "500m",
			CPULimit:      "1000m",
			MemoryRequest: "256Mi",
			MemoryLimit:   "1Gi",
		},
	}
	ch <- wi

	podName := apprenticePodName(attempt)
	gotPod := waitForPod(t, c, ns, podName, 3*time.Second)

	// Exactly-one-pod invariant: a single intent must produce a single
	// pod. The reconciler is not allowed to fan out, schedule siblings,
	// or create auxiliary resources speculatively.
	var pods corev1.PodList
	if err := c.List(ctx, &pods, client.InNamespace(ns)); err != nil {
		t.Fatalf("list pods: %v", err)
	}
	if len(pods.Items) != 1 {
		t.Fatalf("pod count = %d, want 1", len(pods.Items))
	}

	// Cancel and wait for Start to return before asserting against gotPod,
	// so the reconciler can't write to the client during the comparison.
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("reconciler Start did not return within 2s of cancel")
	}

	// Build the expected pod using the shared builder and the exact
	// PodSpec the reconciler would have derived. This is the golden
	// comparison: any divergence from BuildApprenticePod output (modulo
	// the reconciler-specific overlay labels) is a boundary regression.
	wantSpec := agent.PodSpec{
		Name:              podName,
		Namespace:         ns,
		Image:             image,
		AgentName:         podName,
		BeadID:            attempt,
		AttemptID:         attempt,
		FormulaStep:       phase,
		HandoffMode:       runtime.HandoffMode(handoff),
		Backend:           "operator-k8s",
		CredentialsSecret: credentialsSecret,
		DolthubRemote:     dolthubRemote,
		Identity: runtime.RepoIdentity{
			TowerName:  tower,
			Prefix:     prefix,
			RepoURL:    repoURL,
			BaseBranch: branch,
		},
		Resources: podResourcesFromIntent(wi.Resources),
	}
	wantPod, err := agent.BuildApprenticePod(wantSpec)
	if err != nil {
		t.Fatalf("pkg/agent.BuildApprenticePod: %v", err)
	}

	// Canonical name+namespace: the reconciler must not mutate these.
	if gotPod.Name != wantPod.Name {
		t.Errorf("pod Name: got %q, want %q", gotPod.Name, wantPod.Name)
	}
	if gotPod.Namespace != wantPod.Namespace {
		t.Errorf("pod Namespace: got %q, want %q", gotPod.Namespace, wantPod.Namespace)
	}

	// Pod-level invariants: RestartPolicy / PriorityClassName must come
	// from the shared builder unchanged.
	if gotPod.Spec.RestartPolicy != wantPod.Spec.RestartPolicy {
		t.Errorf("RestartPolicy: got %q, want %q", gotPod.Spec.RestartPolicy, wantPod.Spec.RestartPolicy)
	}
	if gotPod.Spec.PriorityClassName != wantPod.Spec.PriorityClassName {
		t.Errorf("PriorityClassName: got %q, want %q", gotPod.Spec.PriorityClassName, wantPod.Spec.PriorityClassName)
	}

	// Volumes must match exactly (same names, same sources).
	assertVolumesEqual(t, gotPod.Spec.Volumes, wantPod.Spec.Volumes)

	// Init containers: tower-attach + repo-bootstrap byte-for-byte.
	assertInitContainersPartialEqual(t, gotPod.Spec.InitContainers, wantPod.Spec.InitContainers, nil)

	// Main container: name, image, command, env, volume mounts.
	if len(gotPod.Spec.Containers) != 1 || len(wantPod.Spec.Containers) != 1 {
		t.Fatalf("containers: got=%d, want=%d; both must be 1",
			len(gotPod.Spec.Containers), len(wantPod.Spec.Containers))
	}
	gotMain := gotPod.Spec.Containers[0]
	wantMain := wantPod.Spec.Containers[0]
	if gotMain.Name != wantMain.Name {
		t.Errorf("main container Name: got %q, want %q", gotMain.Name, wantMain.Name)
	}
	if gotMain.Image != wantMain.Image {
		t.Errorf("main container Image: got %q, want %q", gotMain.Image, wantMain.Image)
	}
	if !stringSlicesEqual(gotMain.Command, wantMain.Command) {
		t.Errorf("main container Command: got %v, want %v", gotMain.Command, wantMain.Command)
	}
	assertVolumeMountsEqual(t, gotMain.VolumeMounts, wantMain.VolumeMounts)
	assertEnvSupersetValuesOnly(t, wantMain.Env, gotMain.Env, nil)

	// Labels: the shared builder's labels must all be present unchanged.
	// The reconciler overlay adds four spire.awell.io/* labels — assert
	// those separately.
	for k, want := range wantPod.Labels {
		if got := gotPod.Labels[k]; got != want {
			t.Errorf("label %q: got %q, want %q (must match shared builder)", k, got, want)
		}
	}
	wantOverlay := map[string]string{
		"spire.awell.io/bead":       attempt,
		"spire.awell.io/managed":    "true",
		"spire.awell.io/reconciler": "intent",
		"spire.awell.io/prefix":     prefix,
	}
	for k, want := range wantOverlay {
		if got := gotPod.Labels[k]; got != want {
			t.Errorf("reconciler overlay label %q: got %q, want %q", k, got, want)
		}
	}

	// Annotations: the shared builder emits attempt-id on every
	// apprentice pod (AttemptID was populated on the spec). The
	// reconciler must not drop it.
	for k, want := range wantPod.Annotations {
		if got := gotPod.Annotations[k]; got != want {
			t.Errorf("annotation %q: got %q, want %q", k, got, want)
		}
	}

	// Resources: requests/limits flow from intent.Resources through
	// podResourcesFromIntent unchanged.
	if !resourcesEqual(gotMain.Resources, wantMain.Resources) {
		t.Errorf("Resources differ:\n  got=%+v\n  want=%+v", gotMain.Resources, wantMain.Resources)
	}
}

// TestIntentReconciler_EmptyStreamCreatesNoPod pins the other half of
// the boundary: no intents in, no pods out. The reconciler must not
// poll beads, invent work, or speculatively create pods. Its only
// trigger is a value on the IntentConsumer channel.
func TestIntentReconciler_EmptyStreamCreatesNoPod(t *testing.T) {
	const ns = "spire"

	scheme := newTestScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()

	// An empty buffered channel — never sends a value. The reconciler's
	// Start loop blocks in the select on ctx.Done vs ch, so ctx
	// cancellation is the only exit.
	ch := make(chan intent.WorkloadIntent)
	consumer := &fakeIntentConsumer{ch: ch}
	resolver := &fakeIdentityResolver{out: identity.ClusterRepoIdentity{
		URL: "git@example.com:x/y.git", BaseBranch: "main", Prefix: "spi",
	}}

	r := &IntentWorkloadReconciler{
		Client:        c,
		Log:           testr.New(t),
		Namespace:     ns,
		Image:         "spire-agent:dev",
		Tower:         "spire",
		DolthubRemote: "https://dolthub.test/x/y",
		Consumer:      consumer,
		Resolver:      resolver,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	if err := r.Start(ctx); err != nil {
		t.Fatalf("Start: unexpected error: %v", err)
	}

	var pods corev1.PodList
	if err := c.List(context.Background(), &pods, client.InNamespace(ns)); err != nil {
		t.Fatalf("list pods: %v", err)
	}
	if len(pods.Items) != 0 {
		t.Fatalf("pod count = %d, want 0 (no intents emitted)", len(pods.Items))
	}
}

// TestIntentReconciler_NilConsumerSelfDisables documents the wave-0
// bring-up behavior: when the scheduler-to-reconciler seam is not wired
// yet, the reconciler starts, logs that it's disabled, and blocks on
// ctx. It must not create a pod, and it must not error out so the
// manager keeps running the other Runnables (AgentMonitor, etc.).
func TestIntentReconciler_NilConsumerSelfDisables(t *testing.T) {
	const ns = "spire"

	scheme := newTestScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()

	r := &IntentWorkloadReconciler{
		Client:    c,
		Log:       testr.New(t),
		Namespace: ns,
		Image:     "spire-agent:dev",
		Tower:     "spire",
		// Consumer intentionally nil — wave-0 default.
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	if err := r.Start(ctx); err != nil {
		t.Fatalf("Start: unexpected error: %v", err)
	}

	var pods corev1.PodList
	if err := c.List(context.Background(), &pods, client.InNamespace(ns)); err != nil {
		t.Fatalf("list pods: %v", err)
	}
	if len(pods.Items) != 0 {
		t.Fatalf("pod count = %d, want 0 (nil consumer must self-disable)", len(pods.Items))
	}
}

// waitForPod polls the fake client until the named pod exists or the
// deadline expires. Returns the pod. Fails the test on timeout.
func waitForPod(t *testing.T, c client.Client, namespace, name string, timeout time.Duration) corev1.Pod {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var pod corev1.Pod
	for {
		err := c.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: name}, &pod)
		if err == nil {
			return pod
		}
		if time.Now().After(deadline) {
			t.Fatalf("pod %s/%s not created within %v (last err: %v)", namespace, name, timeout, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
