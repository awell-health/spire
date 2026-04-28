package logartifact

import (
	"strings"
	"testing"
)

// validIdentity returns a populated identity suitable for the happy
// path. Tests mutate fields to exercise validation.
func validIdentity() Identity {
	return Identity{
		Tower:     "awell-test",
		BeadID:    "spi-b986in",
		AttemptID: "spi-attempt",
		RunID:     "run-001",
		AgentName: "wizard-spi-b986in",
		Role:      RoleWizard,
		Phase:     "implement",
		Provider:  "claude",
		Stream:    StreamTranscript,
	}
}

// TestBuildObjectKey_DesignShape pins the canonical object-key shape
// that the design bead spi-7wzwk2 promised. Other beads (the gateway
// spi-j3r694, the exporter spi-k1cnof, the board spi-egw26j) compute
// the same key independently; if this shape drifts, those callers will
// fail to find artifacts they uploaded.
func TestBuildObjectKey_DesignShape(t *testing.T) {
	got, err := BuildObjectKey("logs", validIdentity(), 0)
	if err != nil {
		t.Fatalf("BuildObjectKey: %v", err)
	}
	want := "logs/awell-test/spi-b986in/spi-attempt/run-001/wizard-spi-b986in/wizard/implement/claude/transcript.jsonl"
	if got != want {
		t.Errorf("BuildObjectKey =\n  %q\nwant\n  %q", got, want)
	}
}

// TestBuildObjectKey_SequenceSuffix ensures sequences > 0 produce the
// `<stream>-<seq>.jsonl` shape and sequence == 0 produces `<stream>.jsonl`.
// This is what makes single-shot artifacts identical across runs and
// lets chunked artifacts coexist in the same prefix.
func TestBuildObjectKey_SequenceSuffix(t *testing.T) {
	cases := []struct {
		seq  int
		want string
	}{
		{0, "transcript.jsonl"},
		{1, "transcript-1.jsonl"},
		{42, "transcript-42.jsonl"},
	}
	for _, tc := range cases {
		got, err := BuildObjectKey("", validIdentity(), tc.seq)
		if err != nil {
			t.Errorf("seq=%d: %v", tc.seq, err)
			continue
		}
		if !strings.HasSuffix(got, tc.want) {
			t.Errorf("seq=%d: got %q, want suffix %q", tc.seq, got, tc.want)
		}
	}
}

// TestBuildObjectKey_OmitsProviderWhenEmpty proves the wizard-operational
// shape (no provider) drops the provider segment cleanly so we don't
// produce `.../implement//stdout.jsonl`.
func TestBuildObjectKey_OmitsProviderWhenEmpty(t *testing.T) {
	id := validIdentity()
	id.Provider = ""
	id.Stream = StreamStdout
	got, err := BuildObjectKey("", id, 0)
	if err != nil {
		t.Fatalf("BuildObjectKey: %v", err)
	}
	want := "awell-test/spi-b986in/spi-attempt/run-001/wizard-spi-b986in/wizard/implement/stdout.jsonl"
	if got != want {
		t.Errorf("BuildObjectKey = %q, want %q", got, want)
	}
}

// TestBuildObjectKey_EmptyPrefix accepts an empty prefix and produces a
// key with no leading slash. Useful for backends that supply their own
// root (LocalStore) or want to compose a per-tower prefix outside the
// helper.
func TestBuildObjectKey_EmptyPrefix(t *testing.T) {
	got, err := BuildObjectKey("", validIdentity(), 0)
	if err != nil {
		t.Fatalf("BuildObjectKey: %v", err)
	}
	if strings.HasPrefix(got, "/") {
		t.Errorf("BuildObjectKey leaked leading slash: %q", got)
	}
}

// TestBuildObjectKey_PrefixSlashTolerant strips leading and trailing
// slashes from the prefix so callers can pass either form.
func TestBuildObjectKey_PrefixSlashTolerant(t *testing.T) {
	cases := []string{"logs", "/logs", "logs/", "/logs/"}
	for _, p := range cases {
		got, err := BuildObjectKey(p, validIdentity(), 0)
		if err != nil {
			t.Errorf("prefix=%q: %v", p, err)
			continue
		}
		if !strings.HasPrefix(got, "logs/") {
			t.Errorf("prefix=%q produced %q (want logs/...)", p, got)
		}
		if strings.Contains(got, "//") {
			t.Errorf("prefix=%q produced double-slash key %q", p, got)
		}
	}
}

// TestBuildObjectKey_NeverEmbedsPodName is the regression for the design
// constraint that pod/node names must never appear in the key. We can't
// detect every pod-naming convention, but we can prove that an
// AgentName containing common pod-only tokens like "-pod-" or a
// kubernetes UID does in fact leak through (since AgentName is part of
// the identity) — which is the apprentice's responsibility to set
// correctly. This test instead pins the structural shape: only the
// nine canonical identity slots end up in the key.
func TestBuildObjectKey_NeverEmbedsPodName(t *testing.T) {
	id := validIdentity()
	got, err := BuildObjectKey("logs", id, 0)
	if err != nil {
		t.Fatalf("BuildObjectKey: %v", err)
	}
	parts := strings.Split(got, "/")
	// prefix + 8 identity segments + leaf = 10 segments (with provider)
	if len(parts) != 10 {
		t.Errorf("BuildObjectKey produced %d segments, want 10: %v", len(parts), parts)
	}
}

// TestBuildObjectKey_RejectsTraversalSegments proves the sanitizer
// catches path-traversal sequences in any identity slot. A backend that
// trusts the key would otherwise let a malformed identity escape the
// per-tower prefix.
func TestBuildObjectKey_RejectsTraversalSegments(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*Identity)
	}{
		{"tower-traversal", func(id *Identity) { id.Tower = ".." }},
		{"bead-slash", func(id *Identity) { id.BeadID = "spi/escape" }},
		{"attempt-empty", func(id *Identity) { id.AttemptID = "" }},
		{"agent-backslash", func(id *Identity) { id.AgentName = "wizard\\evil" }},
		{"phase-null", func(id *Identity) { id.Phase = "implement\x00" }},
		{"provider-double-dot", func(id *Identity) { id.Provider = ".." }},
		{"stream-empty", func(id *Identity) { id.Stream = "" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id := validIdentity()
			tc.mut(&id)
			if _, err := BuildObjectKey("logs", id, 0); err == nil {
				t.Errorf("expected error for %s", tc.name)
			}
		})
	}
}

// TestBuildObjectKey_RejectsNegativeSequence proves negative sequences
// are caught at the path layer. A backend that didn't validate would
// otherwise produce a `stream--1.jsonl` key.
func TestBuildObjectKey_RejectsNegativeSequence(t *testing.T) {
	if _, err := BuildObjectKey("logs", validIdentity(), -1); err == nil {
		t.Error("expected error for negative sequence")
	}
}

// TestBuildObjectKey_DeterministicAcrossCalls is the design's
// "deterministic naming based on stable Spire identity" promise: two
// calls with the same inputs produce the same key, byte-for-byte.
func TestBuildObjectKey_DeterministicAcrossCalls(t *testing.T) {
	id := validIdentity()
	got1, err := BuildObjectKey("logs", id, 7)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	got2, err := BuildObjectKey("logs", id, 7)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if got1 != got2 {
		t.Errorf("non-deterministic: %q vs %q", got1, got2)
	}
}

// TestStatusValid / TestStreamValid / TestRoleValid pin the known sets.
// They double as regression guards for typos in the constants.
func TestStatusValid(t *testing.T) {
	for _, s := range []Status{StatusWriting, StatusFinalized, StatusFailed} {
		if !s.Valid() {
			t.Errorf("%q should be Valid", s)
		}
	}
	if Status("bogus").Valid() {
		t.Error("unknown status reported Valid")
	}
}

func TestStreamValid(t *testing.T) {
	for _, s := range []Stream{StreamStdout, StreamStderr, StreamTranscript} {
		if !s.Valid() {
			t.Errorf("%q should be Valid", s)
		}
	}
	if Stream("bogus").Valid() {
		t.Error("unknown stream reported Valid")
	}
}

func TestRoleValid(t *testing.T) {
	for _, r := range []Role{RoleWizard, RoleApprentice, RoleSage, RoleCleric, RoleArbiter} {
		if !r.Valid() {
			t.Errorf("%q should be Valid", r)
		}
	}
	if Role("bogus").Valid() {
		t.Error("unknown role reported Valid")
	}
}

// TestIdentityValidate covers the required-field error surface.
func TestIdentityValidate(t *testing.T) {
	if err := validIdentity().Validate(); err != nil {
		t.Errorf("valid identity should pass: %v", err)
	}
	cases := []struct {
		name string
		mut  func(*Identity)
	}{
		{"empty tower", func(id *Identity) { id.Tower = "" }},
		{"empty bead", func(id *Identity) { id.BeadID = "" }},
		{"empty attempt", func(id *Identity) { id.AttemptID = "" }},
		{"empty run", func(id *Identity) { id.RunID = "" }},
		{"empty agent", func(id *Identity) { id.AgentName = "" }},
		{"empty role", func(id *Identity) { id.Role = "" }},
		{"empty phase", func(id *Identity) { id.Phase = "" }},
		{"empty stream", func(id *Identity) { id.Stream = "" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id := validIdentity()
			tc.mut(&id)
			if err := id.Validate(); err == nil {
				t.Errorf("expected validation error for %s", tc.name)
			}
		})
	}
}
