// Package gateway provides the HTTP surface of a Spire tower. Today it has
// a single concern: accept POST /sync and relay the request to a Triggerable
// (typically a pkg/steward.Daemon). Future routes (message submission,
// status, etc.) land here too — gateway.Server is the stable seam between
// outside-the-cluster callers and inside-the-cluster workers.
//
// Boundaries:
//   - gateway owns HTTP wiring, method/verb validation, response shape.
//   - gateway does NOT own debounce, rate limit, sync logic, or dolt access.
//     Those live behind the Triggerable interface so callers (Daemon, tests,
//     fakes) can implement their own throttling policy.
package gateway

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"time"
)

// Triggerable is implemented by anything that can be asked to do work now.
// A returned error means the request was declined (debounced, in-progress,
// etc.) — the gateway surfaces it as a 202 Accepted with a "skipped:"
// response body, not as a 5xx.
type Triggerable interface {
	Trigger(reason string) error
}

// Server is the HTTP gateway. Construct with NewServer and Run under a
// context the caller can cancel for graceful shutdown.
type Server struct {
	addr   string
	target Triggerable
	log    *log.Logger
}

// NewServer wires a server listening on addr (e.g. ":8082") that forwards
// POST /sync to target.Trigger. Pass a non-nil logger to capture request
// logs; nil uses log.Default().
func NewServer(addr string, target Triggerable, logger *log.Logger) *Server {
	if logger == nil {
		logger = log.Default()
	}
	return &Server{addr: addr, target: target, log: logger}
}

// Run blocks on the HTTP server until ctx is done, then shuts down with a
// 5s grace period. Returns the first ListenAndServe error if any, or nil
// on clean shutdown.
func (s *Server) Run(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("/sync", s.handleSync)

	srv := &http.Server{
		Addr:              s.addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		s.log.Printf("[gateway] listening on %s (POST /sync)", s.addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case <-ctx.Done():
		s.log.Printf("[gateway] shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	case err := <-errCh:
		return err
	}
}

func (s *Server) handleSync(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed (POST)", http.StatusMethodNotAllowed)
		return
	}
	reason := r.URL.Query().Get("reason")
	if reason == "" {
		reason = "http"
	}
	if err := s.target.Trigger("http:" + reason); err != nil {
		// Declined (debounced, in-progress) is normal — not a 5xx.
		w.WriteHeader(http.StatusAccepted)
		fmt.Fprintf(w, "skipped: %s\n", err)
		return
	}
	fmt.Fprintln(w, "triggered")
}
