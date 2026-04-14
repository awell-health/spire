package agent

import (
	"encoding/json"
	"testing"
)

func TestWithInstanceID(t *testing.T) {
	var e Entry
	opt := WithInstanceID("inst-abc123")
	opt(&e)
	if e.InstanceID != "inst-abc123" {
		t.Fatalf("expected InstanceID %q, got %q", "inst-abc123", e.InstanceID)
	}
}

func TestWithInstanceIDEmpty(t *testing.T) {
	var e Entry
	opt := WithInstanceID("")
	opt(&e)
	if e.InstanceID != "" {
		t.Fatalf("expected empty InstanceID, got %q", e.InstanceID)
	}
}

func TestRegisterSelfWithInstanceID(t *testing.T) {
	// Override registry path to a temp dir so we don't pollute real state.
	origPath := RegistryPath()
	tmp := t.TempDir()
	t.Setenv("SPIRE_CONFIG_DIR", tmp)
	defer func() {
		// Restore — RegistryPath reads config.Dir() which uses SPIRE_CONFIG_DIR.
		_ = origPath
	}()

	cleanup := RegisterSelf("test-agent", "spi-test", "implement",
		WithInstanceID("inst-xyz"),
	)
	defer cleanup()

	reg := LoadRegistry()
	var found *Entry
	for i := range reg.Wizards {
		if reg.Wizards[i].Name == "test-agent" {
			found = &reg.Wizards[i]
			break
		}
	}
	if found == nil {
		t.Fatal("expected to find test-agent in registry")
	}
	if found.InstanceID != "inst-xyz" {
		t.Fatalf("expected InstanceID %q, got %q", "inst-xyz", found.InstanceID)
	}
	if found.BeadID != "spi-test" {
		t.Fatalf("expected BeadID %q, got %q", "spi-test", found.BeadID)
	}
	if found.Phase != "implement" {
		t.Fatalf("expected Phase %q, got %q", "implement", found.Phase)
	}
}

func TestEntryJSONIncludesInstanceID(t *testing.T) {
	e := Entry{
		Name:       "test-wizard",
		PID:        1234,
		BeadID:     "spi-abc",
		InstanceID: "inst-def",
		Tower:      "my-tower",
	}
	data, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var m map[string]interface{}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m["instance_id"] != "inst-def" {
		t.Fatalf("expected instance_id %q in JSON, got %v", "inst-def", m["instance_id"])
	}
}

func TestEntryJSONOmitsEmptyInstanceID(t *testing.T) {
	e := Entry{
		Name:   "test-wizard",
		PID:    1234,
		BeadID: "spi-abc",
	}
	data, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var m map[string]interface{}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := m["instance_id"]; ok {
		t.Fatal("expected instance_id to be omitted from JSON when empty")
	}
}

func TestEntryJSONRoundTrip(t *testing.T) {
	original := Entry{
		Name:       "wizard-spi-abc",
		PID:        5678,
		BeadID:     "spi-abc",
		InstanceID: "inst-round",
		Tower:      "tower-1",
		Phase:      "implement",
	}
	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded Entry
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.InstanceID != original.InstanceID {
		t.Fatalf("expected InstanceID %q after round-trip, got %q", original.InstanceID, decoded.InstanceID)
	}
}
