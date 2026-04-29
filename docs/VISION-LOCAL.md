# Local-Native Deployment Vision

> Everything runs on your laptop.

Local-native is the zero-infrastructure entry point to Spire. The control plane, the dolt database, and every agent process live on a single machine. It is the mode Spire optimizes for first, the mode that proves every coordination pattern works before the pattern ever touches a pod, and the mode where a developer can start filing work in under five minutes.

## What runs

On a laptop in local-native mode:

- **Dolt server** — a long-lived process on localhost, holding the tower's database
- **Daemon** — `spire up` starts the sync daemon (Linear sync, webhook processing)
- **Steward** — `spire up` also starts the local steward by default; it owns work assignment, hooked-step resume, and lifecycle maintenance. Pass `--no-steward` to skip it for sync-only/debug runs.
- **Agents** — wizards, apprentices, sages, arbiters, and clerics run as child processes of the daemon by default, or as Docker containers if configured

There is no Kubernetes, no remote control plane, no intent queue on the wire. The steward directly dispatches a wizard process; the wizard directly spawns apprentice and sage processes. Claim, dispatch, and handoff all happen through in-process calls plus dolt writes.

## Who it's for

- A solo developer building a backlog and letting agents work through it overnight
- A small team sharing a tower via DoltHub, with each developer running agents on their own laptop
- Anyone evaluating Spire before committing to cluster infrastructure
- The first audience for every new feature — if it does not work in local-native, it does not ship

## What it optimizes for

- **Speed to first bead closed** — `brew install` → tower created → bead filed → agent executing in one sitting
- **Single-process debuggability** — no pods, no CRDs, no operator controllers standing between you and a crash log
- **Offline capability** — with the local filesystem transport, the whole system works with no network except for Anthropic and GitHub API calls
- **Cost predictability** — one laptop, one steward, concurrency cap equals known spend

## How it connects to the other modes

Local-native is the reference implementation. Every seam that cluster-native and attached use — the intent publisher/consumer, the canonical runtime contract, the workspace ownership model — is exercised in local-native first, then composed differently in the other modes. If a cluster pod behaves differently from a local process, the cluster pod is wrong.

A local-native tower can sync to a DoltHub remote for team coordination, attach as a `server-remote` direct-Dolt client to a cluster-hosted Dolt via remotesapi, or stay fully local with filesystem transport only. All three are supported and can be changed without touching the work graph. Cluster-as-truth gateway-attach is a separate topology and is described in [VISION-CLUSTER.md](VISION-CLUSTER.md) and [deployment-modes.md](deployment-modes.md).

## What it does not do

- **No autoscaling** — concurrency is capped by your machine and a `max_concurrent` tower setting. As of v0.48, `spire up` starts the local steward by default, which enforces the per-tower cap; local-native is not a fully hands-off process model.
- **No multi-tenant isolation** — one tower, one set of credentials, one filesystem
- **No persistent background execution** — when you close the laptop, the daemon stops; work resumes when you start it again
- **No managed ops** — you are the SRE

For any of those, see [VISION-CLUSTER.md](VISION-CLUSTER.md).

## Migrating local `spire logs` workflows to cluster mode

Local-native log behaviour is unchanged. `spire logs <wizard>`,
`spire logs pretty <bead>`, and the board's Logs tab continue to read
files under `~/.local/share/spire/wizards/<wizard-name>/...` exactly
as before. If your tower is local-native, nothing about this section
applies — keep using the commands you already use.

What changes when you attach a laptop to a **cluster-as-truth** tower
(`spire tower attach-cluster --tower <name> --url https://<gateway> --token <bearer>`):

- `spire logs pretty <bead-id>` resolves through the gateway against
  the tower manifest, not the local filesystem. The CLI sets
  `X-Spire-Scope: engineer` automatically so engineer-only artifacts
  render raw (mirroring local-native behaviour).
- `spire logs <wizard-name>` (the legacy file-tail) is local-only and
  will report no logs in cluster-attach mode — the cluster wizard's
  files live under an ephemeral pod filesystem that no longer exists.
  Use `spire logs pretty <bead-id>` to read provider transcripts and
  the board's Logs tab to browse the full artifact list.
- `~/.local/share/spire/wizards/...` is empty in cluster-attach mode.
  Old transcripts captured during prior local-native runs stay where
  they were — read them with `spire logs pretty` against a
  local-native tower (`spire tower use <local-tower-name>`) or with
  `tail -f` on the file directly.
- The board's empty-state on a fresh cluster bead is explicit ("no
  logs yet") and the same gateway path populates as the bead runs —
  it is not a regression.

The architectural decision behind this split lives in design bead
spi-7wzwk2. The operator-facing setup, retention model, and failure
modes are documented in
[cluster-logs-runbook.md](cluster-logs-runbook.md) and
[cluster-logs-smoke-test.md](cluster-logs-smoke-test.md).
