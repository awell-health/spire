package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

// LinearGraphQLURL is the Linear API endpoint.
const LinearGraphQLURL = "https://api.linear.app/graphql"

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

// LinearTeam represents a Linear team.
type LinearTeam struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Key  string `json:"key"`
}

// LinearProject represents a Linear project.
type LinearProject struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// LinearAuthHeader returns the correct Authorization header value.
// Personal API keys (lin_api_*) are sent bare; OAuth tokens get "Bearer " prefix.
func LinearAuthHeader(key string) string {
	if strings.HasPrefix(key, "lin_api_") {
		return key
	}
	if strings.HasPrefix(key, "Bearer ") {
		return key
	}
	return "Bearer " + key
}

// LinearGraphQL executes a GraphQL query against the Linear API and returns the
// raw JSON data payload.
func LinearGraphQL(token, query string, variables map[string]any) (json.RawMessage, error) {
	reqBody, _ := json.Marshal(map[string]any{
		"query":     query,
		"variables": variables,
	})

	req, _ := http.NewRequest("POST", LinearGraphQLURL, bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

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
		Data   json.RawMessage `json:"data"`
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
		return nil, fmt.Errorf("graphql: %s", strings.Join(msgs, ", "))
	}

	return result.Data, nil
}

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

// FetchLinearIssue calls the Linear GraphQL API to fetch an issue by identifier.
func FetchLinearIssue(apiKey, identifier string) (*LinearIssue, error) {
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

	req, err := http.NewRequest("POST", LinearGraphQLURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", LinearAuthHeader(apiKey))

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

// PrintLinearContext prints the Linear-enriched section for a grok output.
func PrintLinearContext(issue *LinearIssue) {
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

// ResolveLinearAPIKey gets the Linear API key.
// Priority: LINEAR_API_KEY env > keychain > bd config (legacy key: "linear-api-key").
func ResolveLinearAPIKey() string {
	if key := os.Getenv("LINEAR_API_KEY"); key != "" {
		return key
	}
	if KeychainGet != nil {
		if key, err := KeychainGet("linear.access-token"); err == nil && key != "" {
			return key
		}
	}
	// Fall back to bd config (legacy)
	return LinearAPIKey()
}

// LinearAPIKey resolves the Linear API key from env or store config.
// Priority: LINEAR_API_KEY env > bd config get linear-api-key.
func LinearAPIKey() string {
	if key := os.Getenv("LINEAR_API_KEY"); key != "" {
		return key
	}
	if StoreGetConfig != nil {
		out, _ := StoreGetConfig("linear-api-key")
		if out != "" {
			return out
		}
	}
	return ""
}

// ResolveLinearTeamID gets the Linear team ID.
// Priority: LINEAR_TEAM_ID env > bd config.
func ResolveLinearTeamID() string {
	if id := os.Getenv("LINEAR_TEAM_ID"); id != "" {
		return id
	}
	if StoreGetConfig != nil {
		out, _ := StoreGetConfig("linear.team-id")
		if out != "" {
			return out
		}
	}
	return ""
}

// ResolveLinearProjectID gets the optional Linear project ID.
func ResolveLinearProjectID() string {
	if id := os.Getenv("LINEAR_PROJECT_ID"); id != "" {
		return id
	}
	if StoreGetConfig != nil {
		out, _ := StoreGetConfig("linear.project-id")
		if out != "" {
			return out
		}
	}
	return ""
}
