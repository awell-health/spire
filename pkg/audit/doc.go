// Package audit holds source-level safety checks that run at `go test` time
// to catch cluster-bootstrap regressions before they ship as container
// images. The tests walk the AST of cluster-reachable packages and assert
// invariants that are easy to lose in review but expensive to debug in a
// CGO-disabled pod.
//
// The checks here exist to preempt the failure class audited under spi-ey3obm:
//
//  1. bdpkg.NewClient() usage in cluster paths that doesn't pin server mode
//     or wire a BEADS_DIR, producing "embedded Dolt requires CGO" crashes
//     when the steward image runs (same shape as spi-lfkfgh).
//  2. Direct exec.Command("bd", …) shell-outs in cluster paths that inherit
//     no BEADS_DIR env var, so the bd subprocess walks up from a CWD like
//     /etc/spire and fails to find .beads/.
//  3. config.ExtractSQLValue callers whose SQL shape could regress past the
//     positional parser's contract (same shape as spi-69b6ge / spi-19v3oa).
//
// When a test here fails, the fix is one of:
//   - Pass an explicit server URL to bdpkg.Client.Init (Server: true,
//     ServerHost/Port), OR set client.BeadsDir to the absolute cluster path.
//   - Replace direct exec.Command("bd", …) with bdpkg.Client (which sets
//     BEADS_DIR per-call), or set BEADS_DIR in the call site.
//   - Add the call site to the documented allowlist below with a justification.
package audit
