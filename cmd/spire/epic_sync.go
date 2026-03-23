package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/steveyegge/beads"
)

// syncEpicsToLinear finds unsynced epics and creates Linear issues for them.
// Returns the number of epics synced.
func syncEpicsToLinear() int {
	apiKey := resolveLinearAPIKey()
	if apiKey == "" {
		return 0 // no Linear credentials, skip silently
	}

	teamID := resolveLinearTeamID()
	if teamID == "" {
		return 0
	}

	projectID := resolveLinearProjectID()

	// List all epics (both open and closed, so we can close Linear issues too)
	openEpics, err := storeListBeads(beads.IssueFilter{IssueType: issueTypePtr(beads.TypeEpic), Status: statusPtr(beads.StatusOpen)})
	if err != nil {
		log.Printf("[epic-sync] list open epics: %s", err)
		return 0
	}

	closedEpics, _ := storeListBeads(beads.IssueFilter{IssueType: issueTypePtr(beads.TypeEpic), Status: statusPtr(beads.StatusClosed)})

	synced := 0

	// Sync new open epics → create Linear issues
	for _, epic := range openEpics {
		if hasLabel(epic, "linear:") != "" {
			continue
		}

		// Skip molecules (formula instances) — they have IDs like spi-mol-*
		if strings.Contains(epic.ID, "-mol-") {
			continue
		}

		log.Printf("[epic-sync] new epic: %s — %q", epic.ID, epic.Title)

		issue, err := createLinearIssue(apiKey, teamID, projectID, epic)
		if err != nil {
			log.Printf("[epic-sync] failed to sync %s: %s", epic.ID, err)
			continue
		}

		log.Printf("[epic-sync] created Linear issue %s (%s)", issue.Identifier, issue.URL)

		// Add linear: label to bead
		if err = storeAddLabel(epic.ID, fmt.Sprintf("linear:%s", issue.Identifier)); err != nil {
			log.Printf("[epic-sync] label %s: %s", epic.ID, err)
		}

		// Add comment with Linear URL
		if err = storeAddComment(epic.ID, fmt.Sprintf("Linear issue created: %s — %s", issue.Identifier, issue.URL)); err != nil {
			log.Printf("[epic-sync] comment %s: %s", epic.ID, err)
		}

		synced++
	}

	// Close Linear issues for closed beads epics
	closed := closeLinearForClosedEpics(apiKey, teamID, closedEpics)
	synced += closed

	if synced > 0 {
		log.Printf("[epic-sync] synced %d epic(s) to Linear", synced)
	}

	return synced
}

// createLinearIssue creates a Linear issue from a beads epic via GraphQL.
func createLinearIssue(apiKey, teamID, projectID string, epic Bead) (*LinearIssue, error) {
	// Map beads priority (0=highest) to Linear priority (1=urgent, 4=low)
	priorityMap := map[int]int{0: 1, 1: 2, 2: 3, 3: 4, 4: 4}
	linearPriority := 3
	if p, ok := priorityMap[epic.Priority]; ok {
		linearPriority = p
	}

	description := buildLinearDescription(epic)

	mutation := `
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
	`

	input := map[string]any{
		"title":       epic.Title,
		"description": description,
		"teamId":      teamID,
		"priority":    linearPriority,
	}
	if projectID != "" {
		input["projectId"] = projectID
	}

	reqBody, _ := json.Marshal(map[string]any{
		"query":     mutation,
		"variables": map[string]any{"input": input},
	})

	req, _ := http.NewRequest("POST", linearGraphQLURL, bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", linearAuthHeader(apiKey))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("linear API: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("linear API %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		Data struct {
			IssueCreate struct {
				Success bool         `json:"success"`
				Issue   *LinearIssue `json:"issue"`
			} `json:"issueCreate"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}

	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}

	if len(result.Errors) > 0 {
		msgs := make([]string, len(result.Errors))
		for i, e := range result.Errors {
			msgs[i] = e.Message
		}
		return nil, fmt.Errorf("graphql errors: %s", strings.Join(msgs, ", "))
	}

	if !result.Data.IssueCreate.Success || result.Data.IssueCreate.Issue == nil {
		return nil, fmt.Errorf("issue creation failed")
	}

	return result.Data.IssueCreate.Issue, nil
}

func buildLinearDescription(epic Bead) string {
	var lines []string

	if epic.Description != "" {
		lines = append(lines, epic.Description, "")
	}

	lines = append(lines, "---")
	lines = append(lines, fmt.Sprintf("**Beads epic**: `%s`", epic.ID))

	if len(epic.Labels) > 0 {
		lines = append(lines, fmt.Sprintf("**Labels**: %s", strings.Join(epic.Labels, ", ")))
	}

	lines = append(lines, "")
	lines = append(lines,
		"> This issue was auto-created from a beads epic. "+
			"The bead is the source of truth for task structure and dependencies. "+
			"This Linear issue is the source of truth for PM tracking.")

	return strings.Join(lines, "\n")
}

// closeLinearForClosedEpics finds closed beads epics with a linear: label
// and transitions the corresponding Linear issue to "Done" if it isn't already.
func closeLinearForClosedEpics(apiKey, teamID string, closedEpics []Bead) int {
	closed := 0

	// Resolve the "Done" state ID for this team (cached per daemon cycle)
	doneStateID := ""

	for _, epic := range closedEpics {
		identifier := hasLabel(epic, "linear:")
		if identifier == "" {
			continue
		}

		// Check if we already marked this as synced-closed
		if hasLabel(epic, "linear-closed") != "" {
			continue
		}

		// Fetch the Linear issue to check its current state
		issue, err := fetchLinearIssue(apiKey, identifier)
		if err != nil {
			log.Printf("[epic-sync] fetch %s: %s", identifier, err)
			continue
		}
		if issue == nil {
			log.Printf("[epic-sync] %s: Linear issue %s not found, skipping", epic.ID, identifier)
			continue
		}

		// Skip if already in a completed/cancelled state
		if issue.State.Type == "completed" || issue.State.Type == "canceled" {
			// Mark as synced so we don't check again
			storeAddLabel(epic.ID, "linear-closed")
			continue
		}

		// Resolve the Done state if we haven't yet
		if doneStateID == "" {
			doneStateID, err = findDoneStateID(apiKey, teamID)
			if err != nil {
				log.Printf("[epic-sync] could not find Done state: %s", err)
				return closed
			}
		}

		// Transition the issue to Done
		err = updateLinearIssueState(apiKey, issue.ID, doneStateID)
		if err != nil {
			log.Printf("[epic-sync] close %s (%s): %s", epic.ID, identifier, err)
			continue
		}

		// Mark bead so we don't try again
		storeAddLabel(epic.ID, "linear-closed")

		log.Printf("[epic-sync] closed Linear issue %s (epic %s closed)", identifier, epic.ID)
		closed++
	}

	return closed
}

// findDoneStateID fetches the "Done" (completed-type) workflow state for a team.
func findDoneStateID(apiKey, teamID string) (string, error) {
	reqBody, _ := json.Marshal(map[string]any{
		"query": `query($teamId: String!) {
			team(id: $teamId) {
				states { nodes { id name type } }
			}
		}`,
		"variables": map[string]any{"teamId": teamID},
	})

	req, _ := http.NewRequest("POST", linearGraphQLURL, bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", linearAuthHeader(apiKey))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	var result struct {
		Data struct {
			Team struct {
				States struct {
					Nodes []struct {
						ID   string `json:"id"`
						Name string `json:"name"`
						Type string `json:"type"`
					} `json:"nodes"`
				} `json:"states"`
			} `json:"team"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", err
	}

	// Find the first "completed" type state (usually "Done")
	for _, s := range result.Data.Team.States.Nodes {
		if s.Type == "completed" {
			return s.ID, nil
		}
	}

	return "", fmt.Errorf("no completed-type state found for team %s", teamID)
}

// updateLinearIssueState transitions a Linear issue to a new state.
func updateLinearIssueState(apiKey, issueID, stateID string) error {
	reqBody, _ := json.Marshal(map[string]any{
		"query": `mutation($id: String!, $input: IssueUpdateInput!) {
			issueUpdate(id: $id, input: $input) { success }
		}`,
		"variables": map[string]any{
			"id":    issueID,
			"input": map[string]any{"stateId": stateID},
		},
	})

	req, _ := http.NewRequest("POST", linearGraphQLURL, bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", linearAuthHeader(apiKey))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	var result struct {
		Data struct {
			IssueUpdate struct {
				Success bool `json:"success"`
			} `json:"issueUpdate"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return fmt.Errorf("parse response: %w", err)
	}
	if !result.Data.IssueUpdate.Success {
		return fmt.Errorf("issue update failed: %s", string(body))
	}

	return nil
}

// linearAuthHeader returns the correct Authorization header value.
// Personal API keys (lin_api_*) are sent bare; OAuth tokens get "Bearer " prefix.
func linearAuthHeader(key string) string {
	if strings.HasPrefix(key, "lin_api_") {
		return key
	}
	if strings.HasPrefix(key, "Bearer ") {
		return key
	}
	return "Bearer " + key
}

// resolveLinearAPIKey gets the Linear API key.
// Priority: LINEAR_API_KEY env > keychain > bd config.
func resolveLinearAPIKey() string {
	if key := os.Getenv("LINEAR_API_KEY"); key != "" {
		return key
	}
	if key, err := keychainGet("linear.access-token"); err == nil && key != "" {
		return key
	}
	// Fall back to bd config (legacy)
	return linearAPIKey()
}

// resolveLinearTeamID gets the Linear team ID.
// Priority: LINEAR_TEAM_ID env > bd config.
func resolveLinearTeamID() string {
	if id := os.Getenv("LINEAR_TEAM_ID"); id != "" {
		return id
	}
	out, _ := storeGetConfig("linear.team-id")
	if out != "" {
		return out
	}
	return ""
}

// resolveLinearProjectID gets the optional Linear project ID.
func resolveLinearProjectID() string {
	if id := os.Getenv("LINEAR_PROJECT_ID"); id != "" {
		return id
	}
	out, _ := storeGetConfig("linear.project-id")
	if out != "" {
		return out
	}
	return ""
}
