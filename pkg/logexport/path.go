package logexport

import (
	"path/filepath"
	"strings"

	"github.com/awell-health/spire/pkg/logartifact"
)

// LegacyOperationalLogName is the basename used by wizard / apprentice /
// sage / cleric / arbiter operational logs under the runctx layout. The
// constant is duplicated from pkg/runctx so the exporter does not pull
// agent-side dependencies into the sidecar binary.
const LegacyOperationalLogName = "operational.log"

// FileKind classifies the artifact shape of a tailed file. The exporter
// does not consume FileKind directly — the value rides on PathInfo so
// callers (the StdoutSink, log lines for operational events) can render
// a stable label without re-running the path heuristic.
type FileKind string

const (
	// FileKindOperational is a wizard / apprentice / sage / cleric /
	// arbiter operational log: <root>/<tower>/.../<phase>/operational.log.
	FileKindOperational FileKind = "operational"
	// FileKindTranscript is a per-provider stream: <root>/<tower>/.../<provider>/<stream>.jsonl.
	FileKindTranscript FileKind = "transcript"
)

// PathInfo is the parsed identity / kind / sequence triple for a tailed
// file's relative path under Root. Empty values mean the parser could
// not classify the file as a canonical artifact; the tailer skips such
// files rather than producing a malformed manifest row.
type PathInfo struct {
	Identity logartifact.Identity
	Kind     FileKind
	// Sequence is parsed from a `<stream>-<N>.jsonl` suffix and reserved
	// for future chunked-artifact support. Today every artifact lands at
	// sequence 0 — the exporter does not split a single source file into
	// multiple sequenced chunks.
	Sequence int
}

// Valid reports whether the PathInfo carries a well-formed Identity that
// would survive logartifact.Identity.Validate.
func (p PathInfo) Valid() bool {
	return p.Identity.Validate() == nil
}

// ParsePath classifies a relative path under SPIRE_LOG_ROOT into an
// Identity. The expected layouts (mirrors logartifact.BuildObjectKey
// and runctx.LogPaths):
//
//	<tower>/<bead>/<attempt>/<run>/<agent>/<role>/<phase>/operational.log
//	<tower>/<bead>/<attempt>/<run>/<agent>/<role>/<phase>/<stream>.jsonl
//	<tower>/<bead>/<attempt>/<run>/<agent>/<role>/<phase>/<provider>/<stream>.jsonl
//
// Returns ok=false for any structural mismatch (different leaf
// extension, wrong segment count, empty fields). Callers treat
// ok=false as "skip this file" rather than fatal — the tailer is
// best-effort.
func ParsePath(rel string) (PathInfo, bool) {
	rel = filepath.ToSlash(rel)
	rel = strings.TrimPrefix(rel, "./")
	rel = strings.Trim(rel, "/")
	parts := strings.Split(rel, "/")

	if len(parts) < 8 {
		return PathInfo{}, false
	}
	leaf := parts[len(parts)-1]
	switch {
	case leaf == LegacyOperationalLogName:
		// 8 segments: tower/bead/attempt/run/agent/role/phase/operational.log
		if len(parts) != 8 {
			return PathInfo{}, false
		}
		id := logartifact.Identity{
			Tower:     parts[0],
			BeadID:    parts[1],
			AttemptID: parts[2],
			RunID:     parts[3],
			AgentName: parts[4],
			Role:      logartifact.Role(parts[5]),
			Phase:     parts[6],
			Stream:    logartifact.StreamStdout,
		}
		if id.Validate() != nil {
			return PathInfo{}, false
		}
		return PathInfo{Identity: id, Kind: FileKindOperational}, true

	case strings.HasSuffix(leaf, ".jsonl"):
		// 8 segments without provider, 9 with: see logartifact.parseLocalPath.
		if len(parts) != 8 && len(parts) != 9 {
			return PathInfo{}, false
		}
		stem := strings.TrimSuffix(leaf, ".jsonl")
		stream, sequence := splitStreamSeq(stem)
		id := logartifact.Identity{
			Tower:     parts[0],
			BeadID:    parts[1],
			AttemptID: parts[2],
			RunID:     parts[3],
			AgentName: parts[4],
			Role:      logartifact.Role(parts[5]),
			Phase:     parts[6],
			Stream:    logartifact.Stream(stream),
		}
		if len(parts) == 9 {
			id.Provider = parts[7]
		}
		if id.Validate() != nil {
			return PathInfo{}, false
		}
		return PathInfo{
			Identity: id,
			Kind:     FileKindTranscript,
			Sequence: sequence,
		}, true

	default:
		return PathInfo{}, false
	}
}

// splitStreamSeq extracts the numeric chunk suffix from a `<stream>-<N>`
// stem. Returns sequence=0 when no numeric suffix is present (the
// canonical single-shot artifact).
func splitStreamSeq(stem string) (string, int) {
	dash := strings.LastIndex(stem, "-")
	if dash < 0 {
		return stem, 0
	}
	suffix := stem[dash+1:]
	if suffix == "" {
		return stem, 0
	}
	seq := 0
	for _, r := range suffix {
		if r < '0' || r > '9' {
			return stem, 0
		}
		seq = seq*10 + int(r-'0')
	}
	return stem[:dash], seq
}
