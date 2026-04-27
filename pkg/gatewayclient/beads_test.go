package gatewayclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
)

func TestListBeads_EncodesFilterAsQueryParams(t *testing.T) {
	var gotQuery url.Values
	var gotMethod, gotPath string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotQuery = r.URL.Query()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]BeadRecord{
			{ID: "spi-a3f8", Title: "hello", Status: "open", Priority: 1, Type: "task", Labels: []string{"msg"}},
		})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok")
	got, err := c.ListBeads(context.Background(), ListBeadsFilter{
		Status: "open",
		Label:  "msg,to:wizard",
		Prefix: "spi",
		Type:   "task",
	})
	if err != nil {
		t.Fatalf("ListBeads: %v", err)
	}

	if gotMethod != http.MethodGet {
		t.Errorf("method = %q, want GET", gotMethod)
	}
	if gotPath != "/api/v1/beads" {
		t.Errorf("path = %q, want /api/v1/beads", gotPath)
	}
	if gotQuery.Get("status") != "open" {
		t.Errorf("status = %q, want open", gotQuery.Get("status"))
	}
	if gotQuery.Get("label") != "msg,to:wizard" {
		t.Errorf("label = %q, want msg,to:wizard", gotQuery.Get("label"))
	}
	if gotQuery.Get("prefix") != "spi" {
		t.Errorf("prefix = %q, want spi", gotQuery.Get("prefix"))
	}
	if gotQuery.Get("type") != "task" {
		t.Errorf("type = %q, want task", gotQuery.Get("type"))
	}

	want := []BeadRecord{{
		ID: "spi-a3f8", Title: "hello", Status: "open", Priority: 1,
		Type: "task", Labels: []string{"msg"},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("result = %+v, want %+v", got, want)
	}
}

func TestListBeads_EmptyFilterOmitsQueryString(t *testing.T) {
	var gotRawQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRawQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("[]"))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok")
	if _, err := c.ListBeads(context.Background(), ListBeadsFilter{}); err != nil {
		t.Fatalf("ListBeads: %v", err)
	}
	if gotRawQuery != "" {
		t.Errorf("raw query = %q, want empty", gotRawQuery)
	}
}

func TestGetBead_HappyPath(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(BeadRecord{
			ID: "spi-a3f8", Title: "hello", Status: "open", Priority: 1, Type: "task",
		})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok")
	got, err := c.GetBead(context.Background(), "spi-a3f8")
	if err != nil {
		t.Fatalf("GetBead: %v", err)
	}
	if gotPath != "/api/v1/beads/spi-a3f8" {
		t.Errorf("path = %q, want /api/v1/beads/spi-a3f8", gotPath)
	}
	if got.ID != "spi-a3f8" || got.Title != "hello" {
		t.Errorf("bead = %+v", got)
	}
}

func TestGetBead_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"not found"}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok")
	_, err := c.GetBead(context.Background(), "spi-missing")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestCreateBead_SendsJSONBodyAndReturnsID(t *testing.T) {
	var gotMethod, gotPath, gotCT string
	var gotBody CreateBeadInput

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotCT = r.Header.Get("Content-Type")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"id": "spi-new1"})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok")
	in := CreateBeadInput{
		Title:       "hello world",
		Type:        "task",
		Priority:    2,
		Description: "longer body",
		Labels:      []string{"msg", "to:wizard"},
		Parent:      "spi-epic1",
		Prefix:      "spi",
	}
	id, err := c.CreateBead(context.Background(), in)
	if err != nil {
		t.Fatalf("CreateBead: %v", err)
	}
	if id != "spi-new1" {
		t.Errorf("id = %q, want spi-new1", id)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/api/v1/beads" {
		t.Errorf("path = %q, want /api/v1/beads", gotPath)
	}
	if gotCT != "application/json" {
		t.Errorf("content-type = %q, want application/json", gotCT)
	}
	if !reflect.DeepEqual(gotBody, in) {
		t.Errorf("body = %+v, want %+v", gotBody, in)
	}
}

func TestUpdateBead_SendsPatchAndIgnoresResponse(t *testing.T) {
	var gotMethod, gotPath string
	var gotBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"spi-a3f8","status":"updated"}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok")
	err := c.UpdateBead(context.Background(), "spi-a3f8", map[string]any{"status": "closed"})
	if err != nil {
		t.Fatalf("UpdateBead: %v", err)
	}
	if gotMethod != http.MethodPatch {
		t.Errorf("method = %q, want PATCH", gotMethod)
	}
	if gotPath != "/api/v1/beads/spi-a3f8" {
		t.Errorf("path = %q, want /api/v1/beads/spi-a3f8", gotPath)
	}
	if got := gotBody["status"]; got != "closed" {
		t.Errorf("body.status = %v, want closed", got)
	}
}

func TestUpdateBead_Unauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "bad-token")
	err := c.UpdateBead(context.Background(), "spi-a3f8", map[string]any{"status": "closed"})
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("err = %v, want ErrUnauthorized", err)
	}
}

func TestResetBead_PostsBodyAndReturnsBead(t *testing.T) {
	var gotMethod, gotPath string
	var gotBody ResetBeadOpts
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(BeadRecord{
			ID: "spi-a3f8", Status: "open", Labels: []string{"archmage:jb"},
		})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok")
	got, err := c.ResetBead(context.Background(), "spi-a3f8", ResetBeadOpts{
		To:    "review",
		Force: true,
		Set:   map[string]string{"implement.outputs.outcome": "verified"},
	})
	if err != nil {
		t.Fatalf("ResetBead: %v", err)
	}

	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/api/v1/beads/spi-a3f8/reset" {
		t.Errorf("path = %q, want /api/v1/beads/spi-a3f8/reset", gotPath)
	}
	if gotBody.To != "review" || !gotBody.Force {
		t.Errorf("body = %+v, want To=review Force=true", gotBody)
	}
	if got.ID != "spi-a3f8" || got.Status != "open" {
		t.Errorf("response = %+v, want spi-a3f8/open", got)
	}
}

func TestResetBead_HardFlag(t *testing.T) {
	var gotBody ResetBeadOpts
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(BeadRecord{ID: "spi-a3f8", Status: "open"})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok")
	if _, err := c.ResetBead(context.Background(), "spi-a3f8", ResetBeadOpts{Hard: true}); err != nil {
		t.Fatalf("ResetBead: %v", err)
	}
	if !gotBody.Hard {
		t.Errorf("body.Hard = false, want true")
	}
}

func TestResetBead_NotFoundErrorMaps(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":"bead spi-missing: not found"}`)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok")
	_, err := c.ResetBead(context.Background(), "spi-missing", ResetBeadOpts{})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// TestResetBead_ConflictReturnsHTTPError pins the desktop's path for
// surfacing the canonical 409 / 400 messages: a gateway HTTPError keeps
// the body text intact so the UI can render it verbatim instead of a
// generic "reset failed" toast.
func TestResetBead_ConflictReturnsHTTPError(t *testing.T) {
	const want = `{"error":"cannot rewind spi-a3f8 to \"review\": step has not been reached yet (pass --force to advance anyway)"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = io.WriteString(w, want)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok")
	_, err := c.ResetBead(context.Background(), "spi-a3f8", ResetBeadOpts{To: "review"})
	if err == nil {
		t.Fatal("ResetBead: nil error, want HTTPError")
	}
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) {
		t.Fatalf("err = %v, want *HTTPError", err)
	}
	if httpErr.Status != http.StatusConflict {
		t.Errorf("status = %d, want 409", httpErr.Status)
	}
	if !strings.Contains(httpErr.Body, "step has not been reached") {
		t.Errorf("body = %q, want canonical 409 message", httpErr.Body)
	}
}

