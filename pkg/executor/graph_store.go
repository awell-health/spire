package executor

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"github.com/awell-health/spire/pkg/dolt"
)

// ErrNoTowerBound is returned by ResolveGraphStateStore when the caller
// supplies a zero-valued RepoIdentity (no active tower bound). Commands
// that surface to humans should catch this and print a clear "no tower
// bound" message directing the user to `spire tower create` /
// `spire repo add`.
var ErrNoTowerBound = errors.New("no tower bound: run `spire tower create` or `spire repo add` first")

// ErrAmbiguousPrefix is returned when a tower has multiple registered
// repos and the caller did not pick one. CLI commands should resolve
// this by either accepting an explicit --prefix flag or asking the user
// to choose. This error is never hit at the pkg/executor layer in
// practice — the CLI helpers in cmd/spire resolve identity before
// calling ResolveGraphStateStore — but it is exported here so CLI code
// can compare against it with errors.Is.
var ErrAmbiguousPrefix = errors.New("tower has multiple registered repos — pass --prefix to disambiguate")

// GraphStateStore abstracts graph state persistence.
// FileGraphStateStore (local) and DoltGraphStateStore (cluster) implement this.
type GraphStateStore interface {
	Load(agentName string) (*GraphState, error)
	Save(agentName string, state *GraphState) error
	Remove(agentName string) error
	ListHooked(towerName string) ([]HookedEntry, error)
}

// HookedEntry represents a graph state with hooked steps, used by the steward sweep.
type HookedEntry struct {
	AgentName string
	BeadID    string
	State     *GraphState
}

// --- FileGraphStateStore wraps existing filesystem functions ---

// FileGraphStateStore persists graph state as JSON files on the local filesystem.
type FileGraphStateStore struct {
	ConfigDir func() (string, error)
}

func (fs *FileGraphStateStore) Load(agentName string) (*GraphState, error) {
	return LoadGraphState(agentName, fs.ConfigDir)
}

func (fs *FileGraphStateStore) Save(agentName string, state *GraphState) error {
	return state.Save(agentName, fs.ConfigDir)
}

func (fs *FileGraphStateStore) Remove(agentName string) error {
	RemoveGraphState(agentName, fs.ConfigDir)
	return nil
}

func (fs *FileGraphStateStore) ListHooked(towerName string) ([]HookedEntry, error) {
	dir, err := fs.ConfigDir()
	if err != nil {
		return nil, err
	}
	runtimeDir := filepath.Join(dir, "runtime")
	entries, err := os.ReadDir(runtimeDir)
	if err != nil {
		return nil, nil // no runtime dir yet
	}

	var result []HookedEntry
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		agentName := entry.Name()
		state, err := LoadGraphState(agentName, fs.ConfigDir)
		if err != nil || state == nil {
			continue
		}
		// Skip states belonging to a different tower.
		// Empty TowerName means legacy (pre-migration) — include from any tower.
		if towerName != "" && state.TowerName != "" && state.TowerName != towerName {
			continue
		}
		if state.HasHookedSteps() {
			result = append(result, HookedEntry{
				AgentName: agentName,
				BeadID:    state.BeadID,
				State:     state,
			})
		}
	}
	return result, nil
}

// --- DoltGraphStateStore uses a Dolt MySQL table ---

// DoltGraphStateStore persists graph state in a Dolt database table for cluster mode.
type DoltGraphStateStore struct {
	db *sql.DB
}

// NewDoltGraphStateStore opens a connection to the Dolt server and ensures
// the graph_state table exists.
func NewDoltGraphStateStore(dsn string) (*DoltGraphStateStore, error) {
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("open dolt connection: %w", err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping dolt: %w", err)
	}
	store := &DoltGraphStateStore{db: db}
	if err := store.EnsureTable(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ensure graph_state table: %w", err)
	}
	return store, nil
}

// EnsureTable creates the graph_state table if it doesn't already exist.
func (ds *DoltGraphStateStore) EnsureTable() error {
	_, err := ds.db.Exec(`CREATE TABLE IF NOT EXISTS graph_state (
		agent_name       VARCHAR(255) PRIMARY KEY,
		bead_id          VARCHAR(255) NOT NULL,
		tower            VARCHAR(255) NOT NULL,
		state_json       LONGTEXT NOT NULL,
		has_hooked_steps BOOLEAN NOT NULL DEFAULT FALSE,
		updated_at       DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
	)`)
	return err
}

func (ds *DoltGraphStateStore) Load(agentName string) (*GraphState, error) {
	var stateJSON string
	err := ds.db.QueryRow("SELECT state_json FROM graph_state WHERE agent_name = ?", agentName).Scan(&stateJSON)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("query graph state: %w", err)
	}
	var state GraphState
	if err := json.Unmarshal([]byte(stateJSON), &state); err != nil {
		return nil, fmt.Errorf("parse graph state: %w", err)
	}
	return &state, nil
}

func (ds *DoltGraphStateStore) Save(agentName string, state *GraphState) error {
	state.LastActionAt = time.Now().UTC().Format(time.RFC3339)
	data, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("marshal graph state: %w", err)
	}
	hasHooked := state.HasHookedSteps()
	_, err = ds.db.Exec(`INSERT INTO graph_state (agent_name, bead_id, tower, state_json, has_hooked_steps, updated_at)
		VALUES (?, ?, ?, ?, ?, NOW())
		ON DUPLICATE KEY UPDATE state_json = VALUES(state_json), has_hooked_steps = VALUES(has_hooked_steps), updated_at = NOW()`,
		agentName, state.BeadID, state.TowerName, string(data), hasHooked)
	if err != nil {
		return fmt.Errorf("upsert graph state: %w", err)
	}
	return nil
}

func (ds *DoltGraphStateStore) Remove(agentName string) error {
	_, err := ds.db.Exec("DELETE FROM graph_state WHERE agent_name = ?", agentName)
	return err
}

func (ds *DoltGraphStateStore) ListHooked(towerName string) ([]HookedEntry, error) {
	rows, err := ds.db.Query(
		"SELECT agent_name, bead_id, state_json FROM graph_state WHERE tower = ? AND has_hooked_steps = TRUE",
		towerName)
	if err != nil {
		return nil, fmt.Errorf("query hooked states: %w", err)
	}
	defer rows.Close()

	var result []HookedEntry
	for rows.Next() {
		var agentName, beadID, stateJSON string
		if err := rows.Scan(&agentName, &beadID, &stateJSON); err != nil {
			continue
		}
		var state GraphState
		if err := json.Unmarshal([]byte(stateJSON), &state); err != nil {
			continue
		}
		result = append(result, HookedEntry{
			AgentName: agentName,
			BeadID:    beadID,
			State:     &state,
		})
	}
	return result, nil
}

// --- Factory ---

// ResolveGraphStateStore returns a DoltGraphStateStore when running in
// cluster mode (BEADS_DOLT_SERVER_HOST is a remote host), otherwise a
// FileGraphStateStore.
//
// identity.TowerName is the dolt database the cluster-mode store
// connects to. A zero-valued identity returns ErrNoTowerBound —
// runtime-critical code may NOT derive tower/prefix from ambient CWD.
// Prior versions fell back to dolt.ReadBeadsDBName(os.Getwd) + the
// hardcoded "spire" default; that path is permanently removed.
// See docs/design/spi-xplwy-runtime-contract.md §1.1 ("RepoIdentity is
// always resolved from pkg/config tower state plus pkg/store repo
// registration for Prefix — no path/CWD inference is permitted in
// runtime-critical code").
//
// CLI commands that need a graph-state store are expected to resolve a
// RepoIdentity via pkg/config.ActiveTowerConfig + registered-repo
// lookup (see cmd/spire resolveRepoIdentity helper) and pass it in.
// Commands that legitimately run outside a bound repo (e.g.
// `spire tower create`) must not call this function.
//
// On cluster-mode DSN open / ping failure the function falls back to
// the file store rather than failing the caller — losing cluster-mode
// persistence is surfaced via the Dolt connection error logs, and the
// file store is always usable. The fallback is an existing behavior
// preserved across this refactor.
func ResolveGraphStateStore(identity RepoIdentity, configDirFn func() (string, error)) (GraphStateStore, error) {
	if identity.TowerName == "" {
		return nil, ErrNoTowerBound
	}
	if host := os.Getenv("BEADS_DOLT_SERVER_HOST"); host != "" && host != "127.0.0.1" && host != "localhost" {
		// Ambient-CWD resolution was removed here. The database name is
		// the caller-supplied identity.TowerName. See
		// docs/design/spi-xplwy-runtime-contract.md §1.1 — this is the
		// single biggest ambient-CWD removal in the runtime contract
		// migration (chunk 3 of §4).
		dsn := fmt.Sprintf("root:@tcp(%s:%s)/%s?parseTime=true&timeout=5s",
			host, dolt.Port(), identity.TowerName)
		store, err := NewDoltGraphStateStore(dsn)
		if err == nil {
			return store, nil
		}
		// Fall through to file store on connection error.
	}
	return &FileGraphStateStore{ConfigDir: configDirFn}, nil
}
