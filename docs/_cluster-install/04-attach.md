## 8. Attach the CLI

The cluster is now serving the gateway at `https://spire.example.com`.
Point your local `spire` CLI at it so every bead/message op tunnels through
the gateway instead of going to a local Dolt server.

### Get the bearer token

The token lives in the `spire-gateway-auth` Secret that the chart created
when you set `gateway.apiToken` (you should have copied it during §7 —
this is the same value):

```bash
TOWER_TOKEN=$(kubectl -n spire get secret spire-gateway-auth \
  -o jsonpath='{.data.SPIRE_API_TOKEN}' | base64 -d)
```

> **Design note on `helm get notes`.** The design called for the token to
> be surfaced via `helm get notes spire -n spire`. The current
> `helm/spire` chart's `NOTES.txt` does not print the gateway token (it
> covers dolt/steward/operator readiness instead), so the kubectl path
> above is the canonical retrieval. Treat `helm get notes spire -n spire`
> as informational only.

### Attach

```bash
spire tower attach-cluster \
  --url=https://spire.example.com \
  --tower=spire-tower \
  --token=$TOWER_TOKEN
```

> **Design note on flags.** The design called this
> `spire tower attach-cluster --url=... --token=...`. The implementation
> additionally requires `--tower=<name>` so the CLI can verify the
> remote tower's identity (`GET /api/v1/tower`) matches the name you
> expect — preventing a typo'd URL from silently attaching you to
> someone else's tower. Use the tower name you set when creating the
> Helm release; in this runbook that is `spire-tower`.

What this does:

1. Calls `GET /api/v1/tower` on the gateway with the bearer token, and
   confirms the returned name equals `--tower`.
2. Persists the token in the OS keychain under service `spire-tower`,
   account `<tower-name>-token` (macOS Keychain via `security
   add-generic-password`; Linux secret-service via `secret-tool`).
3. Writes a gateway-mode `TowerConfig` to your local Spire config
   directory (`mode: gateway`, `url`, `token_ref`) and marks the tower
   active.

Subsequent `spire` commands (`spire file`, `spire claim`, `spire
collect`, etc.) detect the gateway-mode tower and route requests over
HTTPS instead of touching a local dolt server.

Optional: pass `--name=<alias>` to attach the cluster under a local
alias different from the remote tower name (useful when you have a
local-mode tower that already shares the name).

### Verify

```bash
spire tower list
```

Expected output (the active gateway-mode tower is marked with `*`):

```
  NAME             PREFIX   DATABASE             KIND       REMOTE
  ----             ------   --------             ----       ------
* spire-tower      spi      spire-tower          gateway    https://spire.example.com

  * = active tower
```

> **Design note on the verify verb.** The design suggested `spire status`
> with output `attached: cluster (https://spire.example.com)`. As
> implemented, `spire status` prints services (dolt, daemon, steward),
> agents, and the work queue — it does not print the active tower URL.
> Use `spire tower list` (above) to confirm the attachment; the `KIND`
> column shows `gateway` and the `REMOTE` column shows the cluster URL.
> If you also want to confirm the gateway is reachable, hit
> `https://spire.example.com/healthz` directly with `curl`.

A quick functional probe — file a throwaway bead through the CLI and
confirm it lands on the cluster's dolt server:

```bash
spire file "smoke test from cluster CLI" -t task -p 4
kubectl -n spire exec deploy/spire-dolt -- \
  dolt sql -q "SELECT id, title FROM \`spire-tower\`.issues ORDER BY created_at DESC LIMIT 1"
```

If the bead you just filed appears in the dolt query, the gateway and
the CLI are wired end-to-end.

### Detach or switch back

To stop using the cluster from this laptop:

```bash
# Detach the gateway-mode tower entirely (removes local config + keychain entry).
spire tower remove spire-tower
```

> **Design note on the detach verb.** The design called for
> `spire tower attach-cluster --remove` and `spire tower attach-local`.
> Neither flag landed as named — `attach-cluster` has no `--remove`
> mode, and there is no `attach-local` verb. The flow is:
>
> - **Detach this tower:** `spire tower remove <name>` — removes the
>   tower config and clears the keychain entry. For gateway-mode towers
>   no database is dropped (there is no local dolt database to drop);
>   for local-mode towers `tower remove` also drops the database, so
>   read the confirmation prompt before typing the tower name.
> - **Switch back to a local-mode tower without removing the cluster
>   attachment:** keep both towers configured and use
>   `spire tower use <local-tower-name>` to flip which one is active.
>   `spire tower list` shows them both.
> - **Re-attach later:** re-run `spire tower attach-cluster` with the
>   same flags. Keychain writes are idempotent; the local config is
>   recreated.

---

## 9. Attach the Desktop

Spire Desktop is the GUI companion to the CLI: a board view, a per-bead
panel, and an agent roster. It is the E5 deliverable from the
production-cluster epic. The first-run flow lets an archmage point the
desktop at the same gateway you just attached the CLI to.

### Where to get it

Spire Desktop lives in a separate repository (`spire-desktop`) and is
not part of the `awell-health/spire` release artifacts. Until the
desktop has its own published release artifacts, build it from source
following the README in that repo. (When desktop builds are published,
the link will go on the project's GitHub releases page; this runbook
will be updated with a direct URL.)

### First-run flow

Launch the desktop. On a clean install with no tower configured, the
welcome screen offers two paths:

- **Use a local tower** — points the desktop at a `spire serve`
  instance running on the laptop. Not what we want here.
- **Attach to cluster** — collects a gateway URL and a bearer token,
  then connects over HTTPS. Pick this.

Walkthrough of the **Attach to cluster** dialog:

1. **URL** — paste `https://spire.example.com` (the same URL you used
   for the CLI). The desktop probes `GET /healthz` before continuing
   and surfaces a clear error if the cert is not yet ready or the host
   does not resolve.
2. **Token** — paste the bearer token from the `spire-gateway-auth`
   Secret (the same `$TOWER_TOKEN` value you used for the CLI).
3. **Connect** — the desktop calls `GET /api/v1/tower` with the bearer
   token, confirms the returned tower name, and saves both pieces.

On success, the desktop transitions to the board view and starts polling
`/api/v1/beads` and `/api/v1/roster`. The first paint should show the
beads currently in the cluster's tower database.

### Where the token is stored

The desktop persists the bearer token in the same OS-native secret
store the CLI uses:

- **macOS:** Keychain, under service `spire-tower`, account
  `<tower-name>-token` (visible in **Keychain Access → login →
  Passwords**).
- **Linux:** secret-service (libsecret), via the
  `org.freedesktop.secrets` D-Bus API (visible in `seahorse` /
  GNOME Passwords under the same service+account labels).

The URL itself, plus a `TokenRef` pointing into the keychain entry,
goes into the desktop's config file (`~/Library/Application
Support/spire-desktop/` on macOS; `~/.config/spire-desktop/` on Linux).
The token never lives in the config file — only the reference does, so
backing up the config directory does not exfiltrate the token.

### Reconnect behaviour

When the gateway pod rolls (helm upgrade, node drain, etc.), in-flight
HTTPS requests fail and websocket subscriptions drop. The desktop's
gateway client treats this as a transient condition: it shows a brief
**Reconnecting…** banner across the top of the window, retries with
backoff, and clears the banner on the first successful poll against
the new gateway pod. No user action is required for routine rolls.

If the banner persists for more than ~30 seconds, that is a real
problem (gateway crash-looping, ingress misconfigured, token revoked) —
check `kubectl -n spire get pods -l app.kubernetes.io/name=spire-gateway`
and `kubectl -n spire logs deploy/spire-gateway --tail=100`. See §12
for the troubleshooting matrix.

### One cluster at a time

Spire Desktop in v1 holds exactly one tower attachment. To point the
desktop at a different cluster, open **Settings → Tower** and either
**Detach** (clears the current attachment and returns to the welcome
screen) or **Attach to cluster** again with new credentials (overwrites
in place).

> Multi-tower attach — switching between several clusters from a single
> desktop window without re-pasting credentials — is explicitly out of
> scope for v1. The CLI already supports multiple towers
> (`spire tower list`, `spire tower use <name>`), so an archmage who
> needs to operate against several clusters can do so from the
> terminal; the desktop will gain multi-tower support after v1 ships.
