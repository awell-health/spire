package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// Review is the structured output from an Opus code review.
type Review struct {
	Verdict string  `json:"verdict"` // "approve", "request_changes", "reject"
	Summary string  `json:"summary"`
	Issues  []Issue `json:"issues,omitempty"`
}

// Issue is a specific problem found during review.
type Issue struct {
	File     string `json:"file"`
	Line     int    `json:"line,omitempty"`
	Severity string `json:"severity"` // "error", "warning"
	Message  string `json:"message"`
}

// tokenUsage tracks API token consumption.
type tokenUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// anthropicRequest is the Messages API request body.
type anthropicRequest struct {
	Model     string          `json:"model"`
	MaxTokens int             `json:"max_tokens"`
	System    string          `json:"system,omitempty"`
	Messages  []anthropicMsg  `json:"messages"`
}

type anthropicMsg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// anthropicResponse is the Messages API response body.
type anthropicResponse struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	Usage struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

const reviewSystemPrompt = `You are a senior staff engineer performing code review. You review diffs against specifications.

Your job is to determine: does this implementation satisfy the specification?

Evaluate:
1. Correctness: Does the code do what the spec says?
2. Completeness: Are all requirements from the spec addressed?
3. Quality: Is the code clean, well-tested, and maintainable?
4. Edge cases: Are error paths and edge cases handled?

Respond ONLY with a JSON object matching this schema:
{
  "verdict": "approve" | "request_changes" | "reject",
  "summary": "1-3 sentence summary of your assessment",
  "issues": [
    {
      "file": "path/to/file",
      "line": 42,
      "severity": "error" or "warning",
      "message": "description of the issue"
    }
  ]
}

Verdicts:
- "approve": Implementation satisfies the spec. Minor style issues are OK to approve.
- "request_changes": Implementation has fixable issues. List them as issues.
- "reject": Fundamental approach is wrong. Needs re-thinking, not just patching.

If there are no issues, use an empty issues array with "approve".`

// callOpusReview sends the diff and spec to Opus for review.
func callOpusReview(model, spec, diff string, child Bead, testOutput string, round int) (*Review, tokenUsage, error) {
	system, user := buildReviewPrompt(spec, diff, child.Title, testOutput, round)

	text, usage, err := callAnthropic(model, system, user)
	if err != nil {
		return nil, usage, fmt.Errorf("opus review: %w", err)
	}

	review, err := parseReview(text)
	if err != nil {
		return nil, usage, fmt.Errorf("parse review: %w", err)
	}

	return review, usage, nil
}

// buildReviewPrompt constructs the system and user messages for the review.
func buildReviewPrompt(spec, diff, title, testOutput string, round int) (string, string) {
	var user strings.Builder

	user.WriteString("## Task\n")
	user.WriteString(title)
	user.WriteString("\n\n")

	if spec != "" {
		user.WriteString("## Specification\n")
		user.WriteString(spec)
		user.WriteString("\n\n")
	}

	user.WriteString("## Diff\n```diff\n")
	// Truncate very large diffs to avoid exceeding context.
	if len(diff) > 500000 {
		user.WriteString(diff[:500000])
		user.WriteString("\n... (diff truncated at 500K characters)\n")
	} else {
		user.WriteString(diff)
	}
	user.WriteString("\n```\n\n")

	if testOutput != "" {
		user.WriteString("## Test Results\n```\n")
		// Truncate test output too.
		if len(testOutput) > 50000 {
			user.WriteString(testOutput[:50000])
			user.WriteString("\n... (output truncated)\n")
		} else {
			user.WriteString(testOutput)
		}
		user.WriteString("\n```\n\n")
	}

	if round > 0 {
		user.WriteString(fmt.Sprintf("## Review Context\nThis is review round %d. The wizard has revised the code based on previous feedback. Focus on whether the previously flagged issues have been addressed.\n", round+1))
	}

	return reviewSystemPrompt, user.String()
}

// callAnthropic sends a request to the Anthropic Messages API.
func callAnthropic(model, system, user string) (string, tokenUsage, error) {
	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	if apiKey == "" {
		return "", tokenUsage{}, fmt.Errorf("ANTHROPIC_API_KEY not set")
	}

	req := anthropicRequest{
		Model:     model,
		MaxTokens: 4096,
		System:    system,
		Messages: []anthropicMsg{
			{Role: "user", Content: user},
		},
	}

	body, err := json.Marshal(req)
	if err != nil {
		return "", tokenUsage{}, fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", bytes.NewReader(body))
	if err != nil {
		return "", tokenUsage{}, fmt.Errorf("create request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", apiKey)
	httpReq.Header.Set("anthropic-version", "2023-06-01")

	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Do(httpReq)
	if err != nil {
		return "", tokenUsage{}, fmt.Errorf("API request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", tokenUsage{}, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != 200 {
		return "", tokenUsage{}, fmt.Errorf("API %d: %s", resp.StatusCode, string(respBody))
	}

	var apiResp anthropicResponse
	if err := json.Unmarshal(respBody, &apiResp); err != nil {
		return "", tokenUsage{}, fmt.Errorf("parse response: %w", err)
	}

	if apiResp.Error != nil {
		return "", tokenUsage{}, fmt.Errorf("API error: %s: %s", apiResp.Error.Type, apiResp.Error.Message)
	}

	usage := tokenUsage{
		InputTokens:  apiResp.Usage.InputTokens,
		OutputTokens: apiResp.Usage.OutputTokens,
	}

	// Extract text from content blocks.
	var text strings.Builder
	for _, block := range apiResp.Content {
		if block.Type == "text" {
			text.WriteString(block.Text)
		}
	}

	return text.String(), usage, nil
}

// parseReview extracts a Review struct from the Opus response text.
// It handles both raw JSON and markdown-wrapped JSON (```json ... ```).
func parseReview(text string) (*Review, error) {
	text = strings.TrimSpace(text)

	// Try direct JSON parse first.
	var review Review
	if err := json.Unmarshal([]byte(text), &review); err == nil {
		if err := validateVerdict(review.Verdict); err == nil {
			return &review, nil
		}
	}

	// Try extracting from markdown code block.
	if idx := strings.Index(text, "```json"); idx >= 0 {
		start := idx + len("```json")
		if end := strings.Index(text[start:], "```"); end >= 0 {
			block := strings.TrimSpace(text[start : start+end])
			if err := json.Unmarshal([]byte(block), &review); err == nil {
				if err := validateVerdict(review.Verdict); err == nil {
					return &review, nil
				}
			}
		}
	}

	// Try finding any JSON object in the text.
	if idx := strings.Index(text, "{"); idx >= 0 {
		// Find the matching closing brace.
		depth := 0
		for i := idx; i < len(text); i++ {
			switch text[i] {
			case '{':
				depth++
			case '}':
				depth--
				if depth == 0 {
					block := text[idx : i+1]
					if err := json.Unmarshal([]byte(block), &review); err == nil {
						if err := validateVerdict(review.Verdict); err == nil {
							return &review, nil
						}
					}
				}
			}
		}
	}

	// Fallback: couldn't parse structured response. Treat as request_changes
	// with the full text as the summary.
	return &Review{
		Verdict: "request_changes",
		Summary: "Could not parse structured review. Raw response: " + truncate(text, 500),
	}, nil
}

func validateVerdict(v string) error {
	switch v {
	case "approve", "request_changes", "reject":
		return nil
	default:
		return fmt.Errorf("invalid verdict: %q", v)
	}
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
