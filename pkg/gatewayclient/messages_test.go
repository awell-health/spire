package gatewayclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestListMessages_PassesToFilter(t *testing.T) {
	var gotTo string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTo = r.URL.Query().Get("to")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]BeadRecord{
			{ID: "spi-msg1", Title: "ping", Status: "open", Type: "message", Labels: []string{"msg", "to:wizard"}},
		})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok")
	got, err := c.ListMessages(context.Background(), "wizard")
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if gotTo != "wizard" {
		t.Errorf("to = %q, want wizard", gotTo)
	}
	if len(got) != 1 || got[0].ID != "spi-msg1" {
		t.Errorf("messages = %+v", got)
	}
}

func TestListMessages_EmptyToOmitsQuery(t *testing.T) {
	var gotRaw string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRaw = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("[]"))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok")
	if _, err := c.ListMessages(context.Background(), ""); err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if gotRaw != "" {
		t.Errorf("raw query = %q, want empty", gotRaw)
	}
}

func TestSendMessage_SendsBodyAndReturnsID(t *testing.T) {
	var gotMethod, gotPath string
	var gotBody SendMessageInput

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"id": "spi-msg99"})
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok")
	in := SendMessageInput{
		To:       "wizard",
		Message:  "hello",
		From:     "apprentice",
		Ref:      "spi-a3f8",
		Priority: 2,
	}
	id, err := c.SendMessage(context.Background(), in)
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if id != "spi-msg99" {
		t.Errorf("id = %q, want spi-msg99", id)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/api/v1/messages" {
		t.Errorf("path = %q, want /api/v1/messages", gotPath)
	}
	if gotBody.To != "wizard" || gotBody.Message != "hello" || gotBody.From != "apprentice" ||
		gotBody.Ref != "spi-a3f8" || gotBody.Priority != 2 {
		t.Errorf("body = %+v", gotBody)
	}
}

func TestMarkMessageRead_PostsToReadPath(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"spi-msg1","status":"read"}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok")
	if err := c.MarkMessageRead(context.Background(), "spi-msg1"); err != nil {
		t.Fatalf("MarkMessageRead: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/api/v1/messages/spi-msg1/read" {
		t.Errorf("path = %q, want /api/v1/messages/spi-msg1/read", gotPath)
	}
}

func TestSendMessage_Unauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "bad-token")
	_, err := c.SendMessage(context.Background(), SendMessageInput{To: "wizard", Message: "hi"})
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("err = %v, want ErrUnauthorized", err)
	}
}
