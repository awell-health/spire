// Package redact masks high-confidence secret patterns out of byte
// streams before they leave the trust boundary of an engineer-only
// artifact. The patterns target common credential shapes (provider API
// keys, AWS/GCP access keys, GitHub tokens, JWT-shaped strings,
// Authorization: Bearer headers, generic api_key=/password= pairs) so an
// accidental token leak in a transcript or stdout buffer doesn't reach
// the desktop board, the gateway, or a public report.
//
// IMPORTANT: this package is hygiene, not a security boundary. A
// determined adversary can phrase a secret in a way that won't match,
// and a buggy regex can match more than intended. The right control is
// "do not put secrets in logs in the first place"; redact() is the
// last-line filter when that contract slips. The cluster-install runbook
// (docs/cluster-install.md) documents this explicitly.
//
// Versioning. Every Redact call returns the version constant it applied
// (CurrentRedactionVersion) so the manifest can record which generation
// of patterns ran. When the pattern set changes meaningfully, bump
// CurrentRedactionVersion. The render layer always re-applies the
// current redactor at read time, so old artifacts pick up new patterns
// without rewriting storage. Version 0 is reserved for "no redactor
// applied" (engineer_only artifacts and pre-existing rows).
package redact

import (
	"bytes"
	"regexp"
)

// CurrentRedactionVersion is the active redaction generation. Bump this
// when the pattern set changes meaningfully (new pattern, broadened
// scope, fix to a false negative). Manifest rows record this value at
// upload so a render-time consumer can decide whether to re-redact.
//
// Version 0 is reserved for "no redactor applied" (engineer_only
// artifacts and pre-existing manifest rows backfilled by migration).
// Version 1 is the first real pattern set.
const CurrentRedactionVersion = 1

// Redactor is the concrete redactor type. Construct with New(); zero
// values are not safe for concurrent use. Tests or future patches that
// want a custom pattern set call NewWithPatterns.
type Redactor struct {
	patterns []*regexp.Regexp
}

// New returns the canonical redactor used at runtime: the union of every
// pattern in DefaultPatterns(). Safe for concurrent use after
// construction.
func New() *Redactor {
	return NewWithPatterns(DefaultPatterns())
}

// NewWithPatterns returns a redactor that applies the supplied patterns
// in order. Tests use this to inject a single pattern in isolation; the
// runtime path always uses New().
func NewWithPatterns(patterns []*regexp.Regexp) *Redactor {
	return &Redactor{patterns: patterns}
}

// mask is the replacement token written in place of every matched
// secret. Single token rather than per-pattern strings so a downstream
// reader can grep for one literal to count redactions across a stream.
const mask = "[REDACTED]"

// Redact returns a copy of b with every match of the redactor's
// patterns replaced by the canonical mask token. The version returned
// is CurrentRedactionVersion when bytes were processed; callers that
// pass through engineer_only material should not call Redact at all.
//
// The function never mutates the input slice. Returned bytes are a
// fresh allocation safe to retain. Empty input returns empty output and
// the current version (the manifest still records that the redactor
// would have run).
func (r *Redactor) Redact(b []byte) ([]byte, int) {
	if r == nil || len(r.patterns) == 0 {
		out := make([]byte, len(b))
		copy(out, b)
		return out, CurrentRedactionVersion
	}
	out := make([]byte, len(b))
	copy(out, b)
	for _, pat := range r.patterns {
		out = pat.ReplaceAll(out, []byte(mask))
	}
	return out, CurrentRedactionVersion
}

// MaskToken is the literal byte sequence Redact writes in place of a
// matched secret. Exposed so tests and downstream tools can grep for it
// without hard-coding the spelling.
var MaskToken = []byte(mask)

// Contains reports whether p contains MaskToken. Convenience for tests
// and tail-render code that wants to flag "this artifact had material
// that the redactor masked".
func Contains(p []byte) bool {
	return bytes.Contains(p, MaskToken)
}
