# Architecture Drift Assessment

Date: 2026-04-21

## Scope

This review focuses on three runtime seams that are currently easy to reason
about incorrectly:

1. apprentice delivery parity across implement and review flows
2. recovery / cleric boundaries and how much of recovery is truly agentic
3. workspace ownership and identity, especially under bundle transport and the
   k8s backend

The goal is not to restate the full architecture. The goal is to identify where
the current contracts are clean, where they drift, and which package-local
README files should carry the real ownership rules.

## Executive Summary

- The formula layer is now cleaner than the runtime boundary docs in several
  places. `task-default`, `subgraph-review`, `subgraph-implement`, and
  `cleric-default` describe a clearer execution shape than older current-state
  docs.
- The most serious live drift is **backend-dependent workspace semantics**.
  `pkg/executor` and `pkg/wizard` now rely on shared `--worktree-dir` flows for
  implement, sage-review, review-fix, and build-fix, but `pkg/agent` only gives
  the special workspace-bearing pod contract to `RoleWizard`. Non-wizard k8s
  pods are still flat pods with no shared workspace materialization.
- A second workspace risk is **ambient identity fallback**. Some runtime paths
  still infer tower/database identity from CWD or loose prefix conventions
  rather than from the already-established tower/prefix binding contract.
- The recovery stack is still split across `pkg/recovery` and `pkg/executor` in
  a way that makes the surface look cleaner than it is. `pkg/recovery` is
  mostly the data model and ranking layer; the real cleric runtime policy lives
  in `pkg/executor`.
- The tactical `spi-fopwn` gap has moved since the earlier review: current
  `main` now bootstraps wizard k8s pods with a `repo-bootstrap` init container
  that clones and `bind-local`s the repo into `/workspace/<prefix>`. That is a
  tactical fix, not the final guild-cache design.

## 1. Apprentice Delivery Parity

### Current Contract

- `task-default` declares a run-scoped `feature` workspace and routes
  `implement` through `wizard.run`, then `review` through a nested
  `subgraph-review`. The parent workspace is propagated into the sub-graph.
- Inside `subgraph-review`, `sage-review` and `review-fix` both run through
  `wizard.run`, which means the executor chooses the role and workspace while
  `pkg/wizard` performs the subprocess.
- Epic child execution uses a different handoff: apprentices deliver work back
  to the parent executor through either bundle transport or push transport.
  `pkg/apprentice.Submit` writes the bundle signal; `pkg/executor` consumes the
  signal and merges into staging.

### Where The Architecture Diverges

- There is **not one universal apprentice-delivery contract today**. Two
  distinct contracts exist:
  - same-workspace handoff for top-level implement/review flows
  - transport-based handoff for child-to-staging integration
- Those two models are both valid, but they are not interchangeable. The code
  currently treats the shared-workspace path as process-local truth and the
  transport path as integration truth.
- Review verdict storage is in better shape than delivery semantics. Sage closes
  the review-round bead through `CloseReviewBead`, and parent routing reads the
  nested graph outcome plus labels like `review-approved`. That contract is more
  coherent than the delivery contract.

### Package Boundary Problems

- `pkg/executor` correctly owns routing and workspace propagation.
- `pkg/wizard` correctly owns how an apprentice or sage subprocess runs.
- `pkg/apprentice` correctly owns bundle signal-write semantics.
- The bleed is that delivery semantics are spread across all three packages:
  executor decides whether integration is local or transport-based, wizard owns
  post-build apprentice delivery, and apprentice owns the bundle metadata shape.
  That makes it easy to accidentally add a third delivery model.

### Testing Gaps

- Good coverage exists for wizard apprentice exit behavior and bundle apply.
- Missing coverage exists for **cross-backend parity**:
  - implement -> sage-review using `--worktree-dir` on k8s
  - review-fix using borrowed workspaces on k8s
  - an end-to-end proof that transport selection is the same across top-level
    and child flows where it is supposed to be

### Key Files

- `pkg/formula/embedded/formulas/task-default.formula.toml`
- `pkg/formula/embedded/formulas/subgraph-review.formula.toml`
- `pkg/executor/graph_actions.go`
- `pkg/executor/action_dispatch.go`
- `pkg/executor/apprentice_bundle.go`
- `pkg/wizard/wizard.go`
- `pkg/wizard/wizard_review.go`
- `pkg/apprentice/submit.go`
- `pkg/bundlestore/README.md`

## 2. Recovery / Cleric Boundaries

### Current Contract

- `pkg/recovery` presents itself as the diagnosis, failure classification,
  action proposal, learning, and metadata package.
- `cleric-default` is the runtime lifecycle: collect context, decide, execute,
  verify, learn, finish.
- `pkg/executor` owns the cleric action handlers, recovery action registry,
  retry protocol, and worktree provisioning for recovery actions.

### Where The Architecture Diverges

- The public boundary is cleaner than the implementation boundary. In practice,
  most recovery runtime behavior lives in `pkg/executor`, not `pkg/recovery`.
- `targeted-fix` is still a placeholder. It records intent and logs, but it does
  not actually dispatch a fix agent.
- The genuinely agentic recovery path today is narrow. `decide` and `learn` are
  Claude-driven, and `resolve-conflicts` can dispatch an apprentice with a
  conflict bundle, but much of the rest of the action surface is mechanical.
- There is also a completion-contract mismatch risk: steward resumes hooked work
  based on recovery-bead outcome metadata, while the learn path is centered on
  verification status plus SQL persistence. That edge is under-documented.
- The future recipe/programmatic-fix direction is visible in
  `pkg/recovery/recipe.go`, but the runtime still mixes recipe-worthy actions,
  placeholder agentic actions, and hardcoded executor loops.

### Package Boundary Problems

- `pkg/recovery` is currently closer to a domain model plus ranking library than
  a runtime package.
- `pkg/executor` owns too much recovery-specific orchestration: action dispatch,
  retry protocol, worktree fallback logic, triage worktree recovery, and the
  agentic conflict resolver.
- That split is not necessarily wrong, but it should be documented honestly.
  Right now the README boundary can make readers think the cleric runtime is
  more package-local to `pkg/recovery` than it is.

### Testing Gaps

- Recovery has substantial unit coverage.
- The main missing tests are behavioral rather than pure logic:
  - no end-to-end proof that `targeted-fix` becomes real work
  - no proof that recipe promotion/demotion actually replaces agentic recovery
  - no proof that cleric-written metadata is exactly what steward later uses to
    resume or leave hooked work
  - no backend-aware tests for recovery worktree assumptions under k8s

### Key Files

- `pkg/recovery/README.md`
- `pkg/recovery/diagnose.go`
- `pkg/recovery/recipe.go`
- `pkg/executor/recovery_phase.go`
- `pkg/executor/recovery_actions.go`
- `pkg/executor/recovery_actions_agentic.go`
- `pkg/executor/recovery_protocol.go`
- `pkg/wizard/wizard_retry.go`
- `pkg/formula/embedded/formulas/cleric-default.formula.toml`

## 3. Workspace Ownership And Identity

### Current Contract

- The formula declares workspace kind, scope, cleanup, and branch/base
  interpolation.
- `pkg/executor` resolves that declaration once and persists runtime workspace
  state.
- `pkg/wizard` either creates its own worktree or resumes a provided
  `--worktree-dir` without taking ownership of cleanup.
- `pkg/git` owns the worktree mechanics and session baselines.
- On current `main`, the k8s wizard pod contract now includes `tower-attach`
  plus a `repo-bootstrap` init container that clones and `bind-local`s the repo
  into `/workspace/<prefix>`.
- The intended source of truth is still the shared tower repo registration:
  prefix -> repo URL / branch in shared state, with local bindings acting as the
  discoverability layer for a host or pod.

### Where The Architecture Diverges

- The workspace contract is **not backend-neutral today**. `pkg/executor` and
  `pkg/wizard` assume that passing `--worktree-dir` to a spawned apprentice or
  sage is enough. That is true for local process execution. It is not true for
  a fresh k8s pod unless the backend also mounts or materializes the same
  workspace.
- `pkg/agent/backend_k8s.go` only gives the special workspace-bearing contract
  to `RoleWizard`. Non-wizard roles still get a flat pod with no shared
  workspace, no `/data`, and no repo bootstrap.
- Recovery paths depend on the same assumption. Triage and agentic conflict
  resolution both work from real filesystem worktree paths.
- There is still operator/backend drift as well: the operator controller path
  remains separate from the canonical `pkg/agent` wizard-pod contract and still
  uses its own prefix and pod-shape assumptions.
- Some cluster-runtime helpers still fall back to ambient state. For example,
  graph-state store selection still derives the Dolt database from CWD and
  defaults to `spire`, which is weaker than using explicit tower binding.

### Package Boundary Problems

- `pkg/formula` and `pkg/executor` correctly own the declaration and selection
  of workspaces.
- `pkg/wizard` correctly owns subprocess use of a workspace.
- `pkg/agent` needs clearer documentation that a backend must either satisfy the
  requested workspace surface or fail fast. Silent local-path assumptions are a
  backend bug, not a wizard bug.
- The tactical repo-bootstrap fix belongs to the backend. A future guild-level
  repo cache belongs at the cluster contract / operator layer, not in
  `pkg/wizard`.

### Testing Gaps

- Good coverage exists for workspace resolution and wizard-pod spec generation.
- Missing coverage exists for:
  - apprentice/sage k8s pod specs when `--worktree-dir` is used
  - backend parity between process and k8s for borrowed-workspace flows
  - operator/current-state parity with the canonical wizard pod contract

### Key Files

- `pkg/executor/workspace.go`
- `pkg/executor/graph_actions.go`
- `pkg/wizard/README.md`
- `pkg/wizard/wizard.go`
- `pkg/wizard/wizard_review.go`
- `pkg/git/README.md`
- `pkg/agent/agent.go`
- `pkg/agent/backend_k8s.go`
- `pkg/agent/backend_k8s_test.go`
- `pkg/steward/steward.go`
- `operator/controllers/agent_monitor.go`
- `operator/controllers/bead_watcher.go`
- `pkg/executor/graph_store.go`
- `cmd/spire/repo_bind_local.go`

## Recommended README First Wave

The first README sweep should focus on the packages where the contract is
currently needed most by implementation agents:

- `pkg/agent`
- `pkg/apprentice`
- `pkg/executor`
- `pkg/wizard`
- `pkg/recovery`

If we do only one thing next, it should be this:

- make `pkg/agent` explicitly document backend obligations for shared workspaces
- make `pkg/apprentice` explicitly document that it owns bundle/no-op submit
  semantics but not transport choice
- then tighten `pkg/executor`, `pkg/wizard`, and `pkg/recovery` so the current
  ownership split is stated plainly instead of optimistically
