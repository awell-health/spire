package bundlestore

import (
	"strings"
	"testing"
)

func TestApprenticeRole(t *testing.T) {
	cases := []struct {
		bead string
		idx  int
		want string
	}{
		{"spi-abc", 0, "apprentice-spi-abc-0"},
		{"spi-abc", 5, "apprentice-spi-abc-5"},
	}
	for _, tc := range cases {
		got := ApprenticeRole(tc.bead, tc.idx)
		if got != tc.want {
			t.Errorf("ApprenticeRole(%q, %d) = %q, want %q", tc.bead, tc.idx, got, tc.want)
		}
	}
}

func TestSignalMetadataKey(t *testing.T) {
	got := SignalMetadataKey("apprentice-spi-abc-0")
	want := "apprentice_signal_apprentice-spi-abc-0"
	if got != want {
		t.Errorf("SignalMetadataKey = %q, want %q", got, want)
	}
}

func TestParseSignal_Bundle(t *testing.T) {
	raw := `{"kind":"bundle","role":"apprentice-spi-abc-0","bundle_key":"spi-abc/spi-att-0.bundle","commits":["sha1","sha2"],"submitted_at":"2026-04-20T16:00:00Z"}`
	s, err := ParseSignal(raw)
	if err != nil {
		t.Fatalf("ParseSignal: %v", err)
	}
	if s.Kind != SignalKindBundle {
		t.Errorf("Kind = %q, want %q", s.Kind, SignalKindBundle)
	}
	if s.Role != "apprentice-spi-abc-0" {
		t.Errorf("Role = %q", s.Role)
	}
	if s.BundleKey != "spi-abc/spi-att-0.bundle" {
		t.Errorf("BundleKey = %q", s.BundleKey)
	}
	if len(s.Commits) != 2 || s.Commits[0] != "sha1" || s.Commits[1] != "sha2" {
		t.Errorf("Commits = %v", s.Commits)
	}
	if s.SubmittedAt != "2026-04-20T16:00:00Z" {
		t.Errorf("SubmittedAt = %q", s.SubmittedAt)
	}
}

func TestParseSignal_NoOp(t *testing.T) {
	raw := `{"kind":"no-op","role":"apprentice-spi-abc-0","submitted_at":"2026-04-20T16:00:00Z"}`
	s, err := ParseSignal(raw)
	if err != nil {
		t.Fatalf("ParseSignal: %v", err)
	}
	if s.Kind != SignalKindNoOp {
		t.Errorf("Kind = %q, want %q", s.Kind, SignalKindNoOp)
	}
	if s.BundleKey != "" {
		t.Errorf("BundleKey should be empty for no-op, got %q", s.BundleKey)
	}
}

func TestParseSignal_Malformed(t *testing.T) {
	cases := []string{
		"not json at all",
		"{",
		"",
	}
	for _, raw := range cases {
		_, err := ParseSignal(raw)
		if err == nil {
			t.Errorf("ParseSignal(%q) expected error, got nil", raw)
			continue
		}
		if !strings.Contains(err.Error(), "parse signal") {
			t.Errorf("ParseSignal(%q) error = %q, want to mention 'parse signal'", raw, err)
		}
	}
}

func TestSignalForRole_Missing(t *testing.T) {
	md := map[string]string{"unrelated_key": "value"}
	s, ok, err := SignalForRole(md, "apprentice-spi-abc-0")
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if ok {
		t.Errorf("ok = true, want false")
	}
	if s.Kind != "" || s.Role != "" || s.BundleKey != "" || len(s.Commits) != 0 || s.SubmittedAt != "" {
		t.Errorf("Signal = %+v, want zero", s)
	}
}

func TestSignalForRole_EmptyValue(t *testing.T) {
	// An empty string at the key is treated as missing — same path as no key
	// at all, so the consumer doesn't need a special branch for blank metadata.
	md := map[string]string{
		SignalMetadataKey("apprentice-spi-abc-0"): "",
	}
	_, ok, err := SignalForRole(md, "apprentice-spi-abc-0")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if ok {
		t.Errorf("ok = true, want false (empty value treated as missing)")
	}
}

func TestSignalForRole_Found(t *testing.T) {
	md := map[string]string{
		SignalMetadataKey("apprentice-spi-abc-0"): `{"kind":"bundle","role":"apprentice-spi-abc-0","bundle_key":"k","submitted_at":"t"}`,
	}
	s, ok, err := SignalForRole(md, "apprentice-spi-abc-0")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if s.Kind != SignalKindBundle {
		t.Errorf("Kind = %q", s.Kind)
	}
}

func TestSignalForRole_ParseError(t *testing.T) {
	// Three-way return: when the value exists but fails to parse, callers
	// need ok=true so they know NOT to treat this as a missing-signal path,
	// and err non-nil so they can surface the corruption.
	md := map[string]string{
		SignalMetadataKey("apprentice-spi-abc-0"): "{not json",
	}
	_, ok, err := SignalForRole(md, "apprentice-spi-abc-0")
	if err == nil {
		t.Fatal("err = nil, want non-nil")
	}
	if !ok {
		t.Errorf("ok = false, want true (signal exists but failed to parse)")
	}
}

func TestSignals_FiltersByPrefix(t *testing.T) {
	md := map[string]string{
		SignalMetadataKey("apprentice-spi-abc-0"): `{"kind":"bundle","role":"r0","bundle_key":"k0","submitted_at":"t"}`,
		SignalMetadataKey("apprentice-spi-abc-1"): `{"kind":"bundle","role":"r1","bundle_key":"k1","submitted_at":"t"}`,
		"unrelated_key": "value",
		"commits":       "[\"sha1\"]",
	}
	got := Signals(md)
	if len(got) != 2 {
		t.Fatalf("Signals returned %d, want 2: %+v", len(got), got)
	}
}

func TestSignals_SkipsMalformed(t *testing.T) {
	// Malformed entries are silently skipped so a single bad apprentice
	// doesn't mask the others. This matches the documented intent.
	md := map[string]string{
		SignalMetadataKey("apprentice-spi-abc-0"): `{"kind":"bundle","role":"r0","bundle_key":"k0","submitted_at":"t"}`,
		SignalMetadataKey("apprentice-spi-abc-1"): "not json",
	}
	got := Signals(md)
	if len(got) != 1 {
		t.Fatalf("Signals returned %d, want 1 (malformed entry skipped): %+v", len(got), got)
	}
	if got[0].Role != "r0" {
		t.Errorf("got role %q, want r0", got[0].Role)
	}
}

func TestSignals_Empty(t *testing.T) {
	if got := Signals(nil); got != nil {
		t.Errorf("Signals(nil) = %v, want nil", got)
	}
	if got := Signals(map[string]string{}); got != nil {
		t.Errorf("Signals(empty) = %v, want nil", got)
	}
}

func TestHandleForSignal(t *testing.T) {
	s := Signal{Kind: SignalKindBundle, BundleKey: "spi-abc/spi-att-0.bundle"}
	h := HandleForSignal("spi-abc", s)
	if h.BeadID != "spi-abc" {
		t.Errorf("BeadID = %q, want spi-abc", h.BeadID)
	}
	if h.Key != "spi-abc/spi-att-0.bundle" {
		t.Errorf("Key = %q", h.Key)
	}
}

func TestHandleForSignal_EmptyKey(t *testing.T) {
	// No-op signals carry an empty BundleKey; the handle still has the
	// BeadID. Callers (deleteApprenticeBundle) treat empty-Key handles as
	// no-ops.
	s := Signal{Kind: SignalKindNoOp, BundleKey: ""}
	h := HandleForSignal("spi-abc", s)
	if h.BeadID != "spi-abc" {
		t.Errorf("BeadID = %q", h.BeadID)
	}
	if h.Key != "" {
		t.Errorf("Key = %q, want empty", h.Key)
	}
}
