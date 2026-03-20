package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// SidecarState tracks the sidecar's current operational state.
type SidecarState struct {
	mu           sync.RWMutex
	Phase        string
	LastCollect  time.Time
	MessageCount int
	WizardAlive  bool
	AgentName    string
	StartedAt    time.Time
	Error        string
	CommsDir     string // set once at startup
}

// SidecarSnapshot is the JSON-serializable snapshot of SidecarState.
type SidecarSnapshot struct {
	Phase        string    `json:"phase"`
	LastCollect  time.Time `json:"lastCollect"`
	MessageCount int       `json:"messageCount"`
	WizardAlive  bool      `json:"wizardAlive"`
	AgentName    string    `json:"agentName"`
	StartedAt    time.Time `json:"startedAt"`
	Error        string    `json:"error,omitempty"`
	Work         *WorkContext `json:"work,omitempty"`
}

// WorkContext describes the wizard's current work, scraped from /comms files.
type WorkContext struct {
	BeadID    string `json:"beadId,omitempty"`
	Title     string `json:"title,omitempty"`
	Branch    string `json:"branch,omitempty"`
	Status    string `json:"status,omitempty"` // "running", "success", "error", "timeout"
	StartedAt string `json:"startedAt,omitempty"`
	Elapsed   string `json:"elapsed,omitempty"`
}

// BeadJSON matches the structure written to /comms/bead.json by the wizard.
type BeadJSON struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

// ResultJSON matches the structure written to /comms/result.json by the wizard.
type ResultJSON struct {
	BeadID    string `json:"beadId"`
	Result    string `json:"result"`
	Branch    string `json:"branch"`
	StartedAt string `json:"startedAt"`
}

func (s *SidecarState) setPhase(phase string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Phase = phase
}

func (s *SidecarState) getPhase() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.Phase
}

func (s *SidecarState) setWizardAlive(alive bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.WizardAlive = alive
}

func (s *SidecarState) setCollectResult(count int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.LastCollect = time.Now()
	s.MessageCount = count
	if err != nil {
		s.Error = err.Error()
	} else {
		s.Error = ""
	}
}

func (s *SidecarState) snapshot() SidecarSnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	snap := SidecarSnapshot{
		Phase:        s.Phase,
		LastCollect:  s.LastCollect,
		MessageCount: s.MessageCount,
		WizardAlive:  s.WizardAlive,
		AgentName:    s.AgentName,
		StartedAt:    s.StartedAt,
		Error:        s.Error,
	}
	// Enrich with work context from /comms files.
	snap.Work = s.readWorkContext()
	return snap
}

// readWorkContext scrapes /comms files to build work context.
func (s *SidecarState) readWorkContext() *WorkContext {
	if s.CommsDir == "" {
		return nil
	}

	wc := &WorkContext{}

	// Read bead.json (written by wizard during claim).
	beadPath := filepath.Join(s.CommsDir, "bead.json")
	if data, err := os.ReadFile(beadPath); err == nil {
		// bead.json may be an array or single object.
		var beads []BeadJSON
		if json.Unmarshal(data, &beads) == nil && len(beads) > 0 {
			wc.BeadID = beads[0].ID
			wc.Title = beads[0].Title
		} else {
			var bead BeadJSON
			if json.Unmarshal(data, &bead) == nil {
				wc.BeadID = bead.ID
				wc.Title = bead.Title
			}
		}
	}

	// Read result.json (written by wizard on exit).
	resultPath := filepath.Join(s.CommsDir, "result.json")
	if data, err := os.ReadFile(resultPath); err == nil {
		var result ResultJSON
		if json.Unmarshal(data, &result) == nil {
			wc.Status = result.Result
			wc.Branch = result.Branch
			if wc.BeadID == "" {
				wc.BeadID = result.BeadID
			}
			if result.StartedAt != "" {
				wc.StartedAt = result.StartedAt
				if t, err := time.Parse(time.RFC3339, result.StartedAt); err == nil {
					wc.Elapsed = time.Since(t).Round(time.Second).String()
				}
			}
		}
	} else if wc.BeadID != "" {
		// No result yet — wizard is still running.
		wc.Status = "running"
		wc.Elapsed = time.Since(s.StartedAt).Round(time.Second).String()
	}

	if wc.BeadID == "" {
		return nil // no work context available
	}

	// Infer branch from bead ID if not in result.
	if wc.Branch == "" && wc.BeadID != "" {
		wc.Branch = "feat/" + wc.BeadID
	}

	return wc
}

func main() {
	commsDir := flag.String("comms-dir", "/comms", "shared communication directory")
	pollInterval := flag.Duration("poll-interval", 10*time.Second, "inbox poll interval")
	port := flag.Int("port", 8080, "health endpoint port")
	agentName := flag.String("agent-name", "", "agent identity for spire collect")
	flag.Parse()

	state := &SidecarState{
		Phase:       "polling",
		WizardAlive: true,
		AgentName:   *agentName,
		StartedAt:   time.Now(),
		CommsDir:    *commsDir,
	}

	// Ensure comms directory exists.
	if err := os.MkdirAll(*commsDir, 0755); err != nil {
		log.Fatalf("failed to create comms dir %s: %v", *commsDir, err)
	}

	log.Printf("spire-sidecar starting (comms=%s, poll=%s, port=%d, agent=%s)",
		*commsDir, *pollInterval, *port, *agentName)

	// Set up graceful shutdown.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	// Start health server.
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", handleHealthz)
	mux.HandleFunc("/readyz", handleReadyz(state))
	mux.HandleFunc("/status", handleStatus(state))
	server := &http.Server{
		Addr:              fmt.Sprintf(":%d", *port),
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		log.Printf("health server listening on :%d", *port)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("health server error: %v", err)
		}
	}()

	// Start main loops.
	var wg sync.WaitGroup

	// Inbox polling loop.
	wg.Add(1)
	go func() {
		defer wg.Done()
		inboxLoop(ctx, state, *commsDir, *pollInterval, *agentName)
	}()

	// Control channel loop.
	wg.Add(1)
	go func() {
		defer wg.Done()
		controlLoop(ctx, state, *commsDir)
	}()

	// Wizard monitoring loop.
	wg.Add(1)
	go func() {
		defer wg.Done()
		wizardMonitorLoop(ctx, state, *commsDir)
	}()

	// Heartbeat loop.
	wg.Add(1)
	go func() {
		defer wg.Done()
		heartbeatLoop(ctx, *commsDir)
	}()

	// Wait for shutdown signal.
	<-sigCh
	log.Println("received shutdown signal, initiating graceful shutdown")
	state.setPhase("stopping")

	// Write STOP to control channel so wizard can pick it up.
	stopPath := filepath.Join(*commsDir, "stop")
	_ = os.WriteFile(stopPath, []byte(time.Now().UTC().Format(time.RFC3339)), 0644)
	log.Println("wrote stop signal for wizard")

	// Give wizard time to exit (up to 30s).
	wizardDone := waitForWizardExit(*commsDir, 30*time.Second)
	if wizardDone {
		log.Println("wizard exited cleanly")
	} else {
		log.Println("wizard did not exit within timeout")
	}

	cancel()
	wg.Wait()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	_ = server.Shutdown(shutdownCtx)

	state.setPhase("stopped")
	log.Println("spire-sidecar stopped")
}

// --- Inbox polling ---

func inboxLoop(ctx context.Context, state *SidecarState, commsDir string, interval time.Duration, agentName string) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Run immediately on start.
	collectAndWrite(state, commsDir, agentName)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if state.getPhase() == "paused" {
				continue
			}
			collectAndWrite(state, commsDir, agentName)
		}
	}
}

func collectAndWrite(state *SidecarState, commsDir, agentName string) {
	args := []string{"collect", "--json"}
	if agentName != "" {
		args = append(args, agentName)
	}

	cmd := exec.Command("spire", args...)
	output, err := cmd.Output()
	if err != nil {
		log.Printf("spire collect failed: %v", err)
		state.setCollectResult(0, err)
		return
	}

	// Count messages (attempt to parse JSON array).
	var messages []json.RawMessage
	count := 0
	if err := json.Unmarshal(output, &messages); err == nil {
		count = len(messages)
	}

	// Write inbox.
	inboxPath := filepath.Join(commsDir, "inbox.json")
	if err := atomicWrite(inboxPath, output); err != nil {
		log.Printf("failed to write inbox: %v", err)
		state.setCollectResult(count, err)
		return
	}

	state.setCollectResult(count, nil)
	if count > 0 {
		log.Printf("collected %d messages", count)
	}
}

// --- Control channel ---

func controlLoop(ctx context.Context, state *SidecarState, commsDir string) {
	controlPath := filepath.Join(commsDir, "control")
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			data, err := os.ReadFile(controlPath)
			if err != nil {
				continue // File doesn't exist yet, that's normal.
			}

			// Remove the control file after reading.
			_ = os.Remove(controlPath)

			command := strings.TrimSpace(string(data))
			if command == "" {
				continue
			}

			log.Printf("control command: %s", command)
			handleControl(state, commsDir, command)
		}
	}
}

func handleControl(state *SidecarState, commsDir, command string) {
	switch {
	case command == "STOP":
		state.setPhase("stopping")
		stopPath := filepath.Join(commsDir, "stop")
		if err := os.WriteFile(stopPath, []byte(time.Now().UTC().Format(time.RFC3339)), 0644); err != nil {
			log.Printf("failed to write stop signal: %v", err)
		}
		log.Println("STOP: wrote stop signal for wizard")

	case strings.HasPrefix(command, "STEER:"):
		message := strings.TrimPrefix(command, "STEER:")
		steerPath := filepath.Join(commsDir, "steer")
		if err := os.WriteFile(steerPath, []byte(message), 0644); err != nil {
			log.Printf("failed to write steer message: %v", err)
		}
		log.Printf("STEER: wrote course correction for wizard")

	case command == "PAUSE":
		state.setPhase("paused")
		log.Println("PAUSE: paused inbox polling")

	case command == "RESUME":
		state.setPhase("polling")
		log.Println("RESUME: resumed inbox polling")

	default:
		log.Printf("unknown control command: %s", command)
	}
}

// --- Wizard monitoring ---

func wizardMonitorLoop(ctx context.Context, state *SidecarState, commsDir string) {
	resultPath := filepath.Join(commsDir, "result.json")
	alivePath := filepath.Join(commsDir, "wizard-alive")
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Check if wizard wrote a result (meaning it exited).
			if _, err := os.Stat(resultPath); err == nil {
				state.setWizardAlive(false)
				log.Println("wizard has exited (result.json found), sidecar shutting down in 10s")
				time.Sleep(10 * time.Second) // grace period for final collect/push
				log.Println("sidecar exiting")
				return
			}

			// Check wizard-alive file freshness. Wizard should touch
			// this periodically. If it's stale (>60s), consider wizard dead.
			info, err := os.Stat(alivePath)
			if err != nil {
				// File doesn't exist yet -- wizard may not have started.
				continue
			}

			stale := time.Since(info.ModTime()) > 60*time.Second
			state.setWizardAlive(!stale)
			if stale {
				log.Println("wizard-alive file is stale (>60s)")
			}
		}
	}
}

// waitForWizardExit polls for the wizard result file or wizard-alive staleness.
func waitForWizardExit(commsDir string, timeout time.Duration) bool {
	resultPath := filepath.Join(commsDir, "result.json")
	deadline := time.After(timeout)
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-deadline:
			return false
		case <-ticker.C:
			if _, err := os.Stat(resultPath); err == nil {
				return true
			}
		}
	}
}

// --- Heartbeat ---

func heartbeatLoop(ctx context.Context, commsDir string) {
	heartbeatPath := filepath.Join(commsDir, "heartbeat")
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	// Write immediately on start.
	writeHeartbeat(heartbeatPath)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			writeHeartbeat(heartbeatPath)
		}
	}
}

func writeHeartbeat(path string) {
	ts := time.Now().UTC().Format(time.RFC3339)
	if err := os.WriteFile(path, []byte(ts), 0644); err != nil {
		log.Printf("failed to write heartbeat: %v", err)
	}
}

// --- Health endpoints ---

func handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func handleReadyz(state *SidecarState) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		snap := state.snapshot()

		// Ready if we've collected at least once and aren't in a failed state.
		if snap.LastCollect.IsZero() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("not ready: no collect yet"))
			return
		}

		// If last collect was more than 5 intervals ago, consider unhealthy.
		if time.Since(snap.LastCollect) > 5*time.Minute {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("not ready: collect stale"))
			return
		}

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ready"))
	}
}

func handleStatus(state *SidecarState) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		snap := state.snapshot()
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(snap)
	}
}

// --- Helpers ---

// atomicWrite writes data to a temp file then renames it, preventing
// partial reads by the wizard.
func atomicWrite(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
