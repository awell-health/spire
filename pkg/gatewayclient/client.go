// Package gatewayclient is the HTTPS client the local spire CLI uses to
// talk to a remote Spire gateway (pkg/gateway). A tower in gateway mode
// exposes /api/v1/* over TLS with bearer-token auth; every laptop-side
// mutation (file a bead, send a message, query deps) tunnels through
// this client instead of hitting Dolt directly.
//
// This file is the scaffold: the Client type, the shared doJSON helper,
// typed errors, and one worked example (GetTower). Follow-on files
// (beads.go, messages.go, deps.go) add endpoint methods on *Client and
// MUST delegate to doJSON rather than rolling their own request code.
package gatewayclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client talks to a Spire gateway over HTTPS with a static bearer token.
// Construct via NewClient; the zero value is not usable.
type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

// TowerInfo is the subset of the gateway's /api/v1/tower response the
// CLI needs to verify tower identity during attach-cluster. The gateway
// may include additional fields (version, database, deploy_mode); they
// are ignored here.
type TowerInfo struct {
	Name     string `json:"name"`
	Prefix   string `json:"prefix"`
	DoltURL  string `json:"dolt_url"`
	Archmage string `json:"archmage"`
}

// Sentinel errors callers can match with errors.Is. ErrUnauthorized is
// returned for 401 responses (bad/missing bearer token); ErrNotFound
// for 404. Any other non-2xx status yields an *HTTPError.
var (
	ErrUnauthorized = errors.New("gatewayclient: unauthorized")
	ErrNotFound     = errors.New("gatewayclient: not found")
)

// HTTPError carries the raw status code and response body for non-2xx
// responses that don't map to a sentinel. The body is truncated to a
// few KB so error strings stay sane if the server returns HTML.
type HTTPError struct {
	Status int
	Body   string
}

func (e *HTTPError) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("gatewayclient: HTTP %d", e.Status)
	}
	return fmt.Sprintf("gatewayclient: HTTP %d: %s", e.Status, e.Body)
}

// NewClient returns a Client pointing at the given gateway base URL
// (e.g. "https://spire.example.com") with the supplied bearer token.
// The returned *http.Client has a 30s overall timeout and uses the
// system default TLS roots; pass the returned value unchanged unless
// you need custom transport config.
func NewClient(url, token string) *Client {
	return &Client{
		baseURL: strings.TrimRight(url, "/"),
		token:   token,
		http: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// GetTower calls GET /api/v1/tower and returns the remote tower's
// identity. Used by `spire tower attach-cluster` to verify the
// --tower name before persisting config.
func (c *Client) GetTower(ctx context.Context) (TowerInfo, error) {
	var out TowerInfo
	if err := c.doJSON(ctx, http.MethodGet, "/api/v1/tower", nil, &out); err != nil {
		return TowerInfo{}, err
	}
	return out, nil
}

// doJSON is the shared request engine: encodes body as JSON (nil means
// no body), sets Authorization/Content-Type/Accept, executes the
// request against c.baseURL+path, and decodes a JSON response into out
// (nil means discard). Non-2xx responses are mapped to ErrUnauthorized
// (401), ErrNotFound (404), or *HTTPError.
func (c *Client) doJSON(ctx context.Context, method, path string, body any, out any) error {
	var reqBody io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("gatewayclient: encode body: %w", err)
		}
		reqBody = bytes.NewReader(buf)
	}

	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reqBody)
	if err != nil {
		return fmt.Errorf("gatewayclient: build request: %w", err)
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("gatewayclient: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		switch resp.StatusCode {
		case http.StatusUnauthorized:
			return ErrUnauthorized
		case http.StatusNotFound:
			return ErrNotFound
		}
		return &HTTPError{Status: resp.StatusCode, Body: strings.TrimSpace(string(bodyBytes))}
	}

	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("gatewayclient: decode response: %w", err)
	}
	return nil
}
