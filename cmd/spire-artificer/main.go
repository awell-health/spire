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

	"github.com/awell-health/spire/pkg/repoconfig"
)

// Bead represents a work item from the beads database.
type Bead struct {
	ID          string   `json:"id"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Status      string   `json:"status"`
	Priority    int      `json:"priority"`
	Type        string   `json:"issue_type"`
	Labels      []string `json:"labels"`
	Parent      string   `json:"parent"`
}

// ChildState tracks the review/merge state for each child bead.
type ChildState struct {
	BeadID             string `json:"bead_id"`
	Branch             string `json:"branch"`
	LastReviewedCommit string `json:"last_reviewed_commit"`
	ReviewRounds       int    `json:"review_rounds"`
	Verdict            string `json:"verdict"` // "pending", "approved", "request_changes", "rejected", "merged"
	PRNumber           int    `json:"pr_number"`
	InMergeQueue       bool   `json:"in_merge_queue"`
}

// ArtificerState tracks the overall state of the artificer (thread-safe).
type ArtificerState struct {
	mu        sync.RWMutex
	Phase     string
	EpicID    string
	Model     string
	MaxRounds int
	Children  int
	Merged    int
	Approved  int
	StartedAt time.Time
	LastCycle time.Time
	Error     string
}

type ArtificerSnapshot struct {
	Phase     string    `json:"phase"`
	EpicID    string    `json:"epicId"`
	Model     string    `json:"model"`
	Children  int       `json:"children"`
	Merged    int       `json:"merged"`
	Approved  int       `json:"approved"`
	StartedAt time.Time `json:"startedAt"`
	LastCycle time.Time `json:"lastCycle"`
	Error     string    `json:"error,omitempty"`
}

func (s *ArtificerState) setPhase(phase string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Phase = phase
}

func (s *ArtificerState) snapshot() ArtificerSnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return ArtificerSnapshot{
		Phase:     s.Phase,
		EpicID:    s.EpicID,
		Model:     s.Model,
		Children:  s.Children,
		Merged:    s.Merged,
		Approved:  s.Approved,
		StartedAt: s.StartedAt,
		LastCycle: s.LastCycle,
		Error:     s.Error,
	}
}

func (s *ArtificerState) updateCounts(states map[string]*ChildState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Children = len(states)
	s.Merged = 0
	s.Approved = 0
	for _, cs := range states {
		switch cs.Verdict {
		case "merged":
			s.Merged++
		case "approved":
			s.Approved++
		}
	}
	s.LastCycle = time.Now()
}

func main() {
	epicID := flag.String("epic-id", envOr("SPIRE_EPIC_ID", ""), "epic bead ID to manage")
	model := flag.String("model", envOr("ARTIFICER_MODEL", "claude-opus-4-6"), "model for code review")
	maxRounds := flag.Int("max-rounds", envOrInt("ARTIFICER_MAX_REVIEW_ROUNDS", 3), "max review rounds before escalation")
	commsDir := flag.String("comms-dir", envOr("SPIRE_COMMS_DIR", "/comms"), "shared comms directory")
	workspaceDir := flag.String("workspace-dir", envOr("SPIRE_WORKSPACE_DIR", "/workspace"), "git workspace directory")
	stateDir := flag.String("state-dir", envOr("SPIRE_STATE_DIR", "/data"), "beads state directory")
	pollInterval := flag.Duration("poll-interval", 30*time.Second, "main loop poll interval")
	port := flag.Int("port", 9090, "health endpoint port")
	once := flag.Bool("once", false, "run one cycle and exit")
	flag.Parse()

	if *epicID == "" {
		log.Fatal("--epic-id (or SPIRE_EPIC_ID) is required")
	}

	log.Printf("spire-artificer starting (epic=%s, model=%s, maxRounds=%d, poll=%s)",
		*epicID, *model, *maxRounds, *pollInterval)

	state := &ArtificerState{
		Phase:     "initializing",
		EpicID:    *epicID,
		Model:     *model,
		MaxRounds: *maxRounds,
		StartedAt: time.Now(),
	}

	// Ensure directories exist.
	for _, d := range []string{*commsDir, *workspaceDir, *stateDir} {
		if err := os.MkdirAll(d, 0755); err != nil {
			log.Fatalf("failed to create directory %s: %v", d, err)
		}
	}

	// Initialize beads state and connect to shared cluster dolt.
	if err := initBeadsState(*stateDir); err != nil {
		log.Fatalf("beads init failed: %v", err)
	}
	// Set BEADS_DIR so all bd commands find the .beads directory regardless of cwd.
	os.Setenv("BEADS_DIR", filepath.Join(*stateDir, ".beads"))

	// Initialize: clone repo if needed, load config, load epic.
	repoCfg, err := initWorkspace(*workspaceDir, *stateDir)
	if err != nil {
		log.Fatalf("workspace init failed: %v", err)
	}

	epic, err := loadEpic(*epicID)
	if err != nil {
		log.Fatalf("failed to load epic %s: %v", *epicID, err)
	}
	log.Printf("[artificer] epic: %s — %s", epic.ID, epic.Title)

	children, err := loadChildren(*epicID)
	if err != nil {
		log.Fatalf("failed to load children for %s: %v", *epicID, err)
	}
	log.Printf("[artificer] %d child beads", len(children))

	childStates := loadOrInitChildStates(*stateDir, children, repoCfg)
	state.updateCounts(childStates)
	state.setPhase("running")

	if *once {
		artificerCycle(*workspaceDir, *stateDir, *epicID, *model, *maxRounds, repoCfg, childStates, state)
		saveChildStates(*stateDir, childStates)
		return
	}

	// Set up graceful shutdown.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	// Health server.
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		snap := state.snapshot()
		if snap.Phase == "initializing" {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("initializing"))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ready"))
	})
	mux.HandleFunc("/status", func(w http.ResponseWriter, _ *http.Request) {
		snap := state.snapshot()
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(snap)
	})

	server := &http.Server{
		Addr:              fmt.Sprintf(":%d", *port),
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		log.Printf("health server listening on :%d", *port)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("health server error: %v", err)
		}
	}()

	// Main loops.
	var wg sync.WaitGroup

	// Artificer loop.
	wg.Add(1)
	go func() {
		defer wg.Done()
		artificerLoop(ctx, *workspaceDir, *stateDir, *epicID, *model, *maxRounds, *pollInterval, repoCfg, childStates, state)
	}()

	// Heartbeat loop.
	wg.Add(1)
	go func() {
		defer wg.Done()
		heartbeatLoop(ctx, *commsDir)
	}()

	// Comms monitor (watch for /comms/stop).
	wg.Add(1)
	go func() {
		defer wg.Done()
		commsMonitorLoop(ctx, state, *commsDir, cancel)
	}()

	// Wait for signal or context cancellation.
	select {
	case <-sigCh:
		log.Println("received shutdown signal")
	case <-ctx.Done():
		log.Println("context cancelled (stop received)")
	}

	state.setPhase("stopping")
	cancel()
	wg.Wait()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	_ = server.Shutdown(shutdownCtx)

	// Save state before exit.
	saveChildStates(*stateDir, childStates)

	state.setPhase("stopped")
	log.Println("spire-artificer stopped")
}

// artificerLoop runs the main review/merge cycle on an interval.
func artificerLoop(ctx context.Context, workspaceDir, stateDir, epicID, model string, maxRounds int, interval time.Duration, cfg *repoconfig.RepoConfig, childStates map[string]*ChildState, state *ArtificerState) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Run immediately.
	artificerCycle(workspaceDir, stateDir, epicID, model, maxRounds, cfg, childStates, state)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			artificerCycle(workspaceDir, stateDir, epicID, model, maxRounds, cfg, childStates, state)
		}
	}
}

// artificerCycle is one pass of the artificer's main loop.
func artificerCycle(workspaceDir, stateDir, epicID, model string, maxRounds int, cfg *repoconfig.RepoConfig, childStates map[string]*ChildState, state *ArtificerState) {
	log.Println("[artificer] --- cycle start ---")

	// Pull latest bead state.
	if _, err := bd("dolt", "pull"); err != nil {
		log.Printf("[artificer] warning: dolt pull failed: %v", err)
	}

	// Fetch latest git branches.
	if err := gitFetch(workspaceDir); err != nil {
		log.Printf("[artificer] warning: git fetch failed: %v", err)
	}

	// Refresh children (new children may have been added).
	children, err := loadChildren(epicID)
	if err != nil {
		log.Printf("[artificer] warning: could not refresh children: %v", err)
	} else {
		for _, child := range children {
			if _, exists := childStates[child.ID]; !exists {
				branch := resolveBranch(child.ID, cfg.Branch.Pattern)
				childStates[child.ID] = &ChildState{
					BeadID:  child.ID,
					Branch:  branch,
					Verdict: "pending",
				}
				log.Printf("[artificer] new child detected: %s", child.ID)
			}
		}
	}

	// Load epic spec for reviews.
	epic, _ := loadEpic(epicID)
	spec := ""
	if epic != nil {
		spec = epic.Description
	}

	base := cfg.Branch.Base

	// Review each child.
	for childID, cs := range childStates {
		if cs.Verdict == "merged" || cs.Verdict == "rejected" {
			continue
		}

		// Check if branch exists.
		if !branchExists(workspaceDir, cs.Branch) {
			continue
		}

		// Check for new commits.
		head, err := getHeadCommit(workspaceDir, cs.Branch)
		if err != nil {
			continue
		}
		if head == cs.LastReviewedCommit {
			continue
		}

		reviewStart := time.Now()
		log.Printf("[artificer] reviewing %s (new commits since %s)", childID, truncate(cs.LastReviewedCommit, 8))

		// Load child bead for context.
		child := Bead{ID: childID, Title: childID}
		if c, err := loadBead(childID); err == nil {
			child = *c
		}

		// Checkout branch and run tests.
		if err := gitCheckout(workspaceDir, cs.Branch); err != nil {
			log.Printf("[artificer] failed to checkout %s: %v", cs.Branch, err)
			continue
		}

		testResult := runTests(workspaceDir, cs.Branch, cfg)
		if !testResult.Passed {
			log.Printf("[artificer] tests failed on %s during %s", childID, testResult.Stage)
			sendTestFailure(child, testResult) //nolint:errcheck
			recordRun(child, epicID, model, "test_failure", nil, tokenUsage{}, [3]int{}, reviewStart) //nolint:errcheck
			cs.LastReviewedCommit = head
			saveChildStates(stateDir, childStates)
			continue
		}

		// Get diff for review.
		diff, err := gitDiff(workspaceDir, base, cs.Branch)
		if err != nil {
			log.Printf("[artificer] failed to get diff for %s: %v", childID, err)
			continue
		}

		// Get diff stats for metrics.
		filesChanged, linesAdded, linesRemoved, _ := gitDiffStats(workspaceDir, base, cs.Branch)

		// Call Opus review.
		review, usage, err := callOpusReview(model, spec, diff, child, testResult.Output, cs.ReviewRounds)
		if err != nil {
			log.Printf("[artificer] review failed for %s: %v", childID, err)
			cs.LastReviewedCommit = head
			continue
		}

		log.Printf("[artificer] %s verdict: %s — %s", childID, review.Verdict, review.Summary)
		recordRun(child, epicID, model, "success", review, usage, [3]int{filesChanged, linesAdded, linesRemoved}, reviewStart) //nolint:errcheck
		cs.LastReviewedCommit = head

		switch review.Verdict {
		case "approve":
			prNum, err := createOrUpdatePR(workspaceDir, child, cs.Branch, review, cfg)
			if err != nil {
				log.Printf("[artificer] failed to create PR for %s: %v", childID, err)
			} else {
				cs.PRNumber = prNum
			}
			cs.Verdict = "approved"
			cs.InMergeQueue = true
			log.Printf("[artificer] %s approved, PR #%d, added to merge queue", childID, cs.PRNumber)

		case "request_changes":
			cs.ReviewRounds++
			cs.Verdict = "request_changes"
			if cs.ReviewRounds >= maxRounds {
				log.Printf("[artificer] %s exceeded max review rounds (%d), escalating", childID, maxRounds)
				escalateToHuman(child, review, cs.ReviewRounds) //nolint:errcheck
			} else {
				log.Printf("[artificer] %s needs changes (round %d/%d)", childID, cs.ReviewRounds, maxRounds)
				sendReviewToWizard(child, review) //nolint:errcheck
			}

		case "reject":
			cs.Verdict = "rejected"
			log.Printf("[artificer] %s rejected", childID)
			reportToSteward(child, review) //nolint:errcheck
		}

		saveChildStates(stateDir, childStates)
	}

	// Process merge queue.
	if err := processMergeQueue(workspaceDir, childStates, cfg, epicID); err != nil {
		log.Printf("[artificer] merge queue error: %v", err)
	}

	// Check if epic is complete.
	allMerged := true
	for _, cs := range childStates {
		if cs.Verdict != "merged" && cs.Verdict != "rejected" {
			allMerged = false
			break
		}
	}

	if allMerged && len(childStates) > 0 {
		log.Printf("[artificer] all children complete, closing epic %s", epicID)
		reportEpicProgress(epicID, childStates) //nolint:errcheck
		bdComment(epicID, "All children merged or rejected. Closing epic.") //nolint:errcheck
		bd("close", epicID) //nolint:errcheck
		bd("dolt", "push")  //nolint:errcheck
	} else {
		reportEpicProgress(epicID, childStates) //nolint:errcheck
		bd("dolt", "push")                      //nolint:errcheck
	}

	state.updateCounts(childStates)
	saveChildStates(stateDir, childStates)
	log.Println("[artificer] --- cycle end ---")
}

// --- Initialization ---

// initBeadsState connects to the shared cluster dolt service and initializes beads.
func initBeadsState(stateDir string) error {
	doltHost := os.Getenv("DOLT_HOST")
	if doltHost == "" {
		doltHost = "spire-dolt.spire.svc"
	}
	doltPort := os.Getenv("DOLT_PORT")
	if doltPort == "" {
		doltPort = "3306"
	}

	beadsDir := filepath.Join(stateDir, ".beads")
	if _, err := os.Stat(beadsDir); os.IsNotExist(err) {
		log.Printf("[artificer] initializing beads (dolt: %s:%s)", doltHost, doltPort)
		exec.Command("git", "-C", stateDir, "init", "-q").Run()                                   //nolint
		exec.Command("git", "-C", stateDir, "config", "user.name", "artificer").Run()              //nolint
		exec.Command("git", "-C", stateDir, "config", "user.email", "artificer@spire.local").Run() //nolint

		cmd := exec.Command("bd", "init", "--force", "--prefix", "spi")
		cmd.Dir = stateDir
		cmd.Env = append(os.Environ(),
			"BEADS_DOLT_SERVER_HOST="+doltHost,
			"BEADS_DOLT_SERVER_PORT="+doltPort,
		)
		// Tolerate init failure: shared dolt may already have schema from steward/other agents.
		if out, ierr := cmd.CombinedOutput(); ierr != nil {
			if _, serr := os.Stat(beadsDir); os.IsNotExist(serr) {
				return fmt.Errorf("bd init: %w\n%s", ierr, out)
			}
			log.Printf("[artificer] bd init warning (schema may already exist): %s", strings.TrimSpace(string(out)))
		}

		routesPath := filepath.Join(beadsDir, "routes.jsonl")
		os.WriteFile(routesPath, []byte("{\"prefix\":\"spi-\",\"path\":\".\"}\n"), 0644) //nolint
	}

	// Stop any local dolt server that bd init may have started, then point at the shared service.
	bdCmd := func(args ...string) *exec.Cmd {
		c := exec.Command("bd", args...)
		c.Dir = stateDir
		return c
	}
	bdCmd("dolt", "stop").Run() //nolint — stop local server if running
	bdCmd("dolt", "set", "host", doltHost).Run() //nolint
	bdCmd("dolt", "set", "port", doltPort).Run() //nolint
	os.Remove(filepath.Join(beadsDir, "dolt-server.lock"))
	os.Remove(filepath.Join(beadsDir, "dolt-server.pid"))
	os.Remove(filepath.Join(beadsDir, "dolt-server.port"))
	// Remove deprecated dolt_server_port from metadata.json to prevent port confusion.
	metaPath := filepath.Join(beadsDir, "metadata.json")
	if data, err := os.ReadFile(metaPath); err == nil {
		var meta map[string]any
		if json.Unmarshal(data, &meta) == nil {
			delete(meta, "dolt_server_port")
			if jdata, merr := json.MarshalIndent(meta, "", "  "); merr == nil {
				os.WriteFile(metaPath, jdata, 0644) //nolint
			}
		}
	}

	// Wait for shared dolt to be reachable.
	connected := false
	for i := 0; i < 15; i++ {
		if bdCmd("dolt", "test").Run() == nil {
			log.Printf("[artificer] dolt connected (%s:%s)", doltHost, doltPort)
			connected = true
			break
		}
		if i == 14 {
			log.Printf("[artificer] warning: dolt at %s:%s not reachable after 15s", doltHost, doltPort)
		}
		time.Sleep(time.Second)
	}

	// Align project ID with the dolt server's existing project.
	// The artificer uses emptyDir, so bd init generates a new project ID each time.
	if connected {
		out, err := exec.Command("dolt", "--host", doltHost, "--port", doltPort,
			"--user", "root", "--no-tls", "sql", "-q",
			"USE spi; SELECT value FROM metadata WHERE `key`='_project_id'",
			"-r", "csv").CombinedOutput()
		if err == nil {
			lines := strings.Split(strings.TrimSpace(string(out)), "\n")
			if len(lines) >= 2 {
				serverPID := strings.TrimSpace(lines[len(lines)-1])
				metaPath := filepath.Join(beadsDir, "metadata.json")
				if data, rerr := os.ReadFile(metaPath); rerr == nil {
					updated := strings.Replace(string(data),
						`"project_id"`, `"project_id"`, 1) // no-op, just parse
					// Simple JSON field replacement.
					var meta map[string]any
					if jerr := json.Unmarshal(data, &meta); jerr == nil {
						meta["project_id"] = serverPID
						if jdata, merr := json.MarshalIndent(meta, "", "  "); merr == nil {
							os.WriteFile(metaPath, jdata, 0644) //nolint
							log.Printf("[artificer] aligned project ID: %s", serverPID)
						}
					}
					_ = updated
				}
			}
		}
	}

	return nil
}

// initWorkspace clones the repo (if needed) and loads repo config.
func initWorkspace(workspaceDir, stateDir string) (*repoconfig.RepoConfig, error) {
	repoURL := os.Getenv("SPIRE_REPO_URL")
	repoBranch := os.Getenv("SPIRE_REPO_BRANCH")
	if repoBranch == "" {
		repoBranch = "main"
	}

	// Clone if workspace is empty.
	entries, _ := os.ReadDir(workspaceDir)
	if len(entries) == 0 && repoURL != "" {
		log.Printf("[artificer] cloning %s (branch %s) into %s", repoURL, repoBranch, workspaceDir)
		if err := gitClone(repoURL, repoBranch, workspaceDir); err != nil {
			return nil, fmt.Errorf("clone: %w", err)
		}
	} else if len(entries) > 0 {
		// Fetch latest.
		gitFetch(workspaceDir) //nolint:errcheck
	}

	cfg, err := repoconfig.Load(workspaceDir)
	if err != nil {
		return nil, fmt.Errorf("load repo config: %w", err)
	}
	log.Printf("[artificer] repo config: lang=%s, test=%s, base=%s", cfg.Runtime.Language, cfg.Runtime.Test, cfg.Branch.Base)

	return cfg, nil
}

// loadEpic loads a bead by ID.
func loadEpic(epicID string) (*Bead, error) {
	return loadBead(epicID)
}

// loadBead loads a single bead by ID.
func loadBead(beadID string) (*Bead, error) {
	out, err := bd("show", beadID, "--json")
	if err != nil {
		return nil, err
	}
	// bd show --json may return an array or a single object.
	var beads []Bead
	if err := json.Unmarshal([]byte(out), &beads); err == nil && len(beads) > 0 {
		return &beads[0], nil
	}
	var bead Bead
	if err := json.Unmarshal([]byte(out), &bead); err != nil {
		return nil, fmt.Errorf("parse bead %s: %w", beadID, err)
	}
	return &bead, nil
}

// loadChildren returns all child beads of the given epic.
func loadChildren(epicID string) ([]Bead, error) {
	// Try bd children first.
	var children []Bead
	if err := bdJSON(&children, "children", epicID); err == nil && len(children) > 0 {
		return children, nil
	}

	// Fallback: list all and filter by parent.
	var all []Bead
	if err := bdJSON(&all, "list"); err != nil {
		return nil, fmt.Errorf("list beads: %w", err)
	}

	for _, b := range all {
		if b.Parent == epicID {
			children = append(children, b)
		}
	}
	return children, nil
}

// loadOrInitChildStates loads saved child states or initializes new ones.
func loadOrInitChildStates(stateDir string, children []Bead, cfg *repoconfig.RepoConfig) map[string]*ChildState {
	states := make(map[string]*ChildState)

	// Try loading from file.
	statePath := filepath.Join(stateDir, "artificer-state.json")
	if data, err := os.ReadFile(statePath); err == nil {
		var saved map[string]*ChildState
		if err := json.Unmarshal(data, &saved); err == nil {
			states = saved
		}
	}

	// Ensure all children have entries.
	for _, child := range children {
		if _, exists := states[child.ID]; !exists {
			branch := resolveBranch(child.ID, cfg.Branch.Pattern)
			states[child.ID] = &ChildState{
				BeadID:  child.ID,
				Branch:  branch,
				Verdict: "pending",
			}
		}
	}

	return states
}

// saveChildStates persists child states to disk.
func saveChildStates(stateDir string, states map[string]*ChildState) {
	statePath := filepath.Join(stateDir, "artificer-state.json")
	data, err := json.MarshalIndent(states, "", "  ")
	if err != nil {
		log.Printf("[artificer] failed to marshal state: %v", err)
		return
	}
	// Atomic write.
	tmp := statePath + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		log.Printf("[artificer] failed to write state: %v", err)
		return
	}
	if err := os.Rename(tmp, statePath); err != nil {
		log.Printf("[artificer] failed to rename state: %v", err)
	}
}

// resolveBranch turns a bead ID into a branch name using the pattern from spire.yaml.
func resolveBranch(beadID, pattern string) string {
	if pattern == "" {
		pattern = "feat/{bead-id}"
	}
	return strings.ReplaceAll(pattern, "{bead-id}", beadID)
}

// --- Background loops ---

// heartbeatLoop writes a timestamp to /comms/heartbeat periodically.
func heartbeatLoop(ctx context.Context, commsDir string) {
	heartbeatPath := filepath.Join(commsDir, "heartbeat")
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	writeTimestamp(heartbeatPath)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			writeTimestamp(heartbeatPath)
		}
	}
}

// commsMonitorLoop watches for /comms/stop to trigger graceful shutdown.
func commsMonitorLoop(ctx context.Context, state *ArtificerState, commsDir string, cancel context.CancelFunc) {
	stopPath := filepath.Join(commsDir, "stop")
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := os.Stat(stopPath); err == nil {
				log.Println("[artificer] stop signal received via /comms/stop")
				state.setPhase("stopping")
				cancel()
				return
			}
		}
	}
}

// --- Helpers ---

func writeTimestamp(path string) {
	ts := time.Now().UTC().Format(time.RFC3339)
	_ = os.WriteFile(path, []byte(ts), 0644)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envOrInt(key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	var n int
	if _, err := fmt.Sscanf(v, "%d", &n); err != nil {
		return fallback
	}
	return n
}
