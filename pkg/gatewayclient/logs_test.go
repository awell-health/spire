package gatewayclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestListBeadLogs_HappyPath verifies the request shape and response
// decode of the bead-logs list endpoint. The gateway envelope must
// roundtrip through the typed LogsListResponse without losing the
// links block clients depend on for raw/pretty navigation.
func TestListBeadLogs_HappyPath(t *testing.T) {
	var gotPath, gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path + "?" + r.URL.RawQuery
		gotMethod = r.Method
		_ = json.NewEncoder(w).Encode(LogsListResponse{
			Artifacts: []LogArtifactRecord{
				{
					ID:        "art-1",
					BeadID:    "spi-x",
					AgentName: "wizard-spi-x",
					Role:      "wizard",
					Phase:     "implement",
					Provider:  "claude",
					Stream:    "transcript",
					Sequence:  0,
					Status:    "finalized",
					Links: LogArtifactLinks{
						Raw:    "/api/v1/beads/spi-x/logs/art-1/raw",
						Pretty: "/api/v1/beads/spi-x/logs/art-1/pretty",
					},
				},
			},
			NextCursor: "",
		})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok")
	resp, err := c.ListBeadLogs(context.Background(), "spi-x", "", 0)
	if err != nil {
		t.Fatalf("ListBeadLogs: %v", err)
	}
	if gotMethod != http.MethodGet {
		t.Errorf("method = %q, want GET", gotMethod)
	}
	if !strings.Contains(gotPath, "/api/v1/beads/spi-x/logs") {
		t.Errorf("path = %q, want bead-logs path", gotPath)
	}
	if len(resp.Artifacts) != 1 {
		t.Fatalf("artifacts = %d, want 1", len(resp.Artifacts))
	}
	if resp.Artifacts[0].Links.Raw == "" {
		t.Errorf("raw link missing from decoded response")
	}
}

// TestListBeadLogs_NotFoundMapsToErrNotFound makes sure a 404 from the
// gateway surfaces as the package's typed sentinel so callers can
// distinguish "no such bead" from a transport error.
func TestListBeadLogs_NotFoundMapsToErrNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"bead not found"}`, http.StatusNotFound)
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "tok")

	_, err := c.ListBeadLogs(context.Background(), "spi-nope", "", 0)
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("got %v, want ErrNotFound", err)
	}
}

// TestListAllBeadLogs_WalksCursor exercises the pagination wrapper:
// clients should not have to manage cursor state themselves for the
// common "fetch the whole bead" case.
func TestListAllBeadLogs_WalksCursor(t *testing.T) {
	pages := [][]LogArtifactRecord{
		{{ID: "art-1", BeadID: "spi-y"}, {ID: "art-2", BeadID: "spi-y"}},
		{{ID: "art-3", BeadID: "spi-y"}},
	}
	pageIdx := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		out := LogsListResponse{Artifacts: pages[pageIdx]}
		if pageIdx == 0 {
			out.NextCursor = "next"
		}
		pageIdx++
		_ = json.NewEncoder(w).Encode(out)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok")
	all, err := c.ListAllBeadLogs(context.Background(), "spi-y")
	if err != nil {
		t.Fatalf("ListAllBeadLogs: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("got %d artifacts, want 3 (pagination did not walk both pages)", len(all))
	}
}

// TestFetchBeadLogRaw_StreamsBytes verifies the byte-stream fetch path
// surfaces the gateway's response body verbatim.
func TestFetchBeadLogRaw_StreamsBytes(t *testing.T) {
	body := []byte(`{"type":"system","subtype":"init"}` + "\n")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok")
	got, err := c.FetchBeadLogRaw(context.Background(), "spi-x", "art-1", false)
	if err != nil {
		t.Fatalf("FetchBeadLogRaw: %v", err)
	}
	if string(got) != string(body) {
		t.Errorf("got bytes %q, want %q", got, body)
	}
}

// TestFetchBeadLogRaw_AsEngineerSetsHeader confirms engineer-scope reads
// stamp the X-Spire-Scope header, which is what the gateway's
// scopeFromRequest helper keys on.
func TestFetchBeadLogRaw_AsEngineerSetsHeader(t *testing.T) {
	var gotScope string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotScope = r.Header.Get("X-Spire-Scope")
		_, _ = io.WriteString(w, "ok")
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok")
	if _, err := c.FetchBeadLogRaw(context.Background(), "spi-x", "art-1", true); err != nil {
		t.Fatalf("FetchBeadLogRaw: %v", err)
	}
	if gotScope != "engineer" {
		t.Errorf("X-Spire-Scope = %q, want engineer", gotScope)
	}
}

// TestFollowBeadLogs_RoundTrip exercises the follow endpoint client.
// The server response decodes into a FollowResponse, the request URL
// stamps follow=true and any cursor the caller provides, and the
// returned cursor + done flag round-trip unchanged.
func TestFollowBeadLogs_RoundTrip(t *testing.T) {
	var gotURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURL = r.URL.Path + "?" + r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(FollowResponse{
			Events: []LogEvent{
				{ArtifactID: "log-a", Sequence: 0, Stream: "transcript", Line: "alpha"},
				{ArtifactID: "log-a", Sequence: 6, Stream: "transcript", Line: "beta"},
			},
			Cursor: "next-cursor",
			Done:   false,
		})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok")
	resp, err := c.FollowBeadLogs(context.Background(), "spi-x", "prev-cursor")
	if err != nil {
		t.Fatalf("FollowBeadLogs: %v", err)
	}
	if !strings.Contains(gotURL, "follow=true") {
		t.Errorf("URL %q missing follow=true", gotURL)
	}
	if !strings.Contains(gotURL, "cursor=prev-cursor") {
		t.Errorf("URL %q missing cursor=prev-cursor", gotURL)
	}
	if len(resp.Events) != 2 || resp.Events[0].Line != "alpha" {
		t.Errorf("events = %+v, want alpha+beta", resp.Events)
	}
	if resp.Cursor != "next-cursor" || resp.Done {
		t.Errorf("cursor/done = %q,%v want next-cursor,false", resp.Cursor, resp.Done)
	}
}

// TestFollowBeadLogs_DoneTerminatesPolling guards the contract that a
// done=true response signals the client to stop polling. The cursor
// may or may not be set on a done response.
func TestFollowBeadLogs_DoneTerminatesPolling(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(FollowResponse{
			Events: []LogEvent{},
			Done:   true,
		})
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "tok")
	resp, err := c.FollowBeadLogs(context.Background(), "spi-x", "")
	if err != nil {
		t.Fatalf("FollowBeadLogs: %v", err)
	}
	if !resp.Done {
		t.Errorf("done = false, want true")
	}
}

// TestFollowBeadLogs_NotFoundMapsToErrNotFound matches the 404
// behavior of ListBeadLogs so error handling stays uniform across
// the bead-logs surface.
func TestFollowBeadLogs_NotFoundMapsToErrNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"bead not found"}`, http.StatusNotFound)
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "tok")
	_, err := c.FollowBeadLogs(context.Background(), "spi-nope", "")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("got %v, want ErrNotFound", err)
	}
}

// TestFetchBeadLogRaw_404MapsToErrNotFound and 410 (artifact bytes
// removed by lifecycle) also surface as ErrNotFound — both mean
// "you can't read this artifact" and the caller treats them the same.
func TestFetchBeadLogRaw_404Or410MapToErrNotFound(t *testing.T) {
	cases := []int{http.StatusNotFound, http.StatusGone}
	for _, code := range cases {
		t.Run(http.StatusText(code), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, `{"error":"not found"}`, code)
			}))
			defer srv.Close()
			c := NewClient(srv.URL, "tok")
			_, err := c.FetchBeadLogRaw(context.Background(), "spi-x", "art-1", false)
			if !errors.Is(err, ErrNotFound) {
				t.Errorf("got %v, want ErrNotFound", err)
			}
		})
	}
}
