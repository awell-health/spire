package logartifact

import (
	"encoding/json"
	"fmt"
)

// Visibility is the access class of a log artifact. It is a non-optional
// argument to Store.Put: the design (spi-cmy90h) requires that broader
// exposure than engineer_only be a deliberate choice at the call site.
//
// The three classes are:
//
//   - VisibilityEngineerOnly: raw bytes, untouched at upload. Forensic
//     replay fidelity is the whole point — provider transcripts and
//     wizard operational logs land here. The render layer still passes
//     bytes through the redactor on read for any non-engineer caller
//     (defense in depth), but the durable artifact is unmodified. This
//     is the default for any artifact that does not explicitly declare a
//     wider class.
//
//   - VisibilityDesktopSafe: redacted at upload AND re-redacted on read.
//     Suitable for the desktop board, summarized previews, and any UI
//     surface served to non-engineering users. The redactor's pattern set
//     is hygiene, not a security boundary — see pkg/logartifact/redact.
//
//   - VisibilityPublic: redacted at upload AND re-redacted on read,
//     intended for surfaces that may be shared outside the operating
//     organization (incident postmortems, public-facing reports). At
//     present this class behaves identically to desktop_safe; it exists
//     so callers can record intent today and future redactor versions
//     can apply stricter rules without rewriting the schema.
//
// The zero value is VisibilityEngineerOnly so a forgetful caller fails
// closed (raw bytes preserved on disk, but render-time gating still
// requires engineer scope to read them out).
type Visibility string

const (
	// VisibilityEngineerOnly is the safe default: raw bytes preserved at
	// upload, render-time gate refuses non-engineer callers.
	VisibilityEngineerOnly Visibility = "engineer_only"
	// VisibilityDesktopSafe applies the redactor at upload and again at
	// render. Intended for the desktop board and summary previews.
	VisibilityDesktopSafe Visibility = "desktop_safe"
	// VisibilityPublic applies the redactor at upload and again at
	// render. Currently identical to desktop_safe; reserved for surfaces
	// shared outside the operating organization.
	VisibilityPublic Visibility = "public"
)

// Valid reports whether v is one of the known visibility values.
func (v Visibility) Valid() bool {
	switch v {
	case VisibilityEngineerOnly, VisibilityDesktopSafe, VisibilityPublic:
		return true
	default:
		return false
	}
}

// String implements fmt.Stringer.
func (v Visibility) String() string { return string(v) }

// MarshalJSON encodes Visibility as its canonical lowercase string. The
// zero value marshals as "engineer_only" so JSON consumers see the safe
// default, not an empty string.
func (v Visibility) MarshalJSON() ([]byte, error) {
	if v == "" {
		v = VisibilityEngineerOnly
	}
	return json.Marshal(string(v))
}

// UnmarshalJSON decodes a Visibility string and rejects unknown values.
// Empty strings decode as the safe default.
func (v *Visibility) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	if s == "" {
		*v = VisibilityEngineerOnly
		return nil
	}
	parsed := Visibility(s)
	if !parsed.Valid() {
		return fmt.Errorf("logartifact: unknown visibility %q", s)
	}
	*v = parsed
	return nil
}

// RedactsAtUpload reports whether bytes for this visibility are passed
// through the redactor before they hit the byte store. Engineer-only
// artifacts skip the redactor at upload (forensic fidelity); higher
// visibilities apply it both at upload and at render.
func (v Visibility) RedactsAtUpload() bool {
	switch v {
	case VisibilityDesktopSafe, VisibilityPublic:
		return true
	default:
		return false
	}
}
