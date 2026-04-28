package logstream

import "context"

// Artifact is a single log artifact returned by a Source. Carries the
// minimum metadata + bytes the inspector and CLI need to render either
// through a provider Adapter (Provider != "") or as plain text
// (Provider == "").
//
// The local backend preloads Content / StderrContent at List time so
// callers don't have to re-Open. The gateway backend preloads on the
// same contract — keeping List the single source of bytes simplifies
// the inspector's render path, which already expects Content to be
// populated. Lazy fetch is a future optimisation when live-follow lands
// (see spi-bkha5x).
type Artifact struct {
	// Name is the human-readable display label (e.g. "wizard",
	// "implement-1", "implement-1/claude (14:02)"). The local source
	// derives this from the legacy filename convention; the gateway
	// source synthesises it from manifest fields. Both must produce the
	// same shape so cycle tagging in the inspector matches identically.
	Name string

	// Provider names the transcript adapter ("claude", "codex"). Empty
	// for non-provider streams (operational stdout). Adapter selection
	// in the inspector and `spire logs pretty` keys off this.
	Provider string

	// Path is an optional filesystem path. Local source: absolute path
	// to the artifact file. Gateway source: empty (clients never see
	// a server-side path). Carried so the inspector can keep its
	// "Path" column in sync with existing tests, and so `--claude-file`
	// flows continue to work in local-native mode.
	Path string

	// StderrPath is the matching sidecar path for Provider transcripts.
	// Local source: <Path with .stderr.log suffix>; empty when no sidecar
	// exists. Gateway source: empty.
	StderrPath string

	// Content is the preloaded artifact bytes as a UTF-8 string. The
	// inspector and CLI feed this into adapter.Parse. Adapter contract
	// (see logstream.go) makes Parse responsible for tolerating
	// truncated / invalid bytes, so Content may be a partial copy of
	// the on-disk artifact during in-flight writes.
	Content string

	// StderrContent is the preloaded sidecar bytes. Empty when no
	// sidecar exists for this artifact.
	StderrContent string
}

// Source enumerates log artifacts for a bead. Implementations distinguish
// "no artifacts yet" (empty slice, nil error) from real errors so the
// inspector can render a friendly empty-state instead of treating an
// unwritten bead as broken.
//
// Implementations must be safe for concurrent List calls — the inspector
// and the daemon may both fetch for the same bead — but a single call
// is not required to be deterministic in artifact ordering across
// implementations. Callers that need a specific order should sort.
type Source interface {
	// List returns the artifacts that exist for beadID. An empty result
	// with no error means "no artifacts yet" — distinct from a real
	// error. Implementations should treat a missing wizard log
	// directory or an empty manifest list as the empty state.
	List(ctx context.Context, beadID string) ([]Artifact, error)
}
