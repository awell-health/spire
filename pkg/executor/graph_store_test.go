package executor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
)

// --- FileGraphStateStore tests ---

func TestFileGraphStateStore_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	configDirFn := func() (string, error) { return dir, nil }
	store := &FileGraphStateStore{ConfigDir: configDirFn}

	state := &GraphState{
		BeadID:    "spi-test1",
		AgentName: "wizard-test1",
		Formula:   "task-default",
		TowerName: "test-tower",
		Steps: map[string]StepState{
			"plan":      {Status: "completed"},
			"implement": {Status: "active"},
		},
		Counters:   map[string]int{"review_rounds": 1},
		Workspaces: map[string]WorkspaceState{},
		Vars:       map[string]string{"bead_id": "spi-test1"},
	}

	// Save
	if err := store.Save("wizard-test1", state); err != nil {
		t.Fatalf("Save error: %v", err)
	}

	// Load
	loaded, err := store.Load("wizard-test1")
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	if loaded == nil {
		t.Fatal("Load returned nil")
	}
	if loaded.BeadID != "spi-test1" {
		t.Errorf("BeadID = %q, want spi-test1", loaded.BeadID)
	}
	if loaded.Formula != "task-default" {
		t.Errorf("Formula = %q, want task-default", loaded.Formula)
	}
	if loaded.Steps["plan"].Status != "completed" {
		t.Errorf("plan status = %q, want completed", loaded.Steps["plan"].Status)
	}
	if loaded.Steps["implement"].Status != "active" {
		t.Errorf("implement status = %q, want active", loaded.Steps["implement"].Status)
	}
}

func TestFileGraphStateStore_Remove(t *testing.T) {
	dir := t.TempDir()
	configDirFn := func() (string, error) { return dir, nil }
	store := &FileGraphStateStore{ConfigDir: configDirFn}

	state := &GraphState{
		BeadID:     "spi-rm",
		AgentName:  "wizard-rm",
		Steps:      map[string]StepState{"plan": {Status: "completed"}},
		Counters:   map[string]int{},
		Workspaces: map[string]WorkspaceState{},
		Vars:       map[string]string{},
	}

	if err := store.Save("wizard-rm", state); err != nil {
		t.Fatalf("Save error: %v", err)
	}

	// Remove
	if err := store.Remove("wizard-rm"); err != nil {
		t.Fatalf("Remove error: %v", err)
	}

	// Load should return nil
	loaded, err := store.Load("wizard-rm")
	if err != nil {
		t.Fatalf("Load after Remove error: %v", err)
	}
	if loaded != nil {
		t.Errorf("expected nil after Remove, got %+v", loaded)
	}
}

func TestFileGraphStateStore_ListHooked(t *testing.T) {
	dir := t.TempDir()
	configDirFn := func() (string, error) { return dir, nil }
	store := &FileGraphStateStore{ConfigDir: configDirFn}

	// Create two agents: one with hooked steps, one without.
	hookedState := &GraphState{
		BeadID:    "spi-hooked",
		AgentName: "wizard-hooked",
		TowerName: "test-tower",
		Steps: map[string]StepState{
			"plan":      {Status: "completed"},
			"implement": {Status: "hooked", Outputs: map[string]string{"design_ref": "spi-design1"}},
		},
		Counters:   map[string]int{},
		Workspaces: map[string]WorkspaceState{},
		Vars:       map[string]string{},
	}
	normalState := &GraphState{
		BeadID:    "spi-normal",
		AgentName: "wizard-normal",
		TowerName: "test-tower",
		Steps: map[string]StepState{
			"plan":      {Status: "completed"},
			"implement": {Status: "active"},
		},
		Counters:   map[string]int{},
		Workspaces: map[string]WorkspaceState{},
		Vars:       map[string]string{},
	}
	otherTowerState := &GraphState{
		BeadID:    "spi-other",
		AgentName: "wizard-other",
		TowerName: "other-tower",
		Steps: map[string]StepState{
			"plan": {Status: "hooked"},
		},
		Counters:   map[string]int{},
		Workspaces: map[string]WorkspaceState{},
		Vars:       map[string]string{},
	}

	for name, s := range map[string]*GraphState{
		"wizard-hooked": hookedState,
		"wizard-normal": normalState,
		"wizard-other":  otherTowerState,
	} {
		if err := store.Save(name, s); err != nil {
			t.Fatalf("Save %s error: %v", name, err)
		}
	}

	entries, err := store.ListHooked("test-tower")
	if err != nil {
		t.Fatalf("ListHooked error: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("ListHooked returned %d entries, want 1", len(entries))
	}
	if entries[0].AgentName != "wizard-hooked" {
		t.Errorf("entry AgentName = %q, want wizard-hooked", entries[0].AgentName)
	}
	if entries[0].BeadID != "spi-hooked" {
		t.Errorf("entry BeadID = %q, want spi-hooked", entries[0].BeadID)
	}
}

func TestFileGraphStateStore_LoadNonExistent(t *testing.T) {
	dir := t.TempDir()
	configDirFn := func() (string, error) { return dir, nil }
	store := &FileGraphStateStore{ConfigDir: configDirFn}

	loaded, err := store.Load("wizard-does-not-exist")
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	if loaded != nil {
		t.Errorf("expected nil for non-existent agent, got %+v", loaded)
	}
}

// --- DoltGraphStateStore tests (using go-sqlmock) ---

func TestDoltGraphStateStore_RoundTrip(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	store := &DoltGraphStateStore{db: db}

	state := &GraphState{
		BeadID:    "spi-dolt1",
		AgentName: "wizard-dolt1",
		TowerName: "test-tower",
		Steps: map[string]StepState{
			"plan": {Status: "completed"},
		},
		Counters:   map[string]int{},
		Workspaces: map[string]WorkspaceState{},
		Vars:       map[string]string{},
	}

	// Expect Save (INSERT ... ON DUPLICATE KEY UPDATE) — 5 placeholders (updated_at uses NOW())
	mock.ExpectExec("INSERT INTO graph_state").
		WithArgs("wizard-dolt1", "spi-dolt1", "test-tower", sqlmock.AnyArg(), false).
		WillReturnResult(sqlmock.NewResult(1, 1))

	if err := store.Save("wizard-dolt1", state); err != nil {
		t.Fatalf("Save error: %v", err)
	}

	// Expect Load (SELECT state_json)
	stateJSON, _ := json.Marshal(state)
	mock.ExpectQuery("SELECT state_json FROM graph_state WHERE agent_name").
		WithArgs("wizard-dolt1").
		WillReturnRows(sqlmock.NewRows([]string{"state_json"}).AddRow(string(stateJSON)))

	loaded, err := store.Load("wizard-dolt1")
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	if loaded == nil {
		t.Fatal("Load returned nil")
	}
	if loaded.BeadID != "spi-dolt1" {
		t.Errorf("BeadID = %q, want spi-dolt1", loaded.BeadID)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unfulfilled expectations: %v", err)
	}
}

func TestDoltGraphStateStore_ListHooked(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	store := &DoltGraphStateStore{db: db}

	state := &GraphState{
		BeadID:    "spi-hook1",
		AgentName: "wizard-hook1",
		TowerName: "test-tower",
		Steps: map[string]StepState{
			"plan": {Status: "hooked"},
		},
		Counters:   map[string]int{},
		Workspaces: map[string]WorkspaceState{},
		Vars:       map[string]string{},
	}
	stateJSON, _ := json.Marshal(state)

	mock.ExpectQuery("SELECT agent_name, bead_id, state_json FROM graph_state WHERE tower").
		WithArgs("test-tower").
		WillReturnRows(sqlmock.NewRows([]string{"agent_name", "bead_id", "state_json"}).
			AddRow("wizard-hook1", "spi-hook1", string(stateJSON)))

	entries, err := store.ListHooked("test-tower")
	if err != nil {
		t.Fatalf("ListHooked error: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("ListHooked returned %d entries, want 1", len(entries))
	}
	if entries[0].AgentName != "wizard-hook1" {
		t.Errorf("entry AgentName = %q, want wizard-hook1", entries[0].AgentName)
	}
	if entries[0].BeadID != "spi-hook1" {
		t.Errorf("entry BeadID = %q, want spi-hook1", entries[0].BeadID)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unfulfilled expectations: %v", err)
	}
}

func TestDoltGraphStateStore_Remove(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	store := &DoltGraphStateStore{db: db}

	mock.ExpectExec("DELETE FROM graph_state WHERE agent_name").
		WithArgs("wizard-rm1").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := store.Remove("wizard-rm1"); err != nil {
		t.Fatalf("Remove error: %v", err)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unfulfilled expectations: %v", err)
	}
}

// --- ResolveGraphStateStore tests ---

func TestResolveGraphStateStore_Local(t *testing.T) {
	// Without BEADS_DOLT_SERVER_HOST set, should return FileGraphStateStore.
	t.Setenv("BEADS_DOLT_SERVER_HOST", "")
	dir := t.TempDir()
	configDirFn := func() (string, error) { return dir, nil }
	store := ResolveGraphStateStore(configDirFn)

	if _, ok := store.(*FileGraphStateStore); !ok {
		t.Errorf("expected *FileGraphStateStore, got %T", store)
	}
}

func TestResolveGraphStateStore_LocalHost(t *testing.T) {
	// With BEADS_DOLT_SERVER_HOST=127.0.0.1, should still return FileGraphStateStore.
	t.Setenv("BEADS_DOLT_SERVER_HOST", "127.0.0.1")
	dir := t.TempDir()
	configDirFn := func() (string, error) { return dir, nil }
	store := ResolveGraphStateStore(configDirFn)

	if _, ok := store.(*FileGraphStateStore); !ok {
		t.Errorf("expected *FileGraphStateStore for localhost, got %T", store)
	}
}

func TestResolveGraphStateStore_Cluster(t *testing.T) {
	// With BEADS_DOLT_SERVER_HOST set to a remote host, should attempt DoltGraphStateStore.
	// Since we can't connect, it falls back to FileGraphStateStore.
	t.Setenv("BEADS_DOLT_SERVER_HOST", "192.168.1.100")
	dir := t.TempDir()
	configDirFn := func() (string, error) { return dir, nil }
	store := ResolveGraphStateStore(configDirFn)

	// Falls back to FileGraphStateStore because the connection fails.
	if _, ok := store.(*FileGraphStateStore); !ok {
		t.Errorf("expected fallback to *FileGraphStateStore when Dolt unreachable, got %T", store)
	}
}

// --- File-level integration test ---

func TestFileGraphStateStore_FilePersistence(t *testing.T) {
	// Verify the file is actually written to the expected path.
	dir := t.TempDir()
	configDirFn := func() (string, error) { return dir, nil }
	store := &FileGraphStateStore{ConfigDir: configDirFn}

	state := &GraphState{
		BeadID:     "spi-file1",
		AgentName:  "wizard-file1",
		Steps:      map[string]StepState{"plan": {Status: "pending"}},
		Counters:   map[string]int{},
		Workspaces: map[string]WorkspaceState{},
		Vars:       map[string]string{},
	}

	if err := store.Save("wizard-file1", state); err != nil {
		t.Fatalf("Save error: %v", err)
	}

	expectedPath := filepath.Join(dir, "runtime", "wizard-file1", "graph_state.json")
	if _, err := os.Stat(expectedPath); os.IsNotExist(err) {
		t.Fatalf("expected file at %s, but it doesn't exist", expectedPath)
	}

	// Verify the content is valid JSON.
	data, err := os.ReadFile(expectedPath)
	if err != nil {
		t.Fatalf("ReadFile error: %v", err)
	}
	var parsed GraphState
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("JSON unmarshal error: %v", err)
	}
	if parsed.BeadID != "spi-file1" {
		t.Errorf("persisted BeadID = %q, want spi-file1", parsed.BeadID)
	}
}
