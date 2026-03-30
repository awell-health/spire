package steward

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/awell-health/spire/pkg/agent"
	"github.com/awell-health/spire/pkg/config"
	"github.com/awell-health/spire/pkg/dolt"
	"github.com/awell-health/spire/pkg/integration"
	"github.com/awell-health/spire/pkg/store"
	"github.com/steveyegge/beads"
)

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
	// daemon cycle after runDoltSync returns.
	if user := config.GetCredential(config.CredKeyDolthubUser); user != "" {
		os.Setenv("DOLT_REMOTE_USER", user)
		defer os.Unsetenv("DOLT_REMOTE_USER")
	}
	if pass := config.GetCredential(config.CredKeyDolthubPassword); pass != "" {
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
	if err := dolt.CLIPull(dataDir, false); err != nil {
		log.Printf("[daemon] [%s] dolt pull: %s", tower.Name, err)
		WriteSyncState(SyncState{Tower: tower.Name, Remote: tower.DolthubRemote, At: now, Status: "pull_failed", Error: err.Error()})
		return
	}
	log.Printf("[daemon] [%s] dolt pull complete", tower.Name)

	// Enforce field-level ownership after pull.
	if err := dolt.ApplyMergeOwnership(tower.Database, preCommit); err != nil {
		log.Printf("[daemon] [%s] ownership enforcement: %s", tower.Name, err)
	}

	// Push local commits to the remote.
	if err := dolt.CLIPush(dataDir, false); err != nil {
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

	reg := agent.LoadRegistry()
	for _, w := range reg.Wizards {
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
	reg := agent.LoadRegistry()
	wizards := agent.WizardsForTower(reg, towerName)

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

		if err := agent.RegistryRemove(w.Name); err != nil {
			log.Printf("[daemon] reap %s: remove from registry: %s", w.Name, err)
			continue
		}
		reaped++
	}
	return reaped
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
