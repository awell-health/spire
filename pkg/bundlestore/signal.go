package bundlestore

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Signal kinds produced by `spire apprentice submit`. Consumers (the wizard
// and the operator) route on these.
const (
	SignalKindBundle = "bundle"
	SignalKindNoOp   = "no-op"
)

// SignalMetadataPrefix is the bead-metadata key prefix the apprentice writes
// under. The full key is SignalMetadataPrefix + role.
const SignalMetadataPrefix = "apprentice_signal_"

// Signal is the JSON shape the apprentice writes to bead metadata at
// SignalMetadataKey(role). The producer lives in cmd/spire/apprentice.go;
// this type is the consumer-side mirror. Keep the field tags in lockstep —
// the shape is the wire format between apprentice and wizard.
//
// HandoffMode is the runtime.HandoffMode value the executor selected for
// the apprentice that produced this signal. Empty when the producer
// predates spi-xplwy chunk 5a — consumers MUST treat empty as "unknown,"
// not as HandoffNone.
type Signal struct {
	Kind        string   `json:"kind"`
	Role        string   `json:"role"`
	BundleKey   string   `json:"bundle_key,omitempty"`
	Commits     []string `json:"commits,omitempty"`
	SubmittedAt string   `json:"submitted_at"`
	HandoffMode string   `json:"handoff_mode,omitempty"`
}

// ApprenticeRole returns the canonical role string for an apprentice at
// fan-out index idx on the given bead. Matches the role the apprentice
// computes at submit time from SPIRE_BEAD_ID / SPIRE_APPRENTICE_IDX.
func ApprenticeRole(beadID string, idx int) string {
	return fmt.Sprintf("apprentice-%s-%d", beadID, idx)
}

// SignalMetadataKey returns the bead-metadata key for an apprentice's signal.
func SignalMetadataKey(role string) string {
	return SignalMetadataPrefix + role
}

// ParseSignal unmarshals the JSON-encoded signal value stored at
// SignalMetadataKey(role).
func ParseSignal(raw string) (Signal, error) {
	var s Signal
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		return Signal{}, fmt.Errorf("parse signal: %w", err)
	}
	return s, nil
}

// SignalForRole looks up and parses the signal for the given role. Returns
// (zero, false, nil) when no signal is present. An error is returned only
// when the value exists but fails to parse.
func SignalForRole(md map[string]string, role string) (Signal, bool, error) {
	raw, ok := md[SignalMetadataKey(role)]
	if !ok || raw == "" {
		return Signal{}, false, nil
	}
	s, err := ParseSignal(raw)
	if err != nil {
		return Signal{}, true, err
	}
	return s, true, nil
}

// Signals returns every parsed apprentice signal on the bead. Keys that do
// not parse as signals are skipped silently; the caller is already reading
// untrusted metadata and surfacing per-key errors would mask the useful
// ones.
func Signals(md map[string]string) []Signal {
	var out []Signal
	for k, v := range md {
		if !strings.HasPrefix(k, SignalMetadataPrefix) {
			continue
		}
		s, err := ParseSignal(v)
		if err != nil {
			continue
		}
		out = append(out, s)
	}
	return out
}

// HandleForSignal builds the BundleHandle the wizard passes to Get/Delete.
// The handle's Key is the apprentice-produced bundle key — callers must
// treat it as opaque.
func HandleForSignal(beadID string, s Signal) BundleHandle {
	return BundleHandle{BeadID: beadID, Key: s.BundleKey}
}
