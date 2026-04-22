# End-to-end cache-recovery test

This directory holds the acceptance test for the pinned-identity + wisp
recovery epic (**spi-w860i**). The test walks the full production data
flow from a `WizardGuild` CR's cache failing through to a cleric agent
resolving the wisp and leaving a learning row behind.

## What it covers

```
WizardGuild CR (Cache.Enabled=true)
  → operator reconciler creates pinned identity bead (StatusPinned + pinned=true)
  → [test injects refresh failure via bogus BranchPin]
  → refresh Job backoff exhausted
  → operator files wisp recovery bead (caused-by → pinned, SourceResourceURI stamped)
  → steward hooked-sweep claims wisp
  → cleric runs cleric-default formula
      collect_context → decide (Claude) → execute → verify → learn → finish
  → WriteOutcome stamps recovery_learnings SQL row keyed by SourceResourceURI
  → wisp closed
  → wisp GC reaps wisp row + wisp_dependencies edge
  → recovery_learnings row survives; analytics query by SourceResourceURI returns it
  → on CR delete, finalizer closes open wisps BEFORE the pinned bead
```

Each arrow is an assertion inside `cache_recovery_test.go`. When a
sibling task in the epic is incomplete, the test fails loudly at the
corresponding stage — no `t.Skip`, no silent fallthrough.

## Prerequisites

Before running locally:

1. **Minikube is running** (or any kubeconfig-reachable cluster):
   ```bash
   minikube start
   ```
   Other drivers (kind, docker-desktop) work too; only `kubectl cluster-info`
   must succeed.

2. **Dev images are loaded into minikube**:
   ```bash
   make build load
   ```
   `make build` builds `spire-steward:dev` and `spire-agent:dev`; `make load`
   runs `minikube image load` on both.

3. **`helm` and `kubectl` are on PATH** with the same kubeconfig that points
   at the running cluster.

4. **`ANTHROPIC_API_KEY` is set** if you want the cleric's decide step to
   run against the real model (the test passes the value into the helm
   install as `secrets.anthropicKey`). Without it the decide step falls
   back to recipe-replay only.

5. **`spire` CLI is on PATH** — `seedFixture` calls `spire tower create`
   to register a test tower on the host before opening the beads store.

## Running

The one-shot entrypoint:

```bash
make e2e
```

That target builds the dev images, loads them into minikube, then runs:

```bash
go test -tags=e2e -timeout 30m ./test/e2e/...
```

To run the test without re-building:

```bash
go test -tags=e2e -timeout 30m ./test/e2e/... -v
```

To run a single sub-test (useful during iteration):

```bash
go test -tags=e2e -v -run 'TestCacheRecoveryE2E/PinnedIdentityProvisioned' ./test/e2e/...
```

## Inspecting failures

When a sub-test fails, the fixture is **not** torn down until the parent
`TestCacheRecoveryE2E` returns — helm uninstall happens in `t.Cleanup`.
That gives you a live cluster to inspect while the failure is fresh:

```bash
# See what the operator is doing
kubectl logs -n spire-e2e-<rand> deploy/spire-operator -f

# See wisp beads
kubectl exec -n spire-e2e-<rand> deploy/spire-steward -- \
    bd list --rig=<tower> --status open --json | jq '.[] | select(.type=="recovery")'

# Deep-focus the wisp
spire focus <wisp-id>

# Trace the recovery
spire debug recovery trace <wisp-id>

# Check the refresh Job state
kubectl get jobs -n spire-e2e-<rand> -l spire.awell.io/refresh-job
```

The namespace name is printed early in the test output (look for
`spire-e2e-XXXX`); copy it into each command above.

If you need to keep the cluster around after the test returns, run with
`KEEP_NAMESPACE=1` — the cleanup hook honors it (future work; as of today
you can sidestep teardown by Ctrl-C'ing while the test is paused).

## Failure injection

The test currently injects failures by patching
`Spec.Cache.BranchPin` to a non-existent ref
(`refs/heads/e2e-test-does-not-exist`). The operator's refresh Job then
`git fetch`es a branch that does not exist, the Job backoff-exhausts,
and the operator files a wisp.

See `helpers/failure_injection.go` for the full rationale plus the
alternative mechanisms that were considered and rejected.

## Build tag

All files are gated by `//go:build e2e`. `go build ./...` and
`go test ./...` without the tag will not compile or run these files,
so a missing cluster cannot break the default CI job.

To lint the e2e tree: `go vet -tags=e2e ./test/e2e/...`.
