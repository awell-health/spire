# spire connect linear

**Date**: 2026-03-15
**Status**: Draft
**Parent**: spi-lac (Open-source Spire)

## Problem

Setting up the Linear integration today requires five manual steps:

1. Create a Linear personal API key
2. Find your team UUID
3. Set `LINEAR_API_KEY` and `LINEAR_TEAM_ID` in a `.env` file
4. Go to Linear settings → create a webhook pointing at your deployed app
5. Copy the webhook signing secret into another env var

This is fine for internal use but terrible onboarding for an open-source project. A new user shouldn't need to touch the Linear API console at all.

## Solution

A single CLI command: `spire connect linear`.

Uses OAuth2 with PKCE (no client secret needed) to authenticate the user, then walks them through team/project selection and webhook setup — all in the terminal.

### Full flow

```
$ spire connect linear

  Opening Linear authorization in your browser...
  Waiting for callback on localhost:9876...

  ✓ Authenticated as user@example.com

  Select a team:
    1. Engineering (ENG)
    2. Platform (PLT)
  > 1

  Select a project (optional, enter to skip):
    1. Q1 Roadmap
    2. Tech Debt
  > [enter]

  Set up webhook? This sends Linear events back to Spire.
  Webhook URL (enter to skip): https://my-spire.vercel.app
  ✓ Webhook created → https://my-spire.vercel.app/api/webhook

  ✓ Linear connected
    Team: Engineering (ENG)
    Webhook: https://my-spire.vercel.app/api/webhook
    Credentials saved to beads config
```

### Step by step

#### 1. OAuth2 authorization (PKCE)

Linear supports OAuth2 for public clients with PKCE. No client secret is needed on the device.

**Prerequisites**: A Linear OAuth application registered under the Spire project. This gives us a `client_id`. The OAuth app is configured with:
- Redirect URI: `http://localhost:*` (Linear allows localhost wildcards for CLI tools)
- Scopes: `read`, `write`, `issues:create`, `webhooks:create`

**Flow**:

1. Generate a random `code_verifier` (43-128 chars, URL-safe)
2. Derive `code_challenge` = base64url(SHA256(code_verifier))
3. Start a temporary HTTP server on `localhost:<random-port>`
4. Open the browser to:
   ```
   https://linear.app/oauth/authorize?
     client_id=<SPIRE_CLIENT_ID>&
     response_type=code&
     redirect_uri=http://localhost:<port>/callback&
     scope=read,write,issues:create,webhooks:create&
     code_challenge=<code_challenge>&
     code_challenge_method=S256&
     state=<random>
   ```
5. User approves in browser → Linear redirects to `localhost:<port>/callback?code=<code>&state=<state>`
6. CLI receives the callback, verifies `state`, exchanges the code:
   ```
   POST https://api.linear.app/oauth/token
   Content-Type: application/x-www-form-urlencoded

   grant_type=authorization_code&
   client_id=<SPIRE_CLIENT_ID>&
   redirect_uri=http://localhost:<port>/callback&
   code=<code>&
   code_verifier=<code_verifier>
   ```
7. Receives `{ access_token, token_type, expires_in, scope }`
8. Shut down the temporary server

All of this is Go stdlib: `net/http` for the server, `crypto/sha256` + `encoding/base64` for PKCE, `os/exec` for `open` (macOS) / `xdg-open` (Linux).

#### 2. Team selection

With the access token, fetch the user's teams:

```graphql
query {
  teams {
    nodes {
      id
      name
      key
    }
  }
}
```

Display as a numbered list in the terminal. User picks by number. If only one team, auto-select it.

#### 3. Project selection (optional)

```graphql
query($teamId: String!) {
  team(id: $teamId) {
    projects {
      nodes {
        id
        name
      }
    }
  }
}
```

Display as a numbered list. Enter to skip (no project filter).

#### 4. Webhook setup (optional)

The CLI deploys the webhook receiver and creates the Linear webhook in one step:

```
  Set up webhook? Linear events will flow back to Spire.

  Deploy webhook receiver:
    1. Cloudflare Worker
    2. GCP Cloud Function
    3. AWS Lambda
    4. Self-hosted (spire serve)
    5. I have my own URL
    6. Skip (polling only)
  > 1

  Deploying to Cloudflare Workers...
  ✓ Deployed → https://spire-webhook.your-account.workers.dev
  ✓ Webhook created in Linear
  ✓ Signing secret saved to keychain
```

##### Deployment targets

The webhook handler is ~50 lines of logic: verify HMAC signature, parse JSON, forward to a queue. Each target gets a minimal, self-contained handler bundled in the `spire` binary.

| Target | CLI tool detected | What gets deployed |
|--------|------------------|--------------------|
| Cloudflare Worker | `wrangler` | JS worker (~30 lines) |
| GCP Cloud Function | `gcloud` | Go function |
| AWS Lambda | `aws` | Go binary via provided.al2023 runtime |
| Self-hosted | — | Prints `spire serve --port 8080` instructions |
| Own URL | — | Just creates the Linear webhook pointing at the given URL |

Each serverless handler does the same thing:
1. Verify the Linear signature (HMAC-SHA256) using the signing secret
2. Parse the payload (action, type, identifier)
3. POST to DoltHub's SQL write API (INSERT into `webhook_queue`)

The CLI handles all the deployment plumbing:
- Detects which CLI tools are installed
- Scaffolds the function from an embedded template
- Deploys via the platform's CLI
- Sets environment variables (signing secret, DoltHub token) on the platform
- Returns the public URL
- Creates the Linear webhook pointing at that URL

##### Self-hosted: `spire serve`

For users who can expose a port publicly (or are behind a tunnel like Cloudflare Tunnel or ngrok):

```bash
spire serve --port 8080
```

A Go HTTP handler in the `spire` binary. Receives webhooks and writes directly to the local Dolt database — no DoltHub intermediary needed. This is the simplest option and has no external dependencies.

##### Linear webhook creation

Once we have a URL (from any deployment path), create the webhook:

```graphql
mutation {
  webhookCreate(input: {
    url: "<deployed-url>/webhook"
    teamId: "<team-id>"
    resourceTypes: ["Issue", "Comment", "Project"]
    enabled: true
    label: "Spire"
  }) {
    success
    webhook {
      id
      secret
    }
  }
}
```

The `secret` returned is stored in the system keychain and deployed as an env var to the serverless platform.

If the user skips, webhook processing doesn't happen — epic sync still works via polling.

##### Killing the Next.js webhook app

The `apps/webhook/` Next.js app is no longer needed. The webhook handler is simple enough to live in:
- The Go binary (`spire serve`)
- Embedded serverless templates (Cloudflare/GCP/AWS)

The Next.js app, React, and Vercel dependency can be removed from the monorepo. This also eliminates the only Node.js runtime dependency for the core product — Spire becomes a single Go binary.

#### 5. Store credentials

Secrets and preferences are stored separately:

**Secrets → system keychain** (encrypted, per-machine, never synced):

```
# macOS
security add-generic-password -a "spire" -s "linear.access-token" -w "<token>"
security add-generic-password -a "spire" -s "linear.webhook-secret" -w "<secret>"

# Linux
secret-tool store --label="spire: linear.access-token" service spire key linear.access-token

# Read back
security find-generic-password -a "spire" -s "linear.access-token" -w   # macOS
secret-tool lookup service spire key linear.access-token                  # Linux
```

**Non-secret preferences → bd config** (syncs with Dolt, shared across team):

```bash
bd config set linear.team-id <uuid>
bd config set linear.team-key <key>        # e.g., "ENG"
bd config set linear.project-id <uuid>     # if selected
bd config set linear.webhook-url <url>     # if webhook created
```

**Env vars still work as overrides** for CI/automation where keychains aren't available:

```bash
LINEAR_API_KEY=lin_api_... spire daemon   # overrides keychain
```

### Credential resolution order

When the daemon or epic agent needs the Linear token:

1. `LINEAR_API_KEY` env var (CI/automation override)
2. System keychain lookup (`spire` / `linear.access-token`)
3. Error: "Run `spire connect linear` to authenticate"

This means:
- **Local dev**: `spire connect linear` once, keychain handles it forever
- **CI/server**: set `LINEAR_API_KEY` env var, no keychain needed
- **Team config** (team ID, project, webhook URL) syncs via Dolt — set up once, whole team gets it

### Keychain implementation

Go stdlib doesn't include keychain access, but the CLI commands are simple enough to shell out:

```go
func keychainSet(key, value string) error {
    if runtime.GOOS == "darwin" {
        return exec.Command("security", "add-generic-password",
            "-a", "spire", "-s", key, "-w", value, "-U").Run()
    }
    // Linux: secret-tool
    return exec.Command("secret-tool", "store",
        "--label=spire: "+key, "service", "spire", "key", key).Run()
}

func keychainGet(key string) (string, error) {
    if runtime.GOOS == "darwin" {
        out, err := exec.Command("security", "find-generic-password",
            "-a", "spire", "-s", key, "-w").Output()
        return strings.TrimSpace(string(out)), err
    }
    out, err := exec.Command("secret-tool", "lookup",
        "service", "spire", "key", key).Output()
    return strings.TrimSpace(string(out)), err
}
```

No external Go dependencies. The `-U` flag on macOS updates an existing entry if present (idempotent).

### Disconnect

```
$ spire disconnect linear

  ✓ Webhook deleted from Linear
  ✓ Token removed from keychain
  ✓ Team config removed from beads config
```

Revokes the OAuth token, deletes the webhook, removes keychain entries, clears bd config keys.

### Reconnect / update

Running `spire connect linear` again when already connected:

```
$ spire connect linear

  Linear is already connected (team: Engineering).
  Reconnect? [y/N] y

  Opening Linear authorization in your browser...
  ...
```

## Impact on existing code

### Epic agent (`packages/epic-agent/`)

Currently reads from env vars. Change to credential resolution order:

```javascript
// 1. Env var override (CI/automation)
// 2. System keychain (local dev)
// 3. Error
const token = process.env.LINEAR_API_KEY || keychainGet("linear.access-token");
const teamId = bdConfigGet("linear.team-id") || process.env.LINEAR_TEAM_ID;
```

The token comes from keychain (or env override). The team ID comes from bd config (shared, non-secret).

### Daemon (`cmd/spire/daemon.go`)

Already processes webhook events. Now also reads `linear.webhook-secret` from bd config for signature verification (passed to the webhook app via env, or the webhook app reads it from bd config too).

### Webhook app (`apps/webhook/`)

Currently reads `LINEAR_WEBHOOK_SECRET` from env. Could also read from bd config, but since it runs on Vercel (remote), it needs env vars. The `spire connect` flow should print:

```
Set this env var in your Vercel deployment:
  LINEAR_WEBHOOK_SECRET=<secret>
```

Or better: if the Vercel token is available, set it automatically via the Vercel API.

### Go CLI (`cmd/spire/`)

New files:
- `connect.go` — OAuth flow, team picker, webhook creation
- `disconnect.go` — cleanup

Update `main.go` to add `connect` and `disconnect` commands.

## Extensibility

The `connect` pattern works for any integration:

```
spire connect linear      # OAuth2 + team picker + webhook
spire connect jira        # OAuth2 + project picker + webhook
spire connect github      # OAuth2 + repo picker + webhook
spire connect slack       # OAuth2 + channel picker
```

Each integration implements:
1. An OAuth2 flow (provider-specific scopes and endpoints)
2. A resource picker (team/project/repo/channel)
3. Optional webhook setup
4. Credential storage: secrets in system keychain, preferences in bd config under `<service>.*`

## Prerequisites

- Register a **Linear OAuth application** for the Spire project
  - This is done once by the project maintainers
  - The `client_id` is embedded in the Spire binary (it's not secret — PKCE means no client secret is needed)
  - Redirect URI pattern: `http://localhost:*`

## Open questions

- **Token refresh**: Linear OAuth tokens may expire. Should `connect.go` handle refresh tokens, or just re-run `spire connect linear`?
- **Multi-user**: In a shared Spire hub, should each user have their own Linear token, or is one shared token enough? For v1, one token is fine.
- **Vercel auto-config**: If the user has the Vercel CLI authenticated, should `spire connect linear` automatically deploy the webhook app and set env vars? Nice but complex — probably a follow-up.
