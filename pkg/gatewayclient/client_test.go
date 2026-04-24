package gatewayclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGetTower_HappyPath(t *testing.T) {
	const wantToken = "test-token-42"
	var gotAuth, gotAccept string
	var gotMethod, gotPath string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotAccept = r.Header.Get("Accept")
		gotMethod = r.Method
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"version":     "v0.44.0",
			"name":        "prod-tower",
			"prefix":      "spi",
			"database":    "spire",
			"deploy_mode": "cluster",
			"dolt_url":    "https://dolt.example.com",
			"archmage":    "jbb",
		})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, wantToken)
	info, err := c.GetTower(context.Background())
	if err != nil {
		t.Fatalf("GetTower: %v", err)
	}

	if gotMethod != http.MethodGet {
		t.Errorf("method = %q, want GET", gotMethod)
	}
	if gotPath != "/api/v1/tower" {
		t.Errorf("path = %q, want /api/v1/tower", gotPath)
	}
	if gotAuth != "Bearer "+wantToken {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Bearer "+wantToken)
	}
	if gotAccept != "application/json" {
		t.Errorf("Accept = %q, want application/json", gotAccept)
	}

	want := TowerInfo{
		Name:     "prod-tower",
		Prefix:   "spi",
		DoltURL:  "https://dolt.example.com",
		Archmage: "jbb",
	}
	if info != want {
		t.Errorf("TowerInfo = %+v, want %+v", info, want)
	}
}

func TestGetTower_Unauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized"})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "bad-token")
	_, err := c.GetTower(context.Background())
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("err = %v, want ErrUnauthorized", err)
	}
}

func TestGetTower_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "not found"})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok")
	_, err := c.GetTower(context.Background())
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestDoJSON_HTTPErrorCarriesStatusAndBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("boom"))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok")
	_, err := c.GetTower(context.Background())
	var he *HTTPError
	if !errors.As(err, &he) {
		t.Fatalf("err = %v (%T), want *HTTPError", err, err)
	}
	if he.Status != http.StatusInternalServerError {
		t.Errorf("Status = %d, want %d", he.Status, http.StatusInternalServerError)
	}
	if !strings.Contains(he.Body, "boom") {
		t.Errorf("Body = %q, want to contain %q", he.Body, "boom")
	}
}

func TestNewClient_TrimsTrailingSlash(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("{}"))
	}))
	defer srv.Close()

	// Trailing slash on baseURL must not double up with leading slash on path.
	c := NewClient(srv.URL+"/", "tok")
	if _, err := c.GetTower(context.Background()); err != nil {
		t.Fatalf("GetTower: %v", err)
	}
	if gotPath != "/api/v1/tower" {
		t.Errorf("path = %q, want /api/v1/tower", gotPath)
	}
}
