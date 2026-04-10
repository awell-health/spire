package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"

	"github.com/awell-health/spire/pkg/dolt"
	"github.com/awell-health/spire/pkg/store"
	"github.com/steveyegge/beads"
)

// Store function vars — declared for testability (same pattern as doltSQL).
var (
	storeListBeads     = store.ListBeads
	storeGetBead       = store.GetBead
	storeGetChildren   = store.GetChildren
	storeGetComments   = store.GetComments
	storeGetDepsWithMeta = store.GetDepsWithMeta
	storeAddLabel      = store.AddLabel
	storeRemoveLabel   = store.RemoveLabel
	storeUpdateBead    = store.UpdateBead
	storeAddDepTyped   = store.AddDepTyped
	storeCreateBead    = store.CreateBead
	storeCloseBead     = store.CloseBead
	storeAddComment    = store.AddComment
	storeAddDep        = store.AddDep
)

// StewardTools implements ToolExecutor for the steward sidecar.
type StewardTools struct {
	commsDir string
}

func NewStewardTools(commsDir string) *StewardTools {
	return &StewardTools{commsDir: commsDir}
}

// Execute dispatches a tool call by name.
func (t *StewardTools) Execute(name string, input json.RawMessage) (string, error) {
	switch name {
	case "list_beads":
		return t.listBeads(input)
	case "show_bead":
		return t.showBead(input)
	case "update_bead":
		return t.updateBead(input)
	case "create_bead":
		return t.createBead(input)
	case "close_bead":
		return t.closeBead(input)
	case "add_comment":
		return t.addComment(input)
	case "send_message":
		return t.sendMessage(input)
	case "get_roster":
		return t.getRoster(input)
	case "steer_wizard":
		return t.steerWizard(input)
	case "add_dependency":
		return t.addDependency(input)
	case "list_agents_work":
		return t.listAgentsWork(input)
	default:
		return "", fmt.Errorf("unknown tool: %s", name)
	}
}

// ToolDefinitions returns the tool specs for the Anthropic API.
func ToolDefinitions() []apiTool {
	return []apiTool{
		{
			Name:        "list_beads",
			Description: "List beads with optional filters. Returns ID, title, status, priority, labels, and type for each bead. Use this for semantic search — read the titles and descriptions to find beads matching a concept.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"status": map[string]any{
						"type":        "string",
						"description": "Filter by status: open, in_progress, closed",
					},
					"labels": map[string]any{
						"type":        "string",
						"description": "Comma-separated label filter (e.g. 'review-ready' or 'owner:wizard-1')",
					},
					"parent": map[string]any{
						"type":        "string",
						"description": "Filter by parent bead ID (children of this bead)",
					},
				},
			},
		},
		{
			Name:        "show_bead",
			Description: "Get detailed information about a specific bead, including description, comments, labels, dependencies, and children.",
			InputSchema: map[string]any{
				"type":     "object",
				"required": []string{"id"},
				"properties": map[string]any{
					"id": map[string]any{
						"type":        "string",
						"description": "Bead ID (e.g. spi-a3f8)",
					},
				},
			},
		},
		{
			Name:        "update_bead",
			Description: "Update a bead's metadata. Can add/remove labels, change priority, or set parent.",
			InputSchema: map[string]any{
				"type":     "object",
				"required": []string{"id"},
				"properties": map[string]any{
					"id": map[string]any{
						"type":        "string",
						"description": "Bead ID to update",
					},
					"add_labels": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "Labels to add (e.g. ['ref:docs/design.md', 'directive:use-redis'])",
					},
					"remove_labels": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "Labels to remove",
					},
					"priority": map[string]any{
						"type":        "integer",
						"description": "New priority (0=critical, 4=backlog)",
					},
					"parent": map[string]any{
						"type":        "string",
						"description": "New parent bead ID (re-parent under an epic)",
					},
				},
			},
		},
		{
			Name:        "create_bead",
			Description: "Create a new bead (task, bug, feature, epic, or chore).",
			InputSchema: map[string]any{
				"type":     "object",
				"required": []string{"title", "type", "priority"},
				"properties": map[string]any{
					"title": map[string]any{
						"type":        "string",
						"description": "Bead title",
					},
					"type": map[string]any{
						"type":        "string",
						"enum":        []string{"task", "bug", "feature", "epic", "chore"},
						"description": "Bead type",
					},
					"priority": map[string]any{
						"type":        "integer",
						"description": "Priority (0=critical, 4=backlog)",
					},
					"parent": map[string]any{
						"type":        "string",
						"description": "Parent bead ID (optional, for epic children)",
					},
					"description": map[string]any{
						"type":        "string",
						"description": "Description of the work",
					},
					"labels": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "Labels to add",
					},
				},
			},
		},
		{
			Name:        "close_bead",
			Description: "Close a bead (mark as done).",
			InputSchema: map[string]any{
				"type":     "object",
				"required": []string{"id"},
				"properties": map[string]any{
					"id": map[string]any{
						"type":        "string",
						"description": "Bead ID to close",
					},
				},
			},
		},
		{
			Name:        "add_comment",
			Description: "Add a comment to a bead for context or documentation.",
			InputSchema: map[string]any{
				"type":     "object",
				"required": []string{"id", "comment"},
				"properties": map[string]any{
					"id": map[string]any{
						"type":        "string",
						"description": "Bead ID",
					},
					"comment": map[string]any{
						"type":        "string",
						"description": "Comment text",
					},
				},
			},
		},
		{
			Name:        "send_message",
			Description: "Send a message to an agent. Use for assignments, directives, status updates, or coordination.",
			InputSchema: map[string]any{
				"type":     "object",
				"required": []string{"to", "message"},
				"properties": map[string]any{
					"to": map[string]any{
						"type":        "string",
						"description": "Recipient agent name (e.g. wizard-1, artificer)",
					},
					"message": map[string]any{
						"type":        "string",
						"description": "Message text",
					},
					"ref": map[string]any{
						"type":        "string",
						"description": "Reference bead ID (optional)",
					},
					"priority": map[string]any{
						"type":        "integer",
						"description": "Message priority (0=critical, 4=backlog). Default 3.",
					},
				},
			},
		},
		{
			Name:        "get_roster",
			Description: "Get the current roster of agents with their busy/idle status and what they're working on.",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
		{
			Name:        "steer_wizard",
			Description: "Send a steering command to a running wizard's sidecar. Use this to redirect a wizard that's actively working — e.g., inform them of an architectural decision, add context, or change approach. The wizard's sidecar will write the message to /comms/steer for the wizard to pick up.",
			InputSchema: map[string]any{
				"type":     "object",
				"required": []string{"agent", "message"},
				"properties": map[string]any{
					"agent": map[string]any{
						"type":        "string",
						"description": "Wizard agent name to steer",
					},
					"message": map[string]any{
						"type":        "string",
						"description": "Steering message — be specific about what the wizard should do differently",
					},
				},
			},
		},
		{
			Name:        "add_dependency",
			Description: "Add a blocking dependency between beads. The blocker must be completed before the blocked bead becomes ready.",
			InputSchema: map[string]any{
				"type":     "object",
				"required": []string{"blocked", "blocker"},
				"properties": map[string]any{
					"blocked": map[string]any{
						"type":        "string",
						"description": "Bead ID that is blocked (waiting)",
					},
					"blocker": map[string]any{
						"type":        "string",
						"description": "Bead ID that blocks (must complete first)",
					},
				},
			},
		},
		{
			Name:        "list_agents_work",
			Description: "List all in-progress beads with their owners. Shows what each agent is working on, how long they've been at it, and their molecule progress.",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
	}
}

// --- Tool implementations ---

func (t *StewardTools) listBeads(input json.RawMessage) (string, error) {
	var params struct {
		Status string `json:"status"`
		Labels string `json:"labels"`
		Parent string `json:"parent"`
	}
	json.Unmarshal(input, &params)

	// If filtering by parent, use GetChildren for an exact match.
	if params.Parent != "" {
		children, err := storeGetChildren(params.Parent)
		if err != nil {
			return "", fmt.Errorf("list children of %s: %w", params.Parent, err)
		}
		return marshalJSON(children)
	}

	filter := beads.IssueFilter{}
	if params.Status != "" {
		s := store.ParseStatus(params.Status)
		filter.Status = &s
		// When explicitly requesting closed, override the default exclusion.
		if s == beads.StatusClosed {
			filter.ExcludeStatus = nil
		}
	}
	if params.Labels != "" {
		filter.Labels = strings.Split(params.Labels, ",")
		for i := range filter.Labels {
			filter.Labels[i] = strings.TrimSpace(filter.Labels[i])
		}
	}

	results, err := storeListBeads(filter)
	if err != nil {
		return "", fmt.Errorf("list beads: %w", err)
	}
	return marshalJSON(results)
}

// showBeadResult composes the full show output for an LLM tool call.
type showBeadResult struct {
	store.Bead
	Comments     []*beads.Comment                          `json:"comments,omitempty"`
	Dependencies []*beads.IssueWithDependencyMetadata      `json:"dependencies,omitempty"`
	Children     []store.Bead                              `json:"children,omitempty"`
}

func (t *StewardTools) showBead(input json.RawMessage) (string, error) {
	var params struct {
		ID string `json:"id"`
	}
	json.Unmarshal(input, &params)

	bead, err := storeGetBead(params.ID)
	if err != nil {
		return "", fmt.Errorf("show bead %s: %w", params.ID, err)
	}

	comments, _ := storeGetComments(params.ID)
	deps, _ := storeGetDepsWithMeta(params.ID)
	children, _ := storeGetChildren(params.ID)

	result := showBeadResult{
		Bead:         bead,
		Comments:     comments,
		Dependencies: deps,
		Children:     children,
	}
	return marshalJSON(result)
}

func (t *StewardTools) updateBead(input json.RawMessage) (string, error) {
	var params struct {
		ID           string   `json:"id"`
		AddLabels    []string `json:"add_labels"`
		RemoveLabels []string `json:"remove_labels"`
		Priority     *int     `json:"priority"`
		Parent       string   `json:"parent"`
	}
	json.Unmarshal(input, &params)

	for _, l := range params.AddLabels {
		if err := storeAddLabel(params.ID, l); err != nil {
			return "", fmt.Errorf("add label %q to %s: %w", l, params.ID, err)
		}
	}
	for _, l := range params.RemoveLabels {
		if err := storeRemoveLabel(params.ID, l); err != nil {
			return "", fmt.Errorf("remove label %q from %s: %w", l, params.ID, err)
		}
	}
	if params.Priority != nil {
		if err := storeUpdateBead(params.ID, map[string]interface{}{"priority": *params.Priority}); err != nil {
			return "", fmt.Errorf("update priority for %s: %w", params.ID, err)
		}
	}
	if params.Parent != "" {
		if err := storeAddDepTyped(params.ID, params.Parent, string(beads.DepParentChild)); err != nil {
			return "", fmt.Errorf("set parent %s for %s: %w", params.Parent, params.ID, err)
		}
	}

	return fmt.Sprintf("Updated %s", params.ID), nil
}

func (t *StewardTools) createBead(input json.RawMessage) (string, error) {
	var params struct {
		Title       string   `json:"title"`
		Type        string   `json:"type"`
		Priority    int      `json:"priority"`
		Parent      string   `json:"parent"`
		Description string   `json:"description"`
		Labels      []string `json:"labels"`
	}
	json.Unmarshal(input, &params)

	id, err := storeCreateBead(store.CreateOpts{
		Title:       params.Title,
		Description: params.Description,
		Priority:    params.Priority,
		Type:        store.ParseIssueType(params.Type),
		Labels:      params.Labels,
		Parent:      params.Parent,
	})
	if err != nil {
		return "", fmt.Errorf("create bead: %w", err)
	}
	return fmt.Sprintf("Created %s", id), nil
}

func (t *StewardTools) closeBead(input json.RawMessage) (string, error) {
	var params struct {
		ID string `json:"id"`
	}
	json.Unmarshal(input, &params)

	if err := storeCloseBead(params.ID); err != nil {
		return "", fmt.Errorf("close bead %s: %w", params.ID, err)
	}
	return fmt.Sprintf("Closed %s", params.ID), nil
}

func (t *StewardTools) addComment(input json.RawMessage) (string, error) {
	var params struct {
		ID      string `json:"id"`
		Comment string `json:"comment"`
	}
	json.Unmarshal(input, &params)

	if err := storeAddComment(params.ID, params.Comment); err != nil {
		return "", fmt.Errorf("add comment to %s: %w", params.ID, err)
	}
	return fmt.Sprintf("Comment added to %s", params.ID), nil
}

func (t *StewardTools) sendMessage(input json.RawMessage) (string, error) {
	var params struct {
		To       string `json:"to"`
		Message  string `json:"message"`
		Ref      string `json:"ref"`
		Priority *int   `json:"priority"`
	}
	json.Unmarshal(input, &params)

	args := []string{"send", params.To, params.Message, "--as", "steward"}
	if params.Ref != "" {
		args = append(args, "--ref", params.Ref)
	}
	p := 3
	if params.Priority != nil {
		p = *params.Priority
	}
	args = append(args, "-p", fmt.Sprintf("%d", p))

	return runSpire(args...)
}

func (t *StewardTools) getRoster(_ json.RawMessage) (string, error) {
	// Get roster from k8s or bead registrations.
	roster, err := runKubectl("get", "spireagent", "-n", "spire", "-o", "json")
	if err != nil {
		// Fallback: bead-based roster via store API.
		openStatus := beads.StatusOpen
		agents, listErr := storeListBeads(beads.IssueFilter{
			Labels: []string{"agent"},
			Status: &openStatus,
		})
		if listErr != nil {
			return "", fmt.Errorf("list agent beads: %w", listErr)
		}
		rosterJSON, marshalErr := marshalJSON(agents)
		if marshalErr != nil {
			return "", fmt.Errorf("marshal roster: %w", marshalErr)
		}
		roster = rosterJSON
	}

	// Get busy agents via store API.
	inProgressStatus := beads.StatusInProgress
	busyBeads, err := storeListBeads(beads.IssueFilter{
		Status: &inProgressStatus,
	})
	if err != nil {
		return "", fmt.Errorf("list busy beads: %w", err)
	}
	busyJSON, err := marshalJSON(busyBeads)
	if err != nil {
		return "", fmt.Errorf("marshal busy beads: %w", err)
	}

	return fmt.Sprintf("Roster:\n%s\n\nIn-progress work:\n%s", roster, busyJSON), nil
}

func (t *StewardTools) steerWizard(input json.RawMessage) (string, error) {
	var params struct {
		Agent   string `json:"agent"`
		Message string `json:"message"`
	}
	json.Unmarshal(input, &params)

	// In k8s: write to the wizard pod's /comms/control file via kubectl exec.
	// Locally: write directly if we know the comms path.
	controlContent := "STEER:" + params.Message

	// Try k8s first.
	_, err := runKubectl("exec", "-n", "spire",
		"-l", fmt.Sprintf("spire.awell.io/agent=%s", params.Agent),
		"-c", "sidecar", "--",
		"sh", "-c", fmt.Sprintf("echo '%s' > /comms/control", escapeShell(controlContent)))
	if err != nil {
		// Fallback: write locally (for non-k8s development).
		localPath := fmt.Sprintf("/tmp/spire-comms-%s/control", params.Agent)
		if writeErr := os.WriteFile(localPath, []byte(controlContent), 0644); writeErr != nil {
			return "", fmt.Errorf("steer failed (k8s: %v, local: %v)", err, writeErr)
		}
		return fmt.Sprintf("Steered %s locally: %s", params.Agent, params.Message), nil
	}

	return fmt.Sprintf("Steered %s: %s", params.Agent, params.Message), nil
}

func (t *StewardTools) addDependency(input json.RawMessage) (string, error) {
	var params struct {
		Blocked string `json:"blocked"`
		Blocker string `json:"blocker"`
	}
	json.Unmarshal(input, &params)

	if err := storeAddDep(params.Blocked, params.Blocker); err != nil {
		return "", fmt.Errorf("add dep %s→%s: %w", params.Blocked, params.Blocker, err)
	}
	return fmt.Sprintf("Dependency added: %s blocked by %s", params.Blocked, params.Blocker), nil
}

func (t *StewardTools) listAgentsWork(_ json.RawMessage) (string, error) {
	inProgressStatus := beads.StatusInProgress
	results, err := storeListBeads(beads.IssueFilter{
		Status: &inProgressStatus,
	})
	if err != nil {
		return "", fmt.Errorf("list in-progress beads: %w", err)
	}
	return marshalJSON(results)
}

// --- Helpers ---

// marshalJSON serializes a value to a JSON string for LLM tool responses.
func marshalJSON(v any) (string, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("marshal JSON: %w", err)
	}
	return string(data), nil
}

func runSpire(args ...string) (string, error) {
	cmd := exec.Command("spire", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("spire %s: %w\n%s", strings.Join(args, " "), err, stderr.String())
	}
	return strings.TrimSpace(stdout.String()), nil
}

func runKubectl(args ...string) (string, error) {
	cmd := exec.Command("kubectl", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("kubectl %s: %w\n%s", strings.Join(args, " "), err, stderr.String())
	}
	return strings.TrimSpace(stdout.String()), nil
}

func escapeShell(s string) string {
	return strings.ReplaceAll(s, "'", "'\\''")
}

// ensureProjectID reads the local .beads/metadata.json project_id and the
// dolt server's _project_id, then updates the local file if they disagree.
// Called once at startup before the first inbox poll.
func ensureProjectID() {
	metaPath := ".beads/metadata.json"
	data, err := os.ReadFile(metaPath)
	if err != nil {
		log.Printf("[project-id] cannot read %s: %s", metaPath, err)
		return
	}
	var meta map[string]any
	if err := json.Unmarshal(data, &meta); err != nil {
		log.Printf("[project-id] cannot parse %s: %s", metaPath, err)
		return
	}
	localPID, _ := meta["project_id"].(string)
	log.Printf("[project-id] local: %s", localPID)

	dbName := os.Getenv("BEADS_RIG")
	if dbName == "" {
		dbName = "spi"
	}

	out, err := doltSQL(
		fmt.Sprintf("SELECT value FROM metadata WHERE `key`='_project_id'"),
		false, dbName)
	if err != nil {
		log.Printf("[project-id] cannot query server: %s", err)
		return
	}

	// dolt.SQL returns tabular text; the CSV path was using "-r csv" which
	// produces "value\n<actual>". The default output is a table:
	//   +-------+
	//   | value |
	//   +-------+
	//   | <pid> |
	//   +-------+
	// Parse the last non-border, non-header data line as the value.
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) < 2 {
		log.Printf("[project-id] unexpected server response: %s", out)
		return
	}
	// Walk backwards to find the last data line (skip border lines starting with '+').
	serverPID := ""
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" || strings.HasPrefix(line, "+") {
			continue
		}
		// Strip table cell borders: "| value |" → "value"
		serverPID = strings.Trim(line, "| ")
		break
	}
	if serverPID == "" || strings.EqualFold(serverPID, "value") {
		log.Printf("[project-id] no data row in server response: %s", out)
		return
	}
	log.Printf("[project-id] server: %s", serverPID)

	if localPID == serverPID {
		log.Printf("[project-id] aligned")
		return
	}

	log.Printf("[project-id] MISMATCH — updating local %s → %s", localPID, serverPID)
	meta["project_id"] = serverPID
	updated, _ := json.MarshalIndent(meta, "", "  ")
	if err := os.WriteFile(metaPath, updated, 0644); err != nil {
		log.Printf("[project-id] cannot write %s: %s", metaPath, err)
		return
	}
	log.Printf("[project-id] realigned successfully")
}

// doltSQL wraps dolt.SQL from pkg/dolt. Declared as a var for testability.
var doltSQL = doltSQLImpl

func doltSQLImpl(query string, jsonOutput bool, dbName string) (string, error) {
	return dolt.SQL(query, jsonOutput, dbName, nil)
}
