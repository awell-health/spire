// linear-client.js — Thin Linear GraphQL client for the epic agent

const LINEAR_API_URL = "https://api.linear.app/graphql";

export class LinearClient {
  #apiKey;
  #teamId;
  #projectId;

  constructor({ apiKey, teamId, projectId }) {
    if (!apiKey) throw new Error("LINEAR_API_KEY is required");
    if (!teamId) throw new Error("LINEAR_TEAM_ID is required");
    this.#apiKey = apiKey;
    this.#teamId = teamId;
    this.#projectId = projectId || null;
  }

  async #request(query, variables = {}) {
    const res = await fetch(LINEAR_API_URL, {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        Authorization: this.#apiKey,
      },
      body: JSON.stringify({ query, variables }),
    });

    if (!res.ok) {
      const text = await res.text();
      throw new Error(`Linear API error (${res.status}): ${text}`);
    }

    const data = await res.json();

    if (data.errors?.length) {
      throw new Error(
        `Linear GraphQL errors: ${data.errors.map((e) => e.message).join(", ")}`
      );
    }

    return data.data;
  }

  /**
   * Create a Linear issue from a beads epic.
   *
   * @param {object} epic - The beads epic object (from bd list --json)
   * @param {string} epic.id - Bead ID (e.g., "web-a3f8")
   * @param {string} epic.title - Epic title
   * @param {string} [epic.description] - Epic description
   * @param {number} [epic.priority] - Priority (0-4)
   * @returns {object} - { id, identifier, url }
   */
  async createIssueFromEpic(epic) {
    // Map beads priority (0=highest) to Linear priority (1=urgent, 4=low)
    // Beads: 0=P0, 1=P1, 2=P2, 3=P3, 4=P4
    // Linear: 0=none, 1=urgent, 2=high, 3=medium, 4=low
    const priorityMap = { 0: 1, 1: 2, 2: 3, 3: 4, 4: 4 };
    const linearPriority =
      epic.priority != null ? priorityMap[epic.priority] ?? 3 : 3;

    const description = buildDescription(epic);

    const mutation = `
      mutation IssueCreate($input: IssueCreateInput!) {
        issueCreate(input: $input) {
          success
          issue {
            id
            identifier
            url
          }
        }
      }
    `;

    const input = {
      title: epic.title,
      description,
      teamId: this.#teamId,
      priority: linearPriority,
    };

    if (this.#projectId) {
      input.projectId = this.#projectId;
    }

    const data = await this.#request(mutation, { input });

    if (!data.issueCreate?.success) {
      throw new Error("Linear issue creation failed");
    }

    return data.issueCreate.issue;
  }

  /**
   * Add a comment to an existing Linear issue.
   */
  async addComment(issueId, body) {
    const mutation = `
      mutation CommentCreate($input: CommentCreateInput!) {
        commentCreate(input: $input) {
          success
        }
      }
    `;

    await this.#request(mutation, {
      input: { issueId, body },
    });
  }

  /**
   * Verify the API key and team exist.
   */
  async verify() {
    const query = `
      query Team($id: String!) {
        team(id: $id) {
          id
          name
          key
        }
      }
    `;

    const data = await this.#request(query, { id: this.#teamId });

    if (!data.team) {
      throw new Error(`Linear team ${this.#teamId} not found`);
    }

    return data.team;
  }
}

/**
 * Build a Linear issue description from a beads epic.
 */
function buildDescription(epic) {
  const lines = [];

  if (epic.description) {
    lines.push(epic.description);
    lines.push("");
  }

  lines.push("---");
  lines.push(`**Beads epic**: \`${epic.id}\``);

  if (epic.assignee) {
    lines.push(`**Assignee**: ${epic.assignee}`);
  }

  if (epic.labels?.length) {
    lines.push(`**Labels**: ${epic.labels.join(", ")}`);
  }

  lines.push("");
  lines.push(
    "> This issue was auto-created from a beads epic. " +
    "The bead is the source of truth for task structure and dependencies. " +
    "This Linear issue is the source of truth for PM tracking."
  );

  return lines.join("\n");
}
