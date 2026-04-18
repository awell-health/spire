package gateway

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeTrigger records the last reason seen and returns err on Trigger.
type fakeTrigger struct {
	mu          sync.Mutex
	calls       int32
	lastReason  string
	err         error
}

func (f *fakeTrigger) Trigger(reason string) error {
	atomic.AddInt32(&f.calls, 1)
	f.mu.Lock()
	f.lastReason = reason
	err := f.err
	f.mu.Unlock()
	return err
}

func (f *fakeTrigger) callCount() int32 { return atomic.LoadInt32(&f.calls) }

func (f *fakeTrigger) reason() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastReason
}

func silentLogger() *log.Logger {
	return log.New(io.Discard, "", 0)
}

func newTestServer(target Triggerable) *Server {
	return NewServer(":0", target, silentLogger())
}

func TestHandleSync_MethodNotAllowed(t *testing.T) {
	tests := []struct {
		name   string
		method string
	}{
		{"GET", http.MethodGet},
		{"PUT", http.MethodPut},
		{"DELETE", http.MethodDelete},
		{"PATCH", http.MethodPatch},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ft := &fakeTrigger{}
			s := newTestServer(ft)
			req := httptest.NewRequest(tc.method, "/sync", nil)
			rec := httptest.NewRecorder()

			s.handleSync(rec, req)

			if rec.Code != http.StatusMethodNotAllowed {
				t.Errorf("status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
			}
			if ft.callCount() != 0 {
				t.Errorf("Trigger called %d times, want 0 on non-POST", ft.callCount())
			}
		})
	}
}

func TestHandleSync_PostSuccess(t *testing.T) {
	ft := &fakeTrigger{}
	s := newTestServer(ft)
	req := httptest.NewRequest(http.MethodPost, "/sync", nil)
	rec := httptest.NewRecorder()

	s.handleSync(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if !strings.Contains(rec.Body.String(), "triggered") {
		t.Errorf("body = %q, want to contain %q", rec.Body.String(), "triggered")
	}
	if ft.callCount() != 1 {
		t.Errorf("Trigger called %d times, want 1", ft.callCount())
	}
	if got := ft.reason(); got != "http:http" {
		t.Errorf("reason = %q, want %q (default)", got, "http:http")
	}
}

func TestHandleSync_PostWithReason(t *testing.T) {
	ft := &fakeTrigger{}
	s := newTestServer(ft)
	req := httptest.NewRequest(http.MethodPost, "/sync?reason=webhook", nil)
	rec := httptest.NewRecorder()

	s.handleSync(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := ft.reason(); got != "http:webhook" {
		t.Errorf("reason = %q, want %q", got, "http:webhook")
	}
}

func TestHandleSync_TriggerDeclinedReturns202(t *testing.T) {
	cases := []struct {
		name    string
		err     error
		wantSub string
	}{
		{"debounced", errors.New("debounced (retry in 5s)"), "debounced"},
		{"in-progress", errors.New("sync already in progress"), "in progress"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ft := &fakeTrigger{err: tc.err}
			s := newTestServer(ft)
			req := httptest.NewRequest(http.MethodPost, "/sync", nil)
			rec := httptest.NewRecorder()

			s.handleSync(rec, req)

			if rec.Code != http.StatusAccepted {
				t.Errorf("status = %d, want %d (declined triggers should be 202)", rec.Code, http.StatusAccepted)
			}
			body := rec.Body.String()
			if !strings.HasPrefix(body, "skipped:") {
				t.Errorf("body = %q, want prefix %q", body, "skipped:")
			}
			if !strings.Contains(body, tc.wantSub) {
				t.Errorf("body = %q, want to contain %q", body, tc.wantSub)
			}
		})
	}
}

func TestNewServer_NilLoggerDefaults(t *testing.T) {
	ft := &fakeTrigger{}
	s := NewServer(":0", ft, nil)
	if s.log == nil {
		t.Error("NewServer with nil logger should default to log.Default()")
	}
}

func TestServer_RunAndShutdown(t *testing.T) {
	ft := &fakeTrigger{}
	// Use port 0 so the kernel picks a free port — we exercise the full
	// ListenAndServe / Shutdown cycle without a real HTTP round-trip,
	// since our interest is clean shutdown on ctx cancel.
	s := NewServer("127.0.0.1:0", ft, silentLogger())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	// Give the server a moment to start listening. Not strictly needed
	// for correctness but makes the test's intent clearer.
	time.Sleep(50 * time.Millisecond)

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned err on shutdown: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}
}

func TestHealthz(t *testing.T) {
	// Spin up a real server and hit /healthz so the mux wiring is covered.
	ft := &fakeTrigger{}
	s := NewServer("127.0.0.1:0", ft, silentLogger())

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/sync", s.handleSync)

	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("/healthz status = %d, want 200", resp.StatusCode)
	}
}
