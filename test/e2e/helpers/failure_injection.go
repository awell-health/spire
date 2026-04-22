//go:build e2e

package helpers

import (
	"testing"

	"k8s.io/client-go/dynamic"
)

// BogusBranchPin is a deliberately non-existent git ref. Setting a
// WizardGuild's Cache.BranchPin to this value forces the next refresh
// Job's `git fetch` to exit non-zero, which — after the Job's default
// backoffLimit is reached — trips the BackoffLimitExceeded condition
// the operator treats as a permanent failure (see
// operator/controllers/cache_recovery.go:isRefreshJobBackoffExhausted).
//
// Using a branch pin rather than patching the Job's command directly
// keeps the failure flowing through the production reconciler path:
// the test only talks to the CR surface, and the operator drives
// everything else the way it would in prod.
const BogusBranchPin = "refs/heads/e2e-test-does-not-exist"

// BreakCacheRefresh points the next refresh Job at a non-existent git
// branch by patching Spec.Cache.BranchPin. Documented chosen mechanism:
// branch-pin redirection.
//
// Why branch-pin rather than patching the Job's command to `exit 1`:
//  1. The spec-level patch goes through the operator reconciler, so the
//     failure exercises the same code path real misconfigurations hit in
//     production — no test-only surface area.
//  2. BranchPin is a first-class CacheSpec field, so the change is
//     idempotent (re-pin to the same value is a no-op) and the operator's
//     Job template regeneration naturally picks it up on the next
//     reconcile.
//  3. The alternative — mutating the Job's command template — races the
//     reconciler (it would overwrite our edit on the next pass) and would
//     require the test to maintain a private vocabulary for injected
//     failures.
//
// Rejected alternative mechanisms and why:
//   - Patching Job.spec.template.spec.containers[0].command to `sh -c "exit 1"`:
//     races reconciler, requires knowing the refresh container name, and
//     doesn't survive Job regeneration. The wisp wouldn't carry useful
//     termination-log context because the container's Terminated.Message
//     would be empty.
//   - Scaling down a dependent service (e.g. setting the tower's dolt
//     replicas to 0): far too blunt — it would break every other bead
//     operation in the namespace and mask the wisp-specific path.
//   - Pointing cache origin at a bogus URL: not possible via CacheSpec —
//     the spec intentionally carries no repo URL (spi-xplwy). The operator
//     resolves origin from tower configuration, which is out of reach of
//     the test harness.
func BreakCacheRefresh(t *testing.T, dyn dynamic.Interface, namespace, guildName string) {
	t.Helper()
	PatchWizardGuildCacheBranchPin(t, dyn, namespace, guildName, BogusBranchPin)
}
