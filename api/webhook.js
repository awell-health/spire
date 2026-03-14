import crypto from 'node:crypto';

const DOLTHUB_API = 'https://www.dolthub.com/api/v1alpha1';

export default async function handler(req, res) {
  // Only accept POST
  if (req.method !== 'POST') {
    return res.status(405).json({ error: 'Method not allowed' });
  }

  // Get raw body for signature verification
  const rawBody = typeof req.body === 'string'
    ? req.body
    : JSON.stringify(req.body);

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
  const payload = typeof req.body === 'string'
    ? JSON.parse(req.body)
    : req.body;

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
  writeToQueue(id, eventType, identifier, rawBody).catch((err) => {
    console.error(`[webhook] DoltHub write failed: ${err.message}`);
  });

  // Respond immediately
  return res.status(200).json({ ok: true, id });
}

/**
 * Verify the Linear webhook signature using HMAC-SHA256.
 */
function verifySignature(body, signature, secret) {
  try {
    const hmac = crypto.createHmac('sha256', secret);
    hmac.update(body, 'utf-8');
    const expected = hmac.digest('hex');
    return crypto.timingSafeEqual(
      Buffer.from(signature),
      Buffer.from(expected),
    );
  } catch {
    return false;
  }
}

/**
 * Write a webhook event to the webhook_queue table on DoltHub.
 * Uses the DoltHub SQL write API (async, fire-and-forget).
 */
async function writeToQueue(id, eventType, linearId, payload) {
  const owner = process.env.DOLTHUB_OWNER || 'awell';
  const database = process.env.DOLTHUB_DATABASE || 'spire';
  const token = process.env.DOLTHUB_API_TOKEN;

  if (!token) {
    throw new Error('DOLTHUB_API_TOKEN not set');
  }

  // Escape single quotes in payload for SQL
  const escapedPayload = payload.replace(/'/g, "''");

  const sql = [
    'INSERT INTO webhook_queue (id, event_type, linear_id, payload, processed, created_at)',
    `VALUES ('${id}', '${eventType}', '${linearId}', '${escapedPayload}', 0, NOW())`,
  ].join(' ');

  const url = `${DOLTHUB_API}/${owner}/${database}/write/main/main`;

  const resp = await fetch(url, {
    method: 'POST',
    headers: {
      authorization: `token ${token}`,
      'content-type': 'application/x-www-form-urlencoded',
    },
    body: new URLSearchParams({ q: sql }),
  });

  if (!resp.ok) {
    const text = await resp.text();
    throw new Error(`DoltHub API ${resp.status}: ${text}`);
  }

  const result = await resp.json();
  console.log(
    `[webhook] Queued ${eventType} for ${linearId} (id=${id}, op=${result.operation_name || 'unknown'})`,
  );
}

// Export for testing
export { verifySignature, writeToQueue };
