import crypto from "node:crypto";
import { NextRequest, NextResponse } from "next/server";

const DOLTHUB_API = "https://www.dolthub.com/api/v1alpha1";

export async function POST(request: NextRequest) {
  const rawBody = await request.text();

  // Verify Linear webhook signature
  const signature = request.headers.get("linear-signature");
  const secret = process.env.LINEAR_WEBHOOK_SECRET;

  if (!signature || !secret) {
    console.error("[webhook] Missing signature or secret");
    return NextResponse.json({ error: "Unauthorized" }, { status: 401 });
  }

  if (!verifySignature(rawBody, signature, secret)) {
    console.error("[webhook] Invalid signature");
    return NextResponse.json({ error: "Invalid signature" }, { status: 401 });
  }

  // Parse payload
  const payload = JSON.parse(rawBody);

  const action = payload.action;
  const type = payload.type;
  const identifier = payload.data?.identifier;

  if (!identifier) {
    console.error("[webhook] Missing data.identifier in payload");
    return NextResponse.json(
      { error: "Missing data.identifier" },
      { status: 400 }
    );
  }

  const eventType = `${type}.${action}`;
  const id = crypto.randomUUID();

  // Write to DoltHub queue (fire-and-forget)
  writeToQueue(id, eventType, identifier, rawBody).catch((err) => {
    console.error(`[webhook] DoltHub write failed: ${err.message}`);
  });

  return NextResponse.json({ ok: true, id });
}

/**
 * Verify the Linear webhook signature using HMAC-SHA256.
 */
function verifySignature(
  body: string,
  signature: string,
  secret: string
): boolean {
  try {
    const hmac = crypto.createHmac("sha256", secret);
    hmac.update(body, "utf-8");
    const expected = hmac.digest("hex");
    return crypto.timingSafeEqual(
      Buffer.from(signature),
      Buffer.from(expected)
    );
  } catch {
    return false;
  }
}

/**
 * Write a webhook event to the webhook_queue table on DoltHub.
 * Uses the DoltHub SQL write API.
 */
async function writeToQueue(
  id: string,
  eventType: string,
  linearId: string,
  payload: string
) {
  const owner = process.env.DOLTHUB_OWNER || "awell";
  const database = process.env.DOLTHUB_DATABASE || "spire";
  const token = process.env.DOLTHUB_API_TOKEN;

  if (!token) {
    throw new Error("DOLTHUB_API_TOKEN not set");
  }

  const esc = (s: string) => s.replace(/'/g, "''");

  const sql = [
    "INSERT INTO webhook_queue (id, event_type, linear_id, payload, processed, created_at)",
    `VALUES ('${esc(id)}', '${esc(eventType)}', '${esc(linearId)}', '${esc(payload)}', 0, NOW())`,
  ].join(" ");

  const url = `${DOLTHUB_API}/${owner}/${database}/write/main/main`;

  const resp = await fetch(url, {
    method: "POST",
    headers: {
      authorization: `token ${token}`,
      "content-type": "application/x-www-form-urlencoded",
    },
    body: new URLSearchParams({ q: sql }),
  });

  if (!resp.ok) {
    const text = await resp.text();
    throw new Error(`DoltHub API ${resp.status}: ${text}`);
  }

  const result = await resp.json();
  console.log(
    `[webhook] Queued ${eventType} for ${linearId} (id=${id}, op=${result.operation_name || "unknown"})`
  );
}
