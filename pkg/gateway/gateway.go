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
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/awell-health/spire/pkg/agent"
	"github.com/awell-health/spire/pkg/board"
	closepkg "github.com/awell-health/spire/pkg/close"
	"github.com/awell-health/spire/pkg/config"
	"github.com/awell-health/spire/pkg/dolt"
	"github.com/awell-health/spire/pkg/graph"
	"github.com/awell-health/spire/pkg/metrics/report"
	"github.com/awell-health/spire/pkg/olap"
	"github.com/awell-health/spire/pkg/process"
	"github.com/awell-health/spire/pkg/reset"
	"github.com/awell-health/spire/pkg/store"
	"github.com/awell-health/spire/pkg/summon"
	"github.com/awell-health/spire/pkg/trace"
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
	mux.Handle("/api/v1/metrics", s.corsMiddleware(s.bearerAuth(s.handleMetrics)))
	mux.Handle("/api/v1/workshop/formulas", s.corsMiddleware(s.bearerAuth(s.handleWorkshopFormulas)))
	mux.Handle("/api/v1/workshop/formulas/", s.corsMiddleware(s.bearerAuth(s.handleWorkshopFormulaByName)))
	mux.Handle("/api/v1/attempts/", s.corsMiddleware(s.bearerAuth(s.handleAttemptByID)))

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

// bearerAuth validates the Authorization: Bearer <token> header and then
// resolves the calling archmage identity from X-Archmage-Name /
// X-Archmage-Email headers (or the cluster tower's static archmage as a
// fallback). The resolved ArchmageIdentity is stashed on the request
// context so downstream handlers can read it via IdentityFromContext.
//
// If SPIRE_API_TOKEN is empty, skips validation (dev mode).
//
// Header trust boundary: identity headers are trusted only because the
// bearer authenticated the desktop. Identity is parsed AFTER the bearer
// check so an unauthenticated request never has its claimed identity
// recorded.
func (s *Server) bearerAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.apiToken == "" {
			// Dev mode: no token required. Identity headers are still
			// honoured so end-to-end tests against a tokenless gateway
			// can exercise the same audit paths.
			s.devModeLogOnce.Do(func() {
				s.log.Println("[gateway] dev mode: skipping auth on /api/v1/* requests")
			})
			next(w, attachIdentity(r))
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
		next(w, attachIdentity(r))
	}
}

// attachIdentity resolves the request's archmage identity (header or
// cluster-tower fallback) and returns r with the result stashed on the
// context. Pulled out of bearerAuth so the dev-mode and authenticated
// branches share the same resolution logic.
func attachIdentity(r *http.Request) *http.Request {
	id := resolveRequestIdentity(r, towerArchmageFallback)
	if id.Source == "" {
		return r
	}
	return r.WithContext(WithIdentity(r.Context(), id))
}

// towerArchmageFallback returns the cluster tower's static archmage as an
// ArchmageIdentity so resolveRequestIdentity can fall back to it when the
// caller did not supply X-Archmage-* headers. Wrapped as a package var so
// tests can inject a deterministic fallback without touching real config.
var towerArchmageFallback = func() ArchmageIdentity {
	tower, err := config.ResolveTowerConfig()
	if err != nil || tower == nil {
		return ArchmageIdentity{}
	}
	return ArchmageIdentity{
		Name:  tower.Archmage.Name,
		Email: tower.Archmage.Email,
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

	// Stamp the calling archmage onto the bead's labels so `bd show` and the
	// roster can attribute the bead back to whoever filed it. Labels keep
	// the audit trail auditable through plain bead reads — no schema change
	// to issues / agent_runs is needed for v1 of this attribution.
	labels := body.Labels
	var author string
	if ident, ok := IdentityFromContext(r.Context()); ok && ident.Name != "" {
		labels = appendArchmageLabels(labels, ident)
		author = ident.AuthorString()
	}

	id, err := store.CreateBead(store.CreateOpts{
		Title:       body.Title,
		Description: body.Description,
		Priority:    body.Priority,
		Type:        issType,
		Labels:      labels,
		Parent:      body.Parent,
		Prefix:      body.Prefix,
		Author:      author,
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"id": id})
}

// appendArchmageLabels stamps the calling archmage onto the bead's labels
// so subsequent reads can attribute creation/origin back to the right
// person. We only add labels not already present so callers that pre-stamp
// (e.g. tests) don't end up with duplicates.
func appendArchmageLabels(labels []string, id ArchmageIdentity) []string {
	out := labels
	want := []string{"archmage:" + id.Name}
	if id.Email != "" {
		want = append(want, "archmage-email:"+id.Email)
	}
	for _, w := range want {
		if !containsLabel(out, w) {
			out = append(out, w)
		}
	}
	return out
}

func containsLabel(labels []string, want string) bool {
	for _, l := range labels {
		if l == want {
			return true
		}
	}
	return false
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
		beadID := strings.TrimSuffix(id, "/comments")
		switch r.Method {
		case http.MethodGet:
			s.getBeadComments(w, r, beadID)
		case http.MethodPost:
			s.postBeadComment(w, r, beadID)
		default:
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		}
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
	// /api/v1/beads/{id}/trace
	if strings.HasSuffix(id, "/trace") {
		s.getBeadTrace(w, r, strings.TrimSuffix(id, "/trace"))
		return
	}
	// /api/v1/beads/{id}/graph
	if strings.HasSuffix(id, "/graph") {
		s.getBeadGraph(w, r, strings.TrimSuffix(id, "/graph"))
		return
	}
	// /api/v1/beads/{id}/summon
	if strings.HasSuffix(id, "/summon") {
		s.handleBeadSummon(w, r, strings.TrimSuffix(id, "/summon"))
		return
	}
	// /api/v1/beads/{id}/ready
	if strings.HasSuffix(id, "/ready") {
		s.handleBeadReady(w, r, strings.TrimSuffix(id, "/ready"))
		return
	}
	// /api/v1/beads/{id}/reset
	if strings.HasSuffix(id, "/reset") {
		s.handleBeadReset(w, r, strings.TrimSuffix(id, "/reset"))
		return
	}
	// /api/v1/beads/{id}/close
	if strings.HasSuffix(id, "/close") {
		s.handleBeadClose(w, r, strings.TrimSuffix(id, "/close"))
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

// Recursive CTE queries for parent-child tree walks. Both walk the
// dependencies table (type='parent-child') to an absolute depth cap of 10,
// with a row cap of 2000. Indexes on (depends_on_id, type) and (issue_id)
// make this single-digit-ms against a 6k-row tower. The LEFT JOIN + subquery
// to resolve the parent uses LIMIT 1 to defend against stray duplicate
// parent-child edges.
const treeDescendantsSQL = `
WITH RECURSIVE walk AS (
  SELECT d.issue_id AS id, 1 AS depth
    FROM dependencies d
   WHERE d.depends_on_id = ? AND d.type = 'parent-child'
  UNION ALL
  SELECT d.issue_id, w.depth + 1
    FROM dependencies d JOIN walk w ON d.depends_on_id = w.id
   WHERE d.type = 'parent-child' AND w.depth < 10
)
SELECT i.id, i.title, i.description, i.status, i.priority, i.issue_type,
       COALESCE((SELECT p.depends_on_id FROM dependencies p
                   WHERE p.issue_id = i.id AND p.type = 'parent-child' LIMIT 1), '') AS parent,
       DATE_FORMAT(i.updated_at, '%Y-%m-%dT%H:%i:%sZ') AS updated_at
  FROM walk w
  JOIN issues i ON i.id = w.id
 ORDER BY w.depth, i.id
 LIMIT 2000`

const treeAncestorsSQL = `
WITH RECURSIVE walk AS (
  SELECT d.depends_on_id AS id, 1 AS depth
    FROM dependencies d
   WHERE d.issue_id = ? AND d.type = 'parent-child'
  UNION ALL
  SELECT d.depends_on_id, w.depth + 1
    FROM dependencies d JOIN walk w ON d.issue_id = w.id
   WHERE d.type = 'parent-child' AND w.depth < 10
)
SELECT i.id, i.title, i.description, i.status, i.priority, i.issue_type,
       COALESCE((SELECT p.depends_on_id FROM dependencies p
                   WHERE p.issue_id = i.id AND p.type = 'parent-child' LIMIT 1), '') AS parent,
       DATE_FORMAT(i.updated_at, '%Y-%m-%dT%H:%i:%sZ') AS updated_at
  FROM walk w
  JOIN issues i ON i.id = w.id
 ORDER BY w.depth DESC, i.id
 LIMIT 2000`

// queryWalk runs a recursive tree-walk query bound to a single bead id and
// scans the result rows into store.Bead values. Labels / Metadata /
// Dependencies are left empty — tree/lineage clients consume only the
// lightweight fields.
func queryWalk(db *sql.DB, query, id string) ([]store.Bead, error) {
	rows, err := db.Query(query, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]store.Bead, 0, 32)
	for rows.Next() {
		var b store.Bead
		if err := rows.Scan(&b.ID, &b.Title, &b.Description, &b.Status, &b.Priority, &b.Type, &b.Parent, &b.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// getBeadTree answers GET /api/v1/beads/{id}/tree with the selected bead's
// full ancestor chain (root → target) and descendant subtree (target → leaves).
//
// Earlier implementations either loaded every bead in the tower (ListBoardBeads,
// 40–70s) or did a Go-side BFS with one SQL round-trip per level (~4s on a
// deep tree). This version issues two recursive CTE queries against the raw
// *sql.DB, which returns in tens of ms even for root-level epics.
func (s *Server) getBeadTree(w http.ResponseWriter, _ *http.Request, id string) {
	if _, err := store.Ensure(s.effectiveDataDir()); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	root, err := store.GetBead(id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}
	db, ok := store.ActiveDB()
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "active db not initialised"})
		return
	}
	ancestors, err := queryWalk(db, treeAncestorsSQL, id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	descendants, err := queryWalk(db, treeDescendantsSQL, id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
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
	// Resolve From with header-wins semantics: when the calling archmage
	// supplied X-Archmage-Name, it overrides any conflicting body.From so
	// an over-eager client can't impersonate a different sender. When the
	// body picks a non-archmage value (e.g. "daemon") and no header is
	// present, we keep the body value — matches the existing CLI/daemon
	// behaviour. The "gateway" literal stays as the last-resort fallback.
	ident, hasIdent := IdentityFromContext(r.Context())
	if hasIdent && ident.Name != "" {
		if body.From != "" && body.From != ident.Name {
			s.log.Printf("[gateway] message from-field collision: body=%q header=%q — header wins", body.From, ident.Name)
		}
		body.From = ident.Name
	} else if body.From == "" {
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

	var author string
	if hasIdent && ident.Name != "" && ident.Email != "" {
		author = ident.AuthorString()
	}

	id, err := store.CreateBead(store.CreateOpts{
		Title:    body.Message,
		Priority: body.Priority,
		Type:     beads.IssueType("message"),
		Prefix:   "spi",
		Labels:   labels,
		Parent:   body.Thread,
		Author:   author,
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

	// SkipLocalConflictCheck=true: this server has no writable local
	// Dolt mirror — backend Dolt lives in the cluster, and conflict
	// resolution there is the operator's concern, not a per-HTTP-request
	// warning. Leaving the check on fork-execs `dolt sql` on every
	// /api/v1/board hit and accumulates zombies in the gateway pod.
	// See docs/k8s-v1-punchlist.md item #6.
	opts := board.Opts{SkipLocalConflictCheck: true}
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

	timeout := 15 * time.Minute

	// Dispatch on the active tower's deployment mode rather than on
	// kubectl reachability + registry presence. The previous cascade
	// (k8s → local wizards → agent-labeled beads) silently surfaced
	// stale registration ghosts — spi-jpurm / spi-nbwrdw / spi-nw5d95
	// — when wizards.json reads returned empty, even on towers with
	// no cluster involvement (spi-rx6bf6). Same switch shape as
	// cmdSummon / cmdDismiss (spi-jsxa3v) and the steward orphan
	// gate (spi-40rtru).
	tower, err := resolveTowerForRosterFunc()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	mode := tower.EffectiveDeploymentMode()
	agents, err := board.LiveRoster(r.Context(), mode, timeout, defaultRosterDeps())
	if err != nil {
		if errors.Is(err, board.ErrAttachedRosterNotImplemented) {
			writeJSON(w, http.StatusNotImplemented, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	agents = board.EnrichRosterAgents(agents)
	summary := board.BuildSummary(agents, timeout)
	writeJSON(w, http.StatusOK, summary)
}

// resolveTowerForRosterFunc is the indirection used by handleRoster so
// tests can drive dispatch through a fake tower config without
// touching the real config dir. Production callers leave this alone.
var resolveTowerForRosterFunc = config.ResolveTowerConfig

// defaultRosterDeps builds the RosterDeps used by handleRoster's
// local-native branch. Wires the on-disk wizard registry (via
// pkg/agent's race-safe RegistryList) and the process-alive probe so
// RosterFromLocalWizards can surface in-flight wizards. The error-
// surfacing variant replaces the deprecated agent.LoadRegistry path
// so transient JSON parse / FS errors no longer silently masquerade
// as an empty registry (spi-rx6bf6). Extracted from handleRoster so
// the closures are unit-testable against a temp-dir-backed registry
// without booting an HTTP handler.
func defaultRosterDeps() board.RosterDeps {
	return board.RosterDeps{
		LoadWizardRegistry: func() ([]board.LocalAgent, error) {
			return agent.RegistryList()
		},
		SaveWizardRegistry: func(agents []board.LocalAgent) {
			agent.SaveRegistry(agent.Registry{Wizards: agents})
		},
		CleanDeadWizards: func(agents []board.LocalAgent) []board.LocalAgent {
			return cleanDeadLocalWizards(agents, process.ProcessAlive)
		},
		ProcessAlive:    dolt.ProcessAlive,
		ResolveArchmage: archmageFromTower,
	}
}

// archmageFromTower returns the archmage name that owns the wizard's
// tower. Used by defaultRosterDeps so /api/v1/roster can surface
// per-archmage origin for local-native wizards. Each desktop runs its own
// wizards under its own archmage, so the local TowerConfig.Archmage.Name
// is the right attribution; cluster-mode rows get their archmage from pod
// labels in RosterFromClusterRegistry instead.
//
// Errors and empty fields fail closed to "" (no attribution) so a missing
// or stub tower config never poisons the audit trail.
func archmageFromTower(_ board.LocalAgent) string {
	tower, err := config.ResolveTowerConfig()
	if err != nil || tower == nil {
		return ""
	}
	return tower.Archmage.Name
}

// cleanDeadLocalWizards returns the subset of agents whose PID is alive
// per pidAlive. Entries with PID <= 0 are also dropped. Extracted so the
// filter logic is unit-testable with a fake pidAlive probe.
func cleanDeadLocalWizards(agents []board.LocalAgent, pidAlive func(int) bool) []board.LocalAgent {
	var live []board.LocalAgent
	for _, w := range agents {
		if w.PID > 0 && pidAlive(w.PID) {
			live = append(live, w)
		}
	}
	return live
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

// postBeadComment answers POST /api/v1/beads/{id}/comments — appends a
// comment authored by Actor() (defaults to "spire") and returns the new
// comment id. Used by the desktop Comments-tab composer and the
// CloseBeadModal optional-farewell flow: before this handler existed the
// modal's comment POST failed with 405 and aborted the follow-up status
// flip so beads couldn't be closed with a comment from the desktop.
func (s *Server) postBeadComment(w http.ResponseWriter, r *http.Request, id string) {
	var body struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
		return
	}
	if strings.TrimSpace(body.Text) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "text is required"})
		return
	}
	if err := commentsStoreEnsureFunc(s.effectiveDataDir()); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	// Use the calling archmage as the comment author when the request
	// supplied an identity. Empty author falls through to the existing
	// "spire" Actor() default in commentsAddFunc — preserves direct-mode
	// behaviour for any non-gateway callers that share this seam.
	var author string
	if ident, ok := IdentityFromContext(r.Context()); ok {
		author = ident.AuthorString()
	}
	commentID, err := commentsAddAsFunc(id, author, body.Text)
	if err != nil {
		status := http.StatusInternalServerError
		if strings.Contains(err.Error(), "not found") {
			status = http.StatusNotFound
		}
		writeJSON(w, status, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"id": commentID})
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

// metricsReaderFactory returns a report.Reader and a cleanup func
// the handler defers. The default production implementation opens a
// DuckDB file via the active tower config. Tests overwrite this to
// return a fake Reader so handler branches can be exercised without
// a real DB.
var metricsReaderFactory = func() (report.Reader, func(), error) {
	tc, err := config.ActiveTowerConfig()
	if err != nil {
		return nil, nil, err
	}
	adb, err := olap.Open(tc.OLAPPath())
	if err != nil {
		return nil, nil, err
	}
	return report.NewSQLReader(adb.SqlDB()), func() { adb.Close() }, nil
}

// metricsBuild is the indirection through which handleMetrics calls
// report.Build — swapped in tests so handler-level branches (query
// parsing, error mapping) can be exercised without a real OLAP.
var metricsBuild = report.Build

// handleMetrics answers GET /api/v1/metrics with the full dashboard
// payload consumed by spire-desktop's MetricsView. Query params:
//
//	scope=all|<prefix>         — default: all
//	window=24h|7d|30d|90d|custom — default: 7d
//	aspirational=true|false    — default: false
//	since=<RFC3339>            — required when window=custom
//	until=<RFC3339>            — required when window=custom
//
// Status codes:
//   - 200 on success (MetricsResponse body).
//   - 400 on invalid query params (bad window, missing since/until).
//   - 503 when the OLAP database is unreachable — the frontend falls
//     back to its bundled fixture on non-200, so this keeps the UI
//     usable when DuckDB isn't yet wired up.
//   - 500 on unexpected query errors.
//
// Each request opens a fresh *olap.DB handle so DuckDB's file lock
// doesn't leak between callers; at the 60s poll cadence the per-
// request open cost is negligible.
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	q := r.URL.Query()

	scope := report.ParseScope(q.Get("scope"))
	win, err := report.ParseWindow(q.Get("window"), q.Get("since"), q.Get("until"), time.Now().UTC())
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	aspirational := false
	if v := strings.ToLower(q.Get("aspirational")); v == "true" || v == "1" || v == "yes" {
		aspirational = true
	}

	reader, cleanup, err := metricsReaderFactory()
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": fmt.Sprintf("metrics: OLAP unavailable (%v) — run 'spire up' to start services", err),
		})
		return
	}
	if cleanup != nil {
		defer cleanup()
	}

	opts := report.Options{
		Scope:        scope,
		Window:       win,
		Aspirational: aspirational,
		Now:          time.Now().UTC(),
	}
	resp, err := metricsBuild(r.Context(), reader, opts)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, resp)
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

// lineageEdge is a (from, to, type) triple used by both the upstream and
// downstream walks of getBeadLineage. JSON tags match the desktop
// Graph view contract.
type lineageEdge struct {
	From string `json:"from"`
	To   string `json:"to"`
	Type string `json:"type"`
}

// Recursive CTE queries for bidirectional lineage walks. Depth cap 20,
// row cap 5000 per direction — both safety rails against runaway joins
// or adversarial cycles. Dedupe happens in Go after scan since MySQL
// recursive UNION ALL can emit duplicate rows when multiple paths reach
// the same node.
const lineageUpstreamSQL = `
WITH RECURSIVE up AS (
  SELECT d.issue_id AS from_id, d.depends_on_id AS to_id, d.type AS dep_type, 1 AS depth
    FROM dependencies d
   WHERE d.issue_id = ?
  UNION ALL
  SELECT d.issue_id, d.depends_on_id, d.type, up.depth + 1
    FROM dependencies d JOIN up ON d.issue_id = up.to_id
   WHERE up.depth < 20
)
SELECT from_id, to_id, dep_type FROM up LIMIT 5000`

const lineageDownstreamSQL = `
WITH RECURSIVE down AS (
  SELECT d.issue_id AS from_id, d.depends_on_id AS to_id, d.type AS dep_type, 1 AS depth
    FROM dependencies d
   WHERE d.depends_on_id = ?
  UNION ALL
  SELECT d.issue_id, d.depends_on_id, d.type, down.depth + 1
    FROM dependencies d JOIN down ON d.depends_on_id = down.from_id
   WHERE down.depth < 20
)
SELECT from_id, to_id, dep_type FROM down LIMIT 5000`

// queryLineageEdges runs one of the lineage CTE queries bound to a bead id
// and scans the result rows into lineageEdge values. Callers are responsible
// for deduping — recursive UNION ALL can emit duplicates on diamond graphs.
func queryLineageEdges(db *sql.DB, query, id string) ([]lineageEdge, error) {
	rows, err := db.Query(query, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]lineageEdge, 0, 32)
	for rows.Next() {
		var e lineageEdge
		if err := rows.Scan(&e.From, &e.To, &e.Type); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// dedupeLineageEdges returns the unique edges in input order, keyed on
// (from, to, type).
func dedupeLineageEdges(in []lineageEdge) []lineageEdge {
	seen := make(map[[3]string]struct{}, len(in))
	out := make([]lineageEdge, 0, len(in))
	for _, e := range in {
		k := [3]string{e.From, e.To, e.Type}
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, e)
	}
	return out
}

// getBeadLineage answers GET /api/v1/beads/{id}/lineage with the bidirectional
// transitive closure of this bead's dependency graph:
//
//   - upstream_edges: what this bead depends on (what it came from)
//   - downstream_edges: what depends on this bead (what came out of it)
//
// Both walks cover every dependency type (parent-child, discovered-from,
// blocks, caused-by, related, supersedes) to a depth cap of 20 and a row
// cap of 5000 per direction. The response includes an `edges` alias for
// `upstream_edges` held for one release so CLI/desktop clients can migrate
// smoothly.
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

	db, ok := store.ActiveDB()
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "active db not initialised"})
		return
	}

	upRaw, err := queryLineageEdges(db, lineageUpstreamSQL, id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	downRaw, err := queryLineageEdges(db, lineageDownstreamSQL, id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	upstreamEdges := dedupeLineageEdges(upRaw)
	downstreamEdges := dedupeLineageEdges(downRaw)

	nodes := map[string]store.Bead{id: target}
	nodeIDs := map[string]bool{id: true}
	for _, e := range upstreamEdges {
		nodeIDs[e.From] = true
		nodeIDs[e.To] = true
	}
	for _, e := range downstreamEdges {
		nodeIDs[e.From] = true
		nodeIDs[e.To] = true
	}

	// Batch-fetch beads for all referenced node ids (minus the target, which
	// is already in `nodes`). Single SELECT ... WHERE id IN (...).
	var missing []string
	for nid := range nodeIDs {
		if nid == id {
			continue
		}
		missing = append(missing, nid)
	}
	if len(missing) > 0 {
		placeholders := strings.TrimRight(strings.Repeat("?,", len(missing)), ",")
		q := `SELECT i.id, i.title, i.description, i.status, i.priority, i.issue_type,
		             COALESCE((SELECT p.depends_on_id FROM dependencies p
		                         WHERE p.issue_id = i.id AND p.type = 'parent-child' LIMIT 1), '') AS parent,
		             DATE_FORMAT(i.updated_at, '%Y-%m-%dT%H:%i:%sZ') AS updated_at
		        FROM issues i WHERE i.id IN (` + placeholders + `)`
		args := make([]interface{}, len(missing))
		for i, v := range missing {
			args[i] = v
		}
		nrows, err := db.Query(q, args...)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		defer nrows.Close()
		for nrows.Next() {
			var b store.Bead
			if err := nrows.Scan(&b.ID, &b.Title, &b.Description, &b.Status, &b.Priority, &b.Type, &b.Parent, &b.UpdatedAt); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
				return
			}
			nodes[b.ID] = b
		}
		if err := nrows.Err(); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"id":               id,
		"target":           target,
		"nodes":            nodes,
		"upstream_edges":   upstreamEdges,
		"downstream_edges": downstreamEdges,
		"edges":            upstreamEdges,
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

// traceCollect is the package-level indirection through which
// getBeadTrace calls pkg/trace.Collect. Kept as a var so handler tests
// can inject a fake collector without spinning up a real dolt store.
// Mirrors the store.GetActiveAttemptFunc seam.
var traceCollect = trace.Collect

// getBeadTrace answers GET /api/v1/beads/{id}/trace with the composite
// pipeline + totals + active-agent + log-tail view the desktop's TRACE
// tab consumes. The shape is locked on pkg/trace.Data (see that package's
// doc for field semantics); this handler is a thin HTTP wrapper around
// trace.Collect with query-param parsing and error-to-status mapping.
//
// Query params:
//   - tail=N (default 200, clamped to [0, MaxTailLines]) — 0 returns an
//     empty log_tail array, matching the "no tail requested" semantics.
//
// Status codes:
//   - 200 with empty-shape JSON when the bead exists but has never run.
//   - 404 when the bead ID does not resolve (pkg/trace.NotFoundError).
//   - 500 only for unexpected store/collection failures.
func (s *Server) getBeadTrace(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	// store.Ensure is not called explicitly here: trace.Collect walks the
	// store via getStore(), which auto-ensures through BeadsDirResolver.
	// Keeping the handler lean also makes it unit-testable without a real
	// dolt via the traceCollect indirection.
	tail := trace.DefaultTailLines
	if v := r.URL.Query().Get("tail"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			// Bad input silently clamps to the default rather than 400 —
			// the desktop's pollers prefer a usable response over an
			// error when the user edits the URL by hand.
			n = trace.DefaultTailLines
		}
		if n > trace.MaxTailLines {
			n = trace.MaxTailLines
		}
		tail = n
	}
	data, err := traceCollect(id, trace.Options{Tail: tail})
	if err != nil {
		var nf *trace.NotFoundError
		if errors.As(err, &nf) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": nf.Error()})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, data)
}

// graphCollect is the package-level indirection through which getBeadGraph
// calls pkg/graph.Collect. Kept as a var so handler tests can inject a fake
// collector without spinning up a real dolt store. Mirrors traceCollect.
var graphCollect = graph.Collect

// getBeadGraph answers GET /api/v1/beads/{id}/graph with the descendant
// subgraph the desktop's TRACE tab renders as a "you are here" map. Shape
// is locked on graph.GraphResponse — see that package's doc for field
// semantics.
//
// Query params:
//   - max_depth=N (default DefaultMaxDepth, clamped to [1, MaxMaxDepth]) —
//     bad input clamps to default rather than 400, matching the trace
//     handler's tolerance for hand-edited URLs. Values above MaxMaxDepth
//     return 400 because they would blow the latency budget; clamping
//     would silently lie about how much of the subgraph was walked.
//
// Status codes:
//   - 200 with the assembled response (single-node when the bead is a leaf).
//   - 400 when max_depth is above the server cap.
//   - 404 when the bead ID does not resolve (graph.NotFoundError).
//   - 500 only for unexpected store/query failures.
func (s *Server) getBeadGraph(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	// store.Ensure is not called explicitly here: graph.Collect walks the
	// store via getStore(), which auto-ensures through BeadsDirResolver.
	// Same pattern as getBeadTrace — keeps the handler unit-testable
	// without a real dolt via the graphCollect indirection.
	depth := graph.DefaultMaxDepth
	if v := r.URL.Query().Get("max_depth"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			n = graph.DefaultMaxDepth
		}
		depth = n
	}
	data, err := graphCollect(id, graph.Options{MaxDepth: depth})
	if err != nil {
		var nf *graph.NotFoundError
		if errors.As(err, &nf) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": nf.Error()})
			return
		}
		if errors.Is(err, graph.ErrMaxDepthExceeded) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, data)
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

// stewardAlive reports whether the steward daemon is running.
// Returns false when the PID file is missing, unparsable, or the PID is stale.
// summonRunner and stewardAliveFunc are package-level seams so tests can stub
// out the fork/exec path and the PID-file probe without standing up the real
// steward or dolt services. The ready* seams serve the same purpose for
// handleBeadReady — they let tests exercise the status-switch branches and
// audit-comment path without standing up a real beads store.
var (
	summonRunner = summon.Run

	stewardAliveFunc = func() bool {
		pid := process.ReadPID(dolt.StewardPIDPath())
		if pid <= 0 {
			return false
		}
		return process.ProcessAlive(pid)
	}

	readyStoreEnsureFunc = func(dir string) error {
		_, err := store.Ensure(dir)
		return err
	}
	readyGetBeadFunc    = store.GetBead
	readyUpdateBeadFunc = store.UpdateBead
	readyAddCommentFunc = store.AddCommentReturning

	commentsStoreEnsureFunc = func(dir string) error {
		_, err := store.Ensure(dir)
		return err
	}
	commentsAddFunc = store.AddCommentReturning
	// commentsAddAsFunc is the per-author indirection used by
	// postBeadComment. An empty author flows through store.Actor() ("spire")
	// inside AddCommentAsReturning so this seam preserves direct-mode
	// behaviour for any caller that shares it. Tests stub this var to
	// observe (id, author, text) without booting a real beads store.
	commentsAddAsFunc = store.AddCommentAsReturning
)

// handleBeadSummon answers POST /api/v1/beads/{id}/summon — spawns a wizard
// for the bead. Wraps the same code path as `spire summon <id>` so the
// archmage can kick off work from the desktop without dropping to a terminal.
//
// Request body (optional):
//
//	{ "dispatch": "sequential" | "wave" | "direct" }
//
// Responses:
//
//	200 { id, wizard, comment_id } — wizard spawned
//	400 { error }                 — bad body, bad status, already-running, etc.
//	404 { error }                 — bead not found
//	405                           — non-POST
//	412 { error }                 — steward not running (spire up missing)
//	500 { error }                 — spawn / IO error
func (s *Server) handleBeadSummon(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bead ID required"})
		return
	}

	var body struct {
		Dispatch string `json:"dispatch"`
	}
	// Empty body is OK — only reject malformed JSON.
	if r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
			return
		}
	}
	if err := summon.ValidateDispatch(body.Dispatch); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	// Steward precondition check runs before store.Ensure so that a missing
	// steward returns the well-known 412 even when dolt itself is down.
	if !stewardAliveFunc() {
		writeJSON(w, http.StatusPreconditionFailed, map[string]string{"error": "steward not running — run 'spire up'"})
		return
	}
	if _, err := store.Ensure(s.effectiveDataDir()); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	res, err := summonRunner(id, body.Dispatch)
	if err != nil {
		writeJSON(w, statusForSummonError(err), map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"id":         id,
		"wizard":     res.WizardName,
		"comment_id": res.CommentID,
	})
}

// statusForSummonError maps a summon.Run error to the HTTP status the gateway
// should return. Status-gate / already-running / bad-dispatch errors become
// 400; "not found" becomes 404; anything else becomes 500.
func statusForSummonError(err error) int {
	msg := err.Error()
	if errors.Is(err, summon.ErrAlreadyRunning) {
		return http.StatusConflict
	}
	if strings.Contains(msg, "not found") {
		return http.StatusNotFound
	}
	if strings.Contains(msg, "is closed") ||
		strings.Contains(msg, "is deferred") ||
		strings.Contains(msg, "is a design bead") ||
		strings.Contains(msg, "invalid dispatch mode") {
		return http.StatusBadRequest
	}
	return http.StatusInternalServerError
}

// handleBeadReady answers POST /api/v1/beads/{id}/ready — promotes an open
// bead into the ready queue so the steward picks it up on the next cycle.
// Mirrors `spire ready <id>` semantics. store.UpdateBead fires the canonical
// StampReady lifecycle event the steward's own dispatched→ready recovery
// would have fired, so the audit footprint matches the CLI/steward path; a
// short "promoted to ready" comment is also recorded to give the desktop a
// visible audit row for the action (parity with /summon).
//
// Responses:
//
//	200 { id, status: "ready", comment_id } — flipped (or already ready, idempotent)
//	400 { error }                           — wrong source status
//	404 { error }                           — bead not found
//	405                                     — non-POST
//	500 { error }                           — update error
func (s *Server) handleBeadReady(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bead ID required"})
		return
	}
	if err := readyStoreEnsureFunc(s.effectiveDataDir()); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	bead, err := readyGetBeadFunc(id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}

	switch bead.Status {
	case "open":
		// Proceed.
	case "ready":
		writeJSON(w, http.StatusOK, map[string]string{"id": id, "status": "ready"})
		return
	case "in_progress":
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("bead %s is already in progress", id)})
		return
	case "closed":
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("bead %s is closed", id)})
		return
	case "deferred":
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("bead %s is deferred — undefer it first", id)})
		return
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("bead %s has unexpected status %q", id, bead.Status)})
		return
	}

	if err := readyUpdateBeadFunc(id, map[string]interface{}{"status": "ready"}); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	commentID, cerr := readyAddCommentFunc(id, "promoted to ready")
	if cerr != nil {
		// Non-fatal: lifecycle stamp has already fired via UpdateBead.
		log.Printf("[gateway] ready audit comment for %s: %v", id, cerr)
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": id, "status": "ready", "comment_id": commentID})
}

// resetBeadRequest is the optional body for POST /api/v1/beads/{id}/reset.
// All fields are optional; v1 desktop sends an empty body and gets a default
// soft reset. The fields mirror the CLI's --to / --force / --set flags.
type resetBeadRequest struct {
	To    string            `json:"to,omitempty"`
	Force bool              `json:"force,omitempty"`
	Set   map[string]string `json:"set,omitempty"`
}

// resetBeadFunc is the package-level seam through which handleBeadReset
// dispatches into the soft-reset code path. cmd/spire wires reset.RunFunc
// in its init(); pkg/reset.ResetBead is the canonical entry point. The
// indirection exists so handler tests can swap in a recorder without
// booting the CLI.
var resetBeadFunc = reset.ResetBead

// resetStoreEnsureFunc verifies the server-side Dolt store is available before
// dispatching reset. Kept as a seam so handler tests can exercise reset routing
// without depending on a real server-side .beads directory.
var resetStoreEnsureFunc = func(dataDir string) error {
	_, err := store.Ensure(dataDir)
	return err
}

// resetTowerModeFunc resolves the active tower so handleBeadReset can
// short-circuit to 501 in TowerModeGateway. Held in a var so tests can
// inject a deterministic mode without touching real config.
var resetTowerModeFunc = func() (string, error) {
	tc, err := config.ResolveTowerConfig()
	if err != nil {
		return "", err
	}
	if tc == nil {
		return "", nil
	}
	return tc.Mode, nil
}

// handleBeadReset answers POST /api/v1/beads/{id}/reset — wraps the soft
// form of `spire reset <id>` so the desktop can unsummon an active wizard
// or send a ready bead back to open without dropping to the CLI. Mirrors
// the CLI semantics exactly: SIGTERM the wizard PID (5s grace + SIGKILL),
// remove the registry entry, strip interrupted:* / needs-human labels,
// unhook step children, walk the bead back via softResetV3.
//
// Body (all fields optional):
//
//	{"to": "<step>", "force": <bool>, "set": {"<step>.outputs.<key>": "<value>"}}
//
// Empty body is accepted (and is what v1 desktop sends).
//
// Responses:
//
//	200 ApiBead JSON                — post-reset bead
//	400 { error }                   — invalid JSON in body
//	404 { error }                   — bead not found
//	405                             — non-POST
//	409 { error }                   — bead in an invalid state for reset
//	500 { error }                   — unexpected reset failure
//	501 { error }                   — TowerModeGateway (cluster-mode reset
//	                                  is a follow-up bead; v1 desktop is
//	                                  local-only)
func (s *Server) handleBeadReset(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bead ID required"})
		return
	}

	// Cluster-mode short-circuit. v1 desktop does not support reset against a
	// remote cluster operator — the gateway can't reach the wizard pod
	// directly to SIGTERM it. The cluster path is filed as a follow-up bead
	// (spd-1lu5); returning 501 here is part of the acceptance criterion.
	mode, err := resetTowerModeFunc()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if mode == config.TowerModeGateway {
		writeJSON(w, http.StatusNotImplemented, map[string]string{
			"error": "reset not supported in cluster mode yet — see follow-up bead",
		})
		return
	}

	var body resetBeadRequest
	// Empty body is OK (v1 desktop sends no body) — only reject malformed JSON.
	if r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil &&
			!errors.Is(err, io.EOF) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON: " + err.Error()})
			return
		}
	}

	if err := resetStoreEnsureFunc(s.effectiveDataDir()); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	bead, err := resetBeadFunc(r.Context(), reset.Opts{
		BeadID: id,
		To:     body.To,
		Force:  body.Force,
		Set:    body.Set,
	})
	if err != nil {
		writeJSON(w, statusForResetError(err), map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, bead)
}

// closeBeadFunc is the package-level seam through which handleBeadClose
// dispatches into the close-lifecycle code path. cmd/spire wires
// close.RunFunc in its init(); pkg/close.RunLifecycle is the canonical
// entry point. The indirection exists so handler tests can swap in a
// recorder without booting the CLI.
var closeBeadFunc = closepkg.RunLifecycle

// closeStoreEnsureFunc verifies the server-side Dolt store is available
// before dispatching close. Kept as a seam so handler tests can exercise
// close routing without depending on a real server-side .beads directory.
var closeStoreEnsureFunc = func(dataDir string) error {
	_, err := store.Ensure(dataDir)
	return err
}

// handleBeadClose answers POST /api/v1/beads/{id}/close — runs the full
// `spire close` lifecycle (workflow-step children + label cleanup +
// caused-by alert cascade + parent close) server-side. Cluster-attached
// gateway-mode clients route through this endpoint because their local
// pkg/store cannot discover workflow-step children (GetChildren is
// gateway-unsupported).
//
// Empty body — the lifecycle takes only the bead ID.
//
// Responses:
//
//	200 { id, status: "closed" }   — close lifecycle completed
//	400 { error }                  — empty bead ID
//	404 { error }                  — bead not found
//	405                            — non-POST
//	500 { error }                  — close lifecycle failure
func (s *Server) handleBeadClose(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bead ID required"})
		return
	}
	if err := closeStoreEnsureFunc(s.effectiveDataDir()); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if err := closeBeadFunc(id); err != nil {
		writeJSON(w, statusForCloseError(err), map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": id, "status": "closed"})
}

// statusForCloseError maps a close-lifecycle error to the HTTP status the
// gateway should return. "Not found" becomes 404; anything else becomes
// 500.
func statusForCloseError(err error) int {
	if err == nil {
		return http.StatusOK
	}
	if strings.Contains(err.Error(), "not found") {
		return http.StatusNotFound
	}
	return http.StatusInternalServerError
}

// statusForResetError maps a soft-reset error to the HTTP status the
// gateway should return. "Not found" becomes 404; recognised
// invalid-state errors (cannot rewind, target not reached, no graph
// state) become 409; anything else becomes 500.
func statusForResetError(err error) int {
	if err == nil {
		return http.StatusOK
	}
	msg := err.Error()
	if strings.Contains(msg, "not found") {
		return http.StatusNotFound
	}
	if strings.Contains(msg, "cannot rewind") ||
		strings.Contains(msg, "no graph state") ||
		strings.Contains(msg, "not reached") {
		return http.StatusConflict
	}
	return http.StatusInternalServerError
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
