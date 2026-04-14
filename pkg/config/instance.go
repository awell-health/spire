package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"

	"github.com/google/uuid"
)

// InstanceInfo holds the stable identity of this Spire installation.
// ID is a persistent UUIDv4 (generated once, stored on disk).
// Name is the current hostname (informational, not authoritative).
type InstanceInfo struct {
	ID   string `json:"instance_id"`
	Name string `json:"instance_name"`
}

var (
	instanceOnce sync.Once
	instanceID   string
)

// InstanceID returns the stable instance UUID for this machine.
// On first call it reads (or creates) ~/.config/spire/instance.json.
// The value is cached for the process lifetime via sync.Once.
func InstanceID() string {
	instanceOnce.Do(func() {
		instanceID = loadOrCreateInstanceID()
	})
	return instanceID
}

// InstanceName returns os.Hostname, or "unknown" on error.
// This is informational only — not authoritative for identity.
func InstanceName() string {
	h, err := os.Hostname()
	if err != nil {
		return "unknown"
	}
	return h
}

// GetInstanceInfo returns both the stable ID and the current hostname.
func GetInstanceInfo() InstanceInfo {
	return InstanceInfo{
		ID:   InstanceID(),
		Name: InstanceName(),
	}
}

const instanceFile = "instance.json"

func loadOrCreateInstanceID() string {
	dir, err := Dir()
	if err != nil {
		// Can't resolve config dir — generate an ephemeral ID.
		return uuid.New().String()
	}

	path := filepath.Join(dir, instanceFile)

	data, err := os.ReadFile(path)
	if err == nil {
		var info InstanceInfo
		if json.Unmarshal(data, &info) == nil && info.ID != "" {
			return info.ID
		}
	}

	// First run or corrupt file — generate and persist.
	info := InstanceInfo{
		ID:   uuid.New().String(),
		Name: InstanceName(),
	}

	if err := os.MkdirAll(dir, 0755); err != nil {
		return info.ID
	}

	data, err = json.MarshalIndent(info, "", "  ")
	if err != nil {
		return info.ID
	}

	_ = os.WriteFile(path, data, 0600)
	return info.ID
}
