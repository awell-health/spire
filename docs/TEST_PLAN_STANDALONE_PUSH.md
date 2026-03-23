# Spire Standalone Init → Push Test Plan

> **ARCHIVED** — This test plan references `spire init` and `spire sync`, which were removed in the CLI API cleanup (2026-03-22). The equivalent workflow is now `spire tower create` + `spire repo add` + `spire push`/`spire pull`. This document is retained for historical context only.

This document describes the end-to-end integration test for the **standalone init → file → claim → push** workflow.
It is written for a human tester or an AI agent running a verification pass.

---

## Prerequisites

| Requirement | Check |
|---|---|
| `spire` binary in PATH | `spire version` prints a version string |
| `dolt` binary in PATH | `dolt version` prints a version string |
| Dolt server running | `spire status` shows `dolt server: running` |
| DoltHub credentials | `DOLT_REMOTE_USER` and `DOLT_REMOTE_PASSWORD` env vars set |
| Write access to a DoltHub org | Token must have permission to create repos |

Credentials can be exported from a local file, e.g.:
```bash
export DOLT_REMOTE_USER=jbb
export DOLT_REMOTE_PASSWORD=<your-token>
```

---

## Test Scenario: Full Standalone Lifecycle

### Step 1 — Create a fresh directory

```bash
rm -rf /tmp/push-test
mkdir /tmp/push-test
cd /tmp/push-test
```

The directory does **not** need to be a git repo.

### Step 2 — Initialize as standalone

```bash
spire init --standalone --prefix=ptest
```

**Expected output:**
- ASCII art banner
- `Initializing push-test as standalone (prefix: ptest-)...`
- `Beads initialized (prefix: ptest-)`
- `.envrc written (SPIRE_IDENTITY=ptest)`
- `Routes written`, `Config saved`

**Expected filesystem state:**
```
/tmp/push-test/
  .beads/
    metadata.json     # dolt_database, dolt_server_host, etc.
    routes.jsonl      # routing config
  .envrc              # SPIRE_IDENTITY=ptest
  SPIRE.md
```

**Verify with:**
```bash
bd status   # should show "ptest-" prefix, 0 issues
```

---

### Step 3 — Create beads

```bash
spire file "Test integration bead"
spire file "Second test bead"
```

**Expected output:** Two bead IDs, e.g. `ptest-t1q` and `ptest-uta`.

**Verify with:**
```bash
bd list --json | jq '.[].id'
```

---

### Step 4 — Claim a bead

```bash
spire claim ptest-t1q
```

**Expected output (JSON):**
```json
{"id":"ptest-t1q","status":"in_progress","title":"Test integration bead","type":"task"}
```

Since no remote is configured yet, the pull and push steps in `spire claim` will
silently skip (no-remote errors are suppressed via `isNoRemoteError()`).

---

### Step 5 — Push to DoltHub

```bash
source /path/to/your/creds.env   # sets DOLT_REMOTE_USER + DOLT_REMOTE_PASSWORD
spire push awell/test-db          # short form expanded to full DoltHub URL
```

**Expected output:**
```
  Creating remote database awell/test-db on DoltHub...
  Created awell/test-db
  Adding remote origin → https://doltremoteapi.dolthub.com/awell/test-db
  Pushing to origin...
  Push complete.
```

If the remote database already exists, the "Creating..." line is skipped and you'll see just:
```
  Remote origin: https://doltremoteapi.dolthub.com/awell/test-db
  Pushing to origin...
  Push complete.
```

**What happens under the hood:**
1. `ensureDoltHubDB` checks whether `awell/test-db` exists via `GET /api/v1alpha1/awell/test-db`.
   If not, it creates it via `POST /api/v1alpha1/database` (requires `DOLT_REMOTE_PASSWORD`).
2. The SQL-level remote is set via `bd dolt remote add origin <url>`.
3. The CLI-level remote is set via `dolt remote add origin <url>` directly in the dolt data dir
   (`~/.local/share/spire/<dbname>/`). This is separate from the SQL-level remote and necessary
   because `dolt push` (CLI) reads from `.dolt/config.json`, not from the SQL tables.
4. Any uncommitted working-set changes are committed before the push.
5. `dolt push origin main` runs directly from the data dir, **inheriting the caller's environment**
   so `DOLT_REMOTE_USER` / `DOLT_REMOTE_PASSWORD` are available. This bypasses the dolt server
   process (which doesn't inherit client env vars).
6. If the push fails with a non-fast-forward or no-common-ancestor error (e.g., fresh local vs
   existing remote), it retries with `--force`.

---

### Step 6 — Verify on DoltHub

```bash
curl -s -H "Authorization: token $DOLT_REMOTE_PASSWORD" \
  "https://www.dolthub.com/api/v1alpha1/awell/test-db" | python3 -m json.tool
```

Check that `repository_name` is `test-db`. To verify data, use the DoltHub web UI at
`https://www.dolthub.com/repositories/awell/test-db` and browse the `issues` table.

---

### Step 7 — Teardown

Remove from spire config:
```bash
spire repo remove ptest
```

Remove the local directory:
```bash
rm -rf /tmp/push-test
```

Remove the DoltHub database via the web UI:
- Navigate to `https://www.dolthub.com/repositories/awell/test-db`
- Settings → Danger Zone → Delete repository

> **Note:** DoltHub does not expose a DELETE endpoint in its REST API, so deletion must be done via the web UI.

---

## Additional Scenarios

### Re-pushing after local changes

```bash
cd /tmp/push-test
spire file "Another bead"
spire push                  # no URL needed — remote already configured
```

### Pushing from a satellite worktree

If you have multiple git worktrees registered to the same prefix:
```bash
cd /path/to/worktree
spire push                  # same remote, same database
```

### Divergent history recovery

If local and remote histories have diverged (e.g., remote was reset and re-initialized),
`spire push` automatically retries with `--force`:
```
  Divergent history — retrying with --force...
  Push complete.
```

### Syncing back after a push

```bash
spire sync                  # pulls latest from DoltHub, merges with local
spire sync --hard           # force reset to remote (discards local)
```

---

## Known Limitations

- **DoltHub deletion requires web UI** — no REST API for DELETE.
- **`DOLT_REMOTE_USER` / `DOLT_REMOTE_PASSWORD` must be in the calling process environment** —
  they are not read from `~/.dolt/config` or the dolt server config.
- **Push always targets `main` branch** — multi-branch workflows are not yet supported.

---

## Test Results (2026-03-18)

| Step | Result |
|---|---|
| `spire init --standalone --prefix=ptest` | ✅ pass |
| `spire file "Test integration bead"` | ✅ pass — `ptest-t1q` |
| `spire file "Second test bead"` | ✅ pass — `ptest-uta` |
| `spire claim ptest-t1q` | ✅ pass — JSON output, no noisy warnings |
| `spire push awell/test-db` | ✅ pass — created DB, set remote, pushed |
| DoltHub shows `awell/test-db` | ✅ verified via REST API |
