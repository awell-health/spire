package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
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

	args := []string{"list", "--json"}
	if params.Status != "" {
		args = append(args, "--status="+params.Status)
	}
	if params.Labels != "" {
		args = append(args, "--label", params.Labels)
	}

	out, err := runBD(args...)
	if err != nil {
		return "", err
	}

	// If filtering by parent, do client-side filtering.
	if params.Parent != "" && out != "" {
		var beads []json.RawMessage
		if json.Unmarshal([]byte(out), &beads) == nil {
			var filtered []json.RawMessage
			for _, b := range beads {
				var bead struct {
					ID string `json:"id"`
				}
				json.Unmarshal(b, &bead)
				if strings.HasPrefix(bead.ID, params.Parent+".") {
					filtered = append(filtered, b)
				}
			}
			result, _ := json.Marshal(filtered)
			return string(result), nil
		}
	}

	return out, nil
}

func (t *StewardTools) showBead(input json.RawMessage) (string, error) {
	var params struct {
		ID string `json:"id"`
	}
	json.Unmarshal(input, &params)
	return runBD("show", params.ID, "--json")
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

	args := []string{"update", params.ID}
	for _, l := range params.AddLabels {
		args = append(args, "--add-label", l)
	}
	for _, l := range params.RemoveLabels {
		args = append(args, "--remove-label", l)
	}
	if params.Priority != nil {
		args = append(args, "-p", fmt.Sprintf("%d", *params.Priority))
	}
	if params.Parent != "" {
		args = append(args, "--parent", params.Parent)
	}

	return runBD(args...)
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

	args := []string{"create", params.Title, "-t", params.Type, "-p", fmt.Sprintf("%d", params.Priority)}
	if params.Parent != "" {
		args = append(args, "--parent", params.Parent)
	}
	if params.Description != "" {
		args = append(args, "--description", params.Description)
	}
	if len(params.Labels) > 0 {
		args = append(args, "--labels", strings.Join(params.Labels, ","))
	}

	return runBD(args...)
}

func (t *StewardTools) closeBead(input json.RawMessage) (string, error) {
	var params struct {
		ID string `json:"id"`
	}
	json.Unmarshal(input, &params)
	return runBD("close", params.ID)
}

func (t *StewardTools) addComment(input json.RawMessage) (string, error) {
	var params struct {
		ID      string `json:"id"`
		Comment string `json:"comment"`
	}
	json.Unmarshal(input, &params)
	return runBD("comments", "add", params.ID, params.Comment)
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
		// Fallback: bead-based roster.
		roster, err = runBD("list", "--label", "agent", "--status=open", "--json")
		if err != nil {
			return "", err
		}
	}

	// Get busy agents.
	busy, _ := runBD("list", "--status=in_progress", "--json")

	return fmt.Sprintf("Roster:\n%s\n\nIn-progress work:\n%s", roster, busy), nil
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
	return runBD("dep", "add", params.Blocked, params.Blocker)
}

func (t *StewardTools) listAgentsWork(_ json.RawMessage) (string, error) {
	return runBD("list", "--status=in_progress", "--json")
}

// --- Command helpers ---

func runBD(args ...string) (string, error) {
	cmd := exec.Command("bd", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("bd %s: %w\n%s", strings.Join(args, " "), err, stderr.String())
	}
	return strings.TrimSpace(stdout.String()), nil
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
