package controllers

// Dolt URL plumbing tests for IntentWorkloadReconciler (spi-o4f4eh).
//
// Pins that the reconciler stamps IntentWorkloadReconciler.DoltURL
// onto every PodSpec it builds, for all three phase classes (wizard /
// apprentice / sage), and that pkg/agent.buildEnv propagates the
// resulting DOLT_URL + BEADS_DOLT_SERVER_HOST + BEADS_DOLT_SERVER_PORT
// env onto BOTH the main container AND every init container. Without
// this the in-pod tower-attach init container falls back to the
// laptop-default 127.0.0.1:3307 and the whole cluster-native dispatch
// stalls on the first init container.

import (
	"context"
	"testing"
	"time"

	"github.com/go-logr/logr/testr"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/awell-health/spire/pkg/steward/identity"
	"github.com/awell-health/spire/pkg/steward/intent"
)

func TestIntentReconciler_DoltURLStampedOnAllPhases(t *testing.T) {
	const (
		ns       = "spire-smoke"
		prefix   = "smk"
		repoURL  = "git@example.com:spire-test/repo.git"
		branch   = "main"
		tower    = "smoke"
		image    = "spire-agent:dev"
		doltURL  = "spire-dolt.spire-smoke.svc:3306"
		wantHost = "spire-dolt.spire-smoke.svc"
		wantPort = "3306"
	)

	const dispatchSeq = 1

	cases := []struct {
		name    string
		phase   string
		taskID  string
		podName func(string, int) string
	}{
		{"bead-level wizard", intent.PhaseWizard, "smk-w0", wizardPodName},
		{"step-level implement", intent.PhaseImplement, "smk-i0", apprenticePodName},
		{"review-level review", intent.PhaseReview, "smk-r0", sagePodName},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			scheme := newTestScheme(t)
			c := fake.NewClientBuilder().WithScheme(scheme).Build()

			ch := make(chan intent.WorkloadIntent, 1)
			r := &IntentWorkloadReconciler{
				Client:        c,
				Log:           testr.New(t),
				Namespace:     ns,
				Image:         image,
				Tower:         tower,
				DolthubRemote: "https://dolthub.test/spire/spire",
				DoltURL:       doltURL,
				Consumer:      &fakeIntentConsumer{ch: ch},
				Resolver: &fakeIdentityResolver{out: identity.ClusterRepoIdentity{
					URL: repoURL, BaseBranch: branch, Prefix: prefix,
				}},
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan struct{})
			go func() { defer close(done); _ = r.Start(ctx) }()

			ch <- intent.WorkloadIntent{
				TaskID:      tc.taskID,
				DispatchSeq: dispatchSeq,
				RepoIdentity: intent.RepoIdentity{
					URL: repoURL, BaseBranch: branch, Prefix: prefix,
				},
				FormulaPhase: tc.phase,
				HandoffMode:  "bundle",
			}

			pod := waitForPod(t, c, ns, tc.podName(tc.taskID, dispatchSeq), 3*time.Second)
			cancel()
			<-done

			wantEnv := map[string]string{
				"DOLT_URL":               doltURL,
				"BEADS_DOLT_SERVER_HOST": wantHost,
				"BEADS_DOLT_SERVER_PORT": wantPort,
			}

			if len(pod.Spec.Containers) != 1 {
				t.Fatalf("containers = %d, want 1", len(pod.Spec.Containers))
			}
			assertContainerHasEnv(t, "main", pod.Spec.Containers[0].Env, wantEnv)

			if len(pod.Spec.InitContainers) == 0 {
				t.Fatalf("no init containers on pod %q; wizard/sage/apprentice pods must have tower-attach", pod.Name)
			}
			for _, ic := range pod.Spec.InitContainers {
				assertContainerHasEnv(t, "init:"+ic.Name, ic.Env, wantEnv)
			}
		})
	}
}

// TestIntentReconciler_EmptyDoltURLOmitsEnv pins the opposite boundary:
// when DoltURL is unset, pkg/agent.buildEnv must not emit DOLT_URL or
// the split BEADS_DOLT_SERVER_* env. This is the pre-fix state and
// proves the fix is driven by the new reconciler field, not by a
// silent default.
func TestIntentReconciler_EmptyDoltURLOmitsEnv(t *testing.T) {
	const (
		ns      = "spire"
		prefix  = "spi"
		repoURL = "git@example.com:spire-test/repo.git"
		branch  = "main"
	)

	scheme := newTestScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()

	ch := make(chan intent.WorkloadIntent, 1)
	r := &IntentWorkloadReconciler{
		Client:        c,
		Log:           testr.New(t),
		Namespace:     ns,
		Image:         "spire-agent:dev",
		Tower:         "spire",
		DolthubRemote: "https://dolthub.test/spire/spire",
		// DoltURL intentionally empty.
		Consumer: &fakeIntentConsumer{ch: ch},
		Resolver: &fakeIdentityResolver{out: identity.ClusterRepoIdentity{
			URL: repoURL, BaseBranch: branch, Prefix: prefix,
		}},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); _ = r.Start(ctx) }()

	ch <- intent.WorkloadIntent{
		TaskID:      "spi-x0",
		DispatchSeq: 1,
		RepoIdentity: intent.RepoIdentity{
			URL: repoURL, BaseBranch: branch, Prefix: prefix,
		},
		FormulaPhase: intent.PhaseWizard,
		HandoffMode:  "bundle",
	}

	pod := waitForPod(t, c, ns, wizardPodName("spi-x0", 1), 3*time.Second)
	cancel()
	<-done

	forbidden := []string{"DOLT_URL", "BEADS_DOLT_SERVER_HOST", "BEADS_DOLT_SERVER_PORT"}
	for _, name := range forbidden {
		if v, ok := findEnv(pod.Spec.Containers[0].Env, name); ok {
			t.Errorf("main container has unexpected %s=%q when DoltURL empty", name, v)
		}
		for _, ic := range pod.Spec.InitContainers {
			if v, ok := findEnv(ic.Env, name); ok {
				t.Errorf("init %q has unexpected %s=%q when DoltURL empty", ic.Name, name, v)
			}
		}
	}
}

func assertContainerHasEnv(t *testing.T, label string, env []corev1.EnvVar, want map[string]string) {
	t.Helper()
	for k, v := range want {
		got, ok := findEnv(env, k)
		if !ok {
			t.Errorf("%s: missing env %q (want %q)", label, k, v)
			continue
		}
		if got != v {
			t.Errorf("%s: env %s = %q, want %q", label, k, got, v)
		}
	}
}

func findEnv(env []corev1.EnvVar, name string) (string, bool) {
	for _, e := range env {
		if e.Name == name {
			return e.Value, true
		}
	}
	return "", false
}
