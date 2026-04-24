// Package gateway provides the HTTP surface of a Spire tower. It serves two
// concerns:
//
//  1. POST /sync — accepts webhook triggers and relays them to a Triggerable
//     (typically a pkg/steward.Daemon).
//
//  2. GET/POST/PATCH /api/v1/* — a JSON API for the Electron desktop app to
//     read and mutate beads, messages, board, and roster data.
//
// Boundaries:
//   - gateway owns HTTP wiring, method/verb validation, auth, CORS, response shape.
//   - gateway does NOT own debounce, rate limit, sync logic, or dolt access.
//     Those live behind the Triggerable interface or pkg/store/pkg/board.
package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/awell-health/spire/pkg/board"
	"github.com/awell-health/spire/pkg/config"
	"github.com/awell-health/spire/pkg/dolt"
	"github.com/awell-health/spire/pkg/store"
	"github.com/steveyegge/beads"
)

// Version is the spire binary version string. cmd/spire sets it in init()
// so release builds surface the actual version through /api/v1/tower.
var Version = "dev"

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
	addr     string
	target   Triggerable
	log      *log.Logger
	dataDir  string
	apiToken string

	devModeLogOnce sync.Once
}

// NewServer wires a server listening on addr (e.g. ":3030") that forwards
// POST /sync to target.Trigger. Pass a non-nil logger to capture request
// logs; nil uses log.Default(). dataDir is the .beads directory path used
// by the /api/v1/* handlers. apiToken is the expected Bearer token; if empty
// the server runs without auth (dev mode).
func NewServer(addr string, target Triggerable, logger *log.Logger, dataDir string, apiToken string) *Server {
	if logger == nil {
		logger = log.Default()
	}
	return &Server{
		addr:     addr,
		target:   target,
		log:      logger,
		dataDir:  dataDir,
		apiToken: apiToken,
	}
}

// Run blocks on the HTTP server until ctx is done, then shuts down with a
// 5s grace period. Returns the first ListenAndServe error if any, or nil
// on clean shutdown.
func (s *Server) Run(ctx context.Context) error {
	if s.apiToken == "" {
		s.log.Println("[gateway] SPIRE_API_TOKEN not set, running in dev mode (no auth)")
	}

	mux := http.NewServeMux()

	// Legacy routes (no auth required — pre-existing behaviour). Wrap
	// /healthz with CORS so the Electron renderer's connection probe
	// succeeds from the Vite dev origin (http://localhost:5173).
	mux.Handle("/healthz", s.corsMiddleware(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, "ok")
	}))
	if s.target != nil {
		mux.HandleFunc("/sync", s.handleSync)
	}

	// /api/v1/* routes — all wrapped with CORS + Bearer auth.
	mux.Handle("/api/v1/beads", s.corsMiddleware(s.bearerAuth(s.handleBeads)))
	mux.Handle("/api/v1/beads/", s.corsMiddleware(s.bearerAuth(s.handleBeadByID)))
	mux.Handle("/api/v1/messages", s.corsMiddleware(s.bearerAuth(s.handleMessages)))
	mux.Handle("/api/v1/messages/", s.corsMiddleware(s.bearerAuth(s.handleMessageByID)))
	mux.Handle("/api/v1/board", s.corsMiddleware(s.bearerAuth(s.handleBoard)))
	mux.Handle("/api/v1/roster", s.corsMiddleware(s.bearerAuth(s.handleRoster)))
	mux.Handle("/api/v1/tower", s.corsMiddleware(s.bearerAuth(s.handleTower)))
	mux.Handle("/api/v1/towers", s.corsMiddleware(s.bearerAuth(s.handleTowers)))
	mux.Handle("/api/v1/cleanup/step-beads", s.corsMiddleware(s.bearerAuth(s.handleCleanupStepBeads)))
	mux.Handle("/api/v1/blocked", s.corsMiddleware(s.bearerAuth(s.handleBlocked)))
	mux.Handle("/api/v1/repos", s.corsMiddleware(s.bearerAuth(s.handleRepos)))

	srv := &http.Server{
		Addr:              s.addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		s.log.Printf("[gateway] listening on %s", s.addr)
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

// --------------------------------------------------------------------------
// Middleware
// --------------------------------------------------------------------------

// corsMiddleware sets CORS headers and handles preflight OPTIONS requests.
func (s *Server) corsMiddleware(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}
		next(w, r)
	})
}

// bearerAuth validates the Authorization: Bearer <token> header.
// If SPIRE_API_TOKEN is empty, skips validation (dev mode).
func (s *Server) bearerAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.apiToken == "" {
			// Dev mode: no token required.
			s.devModeLogOnce.Do(func() {
				s.log.Println("[gateway] dev mode: skipping auth on /api/v1/* requests")
			})
			next(w, r)
			return
		}
		authHeader := r.Header.Get("Authorization")
		if !strings.HasPrefix(authHeader, "Bearer ") {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		token := strings.TrimPrefix(authHeader, "Bearer ")
		if token != s.apiToken {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		next(w, r)
	}
}

// --------------------------------------------------------------------------
// /sync handler (legacy)
// --------------------------------------------------------------------------

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

// --------------------------------------------------------------------------
// /api/v1/beads handlers
// --------------------------------------------------------------------------

// handleBeads routes GET (list) and POST (create) for /api/v1/beads.
func (s *Server) handleBeads(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.listBeads(w, r)
	case http.MethodPost:
		s.createBead(w, r)
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

// listBeads handles GET /api/v1/beads
// Query params: status, label, prefix, type
func (s *Server) listBeads(w http.ResponseWriter, r *http.Request) {
	if _, err := store.Ensure(s.effectiveDataDir()); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	// By default, include every status (including closed) so the desktop
	// board can show a CLOSED column. store.ListBeads applies
	// ExcludeStatus=[closed] when the filter is empty; bypass that default
	// with a sentinel that matches no real status.
	filter := beads.IssueFilter{
		ExcludeStatus: []beads.Status{"__none__"},
	}
	if v := r.URL.Query().Get("status"); v != "" {
		bs := beads.Status(v)
		filter.Status = &bs
		filter.ExcludeStatus = nil
	}
	if v := r.URL.Query().Get("label"); v != "" {
		filter.Labels = strings.Split(v, ",")
	}
	if v := r.URL.Query().Get("prefix"); v != "" {
		filter.IDPrefix = v + "-"
	}

	// Use ListBoardBeads so Dependencies are populated — otherwise
	// FindParentID can't resolve and the Parent field is always empty,
	// which breaks client-side grouping on the board.
	boardBeads, err := store.ListBoardBeads(filter)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	// Project back down to the lightweight Bead shape so the response
	// stays small; parent survives because BoardBead.Parent is also
	// derived from the populated dep graph.
	beadList := make([]store.Bead, len(boardBeads))
	for i, bb := range boardBeads {
		beadList[i] = store.Bead{
			ID:          bb.ID,
			Title:       bb.Title,
			Description: bb.Description,
			Status:      bb.Status,
			Priority:    bb.Priority,
			Type:        bb.Type,
			Labels:      bb.Labels,
			Parent:      bb.Parent,
			UpdatedAt:   bb.UpdatedAt,
			Metadata:    bb.Metadata,
		}
	}
	writeJSON(w, http.StatusOK, beadList)
}

// createBead handles POST /api/v1/beads
// Body: {"title":"...", "type":"task", "priority":1, "description":"...", "labels":[], "parent":""}
func (s *Server) createBead(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Title       string   `json:"title"`
		Type        string   `json:"type"`
		Priority    int      `json:"priority"`
		Description string   `json:"description"`
		Labels      []string `json:"labels"`
		Parent      string   `json:"parent"`
		Prefix      string   `json:"prefix"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
		return
	}
	if body.Title == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "title is required"})
		return
	}

	// Ensure store is initialized so store.CreateBead (package-level singleton) works.
	if _, err := store.Ensure(s.effectiveDataDir()); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	issType := beads.IssueType(body.Type)
	if issType == "" {
		issType = beads.TypeTask
	}

	id, err := store.CreateBead(store.CreateOpts{
		Title:       body.Title,
		Description: body.Description,
		Priority:    body.Priority,
		Type:        issType,
		Labels:      body.Labels,
		Parent:      body.Parent,
		Prefix:      body.Prefix,
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"id": id})
}

// handleBeadByID routes GET and PATCH for /api/v1/beads/{id} and
// GET /api/v1/beads/{id}/tree (subtree graph).
func (s *Server) handleBeadByID(w http.ResponseWriter, r *http.Request) {
	id := pathSuffix(r.URL.Path, "/api/v1/beads/")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bead ID required"})
		return
	}
	// /api/v1/beads/{id}/tree
	if strings.HasSuffix(id, "/tree") {
		s.getBeadTree(w, r, strings.TrimSuffix(id, "/tree"))
		return
	}
	// /api/v1/beads/{id}/comments
	if strings.HasSuffix(id, "/comments") {
		s.getBeadComments(w, r, strings.TrimSuffix(id, "/comments"))
		return
	}
	// /api/v1/beads/{id}/logs
	if strings.HasSuffix(id, "/logs") {
		s.getBeadLogs(w, r, strings.TrimSuffix(id, "/logs"))
		return
	}
	// /api/v1/beads/{id}/lineage
	if strings.HasSuffix(id, "/lineage") {
		s.getBeadLineage(w, r, strings.TrimSuffix(id, "/lineage"))
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.getBead(w, r, id)
	case http.MethodPatch:
		s.updateBead(w, r, id)
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

// getBeadTree answers GET /api/v1/beads/{id}/tree with the selected bead's
// full ancestor chain (root → target) and descendant subtree (target → leaves).
// Built in-memory from a single store.ListBeads({}) call — for the local
// tower's ~2k beads this completes in sub-millisecond time.
func (s *Server) getBeadTree(w http.ResponseWriter, _ *http.Request, id string) {
	if _, err := store.Ensure(s.effectiveDataDir()); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	// ListBoardBeads populates dependencies so FindParentID resolves —
	// ListBeads leaves Parent empty since it doesn't populate deps.
	// ListBoardBeads defaults ExcludeStatus=[closed] when ExcludeStatus is
	// empty, so pass a sentinel (harmless) status to bypass the default and
	// surface closed beads in the graph.
	filter := beads.IssueFilter{ExcludeStatus: []beads.Status{"__none__"}}
	all, err := store.ListBoardBeads(filter)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	byID := make(map[string]store.BoardBead, len(all))
	childrenOf := make(map[string][]string)
	for _, b := range all {
		byID[b.ID] = b
		if b.Parent != "" {
			childrenOf[b.Parent] = append(childrenOf[b.Parent], b.ID)
		}
	}
	root, ok := byID[id]
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "bead not found"})
		return
	}
	// Ancestor chain — root-most first.
	var ancestors []store.BoardBead
	for cur := root.Parent; cur != ""; {
		p, ok := byID[cur]
		if !ok {
			break
		}
		ancestors = append([]store.BoardBead{p}, ancestors...)
		cur = p.Parent
	}
	// Descendant subtree — flat list (each with parent pointer) so the client
	// can render its own tree shape.
	var descendants []store.BoardBead
	var walk func(string)
	walk = func(pid string) {
		for _, cid := range childrenOf[pid] {
			if c, ok := byID[cid]; ok {
				descendants = append(descendants, c)
				walk(cid)
			}
		}
	}
	walk(id)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"id":          id,
		"bead":        root,
		"ancestors":   ancestors,
		"descendants": descendants,
	})
}

func (s *Server) getBead(w http.ResponseWriter, _ *http.Request, id string) {
	if _, err := store.Ensure(s.effectiveDataDir()); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	b, err := store.GetBead(id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, b)
}

func (s *Server) updateBead(w http.ResponseWriter, r *http.Request, id string) {
	var updates map[string]interface{}
	if err := json.NewDecoder(r.Body).Decode(&updates); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
		return
	}
	if _, err := store.Ensure(s.effectiveDataDir()); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if err := store.UpdateBead(id, updates); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": id, "status": "updated"})
}

// --------------------------------------------------------------------------
// /api/v1/messages handlers
// --------------------------------------------------------------------------

// handleMessages routes GET (list) and POST (send) for /api/v1/messages.
func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.listMessages(w, r)
	case http.MethodPost:
		s.sendMessage(w, r)
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

// listMessages handles GET /api/v1/messages
// Query params: to (agent name)
func (s *Server) listMessages(w http.ResponseWriter, r *http.Request) {
	if _, err := store.Ensure(s.effectiveDataDir()); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	filter := beads.IssueFilter{
		IDPrefix: "spi-",
		Labels:   []string{"msg"},
	}
	if to := r.URL.Query().Get("to"); to != "" {
		filter.Labels = append(filter.Labels, "to:"+to)
	}
	open := beads.StatusOpen
	filter.Status = &open

	msgs, err := store.ListBeads(filter)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, msgs)
}

// sendMessage handles POST /api/v1/messages
// Body: {"to":"agent", "message":"...", "from":"...", "ref":"spi-xxx", "priority":3}
func (s *Server) sendMessage(w http.ResponseWriter, r *http.Request) {
	var body struct {
		To       string `json:"to"`
		Message  string `json:"message"`
		From     string `json:"from"`
		Ref      string `json:"ref"`
		Thread   string `json:"thread"`
		Priority int    `json:"priority"`
	}
	body.Priority = 3 // default
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
		return
	}
	if body.To == "" || body.Message == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "to and message are required"})
		return
	}
	if body.From == "" {
		body.From = "gateway"
	}

	if _, err := store.Ensure(s.effectiveDataDir()); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	labels := []string{"msg", "to:" + body.To, "from:" + body.From}
	if body.Ref != "" {
		labels = append(labels, "ref:"+body.Ref)
	}

	id, err := store.CreateBead(store.CreateOpts{
		Title:    body.Message,
		Priority: body.Priority,
		Type:     beads.IssueType("message"),
		Prefix:   "spi",
		Labels:   labels,
		Parent:   body.Thread,
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"id": id})
}

// handleMessageByID routes POST /api/v1/messages/{id}/read
func (s *Server) handleMessageByID(w http.ResponseWriter, r *http.Request) {
	// Path: /api/v1/messages/{id}/read  or  /api/v1/messages/{id}
	rest := pathSuffix(r.URL.Path, "/api/v1/messages/")
	parts := strings.SplitN(rest, "/", 2)
	id := parts[0]
	action := ""
	if len(parts) == 2 {
		action = parts[1]
	}
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "message ID required"})
		return
	}
	if action == "read" && r.Method == http.MethodPost {
		s.markRead(w, r, id)
		return
	}
	writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
}

func (s *Server) markRead(w http.ResponseWriter, _ *http.Request, id string) {
	if _, err := store.Ensure(s.effectiveDataDir()); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if err := store.CloseBead(id); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": id, "status": "read"})
}

// --------------------------------------------------------------------------
// /api/v1/board handler
// --------------------------------------------------------------------------

func (s *Server) handleBoard(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if _, err := store.Ensure(s.effectiveDataDir()); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	opts := board.Opts{}
	result, err := board.FetchBoard(opts, "")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, result.Columns.ToJSON(nil))
}

// --------------------------------------------------------------------------
// /api/v1/roster handler
// --------------------------------------------------------------------------

func (s *Server) handleRoster(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if _, err := store.Ensure(s.effectiveDataDir()); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	timeout := 15 * time.Minute

	// Try k8s first, then local wizards, then beads-based.
	var agents []board.RosterAgent
	if a, err := board.RosterFromK8s(timeout); err == nil && len(a) > 0 {
		agents = a
	} else {
		rosterDeps := board.RosterDeps{
			LoadWizardRegistry: func() []board.LocalAgent { return nil },
			SaveWizardRegistry: func([]board.LocalAgent) {},
			CleanDeadWizards:   func(a []board.LocalAgent) []board.LocalAgent { return a },
			ProcessAlive:       dolt.ProcessAlive,
		}
		if local := board.RosterFromLocalWizards(timeout, rosterDeps); len(local) > 0 {
			agents = local
		} else {
			agents = board.RosterFromBeads(timeout)
		}
	}

	agents = board.EnrichRosterAgents(agents)
	summary := board.BuildSummary(agents, timeout)
	writeJSON(w, http.StatusOK, summary)
}

// --------------------------------------------------------------------------
// Helpers
// --------------------------------------------------------------------------

// handleTower answers GET /api/v1/tower with the active tower's identity
// so the desktop header can show the real tower name / deploy mode / dolt
// URL without hard-coding. Returns a sparse object when no tower is
// configured so the client falls back gracefully.
func (s *Server) handleTower(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	out := map[string]string{
		"version": Version,
	}
	if tower, err := config.ResolveTowerConfig(); err == nil && tower != nil {
		out["name"] = tower.Name
		out["prefix"] = tower.HubPrefix
		out["database"] = tower.Database
		out["deploy_mode"] = string(tower.EffectiveDeploymentMode())
		out["dolt_url"] = tower.DolthubRemote
		out["archmage"] = tower.Archmage.Name
	}
	writeJSON(w, http.StatusOK, out)
}

// getBeadComments answers GET /api/v1/beads/{id}/comments — the full
// comment thread in insertion order, newest last. The CLI surfaces the
// same list via `spire focus`; this is how the desktop's Comments tab
// hydrates.
func (s *Server) getBeadComments(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if _, err := store.Ensure(s.effectiveDataDir()); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	comments, err := store.GetComments(id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, comments)
}

// handleRepos answers GET /api/v1/repos with every registered repo in
// the active tower: prefix, repo URL, branch, language. Desktop uses
// this to offer a prefix picker in the file-bead dialog so new beads
// land under the right repo (spi / spd / oo) instead of the tower default.
func (s *Server) handleRepos(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	cfg, err := config.Load()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	activeTower := ""
	if t, err := config.ResolveTowerConfigWith(cfg); err == nil && t != nil {
		activeTower = t.Name
	}
	type repoRow struct {
		Prefix   string `json:"prefix"`
		Path     string `json:"path"`
		Database string `json:"database"`
		Tower    string `json:"tower"`
	}
	var rows []repoRow
	for _, inst := range cfg.Instances {
		if inst == nil {
			continue
		}
		// Only include repos bound to the active tower (or tower-agnostic).
		if inst.Tower != "" && inst.Tower != activeTower {
			continue
		}
		rows = append(rows, repoRow{
			Prefix:   inst.Prefix,
			Path:     inst.Path,
			Database: inst.Database,
			Tower:    inst.Tower,
		})
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"active_tower": activeTower,
		"repos":        rows,
	})
}

// handleBlocked answers GET /api/v1/blocked with the ID set of open beads
// that have unresolved blocking dependencies. The desktop uses this to
// split its OPEN column into READY (unblocked) vs BLOCKED (waiting).
func (s *Server) handleBlocked(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if _, err := store.Ensure(s.effectiveDataDir()); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	blocked, err := store.GetBlockedIssues(beads.WorkFilter{})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	ids := make([]string, 0, len(blocked))
	for _, b := range blocked {
		ids = append(ids, b.ID)
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"count": len(ids),
		"ids":   ids,
	})
}

// handleCleanupStepBeads answers POST /api/v1/cleanup/step-beads and
// closes every open step bead whose parent (task/epic/feature/bug/chore)
// is already closed. Step beads are internal phase markers — once the
// parent work is done they're just noise on the board.
func (s *Server) handleCleanupStepBeads(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if _, err := store.Ensure(s.effectiveDataDir()); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	all, err := store.ListBoardBeads(beads.IssueFilter{
		ExcludeStatus: []beads.Status{"__none__"},
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	byID := make(map[string]store.BoardBead, len(all))
	for _, b := range all {
		byID[b.ID] = b
	}
	closedTypes := map[string]bool{
		"task": true, "epic": true, "feature": true,
		"bug": true, "chore": true, "design": true,
	}
	var closedIDs []string
	for _, b := range all {
		if b.Type != "step" || b.Status == "closed" {
			continue
		}
		p, ok := byID[b.Parent]
		if !ok || p.Status != "closed" || !closedTypes[p.Type] {
			continue
		}
		if err := store.UpdateBead(b.ID, map[string]interface{}{"status": "closed"}); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{
				"error": fmt.Sprintf("closing %s: %s", b.ID, err.Error()),
			})
			return
		}
		closedIDs = append(closedIDs, b.ID)
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"closed_count": len(closedIDs),
		"closed_ids":   closedIDs,
	})
}

// getBeadLineage answers GET /api/v1/beads/{id}/lineage with the transitive
// closure of "what this bead came from": walks every outgoing dependency
// (parent-child, discovered-from, blocks, caused-by, related, supersedes)
// up to depth 5, then returns the node set and typed edges so the desktop
// Graph view can render "where did this bug come from?" provenance.
func (s *Server) getBeadLineage(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if _, err := store.Ensure(s.effectiveDataDir()); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	target, err := store.GetBead(id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}

	type edge struct {
		From string `json:"from"`
		To   string `json:"to"`
		Type string `json:"type"`
	}
	nodes := map[string]store.Bead{id: target}
	var edges []edge
	visited := map[string]bool{id: true}

	const maxDepth = 5
	var walk func(beadID string, depth int)
	walk = func(beadID string, depth int) {
		if depth >= maxDepth {
			return
		}
		deps, err := store.GetDepsWithMeta(beadID)
		if err != nil {
			return
		}
		for _, dep := range deps {
			edges = append(edges, edge{
				From: beadID,
				To:   dep.ID,
				Type: string(dep.DependencyType),
			})
			if visited[dep.ID] {
				continue
			}
			visited[dep.ID] = true
			if b, err := store.GetBead(dep.ID); err == nil {
				nodes[dep.ID] = b
			}
			walk(dep.ID, depth+1)
		}
	}
	walk(id, 0)

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"id":     id,
		"target": target,
		"nodes":  nodes,
		"edges":  edges,
	})
}

// getBeadLogs answers GET /api/v1/beads/{id}/logs with every wizard /
// apprentice / sage log the TUI inspector surfaces — name, full content,
// reset-cycle tag, and whether a sidecar stderr file exists. The desktop
// Logs tab renders this list grouped by Cycle the same way the TUI does.
func (s *Server) getBeadLogs(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if _, err := store.Ensure(s.effectiveDataDir()); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	// Inspector needs a BoardBead — fetch via ListBoardBeads filtered to this id.
	listed, err := store.ListBoardBeads(beads.IssueFilter{
		IDPrefix:      id,
		ExcludeStatus: []beads.Status{"__none__"},
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	var target *store.BoardBead
	for i := range listed {
		if listed[i].ID == id {
			target = &listed[i]
			break
		}
	}
	if target == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "bead not found"})
		return
	}
	data := board.FetchInspectorData(*target)
	// Project LogView to a JSON-friendly shape; stderr content stays on the
	// server (we ship names only; desktop will later fetch content on demand).
	type logRow struct {
		Name    string `json:"name"`
		Path    string `json:"path"`
		Cycle   int    `json:"cycle"`
		Size    int    `json:"size"`
		Content string `json:"content"`
	}
	out := make([]logRow, 0, len(data.Logs))
	for _, lv := range data.Logs {
		out = append(out, logRow{
			Name:    lv.Name,
			Path:    lv.Path,
			Cycle:   lv.Cycle,
			Size:    len(lv.Content),
			Content: lv.Content,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleTowers answers GET /api/v1/towers with every tower config on disk
// plus the currently-active one. Consumed by the desktop's tower picker;
// switching still requires `spire tower use <name>` from the CLI because
// the gateway process is bound to one dolt/config at boot.
func (s *Server) handleTowers(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	towers, _ := config.ListTowerConfigs()
	active := ""
	if t, err := config.ResolveTowerConfig(); err == nil && t != nil {
		active = t.Name
	}
	type towerRow struct {
		Name     string `json:"name"`
		Prefix   string `json:"prefix"`
		Database string `json:"database"`
		Active   bool   `json:"active"`
	}
	out := struct {
		Active string     `json:"active"`
		Towers []towerRow `json:"towers"`
	}{Active: active}
	for _, t := range towers {
		out.Towers = append(out.Towers, towerRow{
			Name:     t.Name,
			Prefix:   t.HubPrefix,
			Database: t.Database,
			Active:   t.Name == active,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// effectiveDataDir returns dataDir if set, otherwise falls back to BEADS_DIR env.
func (s *Server) effectiveDataDir() string {
	if s.dataDir != "" {
		return s.dataDir
	}
	return os.Getenv("BEADS_DIR")
}

// pathSuffix strips a URL path prefix and returns the remaining segment(s).
func pathSuffix(path, prefix string) string {
	return strings.TrimPrefix(path, prefix)
}

// writeJSON encodes v as JSON and writes it to w with the given status code.
func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		// Header already written — best effort.
		fmt.Fprintf(w, `{"error":"encode error: %s"}`, err)
	}
}
