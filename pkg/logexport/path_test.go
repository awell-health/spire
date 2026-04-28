package logexport

import (
	"testing"

	"github.com/awell-health/spire/pkg/logartifact"
)

// TestParsePath_OperationalLog asserts that a wizard / apprentice
// operational log resolves to FileKindOperational with the seven
// path-shaping segments mapped to the Identity tuple. Stream defaults
// to stdout because the operational file aggregates both stdout and
// stderr into a single tee'd stream by convention.
func TestParsePath_OperationalLog(t *testing.T) {
	rel := "tower-a/spi-bead/spi-attempt/run-001/wizard-spi-bead/wizard/implement/operational.log"

	info, ok := ParsePath(rel)
	if !ok {
		t.Fatalf("ParsePath(%q) ok=false; want true", rel)
	}
	if info.Kind != FileKindOperational {
		t.Errorf("Kind = %q, want %q", info.Kind, FileKindOperational)
	}
	want := logartifact.Identity{
		Tower:     "tower-a",
		BeadID:    "spi-bead",
		AttemptID: "spi-attempt",
		RunID:     "run-001",
		AgentName: "wizard-spi-bead",
		Role:      logartifact.RoleWizard,
		Phase:     "implement",
		Stream:    logartifact.StreamStdout,
	}
	if info.Identity != want {
		t.Errorf("Identity = %+v, want %+v", info.Identity, want)
	}
	if info.Sequence != 0 {
		t.Errorf("Sequence = %d, want 0", info.Sequence)
	}
}

// TestParsePath_Transcript covers the provider-segment shape: the
// Identity must carry the provider, and Stream comes from the leaf
// stem. Sequence stays at 0 for the unsuffixed leaf.
func TestParsePath_Transcript(t *testing.T) {
	rel := "tower-a/spi-bead/spi-attempt/run-001/apprentice-spi-bead-0/apprentice/implement/claude/transcript.jsonl"

	info, ok := ParsePath(rel)
	if !ok {
		t.Fatalf("ParsePath(%q) ok=false", rel)
	}
	if info.Kind != FileKindTranscript {
		t.Errorf("Kind = %q, want %q", info.Kind, FileKindTranscript)
	}
	if info.Identity.Provider != "claude" {
		t.Errorf("Provider = %q, want claude", info.Identity.Provider)
	}
	if info.Identity.Stream != logartifact.StreamTranscript {
		t.Errorf("Stream = %q, want transcript", info.Identity.Stream)
	}
	if info.Sequence != 0 {
		t.Errorf("Sequence = %d, want 0", info.Sequence)
	}
}

// TestParsePath_TranscriptChunked verifies the `<stream>-<N>.jsonl`
// chunked-artifact suffix is parsed into Sequence > 0.
func TestParsePath_TranscriptChunked(t *testing.T) {
	rel := "tower-a/spi-bead/spi-attempt/run-001/apprentice-spi-bead-0/apprentice/implement/claude/transcript-7.jsonl"

	info, ok := ParsePath(rel)
	if !ok {
		t.Fatalf("ParsePath(%q) ok=false", rel)
	}
	if info.Identity.Stream != logartifact.StreamTranscript {
		t.Errorf("Stream = %q, want transcript", info.Identity.Stream)
	}
	if info.Sequence != 7 {
		t.Errorf("Sequence = %d, want 7", info.Sequence)
	}
}

// TestParsePath_TranscriptWithoutProvider covers the operational stream
// shape where Provider is empty (8 segments, .jsonl leaf, no provider
// segment).
func TestParsePath_TranscriptWithoutProvider(t *testing.T) {
	rel := "tower-a/spi-bead/spi-attempt/run-001/wizard-spi-bead/wizard/implement/stderr.jsonl"

	info, ok := ParsePath(rel)
	if !ok {
		t.Fatalf("ParsePath(%q) ok=false", rel)
	}
	if info.Identity.Provider != "" {
		t.Errorf("Provider = %q, want empty", info.Identity.Provider)
	}
	if info.Identity.Stream != logartifact.StreamStderr {
		t.Errorf("Stream = %q, want stderr", info.Identity.Stream)
	}
}

// TestParsePath_RejectsBadShapes pins the negative cases: too few
// segments, unknown leaf extension, and empty fields all return ok=false.
func TestParsePath_RejectsBadShapes(t *testing.T) {
	cases := map[string]string{
		"too few segments":         "tower/bead/attempt/run/agent/role/operational.log",
		"unknown leaf extension":   "tower/bead/attempt/run/agent/role/phase/transcript.txt",
		"empty tower":              "/bead/attempt/run/agent/role/phase/operational.log",
		"path traversal":           "../tower/bead/attempt/run/agent/role/phase/operational.log",
		"trailing slash":           "tower/bead/attempt/run/agent/role/phase/",
	}
	for name, rel := range cases {
		t.Run(name, func(t *testing.T) {
			if _, ok := ParsePath(rel); ok {
				t.Errorf("ParsePath(%q) ok=true; want false", rel)
			}
		})
	}
}

// TestParsePath_AcceptsLeadingSlash exercises the input form WalkDir
// produces (an absolute-rel-to-root key); the parser strips leading
// "./" and "/" to keep the call sites uniform.
func TestParsePath_AcceptsLeadingSlash(t *testing.T) {
	rel := "/tower-a/spi-bead/spi-attempt/run-001/wizard-spi-bead/wizard/implement/operational.log"
	if _, ok := ParsePath(rel); !ok {
		t.Errorf("ParsePath(%q) ok=false; want true", rel)
	}
}
