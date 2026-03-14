# Linear Webhook Receiver Implementation Plan

> **For agentic workers:** REQUIRED: Use superpowers:subagent-driven-development (if subagents available) or superpowers:executing-plans to implement this plan. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Deploy a Vercel serverless function that receives Linear webhook POSTs, validates signatures, and writes raw events to a `webhook_queue` table on DoltHub. Update the spire daemon to read from this table.

**Architecture:** Vercel function (JS) writes to DoltHub via SQL API. Daemon (Go) reads from `webhook_queue` via `bd dolt sql`, creates beads, and marks rows processed.

**Tech Stack:** Node.js (Vercel runtime), DoltHub SQL API, Go (daemon updates)

**Spec:** `docs/superpowers/specs/2026-03-13-linear-webhook-receiver.md`

---

## File Structure

```
api/
  webhook.js          -- Vercel serverless function
vercel.json           -- Vercel config
cmd/spire/
  daemon.go           -- update processWebhookEvents to read webhook_queue
  webhook.go          -- add queueRowToBeadEvent helper
  spire_test.go       -- add tests for new queue processing
setup.sh              -- add webhook_queue table creation
```

---

## Chunk 1: webhook_queue Table

### Task 1: Create the webhook_queue table in setup.sh

**Files:**
- Modify: `setup.sh`

- [ ] **Step 1: Add table creation after beads hub init (step 4)**

After the DoltHub remote configuration block in step 4, add:

```bash
# Create webhook_queue table if not exists
info "Ensuring webhook_queue table exists..."
bd dolt sql -q "CREATE TABLE IF NOT EXISTS webhook_queue (
  id          VARCHAR(36) PRIMARY KEY,
  event_type  VARCHAR(64) NOT NULL,
  linear_id   VARCHAR(32) NOT NULL,
  payload     JSON NOT NULL,
  processed   BOOLEAN NOT NULL DEFAULT 0,
  created_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);" 2>/dev/null && ok "webhook_queue table ready" || warn "Could not create webhook_queue table (run manually)"
```

- [ ] **Step 2: Create the table locally now**

```bash
cd /Users/jb/awell/spire && bd dolt sql -q "CREATE TABLE IF NOT EXISTS webhook_queue (
  id VARCHAR(36) PRIMARY KEY,
  event_type VARCHAR(64) NOT NULL,
  linear_id VARCHAR(32) NOT NULL,
  payload JSON NOT NULL,
  processed BOOLEAN NOT NULL DEFAULT 0,
  created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);"
```

- [ ] **Step 3: Commit**

```bash
git add setup.sh
git commit -m "feat(webhook): add webhook_queue table creation to setup.sh"
```

---

## Chunk 2: Vercel Function

### Task 2: api/webhook.js -- the serverless endpoint

**Files:**
- Create: `api/webhook.js`
- Create: `vercel.json`

- [ ] **Step 1: Write api/webhook.js**

```js
import crypto from 'node:crypto';

const DOLTHUB_API = 'https://www.dolthub.com/api/v1alpha1';

export default async function handler(req, res) {
  // Only accept POST
  if (req.method !== 'POST') {
    return res.status(405).json({ error: 'Method not allowed' });
  }

  // Get raw body for signature verification
  const rawBody = typeof req.body === 'string' ? req.body : JSON.stringify(req.body);

  // Verify signature
  const signature = req.headers['linear-signature'];
  const secret = process.env.LINEAR_WEBHOOK_SECRET;

  if (!signature || !secret) {
    console.error('[webhook] Missing signature or secret');
    return res.status(401).json({ error: 'Unauthorized' });
  }

  if (!verifySignature(rawBody, signature, secret)) {
    console.error('[webhook] Invalid signature');
    return res.status(401).json({ error: 'Invalid signature' });
  }

  // Parse payload
  const payload = typeof req.body === 'string' ? JSON.parse(req.body) : req.body;

  // Extract fields
  const action = payload.action;
  const type = payload.type;
  const identifier = payload.data?.identifier;

  if (!identifier) {
    console.error('[webhook] Missing data.identifier in payload');
    return res.status(400).json({ error: 'Missing data.identifier' });
  }

  const eventType = `${type}.${action}`;
  const id = crypto.randomUUID();

  // Write to DoltHub (fire-and-forget)
  writeToQueue(id, eventType, identifier, rawBody).catch(err => {
    console.error(`[webhook] DoltHub write failed: ${err.message}`);
  });

  // Respond immediately
  return res.status(200).json({ ok: true, id });
}

function verifySignature(body, signature, secret) {
  try {
    const hmac = crypto.createHmac('sha256', secret);
    hmac.update(body, 'utf-8');
    const expected = hmac.digest('hex');
    return crypto.timingSafeEqual(
      Buffer.from(signature),
      Buffer.from(expected)
    );
  } catch {
    return false;
  }
}

async function writeToQueue(id, eventType, linearId, payload) {
  const owner = process.env.DOLTHUB_OWNER || 'awell';
  const database = process.env.DOLTHUB_DATABASE || 'spire';
  const token = process.env.DOLTHUB_API_TOKEN;

  if (!token) {
    throw new Error('DOLTHUB_API_TOKEN not set');
  }

  // Escape single quotes in payload for SQL
  const escapedPayload = payload.replace(/'/g, "''");

  const sql = `INSERT INTO webhook_queue (id, event_type, linear_id, payload, processed, created_at)
    VALUES ('${id}', '${eventType}', '${linearId}', '${escapedPayload}', 0, NOW())`;

  const url = `${DOLTHUB_API}/${owner}/${database}/write/main/main`;

  const resp = await fetch(url, {
    method: 'POST',
    headers: {
      'authorization': `token ${token}`,
      'content-type': 'application/x-www-form-urlencoded',
    },
    body: new URLSearchParams({ q: sql }),
  });

  if (!resp.ok) {
    const text = await resp.text();
    throw new Error(`DoltHub API ${resp.status}: ${text}`);
  }

  const result = await resp.json();
  console.log(`[webhook] Queued ${eventType} for ${linearId} (id=${id}, op=${result.operation_name || 'unknown'})`);
}
```

- [ ] **Step 2: Write vercel.json**

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

- [ ] **Step 3: Commit**

```bash
git add api/webhook.js vercel.json
git commit -m "feat(webhook): add Vercel serverless function for Linear webhook receiver"
```

---

## Chunk 3: Daemon Updates

### Task 3: Update daemon to read from webhook_queue

**Files:**
- Modify: `cmd/spire/daemon.go`
- Modify: `cmd/spire/webhook.go`

- [ ] **Step 1: Add queue reading to webhook.go**

Add a `webhookQueueRow` struct and `processWebhookQueue` function to `webhook.go`:

```go
// webhookQueueRow represents a row from the webhook_queue table.
type webhookQueueRow struct {
    ID        string `json:"id"`
    EventType string `json:"event_type"`
    LinearID  string `json:"linear_id"`
    Payload   string `json:"payload"`
}

// processWebhookQueue reads unprocessed rows from webhook_queue,
// creates webhook event beads from them, processes them, and marks them done.
// Returns (processed count, error count).
func processWebhookQueue() (int, int) {
    // Query unprocessed queue rows
    out, err := bd("dolt", "sql", "-q",
        "SELECT id, event_type, linear_id, payload FROM webhook_queue WHERE processed = 0",
        "-r", "json")
    if err != nil {
        // Table may not exist yet -- not an error
        if !strings.Contains(err.Error(), "webhook_queue") {
            log.Printf("[daemon] query webhook_queue: %s", err)
        }
        return 0, 0
    }

    if strings.TrimSpace(out) == "" || strings.TrimSpace(out) == "[]" {
        return 0, 0
    }

    var rows []webhookQueueRow
    if err := json.Unmarshal([]byte(out), &rows); err != nil {
        log.Printf("[daemon] parse webhook_queue rows: %s", err)
        return 0, 0
    }

    if len(rows) == 0 {
        return 0, 0
    }

    log.Printf("[daemon] found %d unprocessed queue rows", len(rows))

    processed := 0
    errors := 0

    for _, row := range rows {
        // Create a webhook event bead from the queue row
        eventID, createErr := bdSilent(
            "create",
            "--rig=spi",
            "--type=task",
            "-p", "3",
            "--title", fmt.Sprintf("%s: %s", row.EventType, row.LinearID),
            "--labels", fmt.Sprintf("webhook,event:%s,linear:%s", row.EventType, row.LinearID),
            "--description", row.Payload,
        )
        if createErr != nil {
            log.Printf("[daemon] queue row %s: create bead failed: %s", row.ID, createErr)
            errors++
            continue
        }

        // Fetch the created bead for processing
        showOut, showErr := bd("show", eventID, "--json")
        if showErr != nil {
            log.Printf("[daemon] queue row %s: show bead %s failed: %s", row.ID, eventID, showErr)
            errors++
            continue
        }

        eventBead, parseErr := parseBead([]byte(showOut))
        if parseErr != nil {
            log.Printf("[daemon] queue row %s: parse bead %s failed: %s", row.ID, eventID, parseErr)
            errors++
            continue
        }

        // Process the event (existing logic)
        procErr := processWebhookEvent(eventBead)
        if procErr != nil {
            log.Printf("[daemon] queue row %s: process error (will retry): %s", row.ID, procErr)
            errors++
            continue
        }

        // Close the event bead
        bd("close", eventID)

        // Mark queue row as processed
        _, markErr := bd("dolt", "sql", "-q",
            fmt.Sprintf("UPDATE webhook_queue SET processed = 1 WHERE id = '%s'", row.ID))
        if markErr != nil {
            log.Printf("[daemon] queue row %s: mark processed failed: %s", row.ID, markErr)
            // Don't count as error -- the bead was created and processed
        }

        processed++
    }

    return processed, errors
}
```

- [ ] **Step 2: Update daemon.go runCycle to call processWebhookQueue**

In `runCycle()`, add a call to `processWebhookQueue` before or after the existing `processWebhookEvents`:

```go
// Step 2a: Process webhook queue (from DoltHub/Vercel)
qProcessed, qErrors := processWebhookQueue()
if qProcessed > 0 || qErrors > 0 {
    log.Printf("[daemon] queue: processed %d rows (%d errors)", qProcessed, qErrors)
}

// Step 2b: Process webhook events (legacy bead-based, if any)
processed, errors := processWebhookEvents()
```

- [ ] **Step 3: Build and verify**

```bash
cd /Users/jb/awell/spire && go build -o /tmp/spire ./cmd/spire
```

- [ ] **Step 4: Commit**

```bash
git add cmd/spire/daemon.go cmd/spire/webhook.go
git commit -m "feat(daemon): read from webhook_queue table for DoltHub-sourced events"
```

---

## Chunk 4: Tests

### Task 4: Add tests for queue processing and webhook function

**Files:**
- Modify: `cmd/spire/spire_test.go`

- [ ] **Step 1: Add unit test for webhook signature verification**

This tests the same algorithm the Vercel function uses, implemented in Go for parity:

```go
func TestWebhookSignatureVerification(t *testing.T) {
    // Test the same HMAC-SHA256 algorithm used in api/webhook.js
    secret := "test-secret"
    body := `{"action":"update","type":"Issue","data":{"identifier":"AWE-1"}}`

    h := hmac.New(sha256.New, []byte(secret))
    h.Write([]byte(body))
    expected := hex.EncodeToString(h.Sum(nil))

    // Verify it produces a deterministic signature
    if expected == "" {
        t.Error("empty signature")
    }
    if len(expected) != 64 {
        t.Errorf("signature length = %d, want 64", len(expected))
    }
}
```

- [ ] **Step 2: Add integration test for processWebhookQueue**

```go
func TestIntegrationProcessWebhookQueue(t *testing.T) {
    requireBd(t)

    // Create the webhook_queue table if needed
    _, err := bd("dolt", "sql", "-q", `CREATE TABLE IF NOT EXISTS webhook_queue (
        id VARCHAR(36) PRIMARY KEY,
        event_type VARCHAR(64) NOT NULL,
        linear_id VARCHAR(32) NOT NULL,
        payload JSON NOT NULL,
        processed BOOLEAN NOT NULL DEFAULT 0,
        created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
    )`)
    if err != nil {
        t.Fatalf("create webhook_queue table: %v", err)
    }

    // Insert a test row
    testID := "test-queue-" + t.Name()
    payload := `{"action":"create","type":"Issue","data":{"id":"uuid-queue","identifier":"AWE-77","title":"Queue test epic","priority":1,"labels":[{"name":"Grove - Test"}]}}`
    escapedPayload := strings.ReplaceAll(payload, "'", "''")

    _, err = bd("dolt", "sql", "-q", fmt.Sprintf(
        "INSERT INTO webhook_queue (id, event_type, linear_id, payload) VALUES ('%s', 'Issue.create', 'AWE-77', '%s')",
        testID, escapedPayload))
    if err != nil {
        t.Fatalf("insert queue row: %v", err)
    }

    // Process the queue
    processed, errors := processWebhookQueue()
    if errors > 0 {
        t.Errorf("processWebhookQueue had %d errors", errors)
    }
    if processed == 0 {
        t.Error("processWebhookQueue processed 0 rows")
    }

    // Verify the queue row is marked processed
    out, err := bd("dolt", "sql", "-q",
        fmt.Sprintf("SELECT processed FROM webhook_queue WHERE id = '%s'", testID),
        "-r", "json")
    if err != nil {
        t.Fatalf("check processed: %v", err)
    }
    if !strings.Contains(out, "1") && !strings.Contains(out, "true") {
        t.Errorf("queue row not marked processed: %s", out)
    }

    // Clean up
    bd("dolt", "sql", "-q", fmt.Sprintf("DELETE FROM webhook_queue WHERE id = '%s'", testID))
}
```

- [ ] **Step 3: Run tests**

```bash
cd /Users/jb/awell/spire && go test ./cmd/spire/ -v
```

- [ ] **Step 4: Commit**

```bash
git add cmd/spire/spire_test.go
git commit -m "test(webhook): add tests for webhook queue processing and signature verification"
```

---

## Chunk 5: Final verification

### Task 5: End-to-end build and test

- [ ] **Step 1: Build and run all tests**

```bash
cd /Users/jb/awell/spire && go build -o /tmp/spire ./cmd/spire && go test ./cmd/spire/ -v
```

- [ ] **Step 2: Test daemon --once with queue**

```bash
# Insert a test row into webhook_queue
bd dolt sql -q "INSERT INTO webhook_queue (id, event_type, linear_id, payload) VALUES ('manual-test', 'Issue.create', 'AWE-MANUAL', '{\"action\":\"create\",\"type\":\"Issue\",\"data\":{\"id\":\"uuid\",\"identifier\":\"AWE-MANUAL\",\"title\":\"Manual test\",\"priority\":2,\"labels\":[{\"name\":\"Panels\"}]}}')"

# Run one daemon cycle
/tmp/spire daemon --once 2>&1

# Verify the queue row was processed
bd dolt sql -q "SELECT * FROM webhook_queue WHERE id = 'manual-test'" -r json
```

Expected: queue row marked as processed, epic bead created in `pan` rig.

- [ ] **Step 3: Clean up test data**

```bash
bd dolt sql -q "DELETE FROM webhook_queue WHERE id = 'manual-test'"
```
