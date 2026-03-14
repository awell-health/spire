#!/usr/bin/env node

// index.js — Epic agent: polls beads for new epics and mirrors them to Linear
//
// Usage:
//   node index.js           # Run in poll mode (default)
//   node index.js --once    # Run once and exit (good for cron)

import { execSync } from "node:child_process";
import { readFileSync, writeFileSync, existsSync } from "node:fs";
import { resolve, dirname } from "node:path";
import { fileURLToPath } from "node:url";
import "dotenv/config";
import { LinearClient } from "./linear-client.js";

const __dirname = dirname(fileURLToPath(import.meta.url));
const LEDGER_PATH = resolve(__dirname, "synced-epics.json");

// ── Config ──────────────────────────────────────────────────────────────────

const config = {
  linearApiKey: process.env.LINEAR_API_KEY,
  linearTeamId: process.env.LINEAR_TEAM_ID,
  linearProjectId: process.env.LINEAR_PROJECT_ID || null,
  pollIntervalMs: parseInt(process.env.POLL_INTERVAL_MS || "30000", 10),
  hubPath: resolve(process.env.HUB_PATH || resolve(__dirname, "..")),
  runOnce: process.argv.includes("--once"),
};

// ── Ledger ──────────────────────────────────────────────────────────────────

function loadLedger() {
  if (!existsSync(LEDGER_PATH)) return {};
  try {
    return JSON.parse(readFileSync(LEDGER_PATH, "utf-8"));
  } catch {
    return {};
  }
}

function saveLedger(ledger) {
  writeFileSync(LEDGER_PATH, JSON.stringify(ledger, null, 2) + "\n");
}

// ── Beads CLI ───────────────────────────────────────────────────────────────

function bdList() {
  try {
    const output = execSync("bd list --json --type epic", {
      cwd: config.hubPath,
      encoding: "utf-8",
      timeout: 15_000,
      env: {
        ...process.env,
        BEADS_DOLT_SHARED_SERVER: "1",
      },
    });

    const parsed = JSON.parse(output.trim() || "[]");
    return Array.isArray(parsed) ? parsed : [];
  } catch (err) {
    console.error(`[agent] Error listing epics: ${err.message}`);
    return [];
  }
}

function bdUpdate(id, label) {
  try {
    execSync(`bd update ${id} --label "${label}"`, {
      cwd: config.hubPath,
      encoding: "utf-8",
      timeout: 10_000,
      env: {
        ...process.env,
        BEADS_DOLT_SHARED_SERVER: "1",
      },
    });
  } catch (err) {
    console.error(`[agent] Error updating bead ${id}: ${err.message}`);
  }
}

function bdComment(id, comment) {
  try {
    // Escape the comment for shell safety
    const escaped = comment.replace(/"/g, '\\"');
    execSync(`bd update ${id} --comment "${escaped}"`, {
      cwd: config.hubPath,
      encoding: "utf-8",
      timeout: 10_000,
      env: {
        ...process.env,
        BEADS_DOLT_SHARED_SERVER: "1",
      },
    });
  } catch (err) {
    console.error(`[agent] Error commenting on bead ${id}: ${err.message}`);
  }
}

// ── Sync Logic ──────────────────────────────────────────────────────────────

async function syncEpics(linear) {
  const epics = bdList();
  const ledger = loadLedger();
  let synced = 0;

  for (const epic of epics) {
    // Skip if already synced
    if (ledger[epic.id]) continue;

    // Skip if already has a linear label (synced outside this agent)
    const hasLinearLabel = epic.labels?.some((l) => l.startsWith("linear:"));
    if (hasLinearLabel) {
      ledger[epic.id] = {
        syncedAt: new Date().toISOString(),
        linearIdentifier: "external",
        note: "Already had linear label, skipped",
      };
      continue;
    }

    console.log(`[agent] New epic: ${epic.id} — "${epic.title}"`);

    try {
      // Create Linear issue
      const issue = await linear.createIssueFromEpic(epic);
      console.log(
        `[agent]   → Created Linear issue ${issue.identifier} (${issue.url})`
      );

      // Link back: add label to bead
      bdUpdate(epic.id, `linear:${issue.identifier}`);

      // Link back: add comment with URL
      bdComment(
        epic.id,
        `Linear issue created: ${issue.identifier} — ${issue.url}`
      );

      // Record in ledger
      ledger[epic.id] = {
        syncedAt: new Date().toISOString(),
        linearId: issue.id,
        linearIdentifier: issue.identifier,
        linearUrl: issue.url,
      };

      synced++;
    } catch (err) {
      console.error(
        `[agent]   ✗ Failed to sync epic ${epic.id}: ${err.message}`
      );
    }
  }

  saveLedger(ledger);

  if (synced > 0) {
    console.log(`[agent] Synced ${synced} new epic(s) to Linear`);
  }

  return synced;
}

// ── Main ────────────────────────────────────────────────────────────────────

async function main() {
  console.log("[agent] Spire — Epic → Linear Agent");
  console.log(`[agent] Hub path: ${config.hubPath}`);
  console.log(`[agent] Poll interval: ${config.pollIntervalMs}ms`);
  console.log("");

  // Validate config
  if (!config.linearApiKey || !config.linearTeamId) {
    console.error(
      "[agent] Error: LINEAR_API_KEY and LINEAR_TEAM_ID must be set in .env"
    );
    console.error("[agent] Copy .env.example to .env and fill in your values.");
    process.exit(1);
  }

  const linear = new LinearClient({
    apiKey: config.linearApiKey,
    teamId: config.linearTeamId,
    projectId: config.linearProjectId,
  });

  // Verify Linear connection
  try {
    const team = await linear.verify();
    console.log(`[agent] Connected to Linear team: ${team.name} (${team.key})`);
  } catch (err) {
    console.error(`[agent] Failed to connect to Linear: ${err.message}`);
    process.exit(1);
  }

  if (config.runOnce) {
    console.log("[agent] Running once...");
    await syncEpics(linear);
    process.exit(0);
  }

  // Poll loop
  console.log("[agent] Starting poll loop...");
  console.log("");

  const poll = async () => {
    try {
      await syncEpics(linear);
    } catch (err) {
      console.error(`[agent] Poll error: ${err.message}`);
    }
  };

  // Run immediately, then on interval
  await poll();
  setInterval(poll, config.pollIntervalMs);
}

main().catch((err) => {
  console.error(`[agent] Fatal: ${err.message}`);
  process.exit(1);
});
