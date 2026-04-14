package store

import (
	"testing"
	"time"
)

// stubMeta is a test helper that replaces the store function variables with
// in-memory fakes, returning the backing map for inspection.
func stubMeta(t *testing.T) map[string]map[string]string {
	t.Helper()
	db := make(map[string]map[string]string) // beadID -> metadata

	origGet := getBeadMetadataFunc
	origSetMap := setBeadMetadataMapFunc
	origSet := setBeadMetadataFunc
	t.Cleanup(func() {
		getBeadMetadataFunc = origGet
		setBeadMetadataMapFunc = origSetMap
		setBeadMetadataFunc = origSet
	})

	getBeadMetadataFunc = func(id string) (map[string]string, error) {
		m := db[id]
		if m == nil {
			return map[string]string{}, nil
		}
		cp := make(map[string]string, len(m))
		for k, v := range m {
			cp[k] = v
		}
		return cp, nil
	}
	setBeadMetadataMapFunc = func(id string, kv map[string]string) error {
		if db[id] == nil {
			db[id] = make(map[string]string)
		}
		for k, v := range kv {
			db[id][k] = v
		}
		return nil
	}
	setBeadMetadataFunc = func(id, key, value string) error {
		if db[id] == nil {
			db[id] = make(map[string]string)
		}
		db[id][key] = value
		return nil
	}

	return db
}

func TestStampAttemptInstance(t *testing.T) {
	db := stubMeta(t)

	err := StampAttemptInstance("att-001", InstanceMeta{
		InstanceID:   "inst-aaa",
		SessionID:    "sess-111",
		InstanceName: "my-laptop",
		Backend:      "process",
		Tower:        "dev",
		StartedAt:    "2026-01-01T00:00:00Z",
		LastSeenAt:   "2026-01-01T00:00:01Z",
	})
	if err != nil {
		t.Fatalf("StampAttemptInstance: %v", err)
	}

	m := db["att-001"]
	expect := map[string]string{
		"instance_id":    "inst-aaa",
		"session_id":     "sess-111",
		"instance_name":  "my-laptop",
		"backend":        "process",
		"tower":          "dev",
		"lease_started_at": "2026-01-01T00:00:00Z",
		"last_seen_at":   "2026-01-01T00:00:01Z",
	}
	for k, want := range expect {
		if got := m[k]; got != want {
			t.Errorf("key %q = %q, want %q", k, got, want)
		}
	}
}

func TestStampAttemptInstance_SkipsEmpty(t *testing.T) {
	db := stubMeta(t)

	err := StampAttemptInstance("att-002", InstanceMeta{
		InstanceID: "inst-bbb",
		// all others empty
	})
	if err != nil {
		t.Fatalf("StampAttemptInstance: %v", err)
	}

	m := db["att-002"]
	if got := m["instance_id"]; got != "inst-bbb" {
		t.Errorf("instance_id = %q, want %q", got, "inst-bbb")
	}
	for _, key := range []string{"session_id", "instance_name", "backend", "tower", "lease_started_at", "last_seen_at"} {
		if _, ok := m[key]; ok {
			t.Errorf("expected key %q to be absent, but it was set to %q", key, m[key])
		}
	}
}

func TestGetAttemptInstance_RoundTrip(t *testing.T) {
	stubMeta(t)

	want := InstanceMeta{
		InstanceID:   "inst-ccc",
		SessionID:    "sess-222",
		InstanceName: "server-1",
		Backend:      "docker",
		Tower:        "prod",
		StartedAt:    "2026-02-01T12:00:00Z",
		LastSeenAt:   "2026-02-01T12:05:00Z",
	}
	if err := StampAttemptInstance("att-003", want); err != nil {
		t.Fatalf("stamp: %v", err)
	}

	got, err := GetAttemptInstance("att-003")
	if err != nil {
		t.Fatalf("GetAttemptInstance: %v", err)
	}
	if got == nil {
		t.Fatal("GetAttemptInstance returned nil")
	}
	if *got != want {
		t.Errorf("round-trip mismatch:\n  got:  %+v\n  want: %+v", *got, want)
	}
}

func TestGetAttemptInstance_NilForUnstamped(t *testing.T) {
	stubMeta(t)

	got, err := GetAttemptInstance("att-unstamped")
	if err != nil {
		t.Fatalf("GetAttemptInstance: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil for unstamped bead, got %+v", got)
	}
}

func TestIsOwnedByInstance_MatchesCorrectly(t *testing.T) {
	stubMeta(t)

	if err := StampAttemptInstance("att-004", InstanceMeta{
		InstanceID:   "inst-ddd",
		InstanceName: "laptop-1",
	}); err != nil {
		t.Fatalf("stamp: %v", err)
	}

	// Same instance → true
	owned, err := IsOwnedByInstance("att-004", "inst-ddd")
	if err != nil {
		t.Fatalf("IsOwnedByInstance: %v", err)
	}
	if !owned {
		t.Error("expected owned=true for matching instance")
	}

	// Different instance → false
	owned, err = IsOwnedByInstance("att-004", "inst-other")
	if err != nil {
		t.Fatalf("IsOwnedByInstance: %v", err)
	}
	if owned {
		t.Error("expected owned=false for non-matching instance")
	}
}

func TestIsOwnedByInstance_TrueForUnstamped(t *testing.T) {
	stubMeta(t)

	owned, err := IsOwnedByInstance("att-unstamped", "inst-any")
	if err != nil {
		t.Fatalf("IsOwnedByInstance: %v", err)
	}
	if !owned {
		t.Error("expected owned=true for unstamped bead (backward compat)")
	}
}

func TestUpdateAttemptHeartbeat(t *testing.T) {
	db := stubMeta(t)

	// Pre-stamp with known values
	if err := StampAttemptInstance("att-005", InstanceMeta{
		InstanceID: "inst-eee",
		StartedAt:  "2026-03-01T00:00:00Z",
		LastSeenAt: "2026-03-01T00:00:00Z",
	}); err != nil {
		t.Fatalf("stamp: %v", err)
	}

	before := time.Now().UTC().Truncate(time.Second)
	if err := UpdateAttemptHeartbeat("att-005"); err != nil {
		t.Fatalf("UpdateAttemptHeartbeat: %v", err)
	}
	after := time.Now().UTC().Truncate(time.Second).Add(time.Second)

	m := db["att-005"]

	// instance_id should be unchanged
	if got := m["instance_id"]; got != "inst-eee" {
		t.Errorf("instance_id changed: got %q", got)
	}

	// lease_started_at should be unchanged
	if got := m["lease_started_at"]; got != "2026-03-01T00:00:00Z" {
		t.Errorf("lease_started_at changed: got %q", got)
	}

	// last_seen_at should be updated to a recent timestamp
	ts, err := time.Parse(time.RFC3339, m["last_seen_at"])
	if err != nil {
		t.Fatalf("parse last_seen_at %q: %v", m["last_seen_at"], err)
	}
	if ts.Before(before) || ts.After(after) {
		t.Errorf("last_seen_at %v not in expected range [%v, %v]", ts, before, after)
	}
}
