package logartifact

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/awell-health/spire/pkg/logartifact/redact"
)

// CallerScope identifies the requester reading an artifact through
// Render. The gateway (spi-j3r694) will eventually source this from
// authenticated identity; until then callers pass a string declaring
// which scope they have.
//
// Engineer scope reads any visibility (raw transcripts included);
// non-engineer scopes can only read artifacts the substrate has
// classified as visible to them. The render layer ALWAYS re-applies the
// current redactor on the served bytes, regardless of what the upload
// path stored — defense in depth so improvements to the pattern set
// catch leaks in older artifacts without rewriting storage.
type CallerScope string

const (
	// ScopeEngineer can read every visibility class. Today's only
	// callers (CLI, tests) run with this scope.
	ScopeEngineer CallerScope = "engineer"

	// ScopeDesktop is the desktop-board scope. It can read
	// VisibilityDesktopSafe and VisibilityPublic; engineer_only
	// requests are refused.
	ScopeDesktop CallerScope = "desktop"

	// ScopePublic is the most-restricted scope, intended for surfaces
	// that may be shared outside the operating organization. Today
	// behaves like ScopeDesktop but reserved for future tightening.
	ScopePublic CallerScope = "public"
)

// canRead reports whether scope is allowed to read an artifact at the
// given visibility. The matrix is intentionally simple — until RBAC
// lands the engineer scope is privileged and everything else is
// fail-closed:
//
//   engineer  → engineer_only, desktop_safe, public
//   desktop   → desktop_safe, public
//   public    → public
//
// TODO(spi-j3r694): once gateway authentication carries real identity,
// replace this with a per-route scope check. The current scope-string
// approach is a placeholder.
func canRead(scope CallerScope, visibility Visibility) bool {
	switch scope {
	case ScopeEngineer:
		return true
	case ScopeDesktop:
		return visibility == VisibilityDesktopSafe || visibility == VisibilityPublic
	case ScopePublic:
		return visibility == VisibilityPublic
	default:
		return false
	}
}

// RenderMeta is the response shape the gateway wraps around rendered
// artifact bytes. The fields are the contract spi-j3r694 will consume:
// any consumer of Render gets enough metadata to display and trust the
// rendered output without re-fetching the manifest.
type RenderMeta struct {
	// ManifestID is the row ID of the artifact the bytes came from.
	// Stable across renders.
	ManifestID string `json:"manifest_id"`

	// Visibility is the artifact's stored visibility. Render will not
	// downgrade or upgrade it; the caller's scope is checked before
	// bytes are read.
	Visibility Visibility `json:"visibility"`

	// RedactionVersion is the redactor generation that ran at READ
	// time, not the version stored on the manifest. Always >= 1 for
	// non-engineer-only renders; 0 means the bytes were served raw
	// (engineer scope reading engineer_only).
	RedactionVersion int `json:"redaction_version"`

	// StoredRedactionVersion is the redactor generation recorded on
	// the manifest at upload. Useful for display ("this artifact was
	// redacted at upload by v1; the gateway re-ran v1 on read").
	StoredRedactionVersion int `json:"stored_redaction_version"`

	// ByteSize is the manifest's recorded artifact size — the
	// on-disk/on-GCS size, NOT the size of the rendered bytes (which
	// may be longer if the redactor expanded a match into a longer
	// mask, though [REDACTED] is shorter than most credentials).
	ByteSize int64 `json:"byte_size"`

	// Checksum is the manifest's recorded checksum
	// (sha256:<lowercase-hex>) of the on-storage bytes. Useful for
	// audit; does NOT cover the rendered bytes.
	Checksum string `json:"checksum,omitempty"`

	// Status is the manifest's lifecycle state.
	Status Status `json:"status"`
}

// ErrAccessDenied is returned by Render when the caller's scope cannot
// read the artifact's visibility. Backends do NOT open the byte store
// in this path, so a denied request is cheap.
var ErrAccessDenied = errors.New("logartifact: access denied for caller scope")

// Render reads an artifact through the supplied store, applies the
// current redactor (regardless of what was stored), and returns the
// rendered bytes plus metadata. The gateway and the future board
// renderer (spi-j3r694, spi-egw26j) call this to serve bytes to a
// client; CLI tools and tests use it indirectly via spire logs pretty.
//
// Defense-in-depth contract:
//
//  1. The artifact's visibility is checked against the caller's scope
//     BEFORE the byte store is opened. A denied request returns
//     ErrAccessDenied with the manifest already loaded so the caller
//     can decide whether to log/audit.
//
//  2. For desktop_safe / public artifacts, the on-storage bytes have
//     already been redacted at upload (and will already be missing the
//     credential) — but the renderer re-runs the current redactor over
//     them. If the pattern set has improved since the artifact was
//     uploaded, the new patterns apply on this read without rewriting
//     storage.
//
//  3. For engineer_only artifacts read at engineer scope, the bytes
//     pass through unmodified. RedactionVersion in the returned meta is
//     0 to make this explicit.
//
// Render reads the entire artifact into memory before returning; for
// the cap-bounded provider transcripts in this design that is fine,
// but a future streaming variant may be added if desktop UX needs to
// page through multi-GB artifacts.
func Render(ctx context.Context, store Store, ref ManifestRef, scope CallerScope) ([]byte, RenderMeta, error) {
	manifest, err := store.Stat(ctx, ref)
	if err != nil {
		return nil, RenderMeta{}, fmt.Errorf("logartifact: render: stat: %w", err)
	}
	if !canRead(scope, manifest.Visibility) {
		return nil, RenderMeta{
			ManifestID:             manifest.ID,
			Visibility:             manifest.Visibility,
			StoredRedactionVersion: manifest.RedactionVersion,
			ByteSize:               manifest.ByteSize,
			Checksum:               manifest.Checksum,
			Status:                 manifest.Status,
		}, ErrAccessDenied
	}

	rc, _, err := store.Get(ctx, ref)
	if err != nil {
		return nil, RenderMeta{}, fmt.Errorf("logartifact: render: get: %w", err)
	}
	defer rc.Close()
	raw, err := io.ReadAll(rc)
	if err != nil {
		return nil, RenderMeta{}, fmt.Errorf("logartifact: render: read: %w", err)
	}

	meta := RenderMeta{
		ManifestID:             manifest.ID,
		Visibility:             manifest.Visibility,
		StoredRedactionVersion: manifest.RedactionVersion,
		ByteSize:               manifest.ByteSize,
		Checksum:               manifest.Checksum,
		Status:                 manifest.Status,
	}

	// Engineer scope reading engineer_only: serve the raw bytes. This
	// is the only path that does NOT re-apply the redactor — forensic
	// fidelity for the operator who explicitly asked for it.
	if scope == ScopeEngineer && manifest.Visibility == VisibilityEngineerOnly {
		meta.RedactionVersion = 0
		return raw, meta, nil
	}

	rendered, version := redact.New().Redact(raw)
	meta.RedactionVersion = version
	return rendered, meta, nil
}
