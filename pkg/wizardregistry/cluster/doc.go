// Package cluster is a Kubernetes pod-backed implementation of
// [wizardregistry.Registry].
//
// # Read-mostly contract
//
// In cluster mode the operator owns wizard-pod lifecycle via its
// reconciliation loop. Clients (steward, board, trace, summon, the
// upcoming refactored OrphanSweep) only read; they MUST NOT create or
// delete wizard pods directly. To enforce that boundary at the type
// level [Registry.Upsert] and [Registry.Remove] return
// [wizardregistry.ErrReadOnly] unconditionally. The conformance suite
// recognises this sentinel and skips write-dependent cases, so cluster
// callers exercise the same interface as local callers without holes
// in the contract.
//
// # Race-safety
//
// Every [Registry] method issues a fresh List against the configured
// [kubernetes.Interface]. No pod state is cached between calls and no
// snapshot is held across per-entry liveness checks. The
// authoritative-source rule of the [wizardregistry.Registry] contract
// is therefore satisfied by construction: a wizard pod that is
// scheduled (or terminated) between two calls is observed at the
// horizon of the kube-apiserver, not against a stale local view.
//
// In production, a typed client-go clientset (created from rest.Config)
// dispatches each List directly to the apiserver. In tests, the same
// interface is satisfied by [fake.NewSimpleClientset], which is
// strongly consistent against its in-memory tracker. Both backends meet
// the no-snapshot rule.
//
// # Dependency choice
//
// The change spec for spi-bsr4sj suggested a controller-runtime
// client. The root spire module already depends on
// [k8s.io/client-go] (see pkg/agent/backend_k8s.go) and does not depend
// on sigs.k8s.io/controller-runtime; pulling in controller-runtime here
// would add a substantial transitive dependency tree for one package
// that only needs to list and inspect pods. Using the typed clientset
// keeps the dependency surface flat and matches the existing k8s
// integration pattern in this repository. The race-safety and
// fakeability requirements are met identically by both options.
//
// # Label convention
//
// Wizard pods are stamped by the shared pod builder in pkg/agent with
// these labels (see pkg/agent/backend_k8s.go::podLabels):
//
//   - spire.agent       = "true"
//   - spire.role        = "wizard"
//   - spire.agent.name  = <opaque wizard ID, e.g. "wizard-spi-abc">
//   - spire.bead        = <bead ID, e.g. "spi-abc">
//
// The cluster registry selects on spire.role=wizard and treats
// spire.agent.name as the wizard-ID label. These defaults are
// overridable via [Options] for callers wiring an alternative scheme.
package cluster
