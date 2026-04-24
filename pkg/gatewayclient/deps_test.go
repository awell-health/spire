package gatewayclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestListDeps_HappyPath(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]DepRecord{
			{IssueID: "spi-a3f8", DependsOnID: "spi-epic1", Type: "parent-child"},
			{IssueID: "spi-a3f8", DependsOnID: "spi-b0", Type: "blocks"},
		})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok")
	got, err := c.ListDeps(context.Background(), "spi-a3f8")
	if err != nil {
		t.Fatalf("ListDeps: %v", err)
	}
	if gotMethod != http.MethodGet {
		t.Errorf("method = %q, want GET", gotMethod)
	}
	if gotPath != "/api/v1/beads/spi-a3f8/deps" {
		t.Errorf("path = %q, want /api/v1/beads/spi-a3f8/deps", gotPath)
	}
	want := []DepRecord{
		{IssueID: "spi-a3f8", DependsOnID: "spi-epic1", Type: "parent-child"},
		{IssueID: "spi-a3f8", DependsOnID: "spi-b0", Type: "blocks"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("deps = %+v, want %+v", got, want)
	}
}

func TestListDeps_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"not found"}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok")
	_, err := c.ListDeps(context.Background(), "spi-missing")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestGetBlockedIssues_HappyPath(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"count": 2,
			"ids":   []string{"spi-a", "spi-b"},
		})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok")
	got, err := c.GetBlockedIssues(context.Background())
	if err != nil {
		t.Fatalf("GetBlockedIssues: %v", err)
	}
	if gotMethod != http.MethodGet {
		t.Errorf("method = %q, want GET", gotMethod)
	}
	if gotPath != "/api/v1/beads/blocked" {
		t.Errorf("path = %q, want /api/v1/beads/blocked", gotPath)
	}
	if got.Count != 2 {
		t.Errorf("count = %d, want 2", got.Count)
	}
	if !reflect.DeepEqual(got.IDs, []string{"spi-a", "spi-b"}) {
		t.Errorf("ids = %+v, want [spi-a spi-b]", got.IDs)
	}
}

func TestGetBlockedIssues_Unauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "bad-token")
	_, err := c.GetBlockedIssues(context.Background())
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("err = %v, want ErrUnauthorized", err)
	}
}
