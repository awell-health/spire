# Linear Webhook Receiver (Vercel Function) -- Design Spec

## Problem

Linear webhooks fire HTTP POSTs when issues change. The spire daemon (spi-ek7) already knows how to process webhook events and create/update epic beads, but nothing receives those webhooks from Linear and stores them for the daemon to pick up. We need a publicly accessible endpoint that Linear can POST to.

## Constraints

- The receiver runs on Vercel (serverless). It cannot run `bd` commands or access the local Dolt server.
- The spire database lives on DoltHub (`awell/spire`). The Vercel function writes to it via the DoltHub SQL API.
- DoltHub's write API is async (submit query, poll for completion). This is fine -- the daemon pulls periodically anyway.
- The function must validate the webhook signature to prevent spoofing.
- No beads logic, no filtering -- just queue insertion. The daemon handles all interpretation.

## Architecture

```
Linear  ──POST──>  Vercel function  ──SQL API──>  DoltHub (awell/spire)
                   /api/webhook                    webhook_queue table
                                                         │
                                                    dolt pull
                                                         │
                                              spire daemon (local)
                                                         │
                                              reads webhook_queue
                                              creates/updates beads
                                              marks rows processed
```

### Why a separate `webhook_queue` table?

The Vercel function does not understand beads. Writing directly to the beads issues table would require knowing the exact schema, ID generation algorithm, label format, etc. Instead, the function writes to a purpose-built `webhook_queue` table with a minimal schema. The daemon reads from this table and creates proper beads.

This separation keeps the Vercel function dead simple and decouples it from beads internals.

## Vercel Function: `api/webhook.js`

Single serverless function at `POST /api/webhook`.

### Request flow

1. **Verify method**: reject anything that is not POST (return 405).
2. **Verify signature**: check the `Linear-Signature` header against the webhook signing secret using HMAC-SHA256. Reject invalid signatures (return 401).
3. **Parse body**: JSON-parse the request body.
4. **Extract fields**: pull `action`, `type`, `data.id`, `data.identifier` from the payload.
5. **Write to DoltHub**: INSERT a row into `webhook_queue` via the DoltHub SQL API.
6. **Return 200**: respond immediately. Do not wait for the DoltHub write to complete (fire-and-forget with logging).

### Signature verification

Linear signs webhooks with HMAC-SHA256 using the webhook signing secret. The signature is in the `Linear-Signature` header.

```js
import crypto from 'node:crypto';

function verifySignature(body, signature, secret) {
  const hmac = crypto.createHmac('sha256', secret);
  hmac.update(body, 'utf-8');
  const expected = hmac.digest('hex');
  return crypto.timingSafeEqual(
    Buffer.from(signature),
    Buffer.from(expected)
  );
}
```

### DoltHub SQL API write

The DoltHub write API is:

```
POST https://www.dolthub.com/api/v1alpha1/{owner}/{database}/write/{from_branch}/{to_branch}
  ?q=INSERT INTO webhook_queue (id, event_type, linear_id, payload, processed, created_at)
     VALUES (...)
Authorization: token <DOLTHUB_API_TOKEN>
```

For simplicity, the function writes from `main` to `main` (direct commit, no branch).

The function fires the POST and logs the response but does not block on polling. The daemon's `bd dolt pull` will pick up the committed data on its next cycle. If the write fails, it is logged but the webhook still returns 200 to Linear (to avoid retries that could cause duplicates).

### Environment variables

| Variable | Description |
|----------|-------------|
| `LINEAR_WEBHOOK_SECRET` | Linear webhook signing secret (from Linear settings) |
| `DOLTHUB_API_TOKEN` | DoltHub API token for write access |
| `DOLTHUB_OWNER` | DoltHub org (e.g., `awell`) |
| `DOLTHUB_DATABASE` | DoltHub database name (e.g., `spire`) |

## `webhook_queue` Table Schema

Created once in the Dolt database via `bd dolt sql`:

```sql
CREATE TABLE IF NOT EXISTS webhook_queue (
  id          VARCHAR(36) PRIMARY KEY,    -- UUID v4
  event_type  VARCHAR(64) NOT NULL,       -- e.g., "Issue.update", "Issue.create"
  linear_id   VARCHAR(32) NOT NULL,       -- e.g., "AWE-123"
  payload     JSON NOT NULL,              -- raw Linear webhook JSON
  processed   BOOLEAN NOT NULL DEFAULT 0, -- false=pending, true=processed
  created_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
```

### Field mapping from webhook payload

| Queue field | Source |
|-------------|--------|
| `id` | Generated UUID v4 |
| `event_type` | `${payload.type}.${payload.action}` (e.g., `Issue.update`) |
| `linear_id` | `payload.data.identifier` (e.g., `AWE-123`) |
| `payload` | Entire raw JSON body |
| `processed` | `0` (false) |
| `created_at` | Current timestamp |

## Daemon Changes

The daemon (`cmd/spire/daemon.go`) currently reads webhook events as beads via `bd list --label webhook`. This must be updated to also (or instead) read from the `webhook_queue` table.

### New flow in `processWebhookEvents`:

1. Query: `bd dolt sql -q "SELECT id, event_type, linear_id, payload FROM webhook_queue WHERE processed = 0"`
2. For each row, parse the payload JSON and run the existing `processWebhookEvent` logic.
3. After successful processing, mark processed: `bd dolt sql -q "UPDATE webhook_queue SET processed = 1 WHERE id = '<id>'"`

The existing bead-based webhook processing can be kept as a fallback or removed. The `webhook_queue` approach is preferred because:
- It is a clean contract between the Vercel function and the daemon.
- The Vercel function does not need to know beads internals.
- Queue rows are simpler to debug than beads with specific label conventions.

### Conversion: queue row to webhook event bead

After reading a queue row, the daemon creates a webhook event bead (same as before) for audit trail:

```bash
bd create --rig=spi --type=task -p 3 \
  --title "Issue updated: AWE-123" \
  --labels "webhook,event:Issue.update,linear:AWE-123" \
  --description '<raw JSON payload>'
```

Then processes it with the existing `processWebhookEvent` function and closes it. This preserves backward compatibility -- the daemon's processing logic does not change, only the input source.

## File Structure

### New files (Vercel project at repo root)

```
api/
  webhook.js        -- Vercel serverless function
vercel.json         -- Vercel config (routes, env)
```

### Modified files

```
cmd/spire/
  daemon.go         -- read from webhook_queue instead of bead labels
  webhook.go        -- add queue-to-bead conversion helper
```

### One-time setup

```
setup.sh            -- add webhook_queue table creation
```

## Error Handling

- **Invalid signature**: 401 response, log warning. Do not write to queue.
- **Non-POST method**: 405 response.
- **Missing required fields** (no `data.identifier`): 400 response, log warning.
- **DoltHub write failure**: log error, return 200 to Linear anyway (prevent retries). The event is lost but Linear will show it in their webhook logs for manual replay.
- **Duplicate events**: Linear may retry webhooks. The UUID primary key prevents exact duplicates. The daemon's idempotent processing handles semantic duplicates (same Linear issue updated twice).

## Vercel Configuration

### `vercel.json`

```json
{
  "functions": {
    "api/webhook.js": {
      "memory": 128,
      "maxDuration": 10
    }
  }
}
```

### Deployment

```bash
# First time
cd ~/awell/spire
npx vercel link
npx vercel env add LINEAR_WEBHOOK_SECRET
npx vercel env add DOLTHUB_API_TOKEN
npx vercel env add DOLTHUB_OWNER
npx vercel env add DOLTHUB_DATABASE
npx vercel deploy --prod

# Subsequent
npx vercel deploy --prod
```

After deployment, configure the Vercel URL as the webhook endpoint in Linear workspace settings.

## Testing

- Unit test: signature verification (pure function, no network)
- Unit test: field extraction from payload (pure function)
- Integration test: POST to local dev server with valid/invalid signatures
- Manual test: configure Linear webhook to point at Vercel deployment, trigger an issue update, verify row appears in DoltHub

## Out of Scope (v1)

- **Retry queue**: if DoltHub write fails, the event is lost. Linear's webhook retry mechanism provides some resilience.
- **Batch writes**: each webhook writes one row. Batch optimization is unnecessary at current volume.
- **Event filtering**: the function writes ALL events. The daemon decides what to process.
- **Multiple Linear workspaces**: single signing secret, single DoltHub database.
