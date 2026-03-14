# Spire Grok Implementation Plan

> **For agentic workers:** REQUIRED: Use superpowers:subagent-driven-development (if subagents available) or superpowers:executing-plans to implement this plan. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a `spire grok` subcommand that does everything `spire focus` does, then enriches the output with live data from the Linear API (status, assignee, description, comments).

**Architecture:** New subcommand in the existing Go binary. Single new file `grok.go` containing the command handler, Linear API client, and output formatting. Shells out to `bd` for bead data (same as focus). Uses `net/http` for Linear GraphQL API calls.

**Tech Stack:** Go 1.26 (stdlib only — net/http, encoding/json), beads CLI (`bd`), Linear GraphQL API

**Spec:** `docs/superpowers/specs/2026-03-13-spire-grok-design.md`

---

## File Structure

```
cmd/spire/
  grok.go        — spire grok subcommand: Linear API client, enriched context output
  main.go        — add "grok" case to switch (one-line change + usage update)
  spire_test.go  — add unit tests for Linear response parsing and API key resolution
```

---

## Chunk 1: Linear API Client and Grok Command

### Task 1: grok.go — command handler, API client, and output

**Files:**
- Create: `cmd/spire/grok.go`

- [ ] **Step 1: Write grok.go**

Create `cmd/spire/grok.go`:

```go
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
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
	out, err := bd("config", "get", "linear-api-key")
	if err == nil && out != "" && !strings.Contains(out, "(not set)") {
		return strings.TrimSpace(out)
	}
	return ""
}

const linearGraphQLURL = "https://api.linear.app/graphql"

const issueByIdentifierQuery = `
query IssueByIdentifier($identifier: String!) {
  issueByIdentifier(identifier: $identifier) {
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
`

// fetchLinearIssue calls the Linear GraphQL API to fetch an issue by identifier.
func fetchLinearIssue(apiKey, identifier string) (*LinearIssue, error) {
	reqBody := struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}{
		Query:     issueByIdentifierQuery,
		Variables: map[string]any{"identifier": identifier},
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
	req.Header.Set("Authorization", apiKey)

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
			IssueByIdentifier *LinearIssue `json:"issueByIdentifier"`
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

	if result.Data.IssueByIdentifier == nil {
		return nil, nil // issue not found
	}

	return result.Data.IssueByIdentifier, nil
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
	if len(args) < 1 {
		return fmt.Errorf("usage: spire grok <bead-id>")
	}
	id := args[0]

	// --- Bead-local context (same as focus) ---

	// 1. Fetch the target bead
	out, err := bd("show", id, "--json")
	if err != nil {
		return fmt.Errorf("grok %s: %w", id, err)
	}
	target, err := parseBead([]byte(out))
	if err != nil {
		return fmt.Errorf("grok %s: parse bead: %w", id, err)
	}

	// 2. Check if a molecule already exists (don't pour — grok is read-only)
	var molID string
	var existingMols []Bead
	_ = bdJSON(&existingMols, "list", "--rig=spi", "--label", fmt.Sprintf("workflow:%s", id), "--status=open")
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
			refOut, refErr := bd("show", refID, "--json")
			if refErr != nil {
				continue
			}
			refBead, refParseErr := parseBead([]byte(refOut))
			if refParseErr == nil {
				fmt.Printf("--- Referenced: %s ---\n", refBead.ID)
				fmt.Printf("Title: %s\n", refBead.Title)
				fmt.Printf("Status: %s\n", refBead.Status)
				if refBead.Description != "" {
					fmt.Printf("Description: %s\n", refBead.Description)
				}
				fmt.Println()
			}
		}
	}

	// Messages that reference this bead
	var referrers []Bead
	_ = bdJSON(&referrers, "list", "--rig=spi", "--label", fmt.Sprintf("msg,ref:%s", id), "--status=open")
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
		parentOut, parentErr := bd("show", target.Parent, "--json")
		if parentErr == nil {
			parentBead, parseErr := parseBead([]byte(parentOut))
			if parseErr == nil {
				fmt.Printf("--- Thread (parent: %s) ---\n", parentBead.ID)
				fmt.Printf("Subject: %s\n", parentBead.Title)

				var siblings []Bead
				_ = bdJSON(&siblings, "children", target.Parent)
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
	}

	// Comments
	var comments []struct {
		Author string `json:"author"`
		Body   string `json:"body"`
	}
	commErr := bdJSON(&comments, "comments", id)
	if commErr == nil && len(comments) > 0 {
		fmt.Printf("--- Comments (%d) ---\n", len(comments))
		for _, c := range comments {
			if c.Author != "" {
				fmt.Printf("[%s]: %s\n", c.Author, c.Body)
			} else {
				fmt.Println(c.Body)
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
```

- [ ] **Step 2: Verify it compiles**

```bash
cd /Users/jb/awell/spire && go build -o /tmp/spire ./cmd/spire
```

Expected: compiles successfully.

---

## Chunk 2: Wire into main.go

### Task 2: Add grok to the command switch and usage

**Files:**
- Modify: `cmd/spire/main.go`

- [ ] **Step 1: Add grok case to switch**

Add between the `"focus"` and `"read"` cases:

```go
	case "grok":
		err = cmdGrok(args)
```

- [ ] **Step 2: Update printUsage**

Add to the usage string after the `focus` line:

```
  grok <bead-id>        Focus + live Linear context (requires LINEAR_API_KEY)
```

- [ ] **Step 3: Build and test basic invocation**

```bash
cd /Users/jb/awell/spire && go build -o /tmp/spire ./cmd/spire

# Test no-arg error
/tmp/spire grok 2>&1
# Expected: "spire: usage: spire grok <bead-id>"

# Test help includes grok
/tmp/spire help 2>&1
# Expected: shows grok command in the list

# Test on a bead without linear: label (should behave like focus)
/tmp/spire grok spi-buk 2>&1
# Expected: same output as focus, no Linear section
```

- [ ] **Step 4: Commit**

```bash
git add cmd/spire/grok.go cmd/spire/main.go
git commit -m "feat(grok): add spire grok subcommand with Linear-enriched context"
```

---

## Chunk 3: Tests

### Task 3: Unit tests for Linear response parsing and API key resolution

**Files:**
- Modify: `cmd/spire/spire_test.go`

- [ ] **Step 1: Add unit tests**

Append to `cmd/spire/spire_test.go`:

```go
// --- Grok / Linear API tests ---

func TestLinearAPIKeyEnv(t *testing.T) {
	os.Setenv("LINEAR_API_KEY", "lin_api_test123")
	defer os.Unsetenv("LINEAR_API_KEY")

	key := linearAPIKey()
	if key != "lin_api_test123" {
		t.Errorf("linearAPIKey() = %q, want %q", key, "lin_api_test123")
	}
}

func TestLinearAPIKeyEmpty(t *testing.T) {
	os.Unsetenv("LINEAR_API_KEY")

	key := linearAPIKey()
	// May return empty or a value from bd config — both are acceptable
	t.Logf("linearAPIKey() = %q (empty is OK if no bd config)", key)
}

func TestParseLinearIssueResponse(t *testing.T) {
	responseJSON := `{
		"data": {
			"issueByIdentifier": {
				"id": "uuid-123",
				"identifier": "AWE-42",
				"title": "Fix auth token refresh",
				"description": "The auth token needs refreshing every 30 minutes.",
				"url": "https://linear.app/awell/issue/AWE-42",
				"priority": 2,
				"priorityLabel": "High",
				"state": {
					"name": "In Progress",
					"type": "started"
				},
				"assignee": {
					"name": "JB",
					"email": "jb@awellhealth.com"
				},
				"labels": {
					"nodes": [
						{"name": "Panels - Design"},
						{"name": "Bug"}
					]
				},
				"comments": {
					"nodes": [
						{
							"body": "Looking into this now",
							"createdAt": "2026-03-12T10:30:00.000Z",
							"user": {"name": "JB"}
						},
						{
							"body": "Priority raised by PM",
							"createdAt": "2026-03-11T08:00:00.000Z",
							"user": {"name": "PM"}
						}
					]
				}
			}
		}
	}`

	var result struct {
		Data struct {
			IssueByIdentifier *LinearIssue `json:"issueByIdentifier"`
		} `json:"data"`
	}

	err := json.Unmarshal([]byte(responseJSON), &result)
	if err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	issue := result.Data.IssueByIdentifier
	if issue == nil {
		t.Fatal("issueByIdentifier is nil")
	}

	if issue.Identifier != "AWE-42" {
		t.Errorf("Identifier = %q, want %q", issue.Identifier, "AWE-42")
	}
	if issue.Title != "Fix auth token refresh" {
		t.Errorf("Title = %q, want %q", issue.Title, "Fix auth token refresh")
	}
	if issue.State.Name != "In Progress" {
		t.Errorf("State.Name = %q, want %q", issue.State.Name, "In Progress")
	}
	if issue.State.Type != "started" {
		t.Errorf("State.Type = %q, want %q", issue.State.Type, "started")
	}
	if issue.Assignee == nil {
		t.Fatal("Assignee is nil")
	}
	if issue.Assignee.Name != "JB" {
		t.Errorf("Assignee.Name = %q, want %q", issue.Assignee.Name, "JB")
	}
	if issue.PriorityLabel != "High" {
		t.Errorf("PriorityLabel = %q, want %q", issue.PriorityLabel, "High")
	}
	if len(issue.Labels.Nodes) != 2 {
		t.Errorf("Labels count = %d, want 2", len(issue.Labels.Nodes))
	}
	if len(issue.Comments.Nodes) != 2 {
		t.Errorf("Comments count = %d, want 2", len(issue.Comments.Nodes))
	}
	if issue.Comments.Nodes[0].User.Name != "JB" {
		t.Errorf("Comment[0].User.Name = %q, want %q", issue.Comments.Nodes[0].User.Name, "JB")
	}
}

func TestParseLinearIssueNotFound(t *testing.T) {
	responseJSON := `{"data": {"issueByIdentifier": null}}`

	var result struct {
		Data struct {
			IssueByIdentifier *LinearIssue `json:"issueByIdentifier"`
		} `json:"data"`
	}

	err := json.Unmarshal([]byte(responseJSON), &result)
	if err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}

	if result.Data.IssueByIdentifier != nil {
		t.Error("expected nil for not-found issue")
	}
}

func TestIntegrationGrokNoLinearLabel(t *testing.T) {
	requireBd(t)

	// Create a task without a linear: label
	taskID, err := bdSilent("create", "--rig=spi", "--type=task", "--title", "Grok test no-linear", "-p", "2")
	if err != nil {
		t.Fatalf("create task error: %v", err)
	}

	// Grok should succeed (output same as focus, no Linear section)
	err = cmdGrok([]string{taskID})
	if err != nil {
		t.Fatalf("grok error: %v", err)
	}

	// Clean up
	bd("close", taskID, "--force")
}
```

- [ ] **Step 2: Run tests**

```bash
cd /Users/jb/awell/spire && go test ./cmd/spire/ -run "TestLinearAPI|TestParseLinearIssue|TestIntegrationGrok" -v
```

Expected: all tests pass.

- [ ] **Step 3: Commit**

```bash
git add cmd/spire/spire_test.go
git commit -m "test(grok): add unit tests for Linear response parsing and API key resolution"
```

---

## Chunk 4: Final verification

### Task 4: End-to-end build and test

- [ ] **Step 1: Build and run all tests**

```bash
cd /Users/jb/awell/spire && go build -o /tmp/spire ./cmd/spire && go test ./cmd/spire/ -v
```

Expected: all tests pass, binary builds.

- [ ] **Step 2: Test grok on a real bead**

```bash
# Without linear: label — should behave like focus
/tmp/spire grok spi-buk 2>&1

# With LINEAR_API_KEY set and a real linear-linked bead (if available)
# LINEAR_API_KEY=lin_api_... /tmp/spire grok <bead-with-linear-label> 2>&1
```

- [ ] **Step 3: Verify help text**

```bash
/tmp/spire help 2>&1
```

Expected: shows grok command in the list.

- [ ] **Step 4: Final commit if any fixes needed**

Only if changes were required during testing:

```bash
git add -A
git commit -m "fix(grok): integration test fixes"
```
