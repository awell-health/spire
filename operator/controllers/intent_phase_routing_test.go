package controllers

// Phase-routing tests for IntentWorkloadReconciler (spi-12rno4).
//
// These tests pin the operator's phase-class router: the reconciler
// switches on intent.FormulaPhase via intent.IsBeadLevelPhase /
// IsStepLevelPhase / IsReviewLevelPhase and routes to wizard /
// apprentice / sage pod builders respectively. An unknown phase must
// be dropped with an error log — never silently fall through to the
// apprentice path.

import (
	"context"
	"testing"
	"time"

	"github.com/go-logr/logr/testr"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/awell-health/spire/pkg/agent"
	"github.com/awell-health/spire/pkg/steward/identity"
	"github.com/awell-health/spire/pkg/steward/intent"
)

func TestIntentReconciler_PhaseRouting(t *testing.T) {
	const (
		ns      = "spire"
		prefix  = "spi"
		repoURL = "git@example.com:spire-test/repo.git"
		branch  = "main"
		tower   = "spire"
		image   = "spire-agent:dev"
		handoff = "bundle"
	)

	cases := []struct {
		name      string
		phase     string
		attemptID string
		wantPod   func(string) string
		wantRole  string
		wantCmd   []string // first three args after "spire"
	}{
		{
			name:      "bead-level wizard",
			phase:     intent.PhaseWizard,
			attemptID: "spi-w0",
			wantPod:   wizardPodName,
			wantRole:  string(agent.RoleWizard),
			wantCmd:   []string{"execute", "spi-w0"},
		},
		{
			name:      "bead-level type=task routes to wizard",
			phase:     "task",
			attemptID: "spi-t0",
			wantPod:   wizardPodName,
			wantRole:  string(agent.RoleWizard),
			wantCmd:   []string{"execute", "spi-t0"},
		},
		{
			name:      "bead-level type=epic routes to wizard",
			phase:     "epic",
			attemptID: "spi-e0",
			wantPod:   wizardPodName,
			wantRole:  string(agent.RoleWizard),
			wantCmd:   []string{"execute", "spi-e0"},
		},
		{
			name:      "step-level implement routes to apprentice",
			phase:     intent.PhaseImplement,
			attemptID: "spi-i0",
			wantPod:   apprenticePodName,
			wantRole:  string(agent.RoleApprentice),
			wantCmd:   []string{"apprentice", "run"},
		},
		{
			name:      "step-level fix routes to apprentice",
			phase:     intent.PhaseFix,
			attemptID: "spi-f0",
			wantPod:   apprenticePodName,
			wantRole:  string(agent.RoleApprentice),
			wantCmd:   []string{"apprentice", "run"},
		},
		{
			name:      "review-level review routes to sage",
			phase:     intent.PhaseReview,
			attemptID: "spi-r0",
			wantPod:   sagePodName,
			wantRole:  string(agent.RoleSage),
			wantCmd:   []string{"sage", "review"},
		},
		{
			name:      "review-level arbiter routes to sage",
			phase:     intent.PhaseArbiter,
			attemptID: "spi-a0",
			wantPod:   sagePodName,
			wantRole:  string(agent.RoleSage),
			wantCmd:   []string{"sage", "review"},
		},
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
				AttemptID: tc.attemptID,
				RepoIdentity: intent.RepoIdentity{
					URL: repoURL, BaseBranch: branch, Prefix: prefix,
				},
				FormulaPhase: tc.phase,
				HandoffMode:  handoff,
			}

			pod := waitForPod(t, c, ns, tc.wantPod(tc.attemptID), 3*time.Second)
			cancel()
			<-done

			if got := pod.Labels["spire.role"]; got != tc.wantRole {
				t.Errorf("spire.role label = %q, want %q", got, tc.wantRole)
			}
			if len(pod.Spec.Containers) != 1 {
				t.Fatalf("containers = %d, want 1", len(pod.Spec.Containers))
			}
			cmd := pod.Spec.Containers[0].Command
			if len(cmd) < 1+len(tc.wantCmd) || cmd[0] != "spire" {
				t.Fatalf("command = %v, want spire %v ...", cmd, tc.wantCmd)
			}
			for i, want := range tc.wantCmd {
				if got := cmd[1+i]; got != want {
					t.Errorf("command[%d] = %q, want %q (full %v)", 1+i, got, want, cmd)
				}
			}
		})
	}
}

func TestIntentReconciler_UnknownPhaseDropsIntent(t *testing.T) {
	const ns = "spire"
	scheme := newTestScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()

	ch := make(chan intent.WorkloadIntent, 1)
	r := &IntentWorkloadReconciler{
		Client:        c,
		Log:           testr.New(t),
		Namespace:     ns,
		Image:         "spire-agent:dev",
		Tower:         "spire",
		DolthubRemote: "https://dolthub.test/x/y",
		Consumer:      &fakeIntentConsumer{ch: ch},
		Resolver: &fakeIdentityResolver{out: identity.ClusterRepoIdentity{
			URL: "git@example.com:x/y.git", BaseBranch: "main", Prefix: "spi",
		}},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); _ = r.Start(ctx) }()

	ch <- intent.WorkloadIntent{
		AttemptID: "spi-bogus-0",
		RepoIdentity: intent.RepoIdentity{
			URL: "git@example.com:x/y.git", BaseBranch: "main", Prefix: "spi",
		},
		FormulaPhase: "totally-not-a-phase",
		HandoffMode:  "bundle",
	}

	<-done
	var pods corev1.PodList
	if err := c.List(context.Background(), &pods, client.InNamespace(ns)); err != nil {
		t.Fatalf("list pods: %v", err)
	}
	if len(pods.Items) != 0 {
		t.Fatalf("pod count = %d, want 0 (unknown phase must be dropped)", len(pods.Items))
	}
}
