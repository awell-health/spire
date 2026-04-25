package controllers

// Role-routing tests for IntentWorkloadReconciler (spi-5bzu9r.1).
//
// These tests pin the operator's role-keyed router: the reconciler
// validates the cluster contract first (intent.Validate), then routes
// by intent.Role through agent.SelectBuilder. wizard / apprentice /
// sage / cleric all materialize via their respective pod builders;
// unknown roles, unsupported (Role, Phase) pairs, and runtime-image
// gaps fail closed at validation rather than silently building the
// wrong pod shape.

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

func TestIntentReconciler_RoleRouting(t *testing.T) {
	const (
		ns      = "spire"
		prefix  = "spi"
		repoURL = "git@example.com:spire-test/repo.git"
		branch  = "main"
		tower   = "spire"
		image   = "spire-agent:dev"
		handoff = "bundle"
	)

	const dispatchSeq = 1

	cases := []struct {
		name     string
		role     intent.Role
		phase    intent.Phase
		taskID   string
		wantPod  func(string, int) string
		wantRole string
		wantCmd  []string // first three args after "spire"
	}{
		{
			name:     "wizard/implement routes to wizard pod",
			role:     intent.RoleWizard,
			phase:    intent.PhaseImplement,
			taskID:   "spi-w0",
			wantPod:  wizardPodName,
			wantRole: string(agent.RoleWizard),
			wantCmd:  []string{"execute", "spi-w0"},
		},
		{
			name:     "apprentice/implement routes to apprentice pod",
			role:     intent.RoleApprentice,
			phase:    intent.PhaseImplement,
			taskID:   "spi-i0",
			wantPod:  apprenticePodName,
			wantRole: string(agent.RoleApprentice),
			wantCmd:  []string{"apprentice", "run"},
		},
		{
			name:     "apprentice/fix routes to apprentice pod",
			role:     intent.RoleApprentice,
			phase:    intent.PhaseFix,
			taskID:   "spi-f0",
			wantPod:  apprenticePodName,
			wantRole: string(agent.RoleApprentice),
			wantCmd:  []string{"apprentice", "run"},
		},
		{
			name:     "apprentice/review-fix routes to apprentice pod",
			role:     intent.RoleApprentice,
			phase:    intent.PhaseReviewFix,
			taskID:   "spi-rf0",
			wantPod:  apprenticePodName,
			wantRole: string(agent.RoleApprentice),
			wantCmd:  []string{"apprentice", "run"},
		},
		{
			name:     "sage/review routes to sage pod",
			role:     intent.RoleSage,
			phase:    intent.PhaseReview,
			taskID:   "spi-r0",
			wantPod:  sagePodName,
			wantRole: string(agent.RoleSage),
			wantCmd:  []string{"sage", "review"},
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
				TaskID:      tc.taskID,
				DispatchSeq: dispatchSeq,
				RepoIdentity: intent.RepoIdentity{
					URL: repoURL, BaseBranch: branch, Prefix: prefix,
				},
				HandoffMode: handoff,
				Role:        tc.role,
				Phase:       tc.phase,
				Runtime:     intent.Runtime{Image: image},
			}

			pod := waitForPod(t, c, ns, tc.wantPod(tc.taskID, dispatchSeq), 3*time.Second)
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

// TestIntentReconciler_ClericRoutesViaRoleNotFormulaPhase pins the
// cleric routing migration: a Role=cleric, Phase=recovery intent
// produces a cleric pod whose main command runs `spire cleric ...`.
// The intent's FormulaPhase is intentionally left empty to prove the
// operator no longer requires (or reads) `formula_phase=recovery` to
// route cleric work — the legacy seam that the operator never
// actually recognized.
func TestIntentReconciler_ClericRoutesViaRoleNotFormulaPhase(t *testing.T) {
	const (
		ns      = "spire"
		prefix  = "spi"
		repoURL = "git@example.com:spire-test/repo.git"
		branch  = "main"
		tower   = "spire"
		image   = "spire-agent:dev"
	)

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
		TaskID:      "spi-rec0",
		DispatchSeq: 1,
		RepoIdentity: intent.RepoIdentity{
			URL: repoURL, BaseBranch: branch, Prefix: prefix,
		},
		HandoffMode:  "bundle",
		Role:         intent.RoleCleric,
		Phase:        intent.PhaseRecovery,
		Runtime:      intent.Runtime{Image: image},
		FormulaPhase: "", // intentionally empty: cleric routing must not depend on it
	}

	pod := waitForPod(t, c, ns, clericPodName("spi-rec0", 1), 3*time.Second)
	cancel()
	<-done

	if len(pod.Spec.Containers) != 1 {
		t.Fatalf("containers = %d, want 1", len(pod.Spec.Containers))
	}
	cmd := pod.Spec.Containers[0].Command
	wantHead := []string{"spire", "cleric", "diagnose", "spi-rec0"}
	if len(cmd) < len(wantHead) {
		t.Fatalf("cmd too short: %v", cmd)
	}
	for i, want := range wantHead {
		if cmd[i] != want {
			t.Errorf("cmd[%d] = %q, want %q (full %v)", i, cmd[i], want, cmd)
		}
	}
}

// TestIntentReconciler_UnsupportedPairDropsIntent covers the
// fail-closed validation path. An intent whose (Role, Phase) pair is
// not in intent.Allowed must be dropped before any pod is created;
// the previous "unknown phase falls through to apprentice" bug must
// not regress.
func TestIntentReconciler_UnsupportedPairDropsIntent(t *testing.T) {
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

	// sage/implement is in the cluster contract's "shape is valid but
	// pair is not allowed" zone — the canonical regression case for
	// the silent-fallthrough bug the new contract closes.
	ch <- intent.WorkloadIntent{
		TaskID:      "spi-bogus",
		DispatchSeq: 1,
		RepoIdentity: intent.RepoIdentity{
			URL: "git@example.com:x/y.git", BaseBranch: "main", Prefix: "spi",
		},
		HandoffMode: "bundle",
		Role:        intent.RoleSage,
		Phase:       intent.PhaseImplement,
		Runtime:     intent.Runtime{Image: "spire-agent:dev"},
	}

	<-done
	var pods corev1.PodList
	if err := c.List(context.Background(), &pods, client.InNamespace(ns)); err != nil {
		t.Fatalf("list pods: %v", err)
	}
	if len(pods.Items) != 0 {
		t.Fatalf("pod count = %d, want 0 (unsupported role/phase must be dropped)", len(pods.Items))
	}
}

// TestIntentReconciler_MissingRoleDropsIntent covers the empty-role
// validation path. An intent without Role set is incomplete under the
// cluster contract and must be dropped at validation.
func TestIntentReconciler_MissingRoleDropsIntent(t *testing.T) {
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
		TaskID:      "spi-noroles",
		DispatchSeq: 1,
		RepoIdentity: intent.RepoIdentity{
			URL: "git@example.com:x/y.git", BaseBranch: "main", Prefix: "spi",
		},
		HandoffMode: "bundle",
		// Role / Phase / Runtime intentionally zero-valued.
	}

	<-done
	var pods corev1.PodList
	if err := c.List(context.Background(), &pods, client.InNamespace(ns)); err != nil {
		t.Fatalf("list pods: %v", err)
	}
	if len(pods.Items) != 0 {
		t.Fatalf("pod count = %d, want 0 (missing role must be dropped)", len(pods.Items))
	}
}
