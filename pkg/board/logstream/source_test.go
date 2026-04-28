package logstream

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/awell-health/spire/pkg/gatewayclient"
)

// --- LocalSource tests ---

// writeLog plants a file under dir with the given content. Used by the
// local-source tests to set up a wizards/ tree without calling into the
// dolt/wizard packages.
func writeLog(t *testing.T, dir, name, body string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return p
}

// TestLocalSource_ListEmptyOnMissingTree confirms that the empty state
// is "no error, empty slice" — the inspector and CLI distinguish "no
// artifacts yet" from real errors by checking len(artifacts) == 0.
func TestLocalSource_ListEmptyOnMissingTree(t *testing.T) {
	src := NewLocalSource(t.TempDir())
	got, err := src.List(context.Background(), "spi-no-such-bead")
	if err != nil {
		t.Fatalf("List returned error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected 0 artifacts on missing tree, got %d: %+v", len(got), got)
	}
}

// TestLocalSource_ListEmptyOnEmptyBeadID confirms an empty beadID does
// not walk the wizards directory by accident — happens when a caller
// forgets to pass the bead ID.
func TestLocalSource_ListEmptyOnEmptyBeadID(t *testing.T) {
	src := NewLocalSource(t.TempDir())
	got, err := src.List(context.Background(), "")
	if err != nil {
		t.Fatalf("List returned error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected 0 artifacts for empty bead, got %d", len(got))
	}
}

// TestLocalSource_ListLegacyLayoutTopLevel covers the canonical local
// layout: <root>/wizard-<bead>.log + per-provider transcripts under
// <root>/wizard-<bead>/<provider>/. The local source must surface the
// top-level log with Name="wizard" so cycle tagging in the inspector
// still has a stable handle.
func TestLocalSource_ListLegacyLayoutTopLevel(t *testing.T) {
	root := t.TempDir()
	beadID := "spi-aaaa"
	wizardName := "wizard-" + beadID

	wizardLog := writeLog(t, root, wizardName+".log", "wizard says hi\n")
	claudeLog := writeLog(t, filepath.Join(root, wizardName, "claude"),
		"epic-plan-20260417-173412.jsonl",
		`{"type":"system","subtype":"init"}`+"\n")
	stderrLog := writeLog(t, filepath.Join(root, wizardName, "claude"),
		"epic-plan-20260417-173412.stderr.log",
		"oh no\n")

	src := NewLocalSource(root)
	got, err := src.List(context.Background(), beadID)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 artifacts (wizard + claude transcript), got %d: %+v", len(got), got)
	}

	// First artifact is the wizard top-level log.
	if got[0].Name != "wizard" {
		t.Errorf("got[0].Name = %q, want %q", got[0].Name, "wizard")
	}
	if got[0].Path != wizardLog {
		t.Errorf("got[0].Path = %q, want %q", got[0].Path, wizardLog)
	}
	if got[0].Provider != "" {
		t.Errorf("got[0].Provider = %q, want empty (operational log)", got[0].Provider)
	}
	if got[0].Content != "wizard says hi\n" {
		t.Errorf("got[0].Content = %q, want preloaded log content", got[0].Content)
	}

	// Second artifact is the claude transcript with a paired stderr sidecar.
	if got[1].Provider != "claude" {
		t.Errorf("got[1].Provider = %q, want claude", got[1].Provider)
	}
	if got[1].Path != claudeLog {
		t.Errorf("got[1].Path = %q, want %q", got[1].Path, claudeLog)
	}
	if got[1].StderrPath != stderrLog {
		t.Errorf("got[1].StderrPath = %q, want %q", got[1].StderrPath, stderrLog)
	}
	if got[1].StderrContent != "oh no\n" {
		t.Errorf("got[1].StderrContent = %q, want sidecar content", got[1].StderrContent)
	}
	// Display name strips the timestamp suffix and renders HH:MM.
	if got[1].Name != "epic-plan (17:34)" {
		t.Errorf("got[1].Name = %q, want %q", got[1].Name, "epic-plan (17:34)")
	}
}

// TestLocalSource_ListSiblingSpawn covers spawn pairing: a sibling
// orchestrator log (wizard-<bead>-implement-1.log) plus claude
// transcripts under wizard-<bead>-implement-1/claude/. The local source
// must produce both an "implement-1" Artifact and an
// "implement-1/<label> (HH:MM)" Artifact so the inspector's cycle
// tagging matches identically across the legacy and new code paths.
func TestLocalSource_ListSiblingSpawn(t *testing.T) {
	root := t.TempDir()
	beadID := "spi-bbbb"
	wizardName := "wizard-" + beadID

	writeLog(t, root, wizardName+".log", "parent\n")
	siblingLog := writeLog(t, root, wizardName+"-implement-1.log", "spawn\n")
	siblingTranscript := writeLog(t, filepath.Join(root, wizardName+"-implement-1", "claude"),
		"implement-20260422-184843.jsonl",
		`{"type":"system","subtype":"init"}`+"\n")

	src := NewLocalSource(root)
	got, err := src.List(context.Background(), beadID)
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	byName := map[string]Artifact{}
	for _, a := range got {
		byName[a.Name] = a
	}

	if a, ok := byName["implement-1"]; !ok {
		t.Errorf("missing sibling spawn Artifact: %v", artifactNames(got))
	} else if a.Path != siblingLog {
		t.Errorf("implement-1 Path = %q, want %q", a.Path, siblingLog)
	}

	wantName := "implement-1/implement (18:48)"
	a, ok := byName[wantName]
	if !ok {
		t.Errorf("missing paired transcript Artifact %q: %v", wantName, artifactNames(got))
	} else if a.Path != siblingTranscript {
		t.Errorf("paired transcript Path = %q, want %q", a.Path, siblingTranscript)
	}
}

// TestLocalSource_ListPreservesLegacyClaudeLogExtension regression-
// guards the .log extension fallback for claude transcripts captured
// before the .jsonl convention landed (spi-7mgv9).
func TestLocalSource_ListPreservesLegacyClaudeLogExtension(t *testing.T) {
	root := t.TempDir()
	beadID := "spi-cccc"
	wizardName := "wizard-" + beadID

	writeLog(t, filepath.Join(root, wizardName, "claude"),
		"epic-20260417-173412.log",
		`{"type":"system","subtype":"init"}`+"\n")

	src := NewLocalSource(root)
	got, err := src.List(context.Background(), beadID)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 legacy claude artifact, got %d: %+v", len(got), got)
	}
	if got[0].Provider != "claude" {
		t.Errorf("Provider = %q, want claude", got[0].Provider)
	}
}

// TestDeriveProviderLogName_Fixtures keeps the display-name derivation
// stable so legacy filenames keep parsing into "<label> (HH:MM)".
func TestDeriveProviderLogName_Fixtures(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"epic-plan-20260417-173412.jsonl", "epic-plan (17:34)"},
		{"epic-20260417-120000.log", "epic (12:00)"},
		{"weird.jsonl", "weird"},
		{"", ""},
	}
	for _, tc := range cases {
		got := DeriveProviderLogName(tc.in)
		if got != tc.want {
			t.Errorf("DeriveProviderLogName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// --- GatewaySource tests ---

// fakeGatewayClient is an in-memory GatewayLogClient used by the
// gateway-source tests. It implements the minimal surface NewGatewaySource
// needs without spinning up an HTTPS server.
type fakeGatewayClient struct {
	records  []gatewayclient.LogArtifactRecord
	bytes    map[string][]byte
	listErr  error
	fetchErr map[string]error
}

func (f *fakeGatewayClient) ListAllBeadLogs(_ context.Context, beadID string) ([]gatewayclient.LogArtifactRecord, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := make([]gatewayclient.LogArtifactRecord, 0, len(f.records))
	for _, r := range f.records {
		if r.BeadID == beadID {
			out = append(out, r)
		}
	}
	return out, nil
}

func (f *fakeGatewayClient) FetchBeadLogRaw(_ context.Context, _ string, artifactID string, _ bool) ([]byte, error) {
	if err, ok := f.fetchErr[artifactID]; ok {
		return nil, err
	}
	if b, ok := f.bytes[artifactID]; ok {
		return b, nil
	}
	return nil, gatewayclient.ErrNotFound
}

// TestGatewaySource_ListEmpty verifies that an empty manifest list
// surfaces as the "no artifacts yet" state rather than an error. The
// inspector and CLI key off this distinction for the friendly empty
// message.
func TestGatewaySource_ListEmpty(t *testing.T) {
	src := NewGatewaySource(&fakeGatewayClient{}, false)
	got, err := src.List(context.Background(), "spi-empty")
	if err != nil {
		t.Fatalf("List returned error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected 0 artifacts, got %d", len(got))
	}
}

// TestGatewaySource_ListNotFound treats gatewayclient.ErrNotFound as
// the empty state so a caller asking about a not-yet-known bead does
// not see a confusing error.
func TestGatewaySource_ListNotFound(t *testing.T) {
	client := &fakeGatewayClient{listErr: gatewayclient.ErrNotFound}
	src := NewGatewaySource(client, false)
	got, err := src.List(context.Background(), "spi-nope")
	if err != nil {
		t.Fatalf("List returned error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected 0 artifacts on 404, got %d", len(got))
	}
}

// TestGatewaySource_ListPropagatesRealErrors guards against the
// reverse mistake — a real error (network, auth) must NOT degrade to
// the empty state, otherwise users get silent failures.
func TestGatewaySource_ListPropagatesRealErrors(t *testing.T) {
	boom := errors.New("network is on fire")
	client := &fakeGatewayClient{listErr: boom}
	src := NewGatewaySource(client, false)
	_, err := src.List(context.Background(), "spi-x")
	if err == nil {
		t.Fatalf("expected error to propagate, got nil")
	}
	if !errors.Is(err, boom) {
		t.Errorf("got %v, want wrapped %v", err, boom)
	}
}

// TestGatewaySource_ListFetchesFinalizedTranscript walks the happy
// path: a transcript manifest with bytes plus a paired stderr sidecar.
// The result is one Artifact whose Content + StderrContent are the
// fetched bytes — gateway parity with the local source's preload
// contract.
func TestGatewaySource_ListFetchesFinalizedTranscript(t *testing.T) {
	beadID := "spi-go"
	transcriptID := "art-trans"
	stderrID := "art-stderr"
	client := &fakeGatewayClient{
		records: []gatewayclient.LogArtifactRecord{
			{
				ID:        transcriptID,
				BeadID:    beadID,
				AgentName: "wizard-" + beadID,
				Role:      "wizard",
				Phase:     "implement",
				Provider:  "claude",
				Stream:    "transcript",
				Sequence:  0,
				Status:    "finalized",
			},
			{
				ID:        stderrID,
				BeadID:    beadID,
				AgentName: "wizard-" + beadID,
				Role:      "wizard",
				Phase:     "implement",
				Provider:  "claude",
				Stream:    "stderr",
				Sequence:  0,
				Status:    "finalized",
			},
		},
		bytes: map[string][]byte{
			transcriptID: []byte(`{"type":"system","subtype":"init"}` + "\n"),
			stderrID:     []byte("warn: something\n"),
		},
	}
	src := NewGatewaySource(client, false)
	got, err := src.List(context.Background(), beadID)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 transcript artifact (stderr paired in), got %d: %+v", len(got), got)
	}
	a := got[0]
	if a.Provider != "claude" {
		t.Errorf("Provider = %q, want claude", a.Provider)
	}
	if !strings.Contains(a.Content, `"type":"system"`) {
		t.Errorf("Content missing fetched bytes: %q", a.Content)
	}
	if !strings.Contains(a.StderrContent, "warn: something") {
		t.Errorf("StderrContent missing sidecar bytes: %q", a.StderrContent)
	}
}

// TestGatewaySource_ListSkipsByteFetchForNonFinalized covers manifest
// rows in writing/failed state. Bytes can't be served, so List returns
// the artifact with empty Content rather than failing the whole list
// (the inspector renders "(empty log)" for these).
func TestGatewaySource_ListSkipsByteFetchForNonFinalized(t *testing.T) {
	beadID := "spi-pending"
	client := &fakeGatewayClient{
		records: []gatewayclient.LogArtifactRecord{
			{
				ID:        "art-pending",
				BeadID:    beadID,
				AgentName: "wizard-" + beadID,
				Role:      "wizard",
				Phase:     "implement",
				Provider:  "claude",
				Stream:    "transcript",
				Sequence:  0,
				Status:    "writing",
			},
		},
		bytes: map[string][]byte{
			"art-pending": []byte(`{"type":"system"}`),
		},
	}
	src := NewGatewaySource(client, false)
	got, err := src.List(context.Background(), beadID)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 artifact (writing row), got %d", len(got))
	}
	if got[0].Content != "" {
		t.Errorf("Content = %q, want empty (writing row should not fetch bytes)", got[0].Content)
	}
}

// TestGatewaySource_NamingMatchesLocalConventions ensures the gateway
// source synthesises display names that mirror the local layout, so
// inspector cycle tagging (which keys off Name) keeps working when a
// bead's logs are read through the gateway.
func TestGatewaySource_NamingMatchesLocalConventions(t *testing.T) {
	beadID := "spi-name"
	cases := []struct {
		record gatewayclient.LogArtifactRecord
		want   string
	}{
		{
			record: gatewayclient.LogArtifactRecord{
				BeadID:    beadID,
				AgentName: "wizard-" + beadID,
				Provider:  "",
				Stream:    "stdout",
			},
			want: "wizard",
		},
		{
			record: gatewayclient.LogArtifactRecord{
				BeadID:    beadID,
				AgentName: "wizard-" + beadID + "-implement-1",
				Provider:  "",
				Stream:    "stdout",
			},
			want: "implement-1",
		},
		{
			record: gatewayclient.LogArtifactRecord{
				BeadID:    beadID,
				AgentName: "wizard-" + beadID + "-implement-1",
				Provider:  "claude",
				Phase:     "implement",
				Stream:    "transcript",
			},
			want: "implement-1/claude (implement)",
		},
	}
	for _, tc := range cases {
		got := gatewayArtifactName(tc.record)
		if got != tc.want {
			t.Errorf("gatewayArtifactName(%+v) = %q, want %q", tc.record, got, tc.want)
		}
	}
}

// TestGatewaySource_ListNilClient guards the zero-value path: a Source
// constructed without a client must still surface the empty state
// rather than panic.
func TestGatewaySource_ListNilClient(t *testing.T) {
	src := NewGatewaySource(nil, false)
	got, err := src.List(context.Background(), "spi-bead")
	if err != nil {
		t.Fatalf("expected no error from nil client, got %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty list, got %d", len(got))
	}
}

// artifactNames is a debugging helper used by failure paths above.
func artifactNames(arts []Artifact) []string {
	out := make([]string, len(arts))
	for i, a := range arts {
		out[i] = a.Name
	}
	return out
}
