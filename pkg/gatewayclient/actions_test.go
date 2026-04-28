package gatewayclient

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGetActions_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/actions" {
			t.Errorf("path = %q, want /api/v1/actions", r.URL.Path)
		}
		if r.Method != http.MethodGet {
			t.Errorf("method = %q, want GET", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"actions":[
				{"name":"resummon","args_schema":{},"destructive":false,"endpoint_path":"/api/v1/beads/{id}/resummon","description":"r"},
				{"name":"dismiss","args_schema":{},"destructive":true,"endpoint_path":"/api/v1/beads/{id}/dismiss","description":"d"}
			]
		}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok")
	out, err := c.GetActions(context.Background())
	if err != nil {
		t.Fatalf("GetActions: %v", err)
	}
	if len(out.Actions) != 2 {
		t.Fatalf("Actions = %d, want 2", len(out.Actions))
	}
	if out.Actions[0].Name != "resummon" {
		t.Errorf("Actions[0].Name = %q, want resummon", out.Actions[0].Name)
	}
	if !out.Actions[1].Destructive {
		t.Errorf("Actions[1].Destructive = false, want true")
	}
}

func TestResummonBead_PostsToCorrectPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %q, want POST", r.Method)
		}
		if r.URL.Path != "/api/v1/beads/spi-abc/resummon" {
			t.Errorf("path = %q, want /api/v1/beads/spi-abc/resummon", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(BeadRecord{ID: "spi-abc", Status: "in_progress"})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok")
	got, err := c.ResummonBead(context.Background(), "spi-abc")
	if err != nil {
		t.Fatalf("ResummonBead: %v", err)
	}
	if got.Status != "in_progress" {
		t.Errorf("Status = %q, want in_progress", got.Status)
	}
}

func TestDismissBead_PostsToCorrectPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/beads/spi-abc/dismiss" {
			t.Errorf("path = %q, want /api/v1/beads/spi-abc/dismiss", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(BeadRecord{ID: "spi-abc", Status: "closed"})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok")
	got, err := c.DismissBead(context.Background(), "spi-abc")
	if err != nil {
		t.Fatalf("DismissBead: %v", err)
	}
	if got.Status != "closed" {
		t.Errorf("Status = %q, want closed", got.Status)
	}
}

func TestUpdateBeadStatus_SendsBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/beads/spi-abc/update_status" {
			t.Errorf("path = %q, want /api/v1/beads/spi-abc/update_status", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		var got UpdateBeadStatusOpts
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if got.To != "open" {
			t.Errorf("To = %q, want open", got.To)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(BeadRecord{ID: "spi-abc", Status: "open"})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok")
	got, err := c.UpdateBeadStatus(context.Background(), "spi-abc", UpdateBeadStatusOpts{To: "open"})
	if err != nil {
		t.Fatalf("UpdateBeadStatus: %v", err)
	}
	if got.Status != "open" {
		t.Errorf("Status = %q, want open", got.Status)
	}
}

func TestUpdateBeadStatus_400Surfaces(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid status transition"}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok")
	_, err := c.UpdateBeadStatus(context.Background(), "spi-abc", UpdateBeadStatusOpts{To: "bogus"})
	if err == nil {
		t.Fatal("UpdateBeadStatus did not return an error on 400 response")
	}
	if !strings.Contains(err.Error(), "400") {
		t.Errorf("err = %v, want mention of 400", err)
	}
}

func TestResetHardBead_PostsToCorrectPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/beads/spi-abc/reset_hard" {
			t.Errorf("path = %q, want /api/v1/beads/spi-abc/reset_hard", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(BeadRecord{ID: "spi-abc", Status: "open"})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok")
	got, err := c.ResetHardBead(context.Background(), "spi-abc")
	if err != nil {
		t.Fatalf("ResetHardBead: %v", err)
	}
	if got.Status != "open" {
		t.Errorf("Status = %q, want open", got.Status)
	}
}

func TestRecoveryCommentRequest_SendsQuestion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/recoveries/spi-abc/comment_request" {
			t.Errorf("path = %q, want /api/v1/recoveries/spi-abc/comment_request", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		var got CommentRequestOpts
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if got.Question != "do I close or escalate?" {
			t.Errorf("Question = %q, want \"do I close or escalate?\"", got.Question)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"id":         "spi-abc",
			"bead":       BeadRecord{ID: "spi-abc", Type: "recovery"},
			"comment_id": "c-1",
		})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok")
	got, err := c.RecoveryCommentRequest(context.Background(), "spi-abc", CommentRequestOpts{
		Question: "do I close or escalate?",
	})
	if err != nil {
		t.Fatalf("RecoveryCommentRequest: %v", err)
	}
	if got.CommentID != "c-1" {
		t.Errorf("CommentID = %q, want c-1", got.CommentID)
	}
	if got.ID != "spi-abc" {
		t.Errorf("ID = %q, want spi-abc", got.ID)
	}
}
