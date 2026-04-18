package gateway

import (
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// fakeTarget records how Trigger was called and can return a preset error.
type fakeTarget struct {
	calls      atomic.Int32
	lastReason atomic.Value // string
	err        error
}

func (f *fakeTarget) Trigger(reason string) error {
	f.calls.Add(1)
	f.lastReason.Store(reason)
	return f.err
}

func (f *fakeTarget) reason() string {
	v := f.lastReason.Load()
	if v == nil {
		return ""
	}
	return v.(string)
}

func newServer(target Triggerable) *Server {
	return &Server{target: target, log: log.New(io.Discard, "", 0)}
}

func TestHandleSync_RejectsNonPOST(t *testing.T) {
	target := &fakeTarget{}
	s := newServer(target)

	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		req := httptest.NewRequest(method, "/sync", nil)
		w := httptest.NewRecorder()
		s.handleSync(w, req)
		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s /sync: status = %d, want %d", method, w.Code, http.StatusMethodNotAllowed)
		}
	}
	if got := target.calls.Load(); got != 0 {
		t.Errorf("target was called %d times; non-POST must not trigger", got)
	}
}

func TestHandleSync_PostTriggers(t *testing.T) {
	target := &fakeTarget{}
	s := newServer(target)

	req := httptest.NewRequest(http.MethodPost, "/sync", nil)
	w := httptest.NewRecorder()
	s.handleSync(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}
	if got := target.calls.Load(); got != 1 {
		t.Errorf("calls = %d, want 1", got)
	}
	if r := target.reason(); r != "http:http" {
		// default reason is "http" which becomes "http:http" after prefix.
		t.Errorf("reason = %q, want %q", r, "http:http")
	}
	if body := w.Body.String(); !strings.Contains(body, "triggered") {
		t.Errorf("body = %q, want to contain 'triggered'", body)
	}
}

func TestHandleSync_ExtractsReasonQueryParam(t *testing.T) {
	target := &fakeTarget{}
	s := newServer(target)

	req := httptest.NewRequest(http.MethodPost, "/sync?reason=signal", nil)
	w := httptest.NewRecorder()
	s.handleSync(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}
	if r := target.reason(); r != "http:signal" {
		t.Errorf("reason = %q, want %q", r, "http:signal")
	}
}

func TestHandleSync_DeclinedReturns202(t *testing.T) {
	target := &fakeTarget{err: errors.New("debounced (retry in 5s)")}
	s := newServer(target)

	req := httptest.NewRequest(http.MethodPost, "/sync", nil)
	w := httptest.NewRecorder()
	s.handleSync(w, req)

	if w.Code != http.StatusAccepted {
		t.Errorf("status = %d, want %d", w.Code, http.StatusAccepted)
	}
	body := w.Body.String()
	if !strings.HasPrefix(body, "skipped: ") {
		t.Errorf("body = %q, want prefix 'skipped: '", body)
	}
	if !strings.Contains(body, "debounced") {
		t.Errorf("body = %q, want to surface trigger error", body)
	}
}

func TestHandleSync_DeclinedInProgressReturns202(t *testing.T) {
	target := &fakeTarget{err: errors.New("sync already in progress")}
	s := newServer(target)

	req := httptest.NewRequest(http.MethodPost, "/sync", nil)
	w := httptest.NewRecorder()
	s.handleSync(w, req)

	if w.Code != http.StatusAccepted {
		t.Errorf("status = %d, want %d (declined must be 202, not 5xx)", w.Code, http.StatusAccepted)
	}
}

func TestNewServer_NilLoggerDefaults(t *testing.T) {
	s := NewServer(":0", &fakeTarget{}, nil)
	if s.log == nil {
		t.Error("NewServer with nil logger: s.log is nil, want default logger")
	}
}
