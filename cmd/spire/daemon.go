package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/steveyegge/beads"
)

// daemonDB is the database name override for the current tower cycle.
// When set, doltSQL() and detectDBName() use it instead of CWD detection.
var daemonDB string

func cmdDaemon(args []string) error {
	// Parse flags
	interval := 2 * time.Minute
	once := false

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--interval":
			if i+1 >= len(args) {
				return fmt.Errorf("--interval requires a value (e.g., 2m, 30s, 5m)")
			}
			i++
			d, err := time.ParseDuration(args[i])
			if err != nil {
				// Try parsing as plain seconds
				secs, serr := strconv.Atoi(args[i])
				if serr != nil {
					return fmt.Errorf("--interval: invalid duration %q", args[i])
				}
				d = time.Duration(secs) * time.Second
			}
			interval = d
		case "--once":
			once = true
		default:
			return fmt.Errorf("unknown flag: %s\nusage: spire daemon [--interval 2m] [--once]", args[i])
		}
	}

	log.Printf("[daemon] starting (interval=%s, once=%v)", interval, once)

	// Write our PID file so spire down can find us
	writePID(daemonPIDPath(), os.Getpid())

	// Run first cycle immediately
	runCycle()

	if once {
		log.Printf("[daemon] --once mode, exiting")
		return nil
	}

	// Set up signal handling for graceful shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			runCycle()
		case sig := <-sigCh:
			log.Printf("[daemon] received %s, shutting down", sig)
			return nil
		}
	}
}

// runCycle iterates all configured towers and runs a cycle for each.
func runCycle() {
	towers, err := listTowerConfigs()
	if err != nil {
		log.Printf("[daemon] list towers: %s", err)
		return
	}

	if len(towers) == 0 {
		log.Printf("[daemon] no towers configured, skipping cycle")
		return
	}

	for _, tower := range towers {
		runTowerCycle(tower)
	}
}

// runTowerCycle runs one daemon cycle scoped to a single tower.
// It opens a store scoped to the tower's .beads directory and sets
// daemonDB so that doltSQL targets the correct database.
func runTowerCycle(tower TowerConfig) {
	beadsDir := filepath.Join(doltDataDir(), tower.Database, ".beads")
	if _, err := os.Stat(beadsDir); os.IsNotExist(err) {
		log.Printf("[daemon] [%s] skipping — no .beads/ at %s", tower.Name, beadsDir)
		return
	}

	log.Printf("[daemon] [%s] cycle start (db=%s)", tower.Name, tower.Database)

	// Open store scoped to this tower
	if _, err := openStoreAt(beadsDir); err != nil {
		log.Printf("[daemon] [%s] open store: %s", tower.Name, err)
		return
	}
	defer resetStore()

	// Keep daemonDB for doltSQL() calls that still need it
	daemonDB = tower.Database
	defer func() { daemonDB = "" }()

	runDoltSync(tower)

	ensureWebhookQueue()

	epicsSynced := syncEpicsToLinear()
	if epicsSynced > 0 {
		log.Printf("[daemon] [%s] synced %d epic(s) to Linear", tower.Name, epicsSynced)
	}

	qProcessed, qErrors := processWebhookQueue()
	if qProcessed > 0 || qErrors > 0 {
		log.Printf("[daemon] [%s] queue: processed %d rows (%d errors)", tower.Name, qProcessed, qErrors)
	}

	processed, errors := processWebhookEvents()
	if processed > 0 || errors > 0 {
		log.Printf("[daemon] [%s] processed %d events (%d errors)", tower.Name, processed, errors)
	}

	delivered := deliverAgentInboxes()
	if delivered > 0 {
		log.Printf("[daemon] [%s] delivered inbox files for %d agent(s)", tower.Name, delivered)
	}

	reaped := reapDeadAgents(tower.Name)
	if reaped > 0 {
		log.Printf("[daemon] [%s] reaped %d dead agent(s)", tower.Name, reaped)
	}

	log.Printf("[daemon] [%s] cycle complete", tower.Name)
}

// processWebhookEvents queries for unprocessed webhook event beads and processes them.
// Returns (processed count, error count).
func processWebhookEvents() (int, int) {
	events, err := storeListBeads(beads.IssueFilter{
		Labels: []string{"webhook"},
		Status: statusPtr(beads.StatusOpen),
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
		err := processWebhookEvent(event)
		if err != nil {
			// Don't close — will be retried next cycle
			log.Printf("[daemon] event %s: error (will retry): %s", event.ID, err)
			errors++
			continue
		}

		// Close the event bead (mark processed)
		if closeErr := storeCloseBead(event.ID); closeErr != nil {
			log.Printf("[daemon] event %s: close failed: %s", event.ID, closeErr)
			errors++
			continue
		}

		processed++
	}

	return processed, errors
}

// syncState tracks the result of the last DoltHub sync for a tower.
type syncState struct {
	Tower  string `json:"tower"`
	Remote string `json:"remote"`
	At     string `json:"at"`
	Status string `json:"status"` // "ok", "pull_failed", "push_failed", "no_remote"
	Error  string `json:"error,omitempty"`
}

// writeSyncState persists sync state to ~/.config/spire/sync/<tower>.json.
func writeSyncState(state syncState) {
	dir, err := configDir()
	if err != nil {
		return
	}
	syncDir := filepath.Join(dir, "sync")
	os.MkdirAll(syncDir, 0755)
	data, _ := json.MarshalIndent(state, "", "  ")
	os.WriteFile(filepath.Join(syncDir, state.Tower+".json"), append(data, '\n'), 0644)
}

// readSyncState reads the last sync state for a tower.
func readSyncState(towerName string) *syncState {
	dir, err := configDir()
	if err != nil {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(dir, "sync", towerName+".json"))
	if err != nil {
		return nil
	}
	var state syncState
	if json.Unmarshal(data, &state) != nil {
		return nil
	}
	return &state
}

// runDoltSync pulls from and pushes to the DoltHub remote for the given tower.
// Called at the start of each runTowerCycle to keep the database in sync with
// the remote before running Linear sync and webhook processing.
// Non-fatal: missing remote or auth errors are logged and skipped.
func runDoltSync(tower TowerConfig) {
	now := time.Now().UTC().Format(time.RFC3339)

	if tower.DolthubRemote == "" {
		// No remote configured — not an error, just not set up for DoltHub sync.
		return
	}

	dataDir := filepath.Join(doltDataDir(), tower.Database)

	// Sync CLI remote config (.dolt/config.json) to match the tower's stored remote.
	setDoltCLIRemote(dataDir, "origin", tower.DolthubRemote)

	// Inject credentials; defer cleanup so they don't leak into the rest of the
	// daemon cycle after runDoltSync returns.
	if user := getCredential(CredKeyDolthubUser); user != "" {
		os.Setenv("DOLT_REMOTE_USER", user)
		defer os.Unsetenv("DOLT_REMOTE_USER")
	}
	if pass := getCredential(CredKeyDolthubPassword); pass != "" {
		os.Setenv("DOLT_REMOTE_PASSWORD", pass)
		defer os.Unsetenv("DOLT_REMOTE_PASSWORD")
	}

	// Record pre-pull commit for ownership enforcement.
	preCommit := getCurrentCommitHash(tower.Database)

	// Pull first — work with the freshest remote state before Linear sync.
	if err := doltCLIPull(dataDir, false); err != nil {
		log.Printf("[daemon] [%s] dolt pull: %s", tower.Name, err)
		writeSyncState(syncState{Tower: tower.Name, Remote: tower.DolthubRemote, At: now, Status: "pull_failed", Error: err.Error()})
		return
	}
	log.Printf("[daemon] [%s] dolt pull complete", tower.Name)

	// Enforce field-level ownership after pull.
	if err := applyMergeOwnership(tower.Database, preCommit); err != nil {
		log.Printf("[daemon] [%s] ownership enforcement: %s", tower.Name, err)
	}

	// Push local commits to the remote.
	if err := doltCLIPush(dataDir, false); err != nil {
		errMsg := err.Error()
		if strings.Contains(errMsg, "non-fast-forward") || strings.Contains(errMsg, "no common ancestor") {
			log.Printf("[daemon] [%s] dolt push: non-fast-forward, skipping (will retry next cycle)", tower.Name)
			writeSyncState(syncState{Tower: tower.Name, Remote: tower.DolthubRemote, At: now, Status: "push_failed", Error: "non-fast-forward"})
		} else {
			log.Printf("[daemon] [%s] dolt push: %s", tower.Name, err)
			writeSyncState(syncState{Tower: tower.Name, Remote: tower.DolthubRemote, At: now, Status: "push_failed", Error: errMsg})
		}
		return
	}
	log.Printf("[daemon] [%s] dolt push complete", tower.Name)

	writeSyncState(syncState{Tower: tower.Name, Remote: tower.DolthubRemote, At: now, Status: "ok"})
}

// deliverAgentInboxes writes inbox.json files for all known agents.
// It discovers agents from the wizard registry and registered agent beads,
// runs spire collect <name> --json for each, and writes the output to
// ~/.config/spire/runtime/<name>/inbox.json.
// Returns the number of agents whose inboxes were written.
func deliverAgentInboxes() int {
	// Collect unique agent names from two sources:
	// 1. Wizard registry (local running wizards)
	// 2. Registered agent beads (persistent agents)
	agents := make(map[string]bool)

	reg := loadWizardRegistry()
	for _, w := range reg.Wizards {
		if w.Name != "" {
			agents[w.Name] = true
		}
	}

	agentBeads, err := storeListBeads(beads.IssueFilter{
		IDPrefix: "spi-",
		Labels:   []string{"agent"},
		Status:   statusPtr(beads.StatusOpen),
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
// the result as an inbox file in the inboxFile format.
func deliverInboxForAgent(agentName string) error {
	messages, err := storeListBeads(beads.IssueFilter{
		IDPrefix: "spi-",
		Labels:   []string{"msg", "to:" + agentName},
		Status:   statusPtr(beads.StatusOpen),
	})
	if err != nil {
		return fmt.Errorf("query messages: %w", err)
	}

	// Check if message set changed before writing
	newIDs := messageIDs(messages)
	if existingData, err := readInboxFile(agentName); err == nil {
		var existing inboxFile
		if json.Unmarshal(existingData, &existing) == nil {
			existingIDs := inboxMessageIDs(existing.Messages)
			if slicesEqual(newIDs, existingIDs) {
				return nil // unchanged, skip write
			}
		}
	}

	inbox := inboxFile{
		Agent:     agentName,
		UpdatedAt: time.Now().UTC().Format(time.RFC3339),
		Messages:  make([]inboxMessage, 0, len(messages)),
	}

	for _, m := range messages {
		msg := inboxMessage{
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

	return writeInboxFile(agentName, data)
}

// messageIDs extracts sorted message IDs from store beads.
func messageIDs(beadList []Bead) []string {
	ids := make([]string, len(beadList))
	for i, b := range beadList {
		ids[i] = b.ID
	}
	sort.Strings(ids)
	return ids
}

// reapDeadAgents checks the wizard registry for dead agent processes with
// unread messages. For each dead agent, it labels their pending messages
// with dead-letter:<name> and removes the wizard from the registry.
// Returns the number of agents reaped.
func reapDeadAgents(towerName string) int {
	reg := loadWizardRegistry()
	wizards := wizardsForTower(reg, towerName)

	reaped := 0
	for _, w := range wizards {
		if w.PID <= 0 {
			continue
		}
		if processAlive(w.PID) {
			continue
		}

		// Agent process is dead — check for unread messages
		messages, err := storeListBeads(beads.IssueFilter{
			IDPrefix: "spi-",
			Labels:   []string{"msg", "to:" + w.Name},
			Status:   statusPtr(beads.StatusOpen),
		})
		if err != nil {
			log.Printf("[daemon] reap %s: list messages: %s", w.Name, err)
			continue
		}

		if len(messages) > 0 {
			log.Printf("[daemon] dead agent %s (pid %d) has %d unread messages", w.Name, w.PID, len(messages))
			for _, m := range messages {
				if err := storeAddLabel(m.ID, "dead-letter:"+w.Name); err != nil {
					log.Printf("[daemon] reap %s: label %s: %s", w.Name, m.ID, err)
				}
			}
		}

		if err := wizardRegistryRemove(w.Name); err != nil {
			log.Printf("[daemon] reap %s: remove from registry: %s", w.Name, err)
			continue
		}
		reaped++
	}
	return reaped
}

// ensureWebhookQueue creates the webhook_queue table if it doesn't exist.
func ensureWebhookQueue() {
	_, err := doltSQL(`CREATE TABLE IF NOT EXISTS webhook_queue (
		id          VARCHAR(36) PRIMARY KEY,
		event_type  VARCHAR(64) NOT NULL,
		linear_id   VARCHAR(32) NOT NULL,
		payload     JSON NOT NULL,
		processed   BOOLEAN NOT NULL DEFAULT 0,
		created_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
	)`, false)
	if err != nil {
		log.Printf("[daemon] ensure webhook_queue: %s", err)
	}
}
