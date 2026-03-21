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

// Session manages a multi-turn conversation with the Anthropic Messages API,
// including tool use. It tracks token usage for context window management.
type Session struct {
	model         string
	system        string
	tools         []apiTool
	messages      []apiMessage
	totalInputTk  int
	totalOutputTk int
	contextLimit  int // max context window tokens (200K for Claude)
	maxTokens     int // max response tokens per call
	toolExec      ToolExecutor
}

// ToolExecutor dispatches tool calls.
type ToolExecutor interface {
	Execute(name string, input json.RawMessage) (string, error)
}

// NewSession creates a fresh conversation session.
func NewSession(model, system string, tools []apiTool, executor ToolExecutor) *Session {
	return &Session{
		model:        model,
		system:       system,
		tools:        tools,
		messages:     nil,
		contextLimit: 200000,
		maxTokens:    4096,
		toolExec:     executor,
	}
}

// RestoreSession creates a session pre-loaded with a checkpoint summary.
func RestoreSession(model, system string, tools []apiTool, executor ToolExecutor, checkpoint string) *Session {
	s := NewSession(model, system, tools, executor)
	if checkpoint != "" {
		s.messages = append(s.messages, userTextMsg(
			"[Session restored from checkpoint. Previous session state:]\n\n"+checkpoint,
		))
		s.messages = append(s.messages, assistantTextMsg(
			"Understood. I've loaded the previous session state and will continue from where I left off.",
		))
	}
	return s
}

// ContextUsage returns the fraction of context window used (0.0–1.0).
// Uses the most recent API call's input tokens as the best estimate of
// current context size, since input_tokens includes the full message history.
func (s *Session) ContextUsage() float64 {
	if s.contextLimit == 0 || s.totalInputTk == 0 {
		return 0
	}
	// Use the last call's input tokens as current context size estimate.
	return float64(s.totalInputTk) / float64(s.contextLimit)
}

// TokenStats returns cumulative token counts.
func (s *Session) TokenStats() (inputTotal, outputTotal int) {
	return s.totalInputTk, s.totalOutputTk
}

// Send adds a user message and runs the conversation to completion,
// executing any tool calls along the way. Returns the final text response.
func (s *Session) Send(userMsg string) (string, error) {
	s.messages = append(s.messages, userTextMsg(userMsg))
	return s.complete()
}

// complete calls the API and handles tool use loops until end_turn.
func (s *Session) complete() (string, error) {
	for {
		resp, err := s.callAPI()
		if err != nil {
			return "", err
		}

		// Track tokens (input_tokens from last call = current context size).
		s.totalInputTk = resp.Usage.InputTokens
		s.totalOutputTk += resp.Usage.OutputTokens

		// Append assistant response to message history.
		s.messages = append(s.messages, apiMessage{
			Role:    "assistant",
			Content: marshalContent(resp.Content),
		})

		// If no tool use, we're done.
		if resp.StopReason != "tool_use" {
			return extractText(resp.Content), nil
		}

		// Execute tool calls and add results.
		var results []contentBlock
		for _, block := range resp.Content {
			if block.Type != "tool_use" {
				continue
			}

			inputJSON, _ := json.Marshal(block.Input)
			result, execErr := s.toolExec.Execute(block.Name, inputJSON)
			if execErr != nil {
				result = fmt.Sprintf("Error: %s", execErr)
			}

			results = append(results, contentBlock{
				Type:      "tool_result",
				ToolUseID: block.ID,
				Content:   truncateResult(result, 50000),
			})
		}

		s.messages = append(s.messages, apiMessage{
			Role:    "user",
			Content: marshalContent(results),
		})
	}
}

// callAPI sends the current conversation to the Anthropic Messages API.
func (s *Session) callAPI() (*apiResponse, error) {
	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	if apiKey == "" {
		return nil, fmt.Errorf("ANTHROPIC_API_KEY not set")
	}

	req := apiRequest{
		Model:     s.model,
		MaxTokens: s.maxTokens,
		System:    s.system,
		Tools:     s.tools,
		Messages:  s.messages,
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-api-key", apiKey)
	httpReq.Header.Set("anthropic-version", "2023-06-01")

	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("API request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("API %d: %s", resp.StatusCode, string(respBody))
	}

	var apiResp apiResponse
	if err := json.Unmarshal(respBody, &apiResp); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}

	if apiResp.Error != nil {
		return nil, fmt.Errorf("API error: %s: %s", apiResp.Error.Type, apiResp.Error.Message)
	}

	return &apiResp, nil
}

// --- API types ---

type apiRequest struct {
	Model     string       `json:"model"`
	MaxTokens int          `json:"max_tokens"`
	System    string       `json:"system,omitempty"`
	Tools     []apiTool    `json:"tools,omitempty"`
	Messages  []apiMessage `json:"messages"`
}

type apiTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"input_schema"`
}

type apiMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type apiResponse struct {
	Content    []contentBlock `json:"content"`
	StopReason string         `json:"stop_reason"`
	Usage      struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

type contentBlock struct {
	Type      string `json:"type"`
	Text      string `json:"text,omitempty"`
	ID        string `json:"id,omitempty"`
	Name      string `json:"name,omitempty"`
	Input     any    `json:"input,omitempty"`
	ToolUseID string `json:"tool_use_id,omitempty"`
	Content   string `json:"content,omitempty"`
}

// --- Message helpers ---

func userTextMsg(text string) apiMessage {
	b, _ := json.Marshal(text)
	return apiMessage{Role: "user", Content: b}
}

func assistantTextMsg(text string) apiMessage {
	b, _ := json.Marshal(text)
	return apiMessage{Role: "assistant", Content: b}
}

func marshalContent(blocks []contentBlock) json.RawMessage {
	b, _ := json.Marshal(blocks)
	return b
}

func extractText(blocks []contentBlock) string {
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func truncateResult(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "\n... (truncated)"
}
