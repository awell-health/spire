#!/usr/bin/env node

/**
 * Spire MCP Server
 *
 * Exposes spire agent messaging as MCP tools for Cursor and Claude Code.
 * Shells out to `bd` with label conventions from the spire messaging spec.
 * When the `spire` Go binary lands, swap bd calls for spire calls.
 */

import { McpServer } from "@modelcontextprotocol/sdk/server/mcp.js";
import { StdioServerTransport } from "@modelcontextprotocol/sdk/server/stdio.js";
import { z } from "zod";
import { execFile } from "node:child_process";
import { promisify } from "node:util";
import { readFile } from "node:fs/promises";
import { join, dirname } from "node:path";

const exec = promisify(execFile);

// ─── Helpers ─────────────────────────────────────────────────────────────────

async function bd(...args) {
  try {
    const { stdout } = await exec("bd", args, {
      timeout: 15_000,
      env: { ...process.env },
    });
    return stdout.trim();
  } catch (err) {
    throw new Error(`bd ${args.join(" ")} failed: ${err.stderr || err.message}`);
  }
}

async function bdJson(...args) {
  const out = await bd(...args, "--json");
  return JSON.parse(out);
}

/**
 * Detect caller identity from the working directory's beads config.
 * Looks for .beads/config.yaml issue-prefix, or falls back to SPIRE_IDENTITY env.
 */
function detectIdentity() {
  if (process.env.SPIRE_IDENTITY) return process.env.SPIRE_IDENTITY;
  // Default to "spi" when running from the hub
  return "spi";
}

// ─── Server ──────────────────────────────────────────────────────────────────

const server = new McpServer({
  name: "spire",
  version: "0.1.0",
});

// ── spire_register ───────────────────────────────────────────────────────────

server.tool(
  "spire_register",
  `Register an agent in the Spire roster.

Usage: spire_register({ name: "pan" })

Creates a bead in the spi- rig with labels [agent, name:<name>].
Idempotent — if an open agent bead with this name already exists, returns the existing ID.
Registered agents appear in spire_roster and can receive messages via spire_send.

Examples:
  spire_register({ name: "pan" })   — register the panels agent
  spire_register({ name: "gro" })   — register the grove agent

The agent stays registered until spire_unregister is called (which closes the bead).`,
  { name: z.string().describe("Agent name — typically the repo prefix (e.g. 'pan', 'gro', 'rel')") },
  async ({ name }) => {
    // Check if already registered
    try {
      const issues = await bdJson(
        "list",
        "--rig=spi",
        "--label",
        `agent,name:${name}`,
        "--status=open"
      );
      if (issues.length > 0) {
        return { content: [{ type: "text", text: `Already registered: ${issues[0].id}` }] };
      }
    } catch {
      // No results or parse error — proceed to create
    }

    const out = await bd(
      "create",
      "--rig=spi",
      "--type=task",
      `--title=${name}`,
      "-p", "4",
      "--labels", `agent,name:${name}`
    );
    return { content: [{ type: "text", text: out }] };
  }
);

// ── spire_unregister ─────────────────────────────────────────────────────────

server.tool(
  "spire_unregister",
  `Unregister an agent from the Spire roster.

Usage: spire_unregister({ name: "pan" })

Finds the open agent bead for this name and closes it. The agent will no longer
appear in spire_roster. Messages sent to this agent after unregistering will still
be created (the recipient may re-register later).

No-op if the agent is not currently registered.`,
  { name: z.string().describe("Agent name to unregister") },
  async ({ name }) => {
    const issues = await bdJson(
      "list",
      "--rig=spi",
      "--label",
      `agent,name:${name}`,
      "--status=open"
    );
    if (issues.length === 0) {
      return { content: [{ type: "text", text: `No active registration found for '${name}'` }] };
    }
    const out = await bd("close", issues[0].id);
    return { content: [{ type: "text", text: `Unregistered ${name}: ${out}` }] };
  }
);

// ── spire_send ───────────────────────────────────────────────────────────────

server.tool(
  "spire_send",
  `Send a message to another agent via Spire.

Usage: spire_send({ to: "pan", message: "deploy is failing on staging" })

Creates a message bead in the spi- rig with labels [msg, to:<to>, from:<caller>].
Caller identity is auto-detected from SPIRE_IDENTITY env var (set per-repo by setup.sh).

Options:
  ref      — Link this message to a bead it's about. Adds a ref:<bead-id> label.
             Use this when reporting on or asking about a specific task.
  thread   — Reply to an existing message. Sets --parent on the new bead,
             creating a conversation thread via beads' parent-child hierarchy.
  priority — 0 (critical) to 4 (backlog), default 3. Use 0-1 for urgent messages.

Examples:
  spire_send({ to: "spi", message: "pan-42 is done", ref: "pan-42" })
  spire_send({ to: "pan", message: "looking into it", thread: "spi-12" })
  spire_send({ to: "rel", message: "release blocked on pan-42", priority: 1 })

Messages are beads: open = unread, closed = read. Use spire_collect to check inbox.`,
  {
    to: z.string().describe("Recipient agent name (e.g. 'pan', 'spi')"),
    message: z.string().describe("Message text (becomes the bead title)"),
    ref: z.string().optional().describe("Bead ID this message is about — adds ref:<id> label"),
    thread: z.string().optional().describe("Parent message ID for threading — creates a reply"),
    priority: z.number().min(0).max(4).default(3).describe("Priority 0=critical, 1=high, 2=medium, 3=low (default), 4=backlog"),
  },
  async ({ to, message, ref, thread, priority }) => {
    const from = detectIdentity();
    const labels = [`msg`, `to:${to}`, `from:${from}`];
    if (ref) labels.push(`ref:${ref}`);

    const args = [
      "create",
      "--rig=spi",
      "--type=task",
      "-p", String(priority),
      `--title=${message}`,
      "--labels", labels.join(","),
    ];
    if (thread) args.push("--parent", thread);

    const out = await bd(...args);
    return { content: [{ type: "text", text: out }] };
  }
);

// ── spire_collect ────────────────────────────────────────────────────────────

server.tool(
  "spire_collect",
  `Check an agent's inbox for unread messages.

Usage: spire_collect()              — check inbox for current repo's agent
       spire_collect({ name: "pan" })  — check inbox for a specific agent

Lists all open (unread) message beads addressed to this agent. Each message shows:
  - Bead ID
  - Sender (from: label)
  - Message text (title)
  - Referenced bead if any (ref: label)

Name defaults to SPIRE_IDENTITY env var (set per-repo by setup.sh).
Returns empty if no unread messages.

After reading a message, call spire_read({ bead_id }) to mark it as read (close it).
Only open messages appear in collect — closed messages are history.`,
  {
    name: z.string().optional().describe("Agent name to check inbox for (defaults to current repo's prefix)"),
  },
  async ({ name }) => {
    const who = name || detectIdentity();
    try {
      const issues = await bdJson(
        "list",
        "--rig=spi",
        "--label",
        `msg,to:${who}`,
        "--status=open"
      );
      if (issues.length === 0) {
        return { content: [{ type: "text", text: `No unread messages for '${who}'` }] };
      }
      const lines = issues.map((i) => {
        const fromLabel = (i.labels || []).find((l) => l.startsWith("from:"));
        const refLabel = (i.labels || []).find((l) => l.startsWith("ref:"));
        const from = fromLabel ? fromLabel.slice(5) : "unknown";
        const ref = refLabel ? ` (ref: ${refLabel.slice(4)})` : "";
        return `${i.id}  from:${from}  "${i.title}"${ref}`;
      });
      return {
        content: [{
          type: "text",
          text: `${issues.length} unread message(s) for '${who}':\n\n${lines.join("\n")}\n\nUse spire_read to mark as read.`,
        }],
      };
    } catch {
      return { content: [{ type: "text", text: `No unread messages for '${who}'` }] };
    }
  }
);

// ── spire_read ───────────────────────────────────────────────────────────────

server.tool(
  "spire_read",
  `Mark a message as read.

Usage: spire_read({ bead_id: "spi-12" })

Closes the message bead. Closed messages no longer appear in spire_collect
but remain in the bead graph for history.

No-op if the message is already closed (already read).

Typically called after processing a message from spire_collect.`,
  { bead_id: z.string().describe("Message bead ID to mark as read (e.g. 'spi-12')") },
  async ({ bead_id }) => {
    try {
      const out = await bd("close", bead_id);
      return { content: [{ type: "text", text: out || `Marked ${bead_id} as read` }] };
    } catch (err) {
      if (err.message.includes("already closed")) {
        return { content: [{ type: "text", text: `${bead_id} already read` }] };
      }
      throw err;
    }
  }
);

// ── spire_focus ──────────────────────────────────────────────────────────────

server.tool(
  "spire_focus",
  `Assemble full context for a bead — the "session start" command.

Usage: spire_focus({ bead_id: "pan-42" })

Fetches everything an agent needs to act on a bead:
  1. Task details — title, status, priority, description, assignee
  2. Referenced messages — any spire messages that mention this bead (ref:<bead-id> labels)
  3. Comments — all comments on the bead

Output is plain text, not JSON — designed for agent context consumption.
Call this at the start of any work session to recover full context from the bead graph.

This is the primary mechanism for context recovery across session boundaries.
Because all state lives in beads, a fresh agent with no conversation history can
call spire_focus and immediately understand what needs to be done.

Example output:
  --- Task pan-42 ---
  Title: Fix staging deploy pipeline
  Status: in_progress
  Priority: P1
  Description: ...

  --- Messages referencing pan-42 ---
  spi-12  from:rel  "deploy is failing on staging"  [open]

  --- Comments (2) ---
  ...`,
  { bead_id: z.string().describe("Bead ID to focus on (e.g. 'pan-42')") },
  async ({ bead_id }) => {
    // Fetch the bead
    const bead = await bdJson("show", bead_id);
    const lines = [];

    lines.push(`--- Task ${bead.id} ---`);
    lines.push(`Title: ${bead.title}`);
    lines.push(`Status: ${bead.status}`);
    lines.push(`Priority: P${bead.priority}`);
    if (bead.description) lines.push(`Description: ${bead.description}`);
    if (bead.assignee) lines.push(`Assignee: ${bead.assignee}`);
    lines.push("");

    // Check for referenced messages (beads with ref:<bead_id> label)
    try {
      const refs = await bdJson(
        "list",
        "--rig=spi",
        "--label",
        `msg,ref:${bead_id}`,
      );
      if (refs.length > 0) {
        lines.push(`--- Messages referencing ${bead_id} ---`);
        for (const ref of refs) {
          const fromLabel = (ref.labels || []).find((l) => l.startsWith("from:"));
          const from = fromLabel ? fromLabel.slice(5) : "unknown";
          lines.push(`${ref.id}  from:${from}  "${ref.title}"  [${ref.status}]`);
        }
        lines.push("");
      }
    } catch {
      // No refs found
    }

    // Fetch comments
    try {
      const comments = await bdJson("comments", bead_id);
      if (comments.length > 0) {
        lines.push(`--- Comments (${comments.length}) ---`);
        for (const c of comments) {
          lines.push(`${c.author || "unknown"}: ${c.body}`);
        }
        lines.push("");
      }
    } catch {
      // No comments or command not supported
    }

    return { content: [{ type: "text", text: lines.join("\n") }] };
  }
);

// ── spire_roster ─────────────────────────────────────────────────────────────

server.tool(
  "spire_roster",
  `List all registered agents.

Usage: spire_roster()

Shows all agents that have called spire_register and are still active (open bead).
Each entry shows the agent's bead ID, name, and status.

Use this to discover who's available before sending messages.
Agents appear here after spire_register and disappear after spire_unregister.`,
  {},
  async () => {
    try {
      const agents = await bdJson(
        "list",
        "--rig=spi",
        "--label",
        "agent",
        "--status=open"
      );
      if (agents.length === 0) {
        return { content: [{ type: "text", text: "No agents registered" }] };
      }
      const lines = agents.map((a) => {
        const nameLabel = (a.labels || []).find((l) => l.startsWith("name:"));
        const name = nameLabel ? nameLabel.slice(5) : a.title;
        return `${a.id}  ${name}  [${a.status}]`;
      });
      return {
        content: [{ type: "text", text: `Registered agents:\n\n${lines.join("\n")}` }],
      };
    } catch {
      return { content: [{ type: "text", text: "No agents registered" }] };
    }
  }
);

// ─── Start ───────────────────────────────────────────────────────────────────

const transport = new StdioServerTransport();
await server.connect(transport);
