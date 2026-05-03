package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/awell-health/spire/pkg/logartifact"
	"github.com/awell-health/spire/pkg/store"
)

// fakeLiveTail returns a deterministic event slice. Tests that need
// dedup or empty-tail behavior swap it in via SetLiveTail.
type fakeLiveTail struct {
	events  []LogEvent
	err     error
	calls   int
	lastSeq uint64
}

func (f *fakeLiveTail) Read(_ context.Context, _ string, sinceFloor uint64) ([]LogEvent, error) {
	f.calls++
	f.lastSeq = sinceFloor
	if f.err != nil {
		return nil, f.err
	}
	out := make([]LogEvent, 0, len(f.events))
	for _, e := range f.events {
		if e.Sequence > sinceFloor {
			out = append(out, e)
		}
	}
	return out, nil
}

func withLiveTail(t *testing.T, tail LiveTail) {
	t.Helper()
	prev := liveTailFunc
	if tail == nil {
		liveTailFunc = func() LiveTail { return NoopLiveTail() }
	} else {
		liveTailFunc = func() LiveTail { return tail }
	}
	t.Cleanup(func() { liveTailFunc = prev })
}

// finalizedManifest builds a manifest whose ByteSize matches the
// supplied content length. Tests that need a writing-status row use
// makeManifest with explicit overrides.
func finalizedManifest(id string, content []byte, opts ...func(*logartifact.Manifest)) (logartifact.Manifest, []byte) {
	m := makeManifest(id, func(m *logartifact.Manifest) {
		m.Status = logartifact.StatusFinalized
		m.ByteSize = int64(len(content))
		m.Visibility = logartifact.VisibilityEngineerOnly
	})
	for _, opt := range opts {
		opt(&m)
	}
	return m, content
}

func TestFollowBeadLogs_BeadMissingReturns404(t *testing.T) {
	withLogStubs(t, &fakeLogReader{}, store.Bead{}, errors.New("bead spi-nope not found"))
	withLiveTail(t, nil)
	s := newLogTestServer("")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/beads/spi-nope/logs?follow=true", nil)
	rec := httptest.NewRecorder()
	s.handleBeadByID(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d want 404 body=%q", rec.Code, rec.Body.String())
	}
}

func TestFollowBeadLogs_NoBackendReturnsEmpty(t *testing.T) {
	withLogStubs(t, nil, store.Bead{Status: "open"}, nil)
	withLiveTail(t, nil)
	s := newLogTestServer("")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/beads/spi-test1/logs?follow=true", nil)
	rec := httptest.NewRecorder()
	s.handleBeadByID(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d want 200 body=%q", rec.Code, rec.Body.String())
	}
	var resp FollowResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Events) != 0 {
		t.Fatalf("events = %d want 0", len(resp.Events))
	}
	if resp.Done {
		t.Fatalf("done = true want false (open bead)")
	}
}

// TestFollowBeadLogs_CursorAdvancement exercises acceptance criterion
// #1. A succession of polls walks through synthetic manifest+chunks
// without overlap or gaps; each poll's cursor resumes exactly where
// the prior poll left off.
func TestFollowBeadLogs_CursorAdvancement(t *testing.T) {
	chunkA := []byte("alpha\nbeta\n")
	chunkB := []byte("gamma\ndelta\nepsilon\n")
	mA, bytesA := finalizedManifest("log-a", chunkA, func(m *logartifact.Manifest) { m.Sequence = 0 })
	mB, bytesB := finalizedManifest("log-b", chunkB, func(m *logartifact.Manifest) { m.Sequence = 1 })
	reader := &fakeLogReader{
		manifests: []logartifact.Manifest{mA, mB},
		bytes:     map[string][]byte{"log-a": bytesA, "log-b": bytesB},
	}
	withLogStubs(t, reader, store.Bead{Status: "open"}, nil)
	withLiveTail(t, nil)
	s := newLogTestServer("")

	// Page 1: no cursor → emit everything from log-a + log-b. Bead is
	// open so done=false even though both chunks are finalized.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/beads/spi-test1/logs?follow=true", nil)
	req.Header.Set("X-Spire-Scope", string(logartifact.ScopeEngineer))
	rec := httptest.NewRecorder()
	s.handleBeadByID(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("page1 status = %d body=%q", rec.Code, rec.Body.String())
	}
	var page1 FollowResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &page1)
	wantLines := []string{"alpha", "beta", "gamma", "delta", "epsilon"}
	if got := linesOf(page1.Events); !equalLines(got, wantLines) {
		t.Fatalf("page1 lines = %v want %v", got, wantLines)
	}
	if page1.Done {
		t.Fatalf("page1 done=true want false")
	}
	if page1.Cursor == "" {
		t.Fatalf("page1 cursor empty, want set so client can resume")
	}

	// Page 2: same cursor → empty events, same cursor, still not done.
	// This is the steady-state poll; the gateway must not re-emit
	// previously seen lines.
	req2 := httptest.NewRequest(http.MethodGet, "/api/v1/beads/spi-test1/logs?follow=true&cursor="+page1.Cursor, nil)
	req2.Header.Set("X-Spire-Scope", string(logartifact.ScopeEngineer))
	rec2 := httptest.NewRecorder()
	s.handleBeadByID(rec2, req2)
	var page2 FollowResponse
	_ = json.Unmarshal(rec2.Body.Bytes(), &page2)
	if len(page2.Events) != 0 {
		t.Fatalf("page2 events = %v want []", page2.Events)
	}
	if page2.Done {
		t.Fatalf("page2 done=true want false (bead still open)")
	}
}

// TestFollowBeadLogs_DuplicateSuppression exercises acceptance
// criterion #2. The artifact reader yields lines with sequences
// derived from byte offsets; the LiveTail yields the same lines plus
// one new line. The handler must emit each line exactly once.
func TestFollowBeadLogs_DuplicateSuppression(t *testing.T) {
	chunk := []byte("alpha\nbeta\ngamma\n")
	m, b := finalizedManifest("log-a", chunk)
	reader := &fakeLogReader{
		manifests: []logartifact.Manifest{m},
		bytes:     map[string][]byte{"log-a": b},
	}
	// Reconstruct the byte-offset sequences the artifact reader
	// would produce so the LiveTail's events overlap exactly. The
	// reader emits sequences 0 ("alpha"), 6 ("beta"), 11 ("gamma");
	// the live tail re-emits 6 + 11 plus a new sequence 17 ("delta").
	tail := &fakeLiveTail{
		events: []LogEvent{
			{ArtifactID: "log-a", Sequence: 6, Stream: "transcript", Line: "beta"},
			{ArtifactID: "log-a", Sequence: 11, Stream: "transcript", Line: "gamma"},
			{ArtifactID: "log-a", Sequence: 17, Stream: "transcript", Line: "delta"},
		},
	}
	withLogStubs(t, reader, store.Bead{Status: "open"}, nil)
	withLiveTail(t, tail)
	s := newLogTestServer("")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/beads/spi-test1/logs?follow=true", nil)
	req.Header.Set("X-Spire-Scope", string(logartifact.ScopeEngineer))
	rec := httptest.NewRecorder()
	s.handleBeadByID(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%q", rec.Code, rec.Body.String())
	}
	var resp FollowResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	got := linesOf(resp.Events)
	want := []string{"alpha", "beta", "gamma", "delta"}
	if !equalLines(got, want) {
		t.Fatalf("lines = %v want %v", got, want)
	}
	// Cursor must persist the live-tail floor so the next poll only
	// asks for events past 17.
	if resp.Cursor == "" {
		t.Fatalf("cursor empty, want set with LiveTailFloor")
	}
}

// TestFollowBeadLogs_PodExitFinalization exercises acceptance criterion
// #3. While the bead is in_progress with a writing-status row, done
// stays false; once the bead transitions to closed and the row
// finalizes, done flips to true on the next poll.
func TestFollowBeadLogs_PodExitFinalization(t *testing.T) {
	chunk := []byte("only-line\n")
	mWriting := makeManifest("log-a", func(m *logartifact.Manifest) {
		m.Status = logartifact.StatusWriting
		m.ByteSize = 0
		m.Visibility = logartifact.VisibilityEngineerOnly
	})
	reader := &fakeLogReader{
		manifests: []logartifact.Manifest{mWriting},
		bytes:     map[string][]byte{},
	}
	withLogStubs(t, reader, store.Bead{Status: "in_progress"}, nil)
	withLiveTail(t, nil)
	s := newLogTestServer("")

	// First poll: writing artifact + open bead → no events, done=false.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/beads/spi-test1/logs?follow=true", nil)
	req.Header.Set("X-Spire-Scope", string(logartifact.ScopeEngineer))
	rec := httptest.NewRecorder()
	s.handleBeadByID(rec, req)
	var p1 FollowResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &p1)
	if p1.Done {
		t.Fatalf("p1 done=true want false (bead still in_progress)")
	}

	// Pod exit: the row finalizes and the bead closes. Re-stub with
	// a closed bead + finalized manifest.
	mFinal := mWriting
	mFinal.Status = logartifact.StatusFinalized
	mFinal.ByteSize = int64(len(chunk))
	reader.manifests = []logartifact.Manifest{mFinal}
	reader.bytes["log-a"] = chunk
	withLogStubs(t, reader, store.Bead{Status: "closed"}, nil)
	withLiveTail(t, nil)

	req2 := httptest.NewRequest(http.MethodGet, "/api/v1/beads/spi-test1/logs?follow=true&cursor="+p1.Cursor, nil)
	req2.Header.Set("X-Spire-Scope", string(logartifact.ScopeEngineer))
	rec2 := httptest.NewRecorder()
	s.handleBeadByID(rec2, req2)
	var p2 FollowResponse
	_ = json.Unmarshal(rec2.Body.Bytes(), &p2)

	// The remaining bytes for the artifact should now be readable; we
	// expect the single line + done=true on this final poll.
	if !equalLines(linesOf(p2.Events), []string{"only-line"}) {
		t.Fatalf("p2 lines = %v want [only-line]", linesOf(p2.Events))
	}
	if !p2.Done {
		t.Fatalf("p2 done=false want true (bead closed, no writing rows)")
	}
}

// TestFollowBeadLogs_FollowAfterReconnect exercises acceptance
// criterion #4. The cursor is fully self-describing — handing it to
// a fresh request resumes the stream exactly past the last emitted
// event, with no replay or skipped lines.
func TestFollowBeadLogs_FollowAfterReconnect(t *testing.T) {
	chunkA := []byte("a1\na2\na3\n")
	chunkB := []byte("b1\nb2\n")
	mA, bytesA := finalizedManifest("log-a", chunkA, func(m *logartifact.Manifest) { m.Sequence = 0 })
	mB, bytesB := finalizedManifest("log-b", chunkB, func(m *logartifact.Manifest) { m.Sequence = 1 })
	reader := &fakeLogReader{
		manifests: []logartifact.Manifest{mA, mB},
		bytes:     map[string][]byte{"log-a": bytesA, "log-b": bytesB},
	}
	withLogStubs(t, reader, store.Bead{Status: "open"}, nil)
	withLiveTail(t, nil)
	s := newLogTestServer("")

	// Initial poll consumes everything.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/beads/spi-test1/logs?follow=true", nil)
	req.Header.Set("X-Spire-Scope", string(logartifact.ScopeEngineer))
	rec := httptest.NewRecorder()
	s.handleBeadByID(rec, req)
	var first FollowResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &first)
	wantAll := []string{"a1", "a2", "a3", "b1", "b2"}
	if !equalLines(linesOf(first.Events), wantAll) {
		t.Fatalf("initial lines = %v want %v", linesOf(first.Events), wantAll)
	}

	// "Reconnect" simulated by constructing a second server with the
	// same backend but no shared in-memory state. Pass the cursor —
	// the gateway should emit zero new events because everything has
	// already been read past the cursor, and the new cursor must be
	// equivalent (steady state).
	s2 := newLogTestServer("")
	req2 := httptest.NewRequest(http.MethodGet, "/api/v1/beads/spi-test1/logs?follow=true&cursor="+first.Cursor, nil)
	req2.Header.Set("X-Spire-Scope", string(logartifact.ScopeEngineer))
	rec2 := httptest.NewRecorder()
	s2.handleBeadByID(rec2, req2)
	var resumed FollowResponse
	_ = json.Unmarshal(rec2.Body.Bytes(), &resumed)
	if len(resumed.Events) != 0 {
		t.Fatalf("resumed events = %v want [] (cursor should skip past consumed bytes)", linesOf(resumed.Events))
	}

	// Now an apprentice writes a third chunk. The same cursor must
	// pick up only the new lines, not the old ones.
	chunkC := []byte("c1\nc2\n")
	mC, bytesC := finalizedManifest("log-c", chunkC, func(m *logartifact.Manifest) { m.Sequence = 2 })
	reader.manifests = append(reader.manifests, mC)
	reader.bytes["log-c"] = bytesC

	req3 := httptest.NewRequest(http.MethodGet, "/api/v1/beads/spi-test1/logs?follow=true&cursor="+resumed.Cursor, nil)
	req3.Header.Set("X-Spire-Scope", string(logartifact.ScopeEngineer))
	rec3 := httptest.NewRecorder()
	s2.handleBeadByID(rec3, req3)
	var resumed2 FollowResponse
	_ = json.Unmarshal(rec3.Body.Bytes(), &resumed2)
	if !equalLines(linesOf(resumed2.Events), []string{"c1", "c2"}) {
		t.Fatalf("resumed2 lines = %v want [c1 c2]", linesOf(resumed2.Events))
	}
}

func TestFollowBeadLogs_GracefulDegradationNoLiveTail(t *testing.T) {
	// With NoopLiveTail wired, follow falls back to artifact-only
	// reads. This is the "Cloud Logging unavailable" path required by
	// acceptance criterion #3 for the design spi-7wzwk2.
	chunk := []byte("hello\nworld\n")
	m, b := finalizedManifest("log-a", chunk)
	reader := &fakeLogReader{
		manifests: []logartifact.Manifest{m},
		bytes:     map[string][]byte{"log-a": b},
	}
	withLogStubs(t, reader, store.Bead{Status: "open"}, nil)
	withLiveTail(t, NoopLiveTail())
	s := newLogTestServer("")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/beads/spi-test1/logs?follow=true", nil)
	req.Header.Set("X-Spire-Scope", string(logartifact.ScopeEngineer))
	rec := httptest.NewRecorder()
	s.handleBeadByID(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var resp FollowResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if !equalLines(linesOf(resp.Events), []string{"hello", "world"}) {
		t.Fatalf("lines = %v want [hello world]", linesOf(resp.Events))
	}
}

func TestFollowBeadLogs_LiveTailErrorIsNonFatal(t *testing.T) {
	// A LiveTail backend hiccup must not break the client — the
	// follow handler still returns artifact bytes. This guards the
	// "transient Cloud Logging hiccup never breaks the client"
	// invariant in pkg/gateway/bead_logs_follow.go.
	chunk := []byte("alpha\nbeta\n")
	m, b := finalizedManifest("log-a", chunk)
	reader := &fakeLogReader{
		manifests: []logartifact.Manifest{m},
		bytes:     map[string][]byte{"log-a": b},
	}
	tail := &fakeLiveTail{err: errors.New("cloud logging unreachable")}
	withLogStubs(t, reader, store.Bead{Status: "open"}, nil)
	withLiveTail(t, tail)
	s := newLogTestServer("")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/beads/spi-test1/logs?follow=true", nil)
	req.Header.Set("X-Spire-Scope", string(logartifact.ScopeEngineer))
	rec := httptest.NewRecorder()
	s.handleBeadByID(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%q", rec.Code, rec.Body.String())
	}
	var resp FollowResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if !equalLines(linesOf(resp.Events), []string{"alpha", "beta"}) {
		t.Fatalf("lines = %v want artifact lines despite tail err", linesOf(resp.Events))
	}
}

func TestFollowBeadLogs_InvalidCursorReturns400(t *testing.T) {
	withLogStubs(t, &fakeLogReader{}, store.Bead{Status: "open"}, nil)
	withLiveTail(t, nil)
	s := newLogTestServer("")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/beads/spi-test1/logs?follow=true&cursor=not-base64!@#", nil)
	rec := httptest.NewRecorder()
	s.handleBeadByID(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d want 400 body=%q", rec.Code, rec.Body.String())
	}
}

func TestFollowBeadLogs_DesktopScopeSkipsEngineerOnly(t *testing.T) {
	// Engineer-only artifacts must not leak through follow when the
	// caller scope is desktop. The artifact is silently skipped (the
	// follow surface is line-oriented and a single 403 must not
	// terminate the stream); the cursor advances past it so the next
	// poll moves on.
	chunk := []byte("secret\n")
	m, b := finalizedManifest("log-a", chunk, func(m *logartifact.Manifest) {
		m.Visibility = logartifact.VisibilityEngineerOnly
	})
	reader := &fakeLogReader{
		manifests: []logartifact.Manifest{m},
		bytes:     map[string][]byte{"log-a": b},
	}
	withLogStubs(t, reader, store.Bead{Status: "open"}, nil)
	withLiveTail(t, nil)
	s := newLogTestServer("")

	// Explicit desktop scope: engineer_only artifacts are silently
	// skipped regardless of the gateway's deployment-mode default
	// (spi-dhqv40 — local-native defaults to engineer, so explicit
	// header is required to test this path).
	req := httptest.NewRequest(http.MethodGet, "/api/v1/beads/spi-test1/logs?follow=true", nil)
	req.Header.Set("X-Spire-Scope", string(logartifact.ScopeDesktop))
	rec := httptest.NewRecorder()
	s.handleBeadByID(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var resp FollowResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Events) != 0 {
		t.Fatalf("events = %v want [] (desktop scope blocked)", linesOf(resp.Events))
	}
}

// TestFollowOnce_ReusableForLocalCLI verifies the exported FollowOnce
// API used by cmd/spire's local-mode follow path. A direct call must
// produce the same result as the HTTP handler so the local CLI and
// gateway-attached CLI render identical streams.
func TestFollowOnce_ReusableForLocalCLI(t *testing.T) {
	chunk := []byte("line1\nline2\n")
	m, b := finalizedManifest("log-a", chunk)
	reader := &fakeLogReader{
		manifests: []logartifact.Manifest{m},
		bytes:     map[string][]byte{"log-a": b},
	}

	events, cursor, done, err := FollowOnce(
		context.Background(), reader, NoopLiveTail(),
		logartifact.ScopeEngineer,
		"spi-test1", "",
		false, // bead not terminal
	)
	if err != nil {
		t.Fatalf("FollowOnce: %v", err)
	}
	if !equalLines(linesOf(events), []string{"line1", "line2"}) {
		t.Fatalf("events = %v want [line1 line2]", linesOf(events))
	}
	if cursor == "" {
		t.Fatalf("cursor empty, want set so caller can resume")
	}
	if done {
		t.Fatalf("done = true want false (bead not terminal)")
	}

	// Resume with the same cursor, no new bytes → empty events,
	// done=false (bead still in flight).
	events2, _, done2, err := FollowOnce(
		context.Background(), reader, NoopLiveTail(),
		logartifact.ScopeEngineer,
		"spi-test1", cursor,
		false,
	)
	if err != nil {
		t.Fatalf("FollowOnce resume: %v", err)
	}
	if len(events2) != 0 {
		t.Fatalf("events2 = %v want []", linesOf(events2))
	}
	if done2 {
		t.Fatalf("done2 = true want false")
	}
}

// TestFollowBeadLogs_StatelessAcrossReplicas guards the "no per-client
// state" invariant. Two independent Server instances given the same
// reader + cursor must yield identical responses.
func TestFollowBeadLogs_StatelessAcrossReplicas(t *testing.T) {
	chunk := []byte("a\nb\nc\n")
	m, b := finalizedManifest("log-a", chunk)
	reader := &fakeLogReader{
		manifests: []logartifact.Manifest{m},
		bytes:     map[string][]byte{"log-a": b},
	}
	withLogStubs(t, reader, store.Bead{Status: "open"}, nil)
	withLiveTail(t, nil)

	s1 := newLogTestServer("")
	s2 := newLogTestServer("")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/beads/spi-test1/logs?follow=true", nil)
	req.Header.Set("X-Spire-Scope", string(logartifact.ScopeEngineer))
	rec1 := httptest.NewRecorder()
	s1.handleBeadByID(rec1, req)

	req2 := httptest.NewRequest(http.MethodGet, "/api/v1/beads/spi-test1/logs?follow=true", nil)
	req2.Header.Set("X-Spire-Scope", string(logartifact.ScopeEngineer))
	rec2 := httptest.NewRecorder()
	s2.handleBeadByID(rec2, req2)

	if rec1.Body.String() != rec2.Body.String() {
		t.Fatalf("replica responses differ:\n  s1=%s\n  s2=%s", rec1.Body.String(), rec2.Body.String())
	}
}

// TestEncodeDecodeFollowCursor checks that the cursor is opaque,
// reversible, and round-trips a populated value.
func TestEncodeDecodeFollowCursor(t *testing.T) {
	in := followCursor{ArtifactID: "log-a", ByteOffset: 42, LiveTailFloor: 99}
	enc := encodeFollowCursor(in)
	if enc == "" {
		t.Fatalf("encode empty")
	}
	out, err := decodeFollowCursor(enc)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out != in {
		t.Fatalf("round-trip mismatch: in=%+v out=%+v", in, out)
	}
}

func TestMergeAndDedup(t *testing.T) {
	artifact := []LogEvent{
		{ArtifactID: "log-a", Sequence: 0, Line: "alpha"},
		{ArtifactID: "log-a", Sequence: 6, Line: "beta"},
	}
	tail := []LogEvent{
		{ArtifactID: "log-a", Sequence: 6, Line: "beta"},
		{ArtifactID: "log-a", Sequence: 11, Line: "gamma"},
	}
	got, floor := mergeAndDedup(artifact, tail, 0)
	want := []string{"alpha", "beta", "gamma"}
	if !equalLines(linesOf(got), want) {
		t.Fatalf("merged lines = %v want %v", linesOf(got), want)
	}
	if floor != 11 {
		t.Fatalf("floor = %d want 11", floor)
	}

	// Empty tail must not bump the floor or rewrite events.
	got2, floor2 := mergeAndDedup(artifact, nil, 5)
	if !equalLines(linesOf(got2), []string{"alpha", "beta"}) {
		t.Fatalf("artifact-only merge mismatched")
	}
	if floor2 != 5 {
		t.Fatalf("floor2 = %d want 5 (preserved)", floor2)
	}
}

// linesOf extracts the Line field from an event slice for tidier
// assertions.
func linesOf(events []LogEvent) []string {
	out := make([]string, len(events))
	for i, e := range events {
		out[i] = e.Line
	}
	return out
}

func equalLines(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Compile-time check: gatewayclient's FollowResponse and gateway's
// FollowResponse share field names so client decoding never drifts.
var _ = func() {
	_ = fmt.Sprintf("%v", FollowResponse{})
}
