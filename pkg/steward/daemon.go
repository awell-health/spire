package steward

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"github.com/awell-health/spire/pkg/config"
	"github.com/awell-health/spire/pkg/dolt"
	"github.com/awell-health/spire/pkg/integration"
	"github.com/awell-health/spire/pkg/olap"
	spireOtel "github.com/awell-health/spire/pkg/otel"
	"github.com/awell-health/spire/pkg/registry"
	"github.com/awell-health/spire/pkg/store"
	"github.com/steveyegge/beads"
)

// --- Shared OLAP + OTLP receiver lifecycle ---
//
// DuckDB writes are serialized through a DuckWriter goroutine. The OTLP
// receiver and ETL sync both submit writes through this writer, preventing
// file lock conflicts. No persistent DuckDB connection is held — each write
// opens the file, writes, and closes (~1ms). The OTLP receiver persists
// across daemon cycles so the gRPC endpoint stays up between ticks.

var (
	sharedOLAPWriter olap.OLAPWriter
	sharedOLAPPath   string
	otlpReceiver     *spireOtel.Receiver
)

// DuckWriter serializes all DuckDB writes through a single goroutine.
// Both OTel event flushes and ETL syncs submit through this writer,
// preventing DuckDB's single-process lock from causing conflicts.
type DuckWriter struct {
	ch     chan writeRequest
	dbPath string
	done   chan struct{}
}

type writeRequest struct {
	fn   func(*sql.Tx) error
	resp chan error
}

// NewDuckWriter creates a new writer for the given DuckDB file path.
func NewDuckWriter(dbPath string) *DuckWriter {
	return &DuckWriter{
		ch:     make(chan writeRequest, 64),
		dbPath: dbPath,
		done:   make(chan struct{}),
	}
}

// Start launches the writer goroutine.
func (dw *DuckWriter) Start() {
	go dw.run()
}

// Stop drains pending writes and stops the writer goroutine.
func (dw *DuckWriter) Stop() {
	close(dw.ch)
	<-dw.done
}

// Close implements olap.OLAPWriter. It stops the writer goroutine.
func (dw *DuckWriter) Close() error {
	dw.Stop()
	return nil
}

// Submit sends a write function to the writer goroutine and waits for it to
// complete. The function is called inside a transaction managed by olap.WriteFunc
// (which handles open→tx→commit→close and retry-on-lock).
func (dw *DuckWriter) Submit(fn func(*sql.Tx) error) error {
	resp := make(chan error, 1)
	dw.ch <- writeRequest{fn: fn, resp: resp}
	return <-resp
}

func (dw *DuckWriter) run() {
	defer close(dw.done)
	for req := range dw.ch {
		req.resp <- olap.WriteFunc(dw.dbPath, req.fn)
	}
}

// ensureSharedOLAP initializes the OLAP writer and OTLP receiver for the tower.
// Idempotent: subsequent calls with the same tower reuse the existing writer.
// The backend is selected via the SPIRE_OLAP_BACKEND env var: "clickhouse" uses
// ClickHouse, anything else (including empty) uses DuckDB.
func ensureSharedOLAP(tower config.TowerConfig) (olap.OLAPWriter, error) {
	olapPath := tower.OLAPPath()

	// If already set up for the same path, reuse.
	if sharedOLAPWriter != nil && sharedOLAPPath == olapPath {
		return sharedOLAPWriter, nil
	}

	// Close previous if path changed (tower switch — rare).
	closeSharedOLAP()

	// ClickHouse backend: connect to an external ClickHouse server.
	backend := os.Getenv("SPIRE_OLAP_BACKEND")
	if backend == "clickhouse" {
		dsn := os.Getenv("SPIRE_CLICKHOUSE_DSN")
		if dsn == "" {
			return nil, fmt.Errorf("SPIRE_OLAP_BACKEND=clickhouse but SPIRE_CLICKHOUSE_DSN is not set")
		}
		cw, err := olap.NewClickHouseWriter(dsn)
		if err != nil {
			return nil, err
		}
		sharedOLAPWriter = cw
		sharedOLAPPath = olapPath

		// Start OTLP receiver with ClickHouse writer.
		if otlpReceiver == nil {
			port := spireOtel.DefaultPort
			if envPort := os.Getenv("SPIRE_OTLP_PORT"); envPort != "" {
				if p, err := strconv.Atoi(envPort); err == nil && p > 0 {
					port = p
				}
			}
			r := spireOtel.NewReceiver(cw.Submit, port, tower.Name)
			if err := r.Start(); err != nil {
				log.Printf("[daemon] OTLP receiver start: %v (telemetry disabled)", err)
			} else {
				otlpReceiver = r
			}
		}
		return cw, nil
	}

	// Default: DuckDB backend.
	if err := os.MkdirAll(filepath.Dir(olapPath), 0700); err != nil {
		return nil, err
	}

	// Initialize schema (open → create tables → close). No persistent connection.
	if err := olap.EnsureSchema(olapPath); err != nil {
		return nil, err
	}

	// Start the single-writer goroutine.
	dw := NewDuckWriter(olapPath)
	dw.Start()
	sharedOLAPWriter = dw
	sharedOLAPPath = olapPath

	// Start OTLP receiver if not running. It submits writes through the DuckWriter.
	if otlpReceiver == nil {
		port := spireOtel.DefaultPort
		if envPort := os.Getenv("SPIRE_OTLP_PORT"); envPort != "" {
			if p, err := strconv.Atoi(envPort); err == nil && p > 0 {
				port = p
			}
		}
		r := spireOtel.NewReceiver(dw.Submit, port, tower.Name)
		if err := r.Start(); err != nil {
			// Non-fatal: telemetry collection is optional.
			log.Printf("[daemon] OTLP receiver start: %v (telemetry disabled)", err)
		} else {
			otlpReceiver = r
		}
	}

	return dw, nil
}

// closeSharedOLAP shuts down the OTLP receiver and OLAP writer.
func closeSharedOLAP() {
	if otlpReceiver != nil {
		otlpReceiver.Stop()
		otlpReceiver = nil
	}
	if sharedOLAPWriter != nil {
		sharedOLAPWriter.Close()
		sharedOLAPWriter = nil
		sharedOLAPPath = ""
	}
}

// StopOTLPReceiver gracefully shuts down the OTLP receiver and DuckWriter.
// Called from the daemon shutdown path (cmd/spire/daemon.go).
func StopOTLPReceiver() {
	closeSharedOLAP()
}

// InboxMessage is a single message in the inbox file.
type InboxMessage struct {
	ID        string `json:"id"`
	From      string `json:"from"`
	Ref       string `json:"ref,omitempty"`
	Text      string `json:"text"`
	Priority  int    `json:"priority"`
	CreatedAt string `json:"created_at"`
}

// InboxFile is the structure of the inbox.json file.
type InboxFile struct {
	Agent     string         `json:"agent"`
	UpdatedAt string         `json:"updated_at"`
	Messages  []InboxMessage `json:"messages"`
}

// DaemonCycle iterates all configured towers and runs a cycle for each.
func DaemonCycle() {
	towers, err := config.ListTowerConfigs()
	if err != nil {
		log.Printf("[daemon] list towers: %s", err)
		return
	}

	if len(towers) == 0 {
		log.Printf("[daemon] no towers configured, skipping cycle")
		return
	}

	for _, tower := range towers {
		DaemonTowerCycle(tower)
	}
}

// DaemonTowerCycle runs one daemon cycle scoped to a single tower.
// It opens a store scoped to the tower's .beads directory and scopes
// doltSQL calls to the tower's database.
func DaemonTowerCycle(tower config.TowerConfig) {
	beadsDir := filepath.Join(dolt.DataDir(), tower.Database, ".beads")
	if _, err := os.Stat(beadsDir); os.IsNotExist(err) {
		log.Printf("[daemon] [%s] skipping — no .beads/ at %s", tower.Name, beadsDir)
		return
	}

	log.Printf("[daemon] [%s] cycle start (db=%s)", tower.Name, tower.Database)

	// Sync derived configs from tower config (single source of truth).
	SyncTowerDerivedConfigs(tower)

	// Open store scoped to this tower
	if _, err := store.OpenAt(beadsDir); err != nil {
		log.Printf("[daemon] [%s] open store: %s", tower.Name, err)
		return
	}
	defer store.Reset()

	// Set DaemonDB for doltSQL() calls that still need it.
	DaemonDB = tower.Database
	defer func() { DaemonDB = "" }()

	runDoltSync(tower)

	// ETL agent_runs from Dolt into the local DuckDB OLAP store.
	if err := syncToOLAP(tower); err != nil {
		log.Printf("[daemon] [%s] olap sync: %v (non-fatal)", tower.Name, err)
	}

	// Reconcile shared repos after Dolt sync so remotely shared repos are visible in the current cycle.
	if err := reconcileSharedRepos(tower); err != nil {
		log.Printf("[daemon] [%s] reconcileSharedRepos: %v", tower.Name, err)
	}

	ensureWebhookQueue()

	epicsSynced := integration.SyncEpicsToLinear()
	if epicsSynced > 0 {
		log.Printf("[daemon] [%s] synced %d epic(s) to Linear", tower.Name, epicsSynced)
	}

	qProcessed, qErrors := integration.ProcessWebhookQueue()
	if qProcessed > 0 || qErrors > 0 {
		log.Printf("[daemon] [%s] queue: processed %d rows (%d errors)", tower.Name, qProcessed, qErrors)
	}

	processed, errors := processWebhookEvents()
	if processed > 0 || errors > 0 {
		log.Printf("[daemon] [%s] processed %d events (%d errors)", tower.Name, processed, errors)
	}

	delivered := DeliverAgentInboxes()
	if delivered > 0 {
		log.Printf("[daemon] [%s] delivered inbox files for %d agent(s)", tower.Name, delivered)
	}

	reaped := ReapDeadAgents(tower.Name)
	if reaped > 0 {
		log.Printf("[daemon] [%s] reaped %d dead agent(s)", tower.Name, reaped)
	}

	// Remove stale updated:<timestamp> labels left by the old heartbeat mechanism.
	CleanUpdatedLabels()

	log.Printf("[daemon] [%s] cycle complete", tower.Name)
}

// DaemonDB is the database name override for the current tower cycle.
// When set, DoltSQL() uses it instead of calling detectDBName().
var DaemonDB string

// SyncTowerDerivedConfigs regenerates derived config files from tower config
// on each daemon cycle, ensuring tower JSON is the single source of truth.
// It regenerates .beads/config.yaml for the tower data directory and all
// registered repo instances, and updates Instance.Database in config.json
// if it has drifted from the tower's current database name.
func SyncTowerDerivedConfigs(tower config.TowerConfig) {
	// Canonical config.yaml content derived from current tower config.
	configYAML := fmt.Sprintf("dolt.host: %q\ndolt.port: %s\n", dolt.Host(), dolt.Port())

	// Regenerate .beads/config.yaml in the tower's data directory.
	towerBeadsDir := filepath.Join(dolt.DataDir(), tower.Database, ".beads")
	if _, err := os.Stat(towerBeadsDir); err == nil {
		if err := os.WriteFile(filepath.Join(towerBeadsDir, "config.yaml"), []byte(configYAML), 0644); err != nil {
			log.Printf("[daemon] [%s] sync tower config.yaml: %s", tower.Name, err)
		}
	}

	// Sync instance map: update Database field and regenerate repo config.yaml
	// for all instances registered under this tower.
	cfg, err := config.Load()
	if err != nil {
		log.Printf("[daemon] [%s] sync: load config: %s", tower.Name, err)
		return
	}

	changed := false
	for prefix, inst := range cfg.Instances {
		if inst.Tower != tower.Name {
			continue
		}
		// Keep Instance.Database aligned with tower config (source of truth).
		if inst.Database != tower.Database {
			log.Printf("[daemon] [%s] updating instance %q database: %q → %q",
				tower.Name, prefix, inst.Database, tower.Database)
			inst.Database = tower.Database
			changed = true
		}
		// Regenerate repo-local .beads/config.yaml if .beads/ exists.
		if inst.Path != "" {
			repoBeadsDir := filepath.Join(inst.Path, ".beads")
			if _, err := os.Stat(repoBeadsDir); err == nil {
				if err := os.WriteFile(filepath.Join(repoBeadsDir, "config.yaml"), []byte(configYAML), 0644); err != nil {
					log.Printf("[daemon] [%s] sync repo config.yaml for %q: %s", tower.Name, prefix, err)
				}
			}
		}
	}

	if changed {
		if err := config.Save(cfg); err != nil {
			log.Printf("[daemon] [%s] sync: save config: %s", tower.Name, err)
		}
	}
}

// processWebhookEvents queries for unprocessed webhook event beads and processes them.
// Returns (processed count, error count).
func processWebhookEvents() (int, int) {
	events, err := store.ListBeads(beads.IssueFilter{
		Labels: []string{"webhook"},
		Status: store.StatusPtr(beads.StatusOpen),
	})
	if err != nil {
		log.Printf("[daemon] list webhook events: %s", err)
		return 0, 0
	}

	if len(events) == 0 {
		return 0, 0
	}

	log.Printf("[daemon] found %d unprocessed webhook events", len(events))

	processed := 0
	errors := 0

	for _, event := range events {
		err := integration.ProcessWebhookEvent(event)
		if err != nil {
			// Don't close — will be retried next cycle
			log.Printf("[daemon] event %s: error (will retry): %s", event.ID, err)
			errors++
			continue
		}

		// Close the event bead (mark processed)
		if closeErr := store.CloseBead(event.ID); closeErr != nil {
			log.Printf("[daemon] event %s: close failed: %s", event.ID, closeErr)
			errors++
			continue
		}

		processed++
	}

	return processed, errors
}

// SyncState tracks the result of the last DoltHub sync for a tower.
type SyncState struct {
	Tower  string `json:"tower"`
	Remote string `json:"remote"`
	At     string `json:"at"`
	Status string `json:"status"` // "ok", "pull_failed", "push_failed", "no_remote"
	Error  string `json:"error,omitempty"`
}

// WriteSyncState persists sync state to ~/.config/spire/sync/<tower>.json.
func WriteSyncState(state SyncState) {
	dir, err := config.Dir()
	if err != nil {
		return
	}
	syncDir := filepath.Join(dir, "sync")
	os.MkdirAll(syncDir, 0755)
	data, _ := json.MarshalIndent(state, "", "  ")
	os.WriteFile(filepath.Join(syncDir, state.Tower+".json"), append(data, '\n'), 0644)
}

// ReadSyncState reads the last sync state for a tower.
func ReadSyncState(towerName string) *SyncState {
	dir, err := config.Dir()
	if err != nil {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(dir, "sync", towerName+".json"))
	if err != nil {
		return nil
	}
	var state SyncState
	if json.Unmarshal(data, &state) != nil {
		return nil
	}
	return &state
}

// runDoltSync pulls from and pushes to the DoltHub remote for the given tower.
// Called at the start of each DaemonTowerCycle to keep the database in sync with
// the remote before running Linear sync and webhook processing.
// Non-fatal: missing remote or auth errors are logged and skipped.
func runDoltSync(tower config.TowerConfig) {
	now := time.Now().UTC().Format(time.RFC3339)

	if tower.DolthubRemote == "" {
		// No remote configured — not an error, just not set up for DoltHub sync.
		return
	}

	dataDir := filepath.Join(dolt.DataDir(), tower.Database)

	// Sync CLI remote config (.dolt/config.json) to match the tower's stored remote.
	dolt.SetCLIRemote(dataDir, "origin", tower.DolthubRemote)

	// Inject credentials; defer cleanup so they don't leak into the rest of the
	// daemon cycle after runDoltSync returns. Credentials are resolved from the
	// tower's RemoteKind — DoltHub draws from the shared dolthub-user/password
	// keys; remotesapi draws from per-tower remotesapi-user-<tower>/remotesapi-password-<tower>.
	user, pass := config.RemoteCredentials(&tower)
	if user != "" {
		os.Setenv("DOLT_REMOTE_USER", user)
		defer os.Unsetenv("DOLT_REMOTE_USER")
	}
	if pass != "" {
		os.Setenv("DOLT_REMOTE_PASSWORD", pass)
		defer os.Unsetenv("DOLT_REMOTE_PASSWORD")
	}

	// Commit pending changes before pulling — dolt cannot merge with uncommitted changes.
	commitSQL := fmt.Sprintf("USE `%s`; CALL DOLT_ADD('-A'); CALL DOLT_COMMIT('-m', 'daemon: auto-commit before sync', '--allow-empty')", tower.Database)
	if _, err := dolt.RawQuery(commitSQL); err != nil {
		// Non-fatal: may fail if nothing to commit or dolt not configured.
		// The --allow-empty flag prevents errors on clean working sets.
	}

	// Record pre-pull commit for ownership enforcement.
	preCommit := dolt.GetCurrentCommitHash(tower.Database)

	// Pull first — work with the freshest remote state before Linear sync.
	// Use a per-operation 60s timeout so a slow/unreachable DoltHub doesn't
	// block the daemon indefinitely (the bug this fixes).
	// TODO: consider exponential backoff on repeated failures to avoid
	// hammering a down DoltHub every cycle (currently retries every ~2min).
	pullCtx, pullCancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer pullCancel()
	pullErr := dolt.CLIPull(pullCtx, dataDir, false)
	if pullErr != nil {
		log.Printf("[daemon] [%s] dolt pull: %s", tower.Name, pullErr)
		// Non-conflict errors (including timeouts) → persist failure state and bail.
		// Conflict errors fall through to ownership enforcement below.
		if !strings.Contains(pullErr.Error(), "CONFLICT") && !strings.Contains(pullErr.Error(), "conflict") {
			WriteSyncState(SyncState{Tower: tower.Name, Remote: tower.DolthubRemote, At: now, Status: "pull_failed", Error: pullErr.Error()})
			return
		}
	} else {
		log.Printf("[daemon] [%s] dolt pull complete", tower.Name)
	}

	// Enforce field-level ownership after pull — this resolves merge conflicts
	// using field-level ownership rules. Must run even (especially) when the
	// pull reports conflicts, since CLIPull returns an error for conflicts
	// but the data is still merged into the working set.
	if err := dolt.ApplyMergeOwnership(tower.Database, preCommit); err != nil {
		log.Printf("[daemon] [%s] ownership enforcement: %s", tower.Name, err)
	}

	// Push local commits to the remote.
	// Separate 60s timeout so a slow pull doesn't eat into the push budget.
	pushCtx, pushCancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer pushCancel()
	if err := dolt.CLIPush(pushCtx, dataDir, false); err != nil {
		errMsg := err.Error()
		if strings.Contains(errMsg, "non-fast-forward") || strings.Contains(errMsg, "no common ancestor") {
			log.Printf("[daemon] [%s] dolt push: non-fast-forward, skipping (will retry next cycle)", tower.Name)
			WriteSyncState(SyncState{Tower: tower.Name, Remote: tower.DolthubRemote, At: now, Status: "push_failed", Error: "non-fast-forward"})
		} else {
			log.Printf("[daemon] [%s] dolt push: %s", tower.Name, err)
			WriteSyncState(SyncState{Tower: tower.Name, Remote: tower.DolthubRemote, At: now, Status: "push_failed", Error: errMsg})
		}
		return
	}
	log.Printf("[daemon] [%s] dolt push complete", tower.Name)

	WriteSyncState(SyncState{Tower: tower.Name, Remote: tower.DolthubRemote, At: now, Status: "ok"})
}

// DeliverAgentInboxes writes inbox.json files for all known agents.
// It discovers agents from the wizard registry and registered agent beads,
// runs spire collect <name> --json for each, and writes the output to
// ~/.config/spire/runtime/<name>/inbox.json.
// Returns the number of agents whose inboxes were written.
func DeliverAgentInboxes() int {
	// Collect unique agent names from two sources:
	// 1. Wizard registry (local running wizards)
	// 2. Registered agent beads (persistent agents)
	agents := make(map[string]bool)

	regEntries, _ := registry.List()
	for _, w := range regEntries {
		if w.Name != "" {
			agents[w.Name] = true
		}
	}

	agentBeads, err := store.ListBeads(beads.IssueFilter{
		IDPrefix: "spi-",
		Labels:   []string{"agent"},
		Status:   store.StatusPtr(beads.StatusOpen),
	})
	if err == nil {
		for _, b := range agentBeads {
			for _, l := range b.Labels {
				if strings.HasPrefix(l, "name:") {
					agents[l[5:]] = true
				}
			}
		}
	}

	if len(agents) == 0 {
		return 0
	}

	delivered := 0
	for name := range agents {
		if err := deliverInboxForAgent(name); err != nil {
			log.Printf("[daemon] inbox delivery for %s: %s", name, err)
			continue
		}
		delivered++
	}
	return delivered
}

// deliverInboxForAgent queries messages for an agent via the store and writes
// the result as an inbox file.
func deliverInboxForAgent(agentName string) error {
	messages, err := store.ListBeads(beads.IssueFilter{
		IDPrefix: "spi-",
		Labels:   []string{"msg", "to:" + agentName},
		Status:   store.StatusPtr(beads.StatusOpen),
	})
	if err != nil {
		return fmt.Errorf("query messages: %w", err)
	}

	// Check if message set changed before writing
	newIDs := MessageIDs(messages)
	if existingData, err := ReadInboxFile(agentName); err == nil {
		var existing InboxFile
		if json.Unmarshal(existingData, &existing) == nil {
			existingIDs := InboxMessageIDs(existing.Messages)
			if SlicesEqual(newIDs, existingIDs) {
				return nil // unchanged, skip write
			}
		}
	}

	inbox := InboxFile{
		Agent:     agentName,
		UpdatedAt: time.Now().UTC().Format(time.RFC3339),
		Messages:  make([]InboxMessage, 0, len(messages)),
	}

	for _, m := range messages {
		msg := InboxMessage{
			ID:       m.ID,
			Text:     m.Title,
			Priority: m.Priority,
		}
		for _, l := range m.Labels {
			if strings.HasPrefix(l, "from:") {
				msg.From = l[5:]
			}
			if strings.HasPrefix(l, "ref:") {
				msg.Ref = l[4:]
			}
		}
		inbox.Messages = append(inbox.Messages, msg)
	}

	data, err := json.MarshalIndent(inbox, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal inbox: %w", err)
	}

	return WriteInboxFile(agentName, data)
}

// MessageIDs extracts sorted message IDs from store beads.
func MessageIDs(beadList []store.Bead) []string {
	ids := make([]string, len(beadList))
	for i, b := range beadList {
		ids[i] = b.ID
	}
	sort.Strings(ids)
	return ids
}

// InboxMessageIDs extracts sorted message IDs from inbox messages.
func InboxMessageIDs(msgs []InboxMessage) []string {
	ids := make([]string, len(msgs))
	for i, m := range msgs {
		ids[i] = m.ID
	}
	sort.Strings(ids)
	return ids
}

// SlicesEqual checks if two string slices are equal.
func SlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// InboxPath returns the path to an agent's inbox file.
func InboxPath(agentName string) string {
	dir, err := config.Dir()
	if err != nil {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".config", "spire")
	}
	return filepath.Join(dir, "runtime", agentName, "inbox.json")
}

// ReadInboxFile reads the inbox file for an agent.
func ReadInboxFile(agentName string) ([]byte, error) {
	return os.ReadFile(InboxPath(agentName))
}

// WriteInboxFile writes the inbox file for an agent. Used by the daemon.
func WriteInboxFile(agentName string, data []byte) error {
	path := InboxPath(agentName)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

// ReapDeadAgents checks the wizard registry for dead agent processes with
// unread messages. For each dead agent, it labels their pending messages
// with dead-letter:<name> and removes the wizard from the registry.
// Returns the number of agents reaped.
func ReapDeadAgents(towerName string) int {
	allEntries, err := registry.List()
	if err != nil {
		log.Printf("[daemon] reap: list registry: %s", err)
		return 0
	}

	// Filter by tower (empty towerName matches all).
	var wizards []registry.Entry
	for _, e := range allEntries {
		if towerName == "" || e.Tower == towerName {
			wizards = append(wizards, e)
		}
	}

	reaped := 0
	for _, w := range wizards {
		if w.PID <= 0 {
			continue
		}
		if dolt.ProcessAlive(w.PID) {
			continue
		}

		// Agent process is dead — check for unread messages
		messages, err := store.ListBeads(beads.IssueFilter{
			IDPrefix: "spi-",
			Labels:   []string{"msg", "to:" + w.Name},
			Status:   store.StatusPtr(beads.StatusOpen),
		})
		if err != nil {
			log.Printf("[daemon] reap %s: list messages: %s", w.Name, err)
			continue
		}

		if len(messages) > 0 {
			log.Printf("[daemon] dead agent %s (pid %d) has %d unread messages", w.Name, w.PID, len(messages))
			for _, m := range messages {
				if err := store.AddLabel(m.ID, "dead-letter:"+w.Name); err != nil {
					log.Printf("[daemon] reap %s: label %s: %s", w.Name, m.ID, err)
				}
			}
		}

		if err := registry.Remove(w.Name); err != nil {
			log.Printf("[daemon] reap %s: remove from registry: %s", w.Name, err)
			continue
		}
		reaped++
	}
	return reaped
}

// syncToOLAP performs incremental ETL from Dolt agent_runs into the tower's
// OLAP store. Writes go through the shared OLAPWriter (DuckDB or ClickHouse)
// so they don't conflict with OTel event flushes.
// Non-fatal: errors are logged by the caller.
func syncToOLAP(tower config.TowerConfig) error {
	w, err := ensureSharedOLAP(tower)
	if err != nil {
		return err
	}

	dsn := fmt.Sprintf("root:@tcp(%s:%s)/%s?parseTime=true", dolt.Host(), dolt.Port(), tower.Database)
	doltConn, err := sql.Open("mysql", dsn)
	if err != nil {
		return err
	}
	defer doltConn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	etl := olap.NewETLWithWriter(tower.OLAPPath(), w.Submit)
	n, err := etl.Sync(ctx, doltConn)
	if err != nil {
		return err
	}
	if n > 0 {
		log.Printf("[daemon] [%s] olap: synced %d agent_runs rows", tower.Name, n)
	}
	return nil
}

// ExportRunDoltSync exposes runDoltSync for backward-compatible callers in cmd/spire.
func ExportRunDoltSync(tower config.TowerConfig) { runDoltSync(tower) }

// ExportProcessWebhookEvents exposes processWebhookEvents for backward-compatible callers.
func ExportProcessWebhookEvents() (int, int) { return processWebhookEvents() }

// ExportEnsureWebhookQueue exposes ensureWebhookQueue for backward-compatible callers.
func ExportEnsureWebhookQueue() { ensureWebhookQueue() }

// ensureWebhookQueue creates the webhook_queue table if it doesn't exist.
func ensureWebhookQueue() {
	_, err := dolt.SQL(`CREATE TABLE IF NOT EXISTS webhook_queue (
		id          VARCHAR(36) PRIMARY KEY,
		event_type  VARCHAR(64) NOT NULL,
		linear_id   VARCHAR(32) NOT NULL,
		payload     JSON NOT NULL,
		processed   BOOLEAN NOT NULL DEFAULT 0,
		created_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
	)`, false, DaemonDB, config.DetectDBName)
	if err != nil {
		log.Printf("[daemon] ensure webhook_queue: %s", err)
	}
}

// reconcileSharedRepos diffs the shared repos table against the tower's local
// bindings and creates "unbound" entries for newly discovered repos.
// Idempotent: skipped/unmanaged/bound repos are not re-prompted.
//
// This function is laptop-only: it mirrors the shared `repos` table into
// per-machine LocalBindings, which have no meaning in cluster-native pods
// where ClusterIdentityResolver reads `repos` directly. When the tower's
// EffectiveDeploymentMode is cluster-native, the function returns early as a
// no-op so replicas do not cross-wire LocalBindings state they do not own.
// Empty/unset modes fall through to the existing local-native behavior;
// attached-reserved is intentionally NOT gated here (it is a declaration of
// intent with no execution surface today).
func reconcileSharedRepos(tower config.TowerConfig) error {
	if tower.EffectiveDeploymentMode() == config.DeploymentModeClusterNative {
		log.Printf("[daemon] [%s] reconcileSharedRepos: skipping (cluster-native mode reads repos directly via ClusterIdentityResolver)", tower.Name)
		return nil
	}

	sql := fmt.Sprintf("SELECT prefix, repo_url, branch FROM `%s`.repos ORDER BY prefix", tower.Database)
	out, err := dolt.RawQuery(sql)
	if err != nil {
		// Dolt unreachable or no repos table — skip silently; next cycle will retry.
		return nil
	}

	rows := dolt.ParseDoltRows(out, []string{"prefix", "repo_url", "branch"})
	if len(rows) == 0 {
		return nil
	}

	// Load fresh tower config from disk to get current LocalBindings.
	updated, err := config.LoadTowerConfig(tower.Name)
	if err != nil {
		return fmt.Errorf("load tower config: %w", err)
	}
	if updated.LocalBindings == nil {
		updated.LocalBindings = make(map[string]*config.LocalRepoBinding)
	}

	changed := false
	for _, r := range rows {
		prefix := r["prefix"]
		repoURL := r["repo_url"]
		branch := r["branch"]

		binding, exists := updated.LocalBindings[prefix]
		if !exists {
			// Newly discovered shared repo — add as unbound.
			updated.LocalBindings[prefix] = &config.LocalRepoBinding{
				Prefix:       prefix,
				State:        "unbound",
				RepoURL:      repoURL,
				SharedBranch: branch,
				DiscoveredAt: time.Now(),
			}
			log.Printf("[spire] new shared repo discovered: %s  %s  (branch: %s)", prefix, repoURL, branch)
			log.Printf("  → run 'spire repo bind %s /local/path' to bind, or 'spire repo skip %s' to skip", prefix, prefix)
			changed = true
			continue
		}

		if binding.State == "unbound" {
			// Reminder for unbound repos.
			log.Printf("[spire] unbound shared repo: %s  %s  (branch: %s)", prefix, repoURL, branch)
			log.Printf("  → run 'spire repo bind %s /local/path' to bind, or 'spire repo skip %s' to skip", prefix, prefix)
		}

		// Detect improperly bound repos: state=bound but missing .beads/
		// bootstrap or Instance registration. Reset to unbound so the user
		// gets re-prompted to run a real bind.
		if binding.State == "bound" {
			needsRebind := false
			if binding.LocalPath == "" {
				needsRebind = true
			} else if _, err := os.Stat(filepath.Join(binding.LocalPath, ".beads", "metadata.json")); err != nil {
				needsRebind = true
			}
			if !needsRebind {
				// Check that a global Instance is registered for this prefix.
				if globalCfg, gErr := config.Load(); gErr == nil {
					if _, ok := globalCfg.Instances[prefix]; !ok {
						needsRebind = true
					}
				}
			}
			if needsRebind {
				log.Printf("[spire] repo %s marked bound but not properly bootstrapped — resetting to unbound", prefix)
				if binding.LocalPath != "" {
					log.Printf("  → run 'spire repo bind %s %s' to fix", prefix, binding.LocalPath)
				} else {
					log.Printf("  → run 'spire repo bind %s /local/path' to fix", prefix)
				}
				binding.State = "unbound"
				changed = true
			}
		}
		// skipped, unmanaged — silent.
	}

	if changed {
		return config.SaveTowerConfig(updated)
	}
	return nil
}
