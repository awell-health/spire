package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/steveyegge/beads"
)

// LinearIssue represents a Linear issue fetched via GraphQL.
type LinearIssue struct {
	ID            string `json:"id"`
	Identifier    string `json:"identifier"`
	Title         string `json:"title"`
	Description   string `json:"description"`
	URL           string `json:"url"`
	Priority      int    `json:"priority"`
	PriorityLabel string `json:"priorityLabel"`
	State         struct {
		Name string `json:"name"`
		Type string `json:"type"`
	} `json:"state"`
	Assignee *struct {
		Name  string `json:"name"`
		Email string `json:"email"`
	} `json:"assignee"`
	Labels struct {
		Nodes []struct {
			Name string `json:"name"`
		} `json:"nodes"`
	} `json:"labels"`
	Comments struct {
		Nodes []struct {
			Body      string `json:"body"`
			CreatedAt string `json:"createdAt"`
			User      *struct {
				Name string `json:"name"`
			} `json:"user"`
		} `json:"nodes"`
	} `json:"comments"`
}

// linearAPIKey resolves the Linear API key.
// Priority: LINEAR_API_KEY env > bd config get linear-api-key.
func linearAPIKey() string {
	if key := os.Getenv("LINEAR_API_KEY"); key != "" {
		return key
	}
	out, _ := storeGetConfig("linear-api-key")
	if out != "" {
		return out
	}
	return ""
}

const linearGraphQLURL = "https://api.linear.app/graphql"

const issueSearchQuery = `
query IssueSearch($term: String!) {
  searchIssues(term: $term, first: 1) {
    nodes {
      id
      identifier
      title
      description
      url
      priority
      priorityLabel
      state {
        name
        type
      }
      assignee {
        name
        email
      }
      labels {
        nodes {
          name
        }
      }
      comments {
        nodes {
          body
          createdAt
          user {
            name
          }
        }
      }
    }
  }
}
`

// fetchLinearIssue calls the Linear GraphQL API to fetch an issue by identifier.
func fetchLinearIssue(apiKey, identifier string) (*LinearIssue, error) {
	reqBody := struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}{
		Query:     issueSearchQuery,
		Variables: map[string]any{"term": identifier},
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequest("POST", linearGraphQLURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", linearAuthHeader(apiKey))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("linear API request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("linear API error (%d): %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		Data struct {
			SearchIssues struct {
				Nodes []LinearIssue `json:"nodes"`
			} `json:"searchIssues"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}

	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}

	if len(result.Errors) > 0 {
		msgs := make([]string, len(result.Errors))
		for i, e := range result.Errors {
			msgs[i] = e.Message
		}
		return nil, fmt.Errorf("linear GraphQL errors: %s", strings.Join(msgs, ", "))
	}

	// Verify exact identifier match (search may return fuzzy results)
	for _, issue := range result.Data.SearchIssues.Nodes {
		if issue.Identifier == identifier {
			return &issue, nil
		}
	}

	return nil, nil // not found
}

// printLinearContext prints the Linear-enriched section for a grok output.
func printLinearContext(issue *LinearIssue) {
	fmt.Printf("--- Linear: %s ---\n", issue.Identifier)
	fmt.Printf("URL: %s\n", issue.URL)
	fmt.Printf("Status: %s (%s)\n", issue.State.Name, issue.State.Type)
	if issue.Assignee != nil {
		fmt.Printf("Assignee: %s (%s)\n", issue.Assignee.Name, issue.Assignee.Email)
	}
	fmt.Printf("Priority: %s\n", issue.PriorityLabel)

	// Labels
	if len(issue.Labels.Nodes) > 0 {
		names := make([]string, len(issue.Labels.Nodes))
		for i, l := range issue.Labels.Nodes {
			names[i] = l.Name
		}
		fmt.Printf("Labels: %s\n", strings.Join(names, ", "))
	}

	// Description
	if issue.Description != "" {
		fmt.Println("Description:")
		for _, line := range strings.Split(issue.Description, "\n") {
			fmt.Printf("  %s\n", line)
		}
	}
	fmt.Println()

	// Comments
	if len(issue.Comments.Nodes) > 0 {
		fmt.Printf("--- Linear Comments (%d) ---\n", len(issue.Comments.Nodes))
		for _, c := range issue.Comments.Nodes {
			user := "Unknown"
			if c.User != nil {
				user = c.User.Name
			}
			// Truncate createdAt to date only
			date := c.CreatedAt
			if len(date) > 10 {
				date = date[:10]
			}
			fmt.Printf("[%s, %s]: %s\n", user, date, c.Body)
		}
		fmt.Println()
	}
}

func cmdGrok(args []string) error {
	if err := requireDolt(); err != nil {
		return err
	}

	if len(args) < 1 {
		return fmt.Errorf("usage: spire grok <bead-id>")
	}
	id := args[0]

	// --- Bead-local context (same as focus) ---

	// 1. Fetch the target bead
	target, err := storeGetBead(id)
	if err != nil {
		return fmt.Errorf("grok %s: %w", id, err)
	}

	// 2. Check if a molecule already exists (don't pour — grok is read-only)
	var molID string
	existingMols, _ := storeListBeads(beads.IssueFilter{IDPrefix: "spi-", Labels: []string{"workflow:" + id}, Status: statusPtr(beads.StatusOpen)})
	if len(existingMols) > 0 {
		molID = existingMols[0].ID
	}

	// 3. Get molecule progress if available
	var progressOut string
	if molID != "" {
		progressOut, _ = bd("mol", "progress", molID)
	}

	// 4. Assemble bead-local output
	fmt.Printf("--- Task %s ---\n", target.ID)
	fmt.Printf("Title: %s\n", target.Title)
	fmt.Printf("Status: %s\n", target.Status)
	fmt.Printf("Priority: P%d\n", target.Priority)
	if target.Description != "" {
		fmt.Printf("Description: %s\n", target.Description)
	}
	fmt.Println()

	// Workflow progress
	if progressOut != "" {
		fmt.Println("--- Workflow (spire-agent-work) ---")
		fmt.Println(progressOut)
		fmt.Println()
	}

	// Referenced beads (from ref: labels)
	for _, l := range target.Labels {
		if strings.HasPrefix(l, "ref:") {
			refID := l[4:]
			refBead, refErr := storeGetBead(refID)
			if refErr != nil {
				continue
			}
			fmt.Printf("--- Referenced: %s ---\n", refBead.ID)
			fmt.Printf("Title: %s\n", refBead.Title)
			fmt.Printf("Status: %s\n", refBead.Status)
			if refBead.Description != "" {
				fmt.Printf("Description: %s\n", refBead.Description)
			}
			fmt.Println()
		}
	}

	// Messages that reference this bead
	referrers, _ := storeListBeads(beads.IssueFilter{IDPrefix: "spi-", Labels: []string{"msg", "ref:" + id}, Status: statusPtr(beads.StatusOpen)})
	for _, m := range referrers {
		from := hasLabel(m, "from:")
		fmt.Printf("--- Referenced by %s ---\n", m.ID)
		if from != "" {
			fmt.Printf("From: %s\n", from)
		}
		fmt.Printf("Subject: %s\n", m.Title)
		fmt.Println()
	}

	// Thread context (parent + siblings)
	if target.Parent != "" {
		parentBead, parentErr := storeGetBead(target.Parent)
		if parentErr == nil {
			fmt.Printf("--- Thread (parent: %s) ---\n", parentBead.ID)
			fmt.Printf("Subject: %s\n", parentBead.Title)

			siblings, _ := storeGetChildren(target.Parent)
			for _, s := range siblings {
				if s.ID == target.ID {
					continue
				}
				from := hasLabel(s, "from:")
				fmt.Printf("  %s [%s]: %s\n", s.ID, from, s.Title)
			}
			fmt.Println()
		}
	}

	// Comments
	comments, commErr := storeGetComments(id)
	if commErr == nil && len(comments) > 0 {
		fmt.Printf("--- Comments (%d) ---\n", len(comments))
		for _, c := range comments {
			if c.Author != "" {
				fmt.Printf("[%s]: %s\n", c.Author, c.Text)
			} else {
				fmt.Println(c.Text)
			}
		}
		fmt.Println()
	}

	// --- Linear enrichment ---

	linearID := hasLabel(target, "linear:")
	if linearID == "" {
		// No linear: label — nothing to enrich, done.
		return nil
	}

	apiKey := linearAPIKey()
	if apiKey == "" {
		fmt.Fprintf(os.Stderr, "spire: warning: LINEAR_API_KEY not set, skipping Linear enrichment\n")
		return nil
	}

	issue, err := fetchLinearIssue(apiKey, linearID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "spire: warning: Linear API error: %s\n", err)
		return nil
	}

	if issue == nil {
		fmt.Fprintf(os.Stderr, "spire: warning: Linear issue %s not found\n", linearID)
		return nil
	}

	printLinearContext(issue)

	return nil
}
