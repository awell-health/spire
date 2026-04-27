# Deployment Modes

This document separates two choices that were getting mixed together:

1. **Server deployment** — where the authoritative tower lives, and where
   execution happens.
2. **Client deployment** — how a local CLI/Desktop instance connects to
   that tower.

Use this as the operator-facing source of truth when you need to answer
"what is running where?" or "should this client sync, or just act as a
frontend?"

## The two decisions

### 1. Server deployment

```mermaid
flowchart LR
    choose[Choose a server deployment]
    choose --> local[Local-native<br/>local tower is authoritative]
    choose --> cluster[Cluster-native<br/>cluster tower is authoritative]
```

- **Local-native** means the local machine owns the tower, the steward,
  and the workers.
- **Cluster-native** means the cluster owns the tower, the control
  plane, and the worker execution surface.

### 2. Client deployment

```mermaid
flowchart LR
    choose[Choose a client deployment]
    choose --> direct[Local + server-remote<br/>local mirror + sync]
    choose --> gateway[Local + cluster-attach<br/>frontend only]
```

- **Local + server-remote** keeps a local Dolt mirror and uses
  `push` / `pull` / `sync` against a remote Dolt server, typically over
  `remotesapi`.
- **Local + cluster-attach** does not keep a writable local mirror for
  the tower. The client talks to the gateway over HTTP and acts as a
  frontend only.

## Canonical topologies

### A. Local-native

Everything lives on one machine. Sync is optional because the local
tower is authoritative.

```mermaid
flowchart LR
    subgraph laptop["Laptop"]
        cli[CLI / Desktop]
        dolt[Local Dolt + .beads]
        steward[Local steward]
        agents[Local wizards]
    end

    cli <--> dolt
    steward <--> dolt
    agents <--> dolt
    dolt -. optional push/pull/sync .-> remote[(DoltHub or remote Dolt server)]
```

Rules:

- Local machine is the source of truth.
- Local Dolt is required.
- `push` / `pull` / `sync` are allowed.

### B. Local + server-remote

This is the direct-Dolt client shape. The laptop keeps a local mirror
and syncs it with a remote server.

```mermaid
flowchart LR
    subgraph client["Local client"]
        cli[CLI / Desktop]
        dolt[Local Dolt mirror]
    end

    remote[(Remote Dolt server<br/>remotesapi)]

    cli <--> dolt
    dolt <--> |push / pull / sync| remote
```

Rules:

- Local client keeps a local mirror.
- Client-side sync is part of the model.
- A local Dolt server may still be running on the laptop.
- The remote server may be used mainly for storage, auth, and shared
  state.

### C. Cluster-native + cluster-attach

This is the cluster-as-truth path. The cluster owns the tower and the
local machine is only a frontend.

```mermaid
flowchart LR
    subgraph client["Local client"]
        cli[CLI / Desktop]
    end

    gateway[Gateway HTTP API]

    subgraph cluster["Cluster-native server deployment"]
        dolt[Cluster Dolt]
        control[Steward + operator]
        workers[Wizards / clerics]
    end

    cli <--> |HTTPS| gateway
    gateway <--> dolt
    control <--> dolt
    workers <--> dolt
```

Rules:

- Cluster is the source of truth.
- No local writable tower mirror is required.
- No client `push` / `pull` / `sync`.
- No local Dolt server is required just to use the tower.

## The important distinction

`server-remote` and `cluster-attach` are **not** the same thing.

- If a client needs `push` / `pull` / `sync`, it is in a
  **server-remote** topology.
- If a client is attaching to a **cluster-as-truth** tower, it
  **must** use **cluster-attach / gateway mode**. Server-remote attach
  to a cluster-as-truth tower is unsupported — it would mint a second
  writer and violate the single-writer invariant the cluster is
  enforcing.

### Supported / unsupported matrix

| Server deployment | Client topology | Status |
|-------------------|-----------------|--------|
| Local-native | (no client — local-only) | Supported |
| Local-native | Local + server-remote (peer / DoltHub mirror) | Supported |
| Cluster-native (direct-Dolt) | Local + server-remote (remotesapi to cluster Dolt) | Supported |
| **Cluster-native (cluster-as-truth)** | **Local + cluster-attach (gateway)** | **Supported — required for this server topology** |
| Cluster-native (cluster-as-truth) | Local + server-remote (remotesapi to cluster Dolt) | **Unsupported.** Mints a second writer; the cluster's single-writer invariant rejects it. |
| Cluster-native (any) | Attached-reserved | Reserved — not implemented (see [attached-mode.md](attached-mode.md)) |

If the cluster operator has chosen cluster-as-truth, every client
attaches via the gateway. If the operator has chosen a direct-Dolt
cluster topology, every client attaches via remotesapi as
`server-remote`. Operators do not mix the two against the same tower.

## Summary table

| Topology | Authoritative tower | Local Dolt mirror | Client sync | Local Dolt server needed |
|----------|---------------------|-------------------|-------------|--------------------------|
| Local-native | Laptop | Yes | Optional | Usually yes |
| Local + server-remote | Remote Dolt server | Yes | Yes | Often yes |
| Cluster-native + cluster-attach | Cluster | No | No | No |

## Current code terms

The user-facing language above maps to the current codebase like this:

- `DeploymentModeLocalNative` — local-native server deployment
- `DeploymentModeClusterNative` — cluster-native server deployment
- `TowerModeGateway` — cluster-attach client mode
- `RemoteKindRemotesAPI` — direct remote Dolt transport used by
  server-remote topologies
