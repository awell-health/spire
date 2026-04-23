// Package controllers — intent reconciler (spi-njzmg).
//
// IntentWorkloadReconciler is the canonical cluster-native entry point
// for turning dispatch intent into running apprentice pods. The steward
// emits pkg/steward/intent.WorkloadIntent values as it claims attempt
// beads; the operator consumes them through intent.IntentConsumer and
// reconciles the target pod (and any associated resources) to match.
//
// The reconciler is explicitly NOT a scheduler: it does not decide which
// bead to run next, which guild to route to, or how to throttle. Those
// decisions live in pkg/steward. Here we translate an already-approved
// WorkloadIntent into a pod and apply it. Pod shape comes from
// pkg/agent.BuildApprenticePod — the operator never reimplements pod
// construction.
//
// Identity invariant
// ------------------
// Cluster repo identity MUST resolve via
// pkg/steward/identity.ClusterIdentityResolver. When the intent carries
// URL/BaseBranch/Prefix fields, the reconciler treats them as projection
// state and reconciles them against the resolver's output. Any drift is
// logged and the canonical resolver value wins. No scheduling decision
// is ever made from CR-only fields.
package controllers

import (
	"context"
	"errors"
	"fmt"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/awell-health/spire/pkg/agent"
	"github.com/awell-health/spire/pkg/runtime"
	"github.com/awell-health/spire/pkg/steward/identity"
	"github.com/awell-health/spire/pkg/steward/intent"
)

// RBAC for the intent reconciler. The reconciler consumes a
// WorkloadIntent stream from the steward and creates a single apprentice
// pod per intent via r.Client.Create. It does not list or delete pods —
// lifecycle + cleanup are owned by AgentMonitor. The pod-create verb
// is already declared on AgentMonitor; repeating it here keeps the
// marker set local to the controller that actually runs the call.
//+kubebuilder:rbac:groups="",resources=pods,verbs=create

// IntentWorkloadReconciler consumes WorkloadIntent values from an
// intent.IntentConsumer and reconciles apprentice pods in the cluster
// via agent.BuildApprenticePod.
//
// The reconciler is the canonical cluster-native dispatch path post
// spi-njzmg. The legacy BeadWatcher/WorkloadAssigner loops are only
// started when the OperatorEnableLegacyScheduler gate flips on — the
// intent reconciler runs unconditionally as the canonical path.
type IntentWorkloadReconciler struct {
	Client    client.Client
	Log       logr.Logger
	Namespace string

	// Image is the apprentice container image. Plumbed from the
	// operator's --steward-image flag (the same image used for wizard
	// pods today).
	Image string

	// Tower is the dolt database name (== tower identity). Plumbed
	// once at startup from --database / $BEADS_DATABASE and stamped
	// onto PodSpec.Identity.TowerName — WorkloadIntent intentionally
	// does not carry tower identity (the tower IS the cluster in
	// cluster-native mode).
	Tower string

	// DolthubRemote is the dolt remote URL the tower-attach init
	// container uses. Plumbed from --dolthub-remote.
	DolthubRemote string

	// CredentialsSecret is the k8s Secret name holding
	// ANTHROPIC_API_KEY_DEFAULT + GITHUB_TOKEN. Empty defers to
	// agent.DefaultCredentialsSecret.
	CredentialsSecret string

	// GCSSecretName, when non-empty, is copied onto every apprentice
	// PodSpec so the in-pod BundleStore GCS client can authenticate
	// via a mounted service-account JSON. Plumbed from the operator
	// pod's SPIRE_GCP_SECRET_NAME env (helm sets it when the chart
	// deploys with bundleStore.backend=gcs). Empty disables the GCS
	// mount — local-backend apprentices stay on the unmounted shape.
	GCSSecretName string
	// GCSMountPath is the in-pod directory where the secret is mounted.
	// Plumbed from SPIRE_GCP_MOUNT_PATH; must match what the chart
	// renders for Values.gcp.mountPath so one
	// GOOGLE_APPLICATION_CREDENTIALS value works across features.
	GCSMountPath string
	// GCSKeyName is the filename of the service-account JSON within the
	// mount (the Secret's data key). Plumbed from SPIRE_GCP_KEY_NAME.
	GCSKeyName string

	// Consumer is the scheduler-to-reconciler seam. pkg/steward writes
	// WorkloadIntents via intent.IntentPublisher; the operator reads
	// them here. Nil disables the reconciler (Start returns
	// immediately) — this is the wave-0 default until the steward
	// emitter lands in wave-1.
	Consumer intent.IntentConsumer

	// Resolver is the canonical cluster repo-identity source. Required
	// by the identity invariant: URL/BaseBranch/Prefix on the intent
	// are projection-only and are reconciled against the resolver's
	// output. Nil disables the drift check (the intent's identity is
	// taken at face value) but emits a one-shot warning on Start.
	Resolver identity.ClusterIdentityResolver
}

// Start implements controller-runtime's Runnable interface. It blocks
// consuming the intent channel until ctx is cancelled or the transport
// closes the channel.
func (r *IntentWorkloadReconciler) Start(ctx context.Context) error {
	if r.Consumer == nil {
		r.Log.Info("intent reconciler disabled: no IntentConsumer wired",
			"backend", "operator-k8s")
		<-ctx.Done()
		return nil
	}
	if r.Resolver == nil {
		r.Log.Info("intent reconciler starting without ClusterIdentityResolver; drift check disabled",
			"backend", "operator-k8s")
	} else {
		r.Log.Info("intent reconciler starting",
			"tower", r.Tower, "namespace", r.Namespace, "backend", "operator-k8s")
	}

	ch, err := r.Consumer.Consume(ctx)
	if err != nil {
		return fmt.Errorf("intent reconciler: consume: %w", err)
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case wi, ok := <-ch:
			if !ok {
				r.Log.Info("intent channel closed; stopping reconciler",
					"backend", "operator-k8s")
				return nil
			}
			r.reconcile(ctx, wi)
		}
	}
}

// reconcile turns a single WorkloadIntent into a cluster apprentice pod
// by resolving canonical identity, building the pod via
// agent.BuildApprenticePod, and applying it through the controller
// client. Errors are logged and swallowed — the transport handles
// retries so the reconciler never falls behind on a single bad intent.
func (r *IntentWorkloadReconciler) reconcile(ctx context.Context, wi intent.WorkloadIntent) {
	if wi.AttemptID == "" {
		r.Log.Error(nil, "dropping intent with empty AttemptID",
			"backend", "operator-k8s")
		return
	}

	canonical, err := r.canonicalIdentity(ctx, wi.RepoIdentity)
	if err != nil {
		r.Log.Error(err, "failed to resolve canonical repo identity; dropping intent",
			"bead_id", wi.AttemptID,
			"intent_prefix", wi.RepoIdentity.Prefix,
			"backend", "operator-k8s")
		return
	}

	podName := apprenticePodName(wi.AttemptID)
	spec := agent.PodSpec{
		Name:              podName,
		Namespace:         r.Namespace,
		Image:             r.Image,
		AgentName:         podName,
		BeadID:            wi.AttemptID,
		AttemptID:         wi.AttemptID,
		FormulaStep:       wi.FormulaPhase,
		HandoffMode:       runtime.HandoffMode(wi.HandoffMode),
		Backend:           "operator-k8s",
		CredentialsSecret: r.CredentialsSecret,
		DolthubRemote:     r.DolthubRemote,
		Identity: runtime.RepoIdentity{
			TowerName:  r.Tower,
			Prefix:     canonical.Prefix,
			RepoURL:    canonical.URL,
			BaseBranch: canonical.BaseBranch,
		},
		GCSSecretName: r.GCSSecretName,
		GCSMountPath:  r.GCSMountPath,
		GCSKeyName:    r.GCSKeyName,
		Resources:     podResourcesFromIntent(wi.Resources),
	}

	pod, err := agent.BuildApprenticePod(spec)
	if err != nil {
		r.Log.Error(err, "agent.BuildApprenticePod failed; dropping intent",
			"bead_id", wi.AttemptID,
			"prefix", canonical.Prefix,
			"backend", "operator-k8s")
		return
	}

	// Mark the pod as reconciler-owned so list selectors can
	// distinguish intent-reconciled pods from legacy AgentMonitor
	// pods during the transitional coexistence window.
	if pod.Labels == nil {
		pod.Labels = map[string]string{}
	}
	pod.Labels["spire.awell.io/bead"] = wi.AttemptID
	pod.Labels["spire.awell.io/managed"] = "true"
	pod.Labels["spire.awell.io/reconciler"] = "intent"
	pod.Labels["spire.awell.io/prefix"] = canonical.Prefix

	if err := r.Client.Create(ctx, pod); err != nil {
		if apierrors.IsAlreadyExists(err) {
			r.Log.V(1).Info("pod already exists; skip create",
				"pod", pod.Name, "bead_id", wi.AttemptID, "backend", "operator-k8s")
			return
		}
		r.Log.Error(err, "failed to create apprentice pod",
			"pod", pod.Name, "bead_id", wi.AttemptID, "backend", "operator-k8s")
		return
	}

	r.Log.Info("reconciled intent into apprentice pod",
		"pod", pod.Name,
		"bead_id", wi.AttemptID,
		"prefix", canonical.Prefix,
		"formula_phase", wi.FormulaPhase,
		"handoff_mode", wi.HandoffMode,
		"backend", "operator-k8s")
}

// canonicalIdentity reconciles the intent's projected RepoIdentity
// against the shared ClusterIdentityResolver. The resolver's output is
// authoritative — when the intent and the resolver disagree the
// reconciler logs drift and returns the resolver value. When no
// resolver is wired (tests / wave-0 bring-up) the intent's fields are
// returned verbatim and a one-line warning is emitted.
func (r *IntentWorkloadReconciler) canonicalIdentity(ctx context.Context, projected intent.RepoIdentity) (identity.ClusterRepoIdentity, error) {
	if projected.Prefix == "" {
		return identity.ClusterRepoIdentity{}, errors.New("intent: empty repo prefix")
	}
	if r.Resolver == nil {
		if projected.URL == "" || projected.BaseBranch == "" {
			return identity.ClusterRepoIdentity{}, errors.New("intent: projected identity missing URL or BaseBranch and no resolver wired")
		}
		return identity.ClusterRepoIdentity{
			URL:        projected.URL,
			BaseBranch: projected.BaseBranch,
			Prefix:     projected.Prefix,
		}, nil
	}

	resolved, err := r.Resolver.Resolve(ctx, projected.Prefix)
	if err != nil {
		return identity.ClusterRepoIdentity{}, err
	}

	if projected.URL != "" && projected.URL != resolved.URL {
		r.Log.Info("intent URL drifts from canonical resolver; using resolver",
			"prefix", projected.Prefix,
			"intent_url", projected.URL,
			"canonical_url", resolved.URL,
			"backend", "operator-k8s")
	}
	if projected.BaseBranch != "" && projected.BaseBranch != resolved.BaseBranch {
		r.Log.Info("intent BaseBranch drifts from canonical resolver; using resolver",
			"prefix", projected.Prefix,
			"intent_branch", projected.BaseBranch,
			"canonical_branch", resolved.BaseBranch,
			"backend", "operator-k8s")
	}
	return resolved, nil
}

// apprenticePodName returns the canonical deterministic pod name for an
// attempt's apprentice. Idempotent — controller-runtime's Create is a
// no-op when the pod already exists, so repeated intents for the same
// attempt ID converge on a single pod.
func apprenticePodName(attemptID string) string {
	name := "apprentice-" + sanitizeK8sName(attemptID)
	if len(name) > 63 {
		name = name[:63]
	}
	return name
}

// podResourcesFromIntent converts the intent's string-quantity envelope
// into corev1.ResourceRequirements. Invalid quantities are dropped
// (logged by the caller when needed) rather than failing the pod build.
func podResourcesFromIntent(r intent.Resources) corev1.ResourceRequirements {
	out := corev1.ResourceRequirements{}
	requests := corev1.ResourceList{}
	limits := corev1.ResourceList{}
	if q, ok := parseQuantity(r.CPURequest); ok {
		requests[corev1.ResourceCPU] = q
	}
	if q, ok := parseQuantity(r.MemoryRequest); ok {
		requests[corev1.ResourceMemory] = q
	}
	if q, ok := parseQuantity(r.CPULimit); ok {
		limits[corev1.ResourceCPU] = q
	}
	if q, ok := parseQuantity(r.MemoryLimit); ok {
		limits[corev1.ResourceMemory] = q
	}
	if len(requests) > 0 {
		out.Requests = requests
	}
	if len(limits) > 0 {
		out.Limits = limits
	}
	return out
}

func parseQuantity(s string) (resource.Quantity, bool) {
	if s == "" {
		return resource.Quantity{}, false
	}
	q, err := resource.ParseQuantity(s)
	if err != nil {
		return resource.Quantity{}, false
	}
	return q, true
}
