package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/awell-health/spire/pkg/logartifact"
)

// LogEvent is the wire shape of a single log line returned by the bead
// log follow endpoint. The artifact reader and the LiveTail backend
// produce values with the same shape so consumers see one ordered stream
// regardless of which substrate served the bytes.
//
// Sequence is the byte offset of the line's first byte within its
// artifact. Two events with the same (ArtifactID, Sequence) describe the
// same line — the follow handler uses that pair to dedup overlapping
// reads from the artifact reader and a LiveTail.
type LogEvent struct {
	Sequence   uint64    `json:"sequence"`
	Timestamp  time.Time `json:"timestamp,omitempty"`
	Stream     string    `json:"stream"`
	Line       string    `json:"line"`
	ArtifactID string    `json:"artifact_id"`
}

// FollowResponse is the envelope for GET /api/v1/beads/{id}/logs?follow=true.
// Cursor is opaque base64 — clients pass it back verbatim on the next
// request. Done is true when the bead has terminated and no more events
// will appear; clients stop polling on done=true.
type FollowResponse struct {
	Events []LogEvent `json:"events"`
	Cursor string     `json:"cursor,omitempty"`
	Done   bool       `json:"done"`
}

// followCursor is the in-memory cursor shape carried across follow
// requests. The fields are JSON-tagged with short keys to keep the
// base64 payload small for clients that print or log the cursor. The
// gateway never inspects the cursor as a string — it always decodes
// then re-encodes — so future field additions are wire-safe.
type followCursor struct {
	// ArtifactID is the artifact whose bytes were last emitted. Empty
	// on the initial request.
	ArtifactID string `json:"a,omitempty"`
	// ByteOffset is the next byte to read inside ArtifactID. When
	// ArtifactID is empty this field is ignored.
	ByteOffset int64 `json:"b,omitempty"`
	// LiveTailFloor is the highest sequence the live tail backend has
	// already emitted, so subsequent calls can ask for events past
	// that point. Persisted across requests so the gateway stays
	// stateless.
	LiveTailFloor uint64 `json:"l,omitempty"`
}

// LiveTail is the optional leading-edge source that runs in front of the
// artifact reader for the actively-writing chunk of a bead's logs. The
// canonical implementation is Cloud Logging in cluster-as-truth mode;
// local-native and any deployment without Cloud Logging credentials
// install the no-op tail returned by NoopLiveTail, which yields no
// events and lets the follow handler degrade gracefully to polling
// finalized artifacts.
//
// The interface is small on purpose: the gateway calls Read once per
// follow request, dedups against artifact-reader output by
// (ArtifactID, Sequence), and asks for events past the cursor's
// LiveTailFloor. Backends that have no notion of a per-bead tail (e.g.
// future S3 or PVC backends) install NoopLiveTail.
type LiveTail interface {
	// Read returns events for beadID whose Sequence is strictly greater
	// than sinceFloor. Implementations must order events ascending by
	// (ArtifactID, Sequence) so dedup against the artifact reader is a
	// single linear pass.
	//
	// Returning (nil, nil) is allowed and signals "no live data
	// available" — equivalent to the no-op tail. Returning a non-nil
	// error is reserved for genuine backend failures (auth, network);
	// the follow handler logs the error and falls back to artifact
	// reads so a transient Cloud Logging hiccup never breaks the
	// client.
	Read(ctx context.Context, beadID string, sinceFloor uint64) ([]LogEvent, error)
}

// NoopLiveTail returns a LiveTail that produces no events. Used when
// Cloud Logging access is unavailable or disabled by config — the
// follow handler then satisfies the "graceful degradation to polling
// finalized artifacts" acceptance criterion (spi-bkha5x).
func NoopLiveTail() LiveTail { return noopLiveTail{} }

type noopLiveTail struct{}

func (noopLiveTail) Read(context.Context, string, uint64) ([]LogEvent, error) {
	return nil, nil
}

// liveTailFunc resolves the LiveTail used by the follow handler. The
// default returns NoopLiveTail so the handler never branches on nil.
// cmd/spire wires the production tail at boot via SetLiveTail.
var liveTailFunc = func() LiveTail { return NoopLiveTail() }

// SetLiveTail installs the gateway-wide live tail. Call once at startup
// from cmd/spire after constructing the Cloud Logging client (or any
// other tail backend). Passing nil restores the no-op tail.
func SetLiveTail(t LiveTail) {
	if t == nil {
		liveTailFunc = func() LiveTail { return NoopLiveTail() }
		return
	}
	liveTailFunc = func() LiveTail { return t }
}

// FollowOnce performs a single follow step: list manifests for the
// bead, read finalized bytes past the cursor, merge the live tail's
// new events, dedup, and return the next-call cursor along with a
// done flag.
//
// Exported so cmd/spire can reuse the same logic for the local CLI's
// `spire logs follow` command — the gateway handler is a thin HTTP
// wrapper around this function. Callers that want the no-op tail pass
// NoopLiveTail() (or nil; nil is treated as no-op).
//
// terminal reports whether the bead has reached a terminal status. A
// terminal bead with no remaining writing artifacts triggers
// done=true so polling stops naturally on pod exit.
//
// Returns the events to emit, the opaque cursor for the next call,
// and the done signal. cursor returns empty only when done=true and
// no events were emitted; in every other case the caller round-trips
// the cursor on the next request.
func FollowOnce(
	ctx context.Context,
	reader LogArtifactReader,
	tail LiveTail,
	scope logartifact.CallerScope,
	beadID, cursor string,
	terminal bool,
) (events []LogEvent, nextCursor string, done bool, err error) {
	in, err := decodeFollowCursor(cursor)
	if err != nil {
		return nil, "", false, fmt.Errorf("invalid cursor: %w", err)
	}
	if reader == nil {
		// No backend wired: nothing to follow. Done depends solely
		// on bead state.
		return []LogEvent{}, "", terminal, nil
	}
	if tail == nil {
		tail = NoopLiveTail()
	}

	manifests, err := reader.List(ctx, logartifact.Filter{BeadID: beadID})
	if err != nil {
		return nil, "", false, err
	}

	out, outCursor, hasWritingBeyond, err := readArtifactEvents(ctx, reader, manifests, in, scope)
	if err != nil {
		return nil, "", false, err
	}

	// Live tail merges events from Cloud Logging (or any future
	// backend) past LiveTailFloor. Errors are non-fatal: callers fall
	// back to artifact-only output so a transient backend problem
	// never breaks the client.
	tailEvents, tailErr := tail.Read(ctx, beadID, in.LiveTailFloor)
	if tailErr == nil && len(tailEvents) > 0 {
		merged, floor := mergeAndDedup(out, tailEvents, in.LiveTailFloor)
		out = merged
		outCursor.LiveTailFloor = floor
	} else {
		outCursor.LiveTailFloor = in.LiveTailFloor
	}

	// done is the pod-exit signal the design calls out (spi-bkha5x):
	// the bead has reached a terminal status AND no artifact rows are
	// still in writing state. Both conditions must hold so a client
	// polling between rotations of an active bead does not see a
	// premature done=true.
	done = terminal && !hasWritingBeyond

	if done && len(out) == 0 {
		// Terminal bead with no new events and nothing left to read:
		// drop the cursor so the client sees a clean stream end.
		return out, "", true, nil
	}
	return out, encodeFollowCursor(outCursor), done, nil
}

// followBeadLogs answers GET /api/v1/beads/{id}/logs?follow=true. The
// handler is a thin wrapper around FollowOnce — the cursor is opaque,
// base64-encoded, and fully self-describing, so the gateway holds no
// per-client state. Any replica can resume any follow stream and a
// reconnect after gateway restart yields an identical event sequence.
func (s *Server) followBeadLogs(w http.ResponseWriter, r *http.Request, beadID string) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if err := logsStoreEnsureFunc(s.effectiveDataDir()); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	bead, err := logsGetBeadFunc(beadID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		return
	}

	reader, _ := logArtifactReaderFunc()
	scope := scopeFromRequest(r)

	events, nextCursor, done, err := FollowOnce(
		r.Context(), reader, liveTailFunc(), scope,
		beadID, r.URL.Query().Get("cursor"),
		beadStatusTerminal(bead.Status),
	)
	if err != nil {
		// Distinguish bad cursor from internal failure for clearer
		// debugging — the FollowOnce wrapper prefixes "invalid cursor"
		// for caller-side mistakes.
		if strings.HasPrefix(err.Error(), "invalid cursor") {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid cursor"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	resp := FollowResponse{
		Events: events,
		Cursor: nextCursor,
		Done:   done,
	}
	if resp.Events == nil {
		resp.Events = []LogEvent{}
	}
	writeJSON(w, http.StatusOK, resp)
}

// readArtifactEvents walks the manifest list past the cursor, fetches
// each finalized artifact's bytes, and converts them into LogEvent
// slices. The returned cursor points just past the last emitted byte.
// hasWritingBeyond reports whether any non-finalized artifact remains
// after the cursor — used by the caller to decide done-ness.
//
// Visibility-gated: any artifact the caller cannot read for its scope
// is skipped silently rather than 403'd at the response layer, since
// the follow surface is line-oriented and a single forbidden artifact
// must not stop the rest of the stream. Engineer scope retains the
// existing gating semantics from the raw endpoint (canReadVisibility).
func readArtifactEvents(
	ctx context.Context,
	reader LogArtifactReader,
	manifests []logartifact.Manifest,
	cursor followCursor,
	scope logartifact.CallerScope,
) ([]LogEvent, followCursor, bool, error) {
	out := []LogEvent{}
	newCursor := cursor

	startIdx := 0
	startOffset := int64(0)
	if cursor.ArtifactID != "" {
		// Walk to the cursor's artifact. Any artifact before the
		// cursor has been emitted in a prior request and is skipped.
		// If the artifact is no longer in the list (compaction,
		// retention) we resume at the next still-present artifact.
		found := false
		for i, m := range manifests {
			if m.ID == cursor.ArtifactID {
				startIdx = i
				startOffset = cursor.ByteOffset
				found = true
				break
			}
		}
		if !found {
			startIdx = len(manifests)
		}
	}

	hasWritingBeyond := false
	for i := startIdx; i < len(manifests); i++ {
		m := manifests[i]
		// Track whether any non-finalized artifact remains in the
		// tail of the list — the caller uses this to decide done.
		if m.Status != logartifact.StatusFinalized {
			hasWritingBeyond = true
			// Writing/failed artifacts have no readable bytes through
			// the artifact reader. The LiveTail covers the leading
			// edge in cluster mode; in degraded mode they show up
			// once the exporter finalizes them.
			continue
		}
		if !canReadVisibility(scope, m.Visibility) {
			// Refuse to leak forbidden artifact bytes, but record
			// that we passed it so the cursor advances past it on
			// subsequent calls. Without this, the loop would re-pin
			// to the forbidden artifact forever.
			newCursor.ArtifactID = m.ID
			newCursor.ByteOffset = m.ByteSize
			continue
		}

		offset := int64(0)
		if i == startIdx {
			offset = startOffset
		}
		if m.ByteSize > 0 && offset >= m.ByteSize {
			// Cursor is already at end-of-artifact (we emitted the
			// last byte on a previous request). Advance to the next
			// artifact without rereading.
			newCursor.ArtifactID = m.ID
			newCursor.ByteOffset = m.ByteSize
			continue
		}

		artEvents, nextOffset, err := readArtifactBytesAsEvents(ctx, reader, m, offset)
		if err != nil {
			// A single artifact failure is non-fatal: log the error
			// at the caller (handled by the wrapper) and emit what
			// we have so far. The cursor stays pinned to this
			// artifact so the next call retries.
			return nil, cursor, hasWritingBeyond, err
		}
		out = append(out, artEvents...)
		newCursor.ArtifactID = m.ID
		newCursor.ByteOffset = nextOffset
	}

	return out, newCursor, hasWritingBeyond, nil
}

// readArtifactBytesAsEvents fetches an artifact's bytes from offset to
// end and splits them into LogEvent values, one per line. The line's
// byte offset within the artifact serves as Sequence so dedup against
// LiveTail works without coordinating IDs.
//
// The reader's Get method does not support range reads on every
// backend, so the handler reads the full artifact and skips the prefix
// in memory. For the design's intended artifact sizes (single MB at
// most for provider transcripts; far less for operational logs) this
// is acceptable; a future optimization can teach the artifact reader
// about ranged Get without changing the wire shape.
func readArtifactBytesAsEvents(
	ctx context.Context,
	reader LogArtifactReader,
	m logartifact.Manifest,
	offset int64,
) ([]LogEvent, int64, error) {
	rc, manifest, err := reader.Get(ctx, logartifact.ManifestRef{ID: m.ID})
	if err != nil {
		if errors.Is(err, logartifact.ErrNotFound) {
			// Manifest exists but the bytes have been pruned. Treat
			// as "nothing to emit" and advance the cursor past the
			// recorded byte_size so we don't loop on the same
			// artifact every poll.
			return nil, m.ByteSize, nil
		}
		return nil, offset, err
	}
	defer rc.Close()

	raw, err := io.ReadAll(rc)
	if err != nil {
		return nil, offset, err
	}
	// Some backends report ByteSize=0 for not-yet-finalized rows.
	// Use the real read length when it disagrees so the cursor is
	// always set to the byte we just emitted.
	byteSize := int64(len(raw))
	if manifest.ByteSize > byteSize {
		byteSize = manifest.ByteSize
	}
	if offset >= int64(len(raw)) {
		return nil, byteSize, nil
	}
	tail := raw[offset:]

	stream := string(manifest.Identity.Stream)
	streamPrefix := offset
	events := []LogEvent{}
	scanner := bufio.NewScanner(bytes.NewReader(tail))
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		// Recover the byte offset of this line's first byte within
		// the artifact: it is the running prefix length (offset +
		// bytes consumed so far).
		ev := LogEvent{
			Sequence:   uint64(streamPrefix),
			Stream:     stream,
			Line:       line,
			ArtifactID: m.ID,
		}
		events = append(events, ev)
		// +1 for the newline scanner stripped, even on the last line
		// without a trailing newline — the next read on this
		// artifact (after finalization completes) starts past the
		// line we just emitted.
		streamPrefix += int64(len(line)) + 1
	}
	if err := scanner.Err(); err != nil {
		return events, offset + int64(len(tail)), err
	}
	// nextOffset is the byte after the last line we emitted. For
	// finalized artifacts that ends exactly at byteSize. For
	// streaming reads (future LiveTail/local-tail) the caller would
	// re-poll and pick up bytes appended past nextOffset.
	nextOffset := offset + int64(len(tail))
	return events, nextOffset, nil
}

// mergeAndDedup combines artifact-reader events with LiveTail events,
// emitting each unique (ArtifactID, Sequence) once and preserving
// ascending order. Returns the merged slice and the new live-tail
// floor (max sequence observed from tailEvents, or the existing floor
// if tailEvents was empty).
//
// The dedup key matches the design: artifact bytes carry Sequence as
// a byte offset, and the StdoutSink emits the same byte offset as its
// Offset field (see pkg/logexport/stdout_sink.go) — so a Cloud Logging
// implementation that maps Offset→Sequence yields the same key.
func mergeAndDedup(artifactEvents, tailEvents []LogEvent, prevFloor uint64) ([]LogEvent, uint64) {
	if len(tailEvents) == 0 {
		return artifactEvents, prevFloor
	}
	seen := make(map[string]struct{}, len(artifactEvents)+len(tailEvents))
	keyOf := func(ev LogEvent) string {
		return ev.ArtifactID + "|" + strconv.FormatUint(ev.Sequence, 10)
	}

	out := make([]LogEvent, 0, len(artifactEvents)+len(tailEvents))
	for _, ev := range artifactEvents {
		k := keyOf(ev)
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, ev)
	}
	floor := prevFloor
	for _, ev := range tailEvents {
		k := keyOf(ev)
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, ev)
		if ev.Sequence > floor {
			floor = ev.Sequence
		}
	}
	return out, floor
}

// beadStatusTerminal reports whether a bead status string represents a
// terminated bead. Mirrors the shared "terminal status" predicate used
// elsewhere in pkg/store; duplicated here so the gateway can decide
// done without taking a dependency on the workflow package.
func beadStatusTerminal(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "closed", "done", "merged", "discarded", "completed":
		return true
	}
	return false
}

// isFollowRequest reports whether r is a follow-style request. Accepts
// the canonical "true" / "1" boolean strings; any other value is
// interpreted as not-follow so a stray query string does not silently
// flip semantics.
func isFollowRequest(r *http.Request) bool {
	v := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("follow")))
	return v == "true" || v == "1" || v == "yes"
}

// encodeFollowCursor returns a base64(JSON) representation of c. The
// cursor is opaque on the wire so clients never parse its internals.
func encodeFollowCursor(c followCursor) string {
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b)
}

// decodeFollowCursor parses a cursor returned by encodeFollowCursor.
// An empty input yields a zero cursor; a malformed input is a 400 to
// the caller, distinct from "no cursor sent".
func decodeFollowCursor(s string) (followCursor, error) {
	if s == "" {
		return followCursor{}, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return followCursor{}, err
	}
	var c followCursor
	if err := json.Unmarshal(raw, &c); err != nil {
		return followCursor{}, err
	}
	return c, nil
}
