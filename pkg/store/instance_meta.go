package store

import "time"

// InstanceMeta holds the execution lease metadata stamped on an attempt bead.
type InstanceMeta struct {
	InstanceID   string // stable UUID identifying the Spire install/runtime
	SessionID    string // unique ID for this particular execution run
	InstanceName string // human-readable hostname for UI
	Backend      string // 'process', 'docker', or 'k8s'
	Tower        string // tower name
	StartedAt    string // RFC3339 lease start timestamp
	LastSeenAt   string // RFC3339 heartbeat timestamp
}

// Metadata key constants for instance lease fields.
const (
	metaKeyInstanceID   = "instance_id"
	metaKeySessionID    = "session_id"
	metaKeyInstanceName = "instance_name"
	metaKeyBackend      = "backend"
	metaKeyTower        = "tower"
	metaKeyStartedAt    = "lease_started_at"
	metaKeyLastSeenAt   = "last_seen_at"
)

// Test-replaceable function variables for store operations.
var (
	getBeadMetadataFunc    = GetBeadMetadata
	setBeadMetadataMapFunc = SetBeadMetadataMap
	setBeadMetadataFunc    = SetBeadMetadata
)

// StampAttemptInstance writes instance ownership metadata onto an attempt bead.
// Only non-empty values are written.
func StampAttemptInstance(attemptID string, m InstanceMeta) error {
	kv := make(map[string]string)
	if m.InstanceID != "" {
		kv[metaKeyInstanceID] = m.InstanceID
	}
	if m.SessionID != "" {
		kv[metaKeySessionID] = m.SessionID
	}
	if m.InstanceName != "" {
		kv[metaKeyInstanceName] = m.InstanceName
	}
	if m.Backend != "" {
		kv[metaKeyBackend] = m.Backend
	}
	if m.Tower != "" {
		kv[metaKeyTower] = m.Tower
	}
	if m.StartedAt != "" {
		kv[metaKeyStartedAt] = m.StartedAt
	}
	if m.LastSeenAt != "" {
		kv[metaKeyLastSeenAt] = m.LastSeenAt
	}
	return setBeadMetadataMapFunc(attemptID, kv)
}

// GetAttemptInstance reads instance ownership metadata from an attempt bead.
// Returns nil (not error) if the instance_id key is absent, indicating a
// pre-migration attempt with no instance stamp.
func GetAttemptInstance(attemptID string) (*InstanceMeta, error) {
	meta, err := getBeadMetadataFunc(attemptID)
	if err != nil {
		return nil, err
	}
	id := meta[metaKeyInstanceID]
	if id == "" {
		return nil, nil
	}
	return &InstanceMeta{
		InstanceID:   id,
		SessionID:    meta[metaKeySessionID],
		InstanceName: meta[metaKeyInstanceName],
		Backend:      meta[metaKeyBackend],
		Tower:        meta[metaKeyTower],
		StartedAt:    meta[metaKeyStartedAt],
		LastSeenAt:   meta[metaKeyLastSeenAt],
	}, nil
}

// IsOwnedByInstance checks whether the attempt bead is owned by the given
// instanceID. Returns true if instance_id matches. Also returns true if no
// instance metadata exists (backward compat: unstamped attempts are assumed
// local).
func IsOwnedByInstance(attemptID, instanceID string) (bool, error) {
	m, err := GetAttemptInstance(attemptID)
	if err != nil {
		return false, err
	}
	if m == nil {
		return true, nil // unstamped = assumed local
	}
	return m.InstanceID == instanceID, nil
}

// UpdateAttemptHeartbeat updates the last_seen_at timestamp on an attempt bead
// to the current UTC time.
func UpdateAttemptHeartbeat(attemptID string) error {
	return setBeadMetadataFunc(attemptID, metaKeyLastSeenAt, time.Now().UTC().Format(time.RFC3339))
}
