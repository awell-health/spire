# Attached Deployment Vision

> Local control plane, remote cluster execution.

Attached mode is the third deployment topology. It is **reserved** today — not implemented — but the seams it will compose already exist and are held to strict invariants. This document describes what attached mode is for, what it will preserve, and why Spire keeps room for it.

## Shape

In attached mode:

- The **control plane** — tower config, repo registration, bead mutation, design and plan authorship, and every CLI the archmage types — runs on a laptop.
- The **execution surface** — apprentice pods, workspace materialization, workers — runs on a remote cluster.
- The two halves connect through the canonical `WorkloadIntent` / `IntentPublisher` / `IntentConsumer` seam. The local side publishes intent; the remote side reconciles it into pods.

Attached mode is emphatically not a new runtime, a new scheduler, or a new ownership model. It is a deployment-time rewiring of *where* the existing scheduler publishes intent and *where* the existing reconciler reads it.

## Who it's for

Attached mode is designed for developers who want:

- Their laptop as the authoritative control plane — fast local iteration, offline design work, direct tower edits
- A remote cluster as the execution surface — unlimited capacity, always-on workers, shared observability
- No cluster-side control-plane deployment — the cluster runs only the operator and the pods it reconciles, not a steward

It is the mode that lets a developer drive a cluster without the cluster having to own their tower. It is what lets a small team share cluster compute without centralizing their control planes.

Attached mode also makes it the natural home for observability. Because the control plane is on the laptop, `spire logs`, `spire trace`, and `spire metrics` resolve from the laptop's CLI without `kubectl` access or a separate dashboard. Telemetry flows back from the cluster through the same seam that carries intent outbound — traces, logs, and metrics land in the tower's OLAP store, which the laptop is already attached to via remotesapi. For many teams this is the single biggest ergonomic win over cluster-native: you can watch and debug your agents with the same CLI you used to file the work.

## What it preserves

Attached mode composes existing contracts. It must not perturb any of them:

- **Runtime contract** — `RepoIdentity`, `WorkspaceHandle`, `HandoffMode`, and `RunContext` hold identically across local process, cluster-native, and attached deployments.
- **Repo-identity ownership** — cluster-side identity resolution stays in the cluster's own resolver. Attached mode ships minimal identity (URL, base branch, prefix) in the intent payload.
- **Attempt-bead ownership** — the attempt bead remains the canonical ownership seam. Attached mode does not introduce parallel ownership through pod names, CR UIDs, or remote-side identifiers.
- **Orthogonality** — attached mode is orthogonal to backend (process/docker/k8s) and transport (syncer/remotesapi/DoltHub). A future implementation must not conflate "my deployment mode is attached" with "my backend is k8s" or "my transport is remotesapi."

These invariants are enforced today by reflection-based tests that guard the intent shape against smuggled machine-local state.

## Why keep it reserved

Attached mode is a higher-leverage deployment topology than either local-native or cluster-native for developers who already have cluster capacity but want laptop-speed control. Building it before the local and cluster foundations are rock-solid would invite the opposite of the orthogonality principle: every future change would have to be validated against three modes instead of two, and the weakest of those three would still be in flux.

Keeping it reserved means:

- The seams it will need are named and enforced
- The invariants it must preserve are documented
- A future track implementing attached mode has an unambiguous starting line
- Consumers that observe `deployment_mode = attached-reserved` return a typed "not implemented" error instead of falling back silently

## Status today

- `pkg/config` declares `DeploymentModeAttachedReserved` as an explicit enum value
- `pkg/steward/attached.AttachedDispatch` returns a typed `ErrAttachedNotImplemented` for any input
- `WorkloadIntent`, `IntentPublisher`, and `IntentConsumer` exist and are exercised by cluster-native today
- The intent payload is guarded against machine-local state by package-level tests

Selecting `deployment_mode = "attached-reserved"` is a declaration of intent, not a runnable configuration. When attached mode graduates from reserved to implemented, this document and the stub will be replaced by the real design.

See [docs/attached-mode.md](attached-mode.md) for the current contract spec.
