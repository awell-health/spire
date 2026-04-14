package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/google/uuid"
)

func TestInstanceID_ValidUUID(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("SPIRE_CONFIG_DIR", tmp)
	resetInstanceOnce()

	id := InstanceID()
	if _, err := uuid.Parse(id); err != nil {
		t.Fatalf("InstanceID() returned invalid UUID %q: %v", id, err)
	}
}

func TestInstanceID_Stable(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("SPIRE_CONFIG_DIR", tmp)
	resetInstanceOnce()

	first := InstanceID()
	second := InstanceID()
	if first != second {
		t.Fatalf("InstanceID() not stable: %q != %q", first, second)
	}
}

func TestInstanceID_PersistsToDisk(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("SPIRE_CONFIG_DIR", tmp)
	resetInstanceOnce()

	id := InstanceID()

	// Read back from file.
	data, err := os.ReadFile(filepath.Join(tmp, "instance.json"))
	if err != nil {
		t.Fatalf("instance.json not written: %v", err)
	}

	var info InstanceInfo
	if err := json.Unmarshal(data, &info); err != nil {
		t.Fatalf("instance.json not valid JSON: %v", err)
	}
	if info.ID != id {
		t.Fatalf("persisted ID %q != returned ID %q", info.ID, id)
	}
}

func TestInstanceID_SurvivesReread(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("SPIRE_CONFIG_DIR", tmp)
	resetInstanceOnce()

	id := InstanceID()

	// Reset the once so InstanceID re-reads from disk.
	resetInstanceOnce()

	id2 := InstanceID()
	if id != id2 {
		t.Fatalf("InstanceID() changed after re-read: %q != %q", id, id2)
	}
}

func TestInstanceName_NonEmpty(t *testing.T) {
	name := InstanceName()
	if name == "" {
		t.Fatal("InstanceName() returned empty string")
	}
}

func TestGetInstanceInfo(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("SPIRE_CONFIG_DIR", tmp)
	resetInstanceOnce()

	info := GetInstanceInfo()
	if _, err := uuid.Parse(info.ID); err != nil {
		t.Fatalf("GetInstanceInfo().ID invalid UUID: %v", err)
	}
	if info.Name == "" {
		t.Fatal("GetInstanceInfo().Name is empty")
	}
}

// resetInstanceOnce clears the sync.Once so each test starts fresh.
func resetInstanceOnce() {
	instanceOnce = sync.Once{}
	instanceID = ""
}
