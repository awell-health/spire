package cluster

import (
	"context"
	"fmt"
	"time"

	"github.com/awell-health/spire/pkg/wizardregistry"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Default label keys and values stamped on every wizard pod by the
// shared pod builder in pkg/agent. Override via [Options] when wiring
// against a non-canonical label scheme.
const (
	DefaultRoleLabel   = "spire.role"
	DefaultRoleValue   = "wizard"
	DefaultIDLabel     = "spire.agent.name"
	DefaultBeadIDLabel = "spire.bead"
)

// Options configures a cluster [Registry].
//
// Namespace is required; it scopes every List to a single Kubernetes
// namespace. The label fields default to the values stamped on wizard
// pods by pkg/agent's shared builder, so most callers can pass only
// Namespace.
type Options struct {
	// Namespace is the Kubernetes namespace to query. Required.
	Namespace string

	// RoleLabel is the label key whose value identifies wizard pods.
	// Defaults to "spire.role".
	RoleLabel string

	// RoleValue is the value of RoleLabel that selects wizard pods.
	// Defaults to "wizard".
	RoleValue string

	// IDLabel is the label key whose value is the wizard ID.
	// Defaults to "spire.agent.name".
	IDLabel string

	// BeadIDLabel is the label key whose value is the bead ID this
	// wizard is orchestrating. Defaults to "spire.bead".
	BeadIDLabel string
}

// Registry is a [wizardregistry.Registry] backed by Kubernetes pods.
//
// Read-mostly: write methods return [wizardregistry.ErrReadOnly]. See
// the package documentation for the contract.
type Registry struct {
	c           kubernetes.Interface
	namespace   string
	baseLabel   string
	idLabel     string
	beadIDLabel string
}

// New returns a Registry that queries the given clientset.
//
// New panics if opts.Namespace is empty: a registry that lists pods
// across every namespace would silently widen the operator-owned
// blast radius and is never the right default.
func New(c kubernetes.Interface, opts Options) *Registry {
	if opts.Namespace == "" {
		panic("wizardregistry/cluster: Options.Namespace is required")
	}
	roleLabel := opts.RoleLabel
	if roleLabel == "" {
		roleLabel = DefaultRoleLabel
	}
	roleValue := opts.RoleValue
	if roleValue == "" {
		roleValue = DefaultRoleValue
	}
	idLabel := opts.IDLabel
	if idLabel == "" {
		idLabel = DefaultIDLabel
	}
	beadIDLabel := opts.BeadIDLabel
	if beadIDLabel == "" {
		beadIDLabel = DefaultBeadIDLabel
	}
	return &Registry{
		c:           c,
		namespace:   opts.Namespace,
		baseLabel:   fmt.Sprintf("%s=%s", roleLabel, roleValue),
		idLabel:     idLabel,
		beadIDLabel: beadIDLabel,
	}
}

// List returns one [wizardregistry.Wizard] per matching pod in the
// configured namespace. Each call dispatches a fresh List to the
// kube-apiserver; no result is cached.
func (r *Registry) List(ctx context.Context) ([]wizardregistry.Wizard, error) {
	pods, err := r.listPods(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]wizardregistry.Wizard, 0, len(pods))
	for i := range pods {
		out = append(out, toWizard(&pods[i], r.idLabel, r.beadIDLabel))
	}
	return out, nil
}

// Get returns the wizard with the given ID. Returns
// [wizardregistry.ErrNotFound] when no matching pod exists. When more
// than one pod carries the same ID label (e.g. mid-rolling-restart),
// Get returns the pod with the most recent CreationTimestamp.
func (r *Registry) Get(ctx context.Context, id string) (wizardregistry.Wizard, error) {
	pod, err := r.podFor(ctx, id)
	if err != nil {
		return wizardregistry.Wizard{}, err
	}
	return toWizard(pod, r.idLabel, r.beadIDLabel), nil
}

// Upsert returns [wizardregistry.ErrReadOnly]. Wizard-pod creation is
// owned by the operator's reconciliation loop; clients do not write to
// the cluster registry.
func (r *Registry) Upsert(_ context.Context, _ wizardregistry.Wizard) error {
	return wizardregistry.ErrReadOnly
}

// Remove returns [wizardregistry.ErrReadOnly]. Wizard-pod deletion is
// owned by the operator's reconciliation loop; clients do not write to
// the cluster registry.
func (r *Registry) Remove(_ context.Context, _ string) error {
	return wizardregistry.ErrReadOnly
}

// IsAlive reports whether the wizard with the given ID is alive.
//
// A pod is alive iff its phase is [corev1.PodRunning] and its
// DeletionTimestamp is nil. Returns (false, [wizardregistry.ErrNotFound])
// when no pod carries the given ID. Returns (false, nil) when the pod
// exists but is no longer running (Pending, Failed, Succeeded, Unknown,
// or terminating).
func (r *Registry) IsAlive(ctx context.Context, id string) (bool, error) {
	pod, err := r.podFor(ctx, id)
	if err != nil {
		return false, err
	}
	return isAlive(pod), nil
}

// Sweep returns the subset of registered wizards whose pod is no
// longer alive. Sweep is predicate-only: it MUST NOT delete any pods.
// The returned slice order is unspecified.
func (r *Registry) Sweep(ctx context.Context) ([]wizardregistry.Wizard, error) {
	pods, err := r.listPods(ctx)
	if err != nil {
		return nil, err
	}
	var dead []wizardregistry.Wizard
	for i := range pods {
		if !isAlive(&pods[i]) {
			dead = append(dead, toWizard(&pods[i], r.idLabel, r.beadIDLabel))
		}
	}
	return dead, nil
}

// listPods issues a fresh List against the kube-apiserver, filtered to
// the wizard-role label in the configured namespace.
func (r *Registry) listPods(ctx context.Context) ([]corev1.Pod, error) {
	pods, err := r.c.CoreV1().Pods(r.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: r.baseLabel,
	})
	if err != nil {
		return nil, fmt.Errorf("wizardregistry/cluster: list pods: %w", err)
	}
	return pods.Items, nil
}

// podFor returns the wizard pod with the given ID label.
// When more than one pod matches (e.g. mid-rolling-restart), podFor
// returns the pod with the most recent CreationTimestamp.
// Returns [wizardregistry.ErrNotFound] when no pod matches.
func (r *Registry) podFor(ctx context.Context, id string) (*corev1.Pod, error) {
	selector := fmt.Sprintf("%s,%s=%s", r.baseLabel, r.idLabel, id)
	pods, err := r.c.CoreV1().Pods(r.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: selector,
	})
	if err != nil {
		return nil, fmt.Errorf("wizardregistry/cluster: get pod %q: %w", id, err)
	}
	if len(pods.Items) == 0 {
		return nil, wizardregistry.ErrNotFound
	}
	return pickMostRecent(pods.Items), nil
}

// isAlive reports whether p is in a state we treat as a live wizard.
// nil is dead; a pod with DeletionTimestamp set is dead even while the
// kubelet is still draining it; all non-Running phases are dead.
func isAlive(p *corev1.Pod) bool {
	if p == nil {
		return false
	}
	if p.DeletionTimestamp != nil {
		return false
	}
	return p.Status.Phase == corev1.PodRunning
}

// toWizard projects a pod into the mode-tagged Wizard struct. ID and
// BeadID are read from the configured labels; PodName and Namespace
// come from object metadata; StartedAt prefers Status.StartTime (set
// when the kubelet starts the pod) and falls back to CreationTimestamp
// (set at apiserver-admission time) when the pod has not yet started.
func toWizard(p *corev1.Pod, idLabel, beadIDLabel string) wizardregistry.Wizard {
	var started time.Time
	if p.Status.StartTime != nil {
		started = p.Status.StartTime.Time
	} else {
		started = p.CreationTimestamp.Time
	}
	return wizardregistry.Wizard{
		ID:        p.Labels[idLabel],
		Mode:      wizardregistry.ModeCluster,
		PodName:   p.Name,
		Namespace: p.Namespace,
		BeadID:    p.Labels[beadIDLabel],
		StartedAt: started,
	}
}

// pickMostRecent returns a pointer to the pod in pods with the latest
// CreationTimestamp. Used to pick the surviving wizard during a
// rolling-restart window when two pods briefly share an ID label.
// pods MUST be non-empty.
func pickMostRecent(pods []corev1.Pod) *corev1.Pod {
	idx := 0
	for i := 1; i < len(pods); i++ {
		if pods[i].CreationTimestamp.Time.After(pods[idx].CreationTimestamp.Time) {
			idx = i
		}
	}
	return &pods[idx]
}

// Compile-time assertion that *Registry satisfies the contract.
var _ wizardregistry.Registry = (*Registry)(nil)
