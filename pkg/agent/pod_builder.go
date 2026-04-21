// pod_builder.go exports the shared pod builder so callers outside of
// the Spawn() lifecycle (most notably the operator's AgentMonitor,
// which applies pods via controller-runtime's client) can produce the
// canonical pod object without going through pkg/agent's spawn path.
//
// The builder itself lives on K8sBackend in backend_k8s.go — this file
// exposes it via a thin exported wrapper so backend_k8s.go's unexported
// surface remains stable under the spi-xplwy package-boundary rules.
// See docs/design/spi-xplwy-runtime-contract.md §4 chunk 4.
package agent

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
)

// NewPodBuilder returns a K8sBackend configured as a pure pod builder
// (no live k8s API calls during BuildPod unless the caller asks for a
// shared-workspace substrate pod, in which case the client is used to
// locate the parent wizard's PVC). Callers that only build pods for
// non-substrate roles (e.g. the operator's wizard pods) may pass a nil
// client — any unexpected client use will then panic loudly, which is
// the desired fail-fast behavior under parity testing.
//
// The namespace, image, and secretName fields set here are used
// verbatim by buildRolePod, so operator-managed pods carry whatever
// settings the operator configured at startup rather than reading from
// process env on every spawn. This is the runtime-contract rule: no
// ambient env for identity inputs.
func NewPodBuilder(client kubernetes.Interface, namespace, image, secretName string) *K8sBackend {
	if secretName == "" {
		secretName = "spire-credentials"
	}
	return &K8sBackend{
		client:     client,
		namespace:  namespace,
		image:      image,
		secretName: secretName,
	}
}

// BuildPod returns the canonical pod for a SpawnConfig WITHOUT creating
// it in the cluster. This is the operator-facing entry point: the
// operator applies the returned *corev1.Pod through controller-runtime's
// client (which lets it attach owner references and merge guild-level
// overrides), rather than using K8sBackend.Spawn (which creates the pod
// directly via client-go).
//
// The pod shape and all container/volume/init-container structure come
// from the shared buildRolePod — the operator and pkg/agent backends
// converge on the same shape. Any operator-specific overrides (image,
// resources, labels, extra env) are applied by the caller after BuildPod
// returns.
func (b *K8sBackend) BuildPod(cfg SpawnConfig) (*corev1.Pod, error) {
	return b.buildRolePod(cfg)
}
