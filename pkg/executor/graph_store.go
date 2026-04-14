package executor

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"github.com/awell-health/spire/pkg/dolt"
)

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

// ResolveGraphStateStore returns a DoltGraphStateStore when running in cluster
// mode (BEADS_DOLT_SERVER_HOST is a remote host), otherwise a FileGraphStateStore.
func ResolveGraphStateStore(configDirFn func() (string, error)) GraphStateStore {
	if host := os.Getenv("BEADS_DOLT_SERVER_HOST"); host != "" && host != "127.0.0.1" && host != "localhost" {
		database := dolt.ReadBeadsDBName(os.Getwd)
		if database == "" {
			database = "spire"
		}
		dsn := fmt.Sprintf("root:@tcp(%s:%s)/%s?parseTime=true&timeout=5s",
			host, dolt.Port(), database)
		store, err := NewDoltGraphStateStore(dsn)
		if err == nil {
			return store
		}
		// Fall through to file store on error
	}
	return &FileGraphStateStore{ConfigDir: configDirFn}
}
