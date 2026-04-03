# Troubleshooting and FAQ

Common problems, fixes, and answers to frequent questions.

---

## Setup issues

### `spire doctor` fails with "dolt not found"

Spire manages its own dolt binary. Run:

```bash
spire doctor --fix
```

This downloads the correct dolt version to `~/.local/share/spire/bin/dolt`. You do not need to install dolt yourself.

If `--fix` fails, check your internet connection and try manually:

```bash
spire version    # shows expected dolt version
```

### `spire tower create` fails with "DoltHub auth error"

Check your DoltHub credentials:

```bash
spire config get dolthub-user
spire config get dolthub-password --unmask
```

The password must be a DoltHub access token, not your account password. Get one at dolthub.com → Account → Credentials.

Test DoltHub access manually:

```bash
DOLT_REMOTE_USER=myuser DOLT_REMOTE_PASSWORD=mytoken \
  dolt login --auth-endpoint https://doltremoteapi.dolthub.com
```

### `spire repo add` fails with "prefix already in use"

Another repo is already registered with that prefix. List existing repos:

```bash
spire repo list
```

Choose a different prefix:

```bash
spire repo add --prefix myrepo2
```

### After `spire up`, commands show "connection refused"

The dolt server didn't start. Check:

```bash
spire status          # shows dolt PID and reachability
spire logs --dolt     # dolt server logs
```

Common causes:
- Port 3307 is in use by another process: `lsof -i :3307`
- Previous dolt didn't clean up: `spire shutdown && spire up`
- Wrong dolt data directory permissions: `spire doctor --fix`

---

## Wizard issues

### Wizard is stuck and not making progress

Check the wizard logs:

```bash
spire roster          # shows elapsed time
spire logs wizard-1   # tail the log
```

If the wizard is past the `stale` threshold (default: 10m), the steward marks it stale in `spire roster`. If it's past `timeout` (default: 15m), the steward kills it.

To dismiss a stuck wizard manually:

```bash
spire dismiss --all
```

### Wizard failed with "bead already claimed"

Another wizard (or a previous session) owns the bead. Check the owner:

```bash
bd show spi-abc --json | jq '.owner'
```

If the previous wizard is gone, release the claim:

```bash
bd update spi-abc --status open
bd update spi-abc --owner ""
```

Then re-summon:

```bash
spire summon 1 --targets spi-abc
```

### Wizard completed but the change did not land

Check the wizard's output:

```bash
spire logs wizard-1
cat ~/.local/share/spire/wizards/wizard-1.log | grep -i "merge\|push\|branch\|github"
```

Common causes:
- GitHub token lacks `repo` scope: regenerate at GitHub → Settings → Developer settings
- Push to the base branch was rejected (branch protection, auth, or diverged history)
- Build or test verification failed during the merge phase
- `branch.base` in `spire.yaml` points at the wrong landing branch

### Wizard keeps failing on tests

The wizard runs `runtime.test` from `spire.yaml` before pushing. If tests consistently fail:

1. Check if tests were passing before the wizard's changes: `git stash && go test ./...`
2. Check the test command in `spire.yaml` is correct
3. If tests require an external service (database, etc.), they may not work in the wizard's environment

Temporarily skip tests during development:

```yaml
runtime:
  test: ""   # skip tests
```

Or limit to unit tests only:

```yaml
runtime:
  test: go test ./internal/...   # skip integration tests
```

### Sage keeps requesting changes

The sage (review agent) is not approving the implementation. Check what it's requesting:

```bash
bd comments spi-abc --json | jq '.[-3:]'  # last 3 comments
```

The sage's comments explain what it wants changed. If the sage is wrong or too strict, you can override:

1. Add a bead comment clarifying the intent or expected behavior
2. Lower `max_rounds` in the formula to trigger arbiter escalation faster
3. If you want to take over manually, land the fix yourself and then close or reopen the bead

### "no .beads/ directory found"

The command can't locate the beads database. Fix:

```bash
# Check if BEADS_DIR is set
echo $BEADS_DIR

# Check what tower you're in
spire tower list

# Verify the repo is registered
spire repo list

# Re-register if needed
cd /path/to/my-repo
spire repo add
```

---

## Sync issues

### Beads filed by one developer don't appear for another

Both developers need to sync with DoltHub:

```bash
# Developer A: push new beads
spire push

# Developer B: pull to see them
spire pull
```

If `spire up` is running, the daemon auto-syncs on the configured interval (default: 2 minutes). Check when the last sync happened:

```bash
spire status    # shows last sync time
spire logs --daemon | grep sync
```

### `spire pull` fails with "diverged history"

Two machines filed beads without syncing first. Fix:

```bash
spire sync --merge
```

This runs a three-way merge. Spire's intended conflict model is field-level ownership, but the automated post-merge fixups are still incomplete. After a merge, inspect the resulting bead state before continuing.

### DoltHub push returns 403

Your DoltHub token may have expired or lack write access to the remote:

```bash
spire config set dolthub-password <new-token>
spire push
```

### Daemon is running but beads aren't syncing

Check daemon logs:

```bash
spire logs --daemon
```

If sync is failing with authentication errors, re-set credentials:

```bash
spire config set dolthub-user myuser
spire config set dolthub-password mytoken
spire down && spire up
```

---

## Board and UI issues

### Board shows "no beads"

Check filters: the board may be scoped or filtered. Try:

```bash
spire board                   # all beads
spire board --json | jq '.'   # machine-readable, no filters
bd list --json | head -5       # raw bead list
```

If the database is genuinely empty:

```bash
spire status               # verify dolt is running
bd list --json | wc -l     # count beads
```

### Board columns don't match expected state

The board primarily reads active workflow-step beads and only falls back to `phase:X` labels:

```bash
bd label list spi-abc     # check phase labels
```

To inspect a bead or retry a failed step:

```bash
spire trace spi-abc
spire reset --to <step> spi-abc
```

### `spire watch` exits immediately

`spire watch` needs the dolt server to be running:

```bash
spire status      # check if dolt is up
spire up          # start if needed
spire watch       # try again
```

---

## FAQ

### How many wizards can I run at once?

As many as your machine can handle. Each wizard runs as a local process in its own git worktree, consuming ~1 claude session.

In k8s, each wizard runs in its own pod. Set `spec.maxConcurrent` on the SpireAgent CRD to limit concurrent work per agent.

### Can I run Spire without a DoltHub account?

Yes, for local-only use. Omit `--dolthub` when creating your tower, and don't configure DoltHub credentials. You won't be able to sync with other developers or use the cluster deployment, but the local workflow works fine.

```bash
spire tower create --name local-tower    # no --dolthub flag
```

### Can multiple developers share one tower?

Yes. One developer creates the tower with `spire tower create`. Others join with `spire tower attach <dolthub-url>`.

Only one machine should run the steward at a time. The steward assigns work — two stewards would race. Run infrastructure only:

```bash
spire up           # dolt + daemon only (no steward)
spire summon N     # manual capacity management
```

One designated machine runs:

```bash
spire up --steward    # dolt + daemon + steward
```

### Can I use Spire with a monorepo?

Yes. Register the monorepo once, and all beads from it share one prefix. Use the `--parent` flag and `bd dep` to structure work within the monorepo.

If you have multiple logical components in a monorepo that should be tracked separately, you can register the monorepo multiple times with different prefixes and `spire.yaml` files:

```bash
# Register with workspace-aware build commands
spire repo add --prefix=fe   # frontend workspace
# spire.yaml: runtime.test: pnpm --filter=frontend test

spire repo add --prefix=be   # backend workspace
# spire.yaml: runtime.test: pnpm --filter=backend test
```

### Where are logs stored?

| Log | Location |
|-----|---------|
| Wizard logs | `~/.local/share/spire/wizards/<name>.log` |
| Daemon log | `~/.local/share/spire/daemon.log` |
| Daemon error log | `~/.local/share/spire/daemon.error.log` |
| Dolt server log | `~/.local/share/spire/dolt.log` |
| Dolt error log | `~/.local/share/spire/dolt.error.log` |

### How do I stop a wizard mid-run?

```bash
spire dismiss 1        # dismiss least-busy wizard
spire dismiss --all    # dismiss all wizards
```

Wizards receive SIGINT and finish their current step before exiting.

### My bead is in an inconsistent state — how do I reset it?

```bash
# View the current state
bd show spi-abc --json

# Reset status
bd update spi-abc --status open

# Remove all phase labels
bd label list spi-abc | grep phase | while read label; do
    bd label remove spi-abc "$label"
done

# Remove ownership
bd update spi-abc --owner ""
```

### Can agents communicate with each other?

Yes, via structured messages:

```bash
# Agent A sends to Agent B
spire send agent-b "Please check the auth module" --ref spi-abc

# Agent B checks inbox
spire collect agent-b

# Agent B replies
spire send agent-a "Checked, looks good" --ref spi-abc --thread spi-msg-xyz
```

Messages are stored in the bead graph and routed by labels. The familiar (sidecar) in k8s delivers inbox files to agent containers.

### How do I connect Spire to Linear?

```bash
spire connect linear
```

This starts an OAuth2 flow. When connected:
- Beads of type `epic` are automatically synced to Linear as issues
- The Linear issue includes the bead ID and links back to the tower
- Status changes in Linear are NOT synced back to beads (Linear is for PM tracking; beads are the source of truth for structure)

### What happens if a wizard lands a bad change?

If the change already landed on the base branch, revert or follow up in git first. Then reopen or re-file the work in Spire:

```bash
# Reopen the bead
bd update spi-abc --status open
bd update spi-abc --owner ""

# Optionally add context about what went wrong
bd comments add spi-abc "Previous attempt failed because X. New approach: Y"

# Re-summon
spire summon 1 --targets spi-abc
```

The new wizard will see the comment and adjust its approach.

---

## Getting help

```bash
spire help              # top-level command reference
spire doctor            # diagnose setup issues
```

File bugs and feature requests at the [Spire GitHub repo](https://github.com/awell-health/spire/issues).
