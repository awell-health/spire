// Package wizardregistry defines the unified contract for tracking
// wizards across local-native and cluster-native deployment modes.
//
// A [Registry] implementation hides the deployment-mode specifics of
// "is this wizard alive?" behind a single interface. Local backends
// answer by probing OS processes; cluster backends answer by querying
// Kubernetes pod phase. Callers (orphan sweeps, board displays, agent
// monitors) consume liveness without caring which world they live in.
//
// # Race-safety rule
//
// [Registry.IsAlive] and [Registry.Sweep] MUST consult the authoritative
// source on each call. Implementations MUST NOT cache liveness across
// calls or operate on a snapshot of the wizard set captured before the
// per-entry liveness check. This rule prevents the OrphanSweep race in
// which a wizard upserted between snapshot capture and predicate
// evaluation is mis-classified as dead.
//
// # Adding a new backend
//
// To add a new [Registry] implementation:
//
//  1. Implement the [Registry] interface in a new sub-package.
//  2. Provide a test helper that satisfies the conformance Control
//     interface (a single SetAlive(id, alive bool) method that toggles
//     the backend's authoritative source for testing).
//  3. Add a `_test.go` file that calls
//     `conformance.Run(t, factory)` where factory returns fresh
//     instances of your registry plus its Control.
//
// The conformance suite (sub-package conformance) verifies the
// race-safety guarantee with a synthetic concurrent-Upsert-during-Sweep
// test that any compliant backend must pass under the Go race detector.
package wizardregistry
