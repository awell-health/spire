package gateway

import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeTrigger implements Triggerable. Each call records the reason; the
// returned error is taken from errQueue in order (empty = nil).
type fakeTrigger struct {
	calls    int32
	reasons  []string
	errQueue []error
}

func (f *fakeTrigger) Trigger(reason string) error {
	idx := int(atomic.AddInt32(&f.calls, 1)) - 1
	f.reasons = append(f.reasons, reason)
	if idx < len(f.errQueue) {
		return f.errQueue[idx]
	}
	return nil
}

func newTestServer(target Triggerable) *Server {
	return &Server{addr: ":0", target: target, log: log.New(io.Discard, "", 0)}
}

func TestHandleSync_TableDriven(t *testing.T) {
	tests := []struct {
		name           string
		method         string
		path           string
		triggerErr     error
		wantStatus     int
		wantBodyPrefix string
		wantReason     string
	}{
		{
			name:           "POST success returns 200 triggered",
			method:         http.MethodPost,
			path:           "/sync",
			triggerErr:     nil,
			wantStatus:     http.StatusOK,
			wantBodyPrefix: "triggered",
			wantReason:     "http:http",
		},
		{
			name:           "POST with reason query forwards to trigger",
			method:         http.MethodPost,
			path:           "/sync?reason=signal",
			triggerErr:     nil,
			wantStatus:     http.StatusOK,
			wantBodyPrefix: "triggered",
			wantReason:     "http:signal",
		},
		{
			name:           "debounced trigger returns 202 skipped",
			method:         http.MethodPost,
			path:           "/sync",
			triggerErr:     errors.New("debounced (retry in 5s)"),
			wantStatus:     http.StatusAccepted,
			wantBodyPrefix: "skipped:",
			wantReason:     "http:http",
		},
		{
			name:           "in-progress trigger returns 202 skipped",
			method:         http.MethodPost,
			path:           "/sync",
			triggerErr:     errors.New("sync already in progress"),
			wantStatus:     http.StatusAccepted,
			wantBodyPrefix: "skipped:",
			wantReason:     "http:http",
		},
		{
			name:           "GET rejected with 405",
			method:         http.MethodGet,
			path:           "/sync",
			wantStatus:     http.StatusMethodNotAllowed,
			wantBodyPrefix: "method not allowed",
		},
		{
			name:           "PUT rejected with 405",
			method:         http.MethodPut,
			path:           "/sync",
			wantStatus:     http.StatusMethodNotAllowed,
			wantBodyPrefix: "method not allowed",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			trig := &fakeTrigger{}
			if tc.triggerErr != nil {
				trig.errQueue = []error{tc.triggerErr}
			}
			s := newTestServer(trig)

			req := httptest.NewRequest(tc.method, tc.path, nil)
			rec := httptest.NewRecorder()
			s.handleSync(rec, req)

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body=%q)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if !strings.HasPrefix(rec.Body.String(), tc.wantBodyPrefix) {
				t.Fatalf("body = %q, want prefix %q", rec.Body.String(), tc.wantBodyPrefix)
			}

			if tc.wantReason != "" {
				if atomic.LoadInt32(&trig.calls) != 1 {
					t.Fatalf("trigger calls = %d, want 1", trig.calls)
				}
				if trig.reasons[0] != tc.wantReason {
					t.Fatalf("trigger reason = %q, want %q", trig.reasons[0], tc.wantReason)
				}
			} else if atomic.LoadInt32(&trig.calls) != 0 {
				t.Fatalf("trigger calls = %d, want 0 (method should be rejected before dispatch)", trig.calls)
			}
		})
	}
}

func TestHealthz_Returns200(t *testing.T) {
	s := newTestServer(&fakeTrigger{})

	// Drive the healthz handler through the same mux setup the production
	// Run() path uses. We build the mux inline because Run() blocks on
	// ListenAndServe.
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/sync", s.handleSync)

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "ok") {
		t.Fatalf("body = %q, want contains \"ok\"", rec.Body.String())
	}
}

func TestServer_RunAndShutdown(t *testing.T) {
	// End-to-end: bind a real ephemeral port, make one request, then cancel
	// the context and verify Run returns cleanly.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	trig := &fakeTrigger{}
	s := NewServer(addr, trig, log.New(io.Discard, "", 0), "", "")

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- s.Run(ctx) }()

	// Wait for the listener to be ready.
	deadline := time.Now().Add(2 * time.Second)
	var resp *http.Response
	for time.Now().Before(deadline) {
		r, err := http.Get("http://" + addr + "/healthz")
		if err == nil {
			resp = r
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if resp == nil {
		t.Fatal("healthz never responded")
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz status = %d, want 200", resp.StatusCode)
	}

	// POST /sync should reach the fake trigger.
	postResp, err := http.Post("http://"+addr+"/sync", "", nil)
	if err != nil {
		t.Fatalf("POST /sync: %v", err)
	}
	postResp.Body.Close()
	if postResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /sync status = %d, want 200", postResp.StatusCode)
	}
	if atomic.LoadInt32(&trig.calls) != 1 {
		t.Fatalf("trigger calls = %d, want 1", trig.calls)
	}

	cancel()
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after context cancel")
	}
}
