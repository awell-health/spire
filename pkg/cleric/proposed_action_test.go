package cleric

import (
	"strings"
	"testing"
)

func TestParseProposedAction_HappyPath(t *testing.T) {
	stdout := []byte(`{"verb":"resummon","reasoning":"transient build flake","failure_class":"build-error"}`)
	pa, err := ParseProposedAction(stdout)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if pa.Verb != "resummon" || pa.FailureClass != "build-error" {
		t.Errorf("got %+v", pa)
	}
}

func TestParseProposedAction_StripsMarkdownFence(t *testing.T) {
	stdout := []byte("```json\n{\"verb\":\"resummon\",\"reasoning\":\"r\",\"failure_class\":\"c\"}\n```\n")
	pa, err := ParseProposedAction(stdout)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if pa.Verb != "resummon" {
		t.Errorf("got verb %q", pa.Verb)
	}
}

func TestParseProposedAction_StripsLeadingProse(t *testing.T) {
	stdout := []byte("Here is the proposal:\n{\"verb\":\"resummon\",\"reasoning\":\"r\",\"failure_class\":\"c\"}")
	pa, err := ParseProposedAction(stdout)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if pa.Verb != "resummon" {
		t.Errorf("got verb %q", pa.Verb)
	}
}

func TestParseProposedAction_RejectsEmpty(t *testing.T) {
	if _, err := ParseProposedAction([]byte("")); err == nil {
		t.Fatal("expected error for empty stdout")
	}
}

func TestParseProposedAction_RejectsUnknownVerb(t *testing.T) {
	stdout := []byte(`{"verb":"unleash-the-kraken","reasoning":"r","failure_class":"c"}`)
	_, err := ParseProposedAction(stdout)
	if err == nil || !strings.Contains(err.Error(), "unknown verb") {
		t.Fatalf("expected unknown-verb error, got %v", err)
	}
}

func TestParseProposedAction_RejectsMissingFields(t *testing.T) {
	cases := map[string]string{
		"missing verb":          `{"reasoning":"r","failure_class":"c"}`,
		"missing reasoning":     `{"verb":"resummon","failure_class":"c"}`,
		"missing failure_class": `{"verb":"resummon","reasoning":"r"}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseProposedAction([]byte(body)); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestParseProposedAction_RejectsBadConfidence(t *testing.T) {
	stdout := []byte(`{"verb":"resummon","reasoning":"r","failure_class":"c","confidence":1.5}`)
	if _, err := ParseProposedAction(stdout); err == nil {
		t.Fatal("expected error for confidence>1")
	}
	stdout = []byte(`{"verb":"resummon","reasoning":"r","failure_class":"c","confidence":-0.1}`)
	if _, err := ParseProposedAction(stdout); err == nil {
		t.Fatal("expected error for confidence<0")
	}
}

func TestParseProposedAction_RejectsUnknownArg(t *testing.T) {
	stdout := []byte(`{"verb":"reset --to <step>","args":{"step":"implement","extra":"nope"},"reasoning":"r","failure_class":"c"}`)
	_, err := ParseProposedAction(stdout)
	if err == nil || !strings.Contains(err.Error(), "unknown arg") {
		t.Fatalf("expected unknown-arg error, got %v", err)
	}
}

func TestParseProposedAction_RequiresArg(t *testing.T) {
	stdout := []byte(`{"verb":"reset --to <step>","reasoning":"r","failure_class":"c"}`)
	_, err := ParseProposedAction(stdout)
	if err == nil || !strings.Contains(err.Error(), "requires arg") {
		t.Fatalf("expected requires-arg error, got %v", err)
	}
}

func TestParseProposedAction_PropagatesManifestDestructiveDefault(t *testing.T) {
	// reset --hard has DefaultDestructive=true; cleric didn't set
	// destructive, but the parsed action should carry the default.
	stdout := []byte(`{"verb":"reset --hard","reasoning":"r","failure_class":"c"}`)
	pa, err := ParseProposedAction(stdout)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !pa.Destructive {
		t.Error("expected destructive=true from manifest default")
	}
}

func TestParseProposedAction_HonorsExplicitDestructive(t *testing.T) {
	stdout := []byte(`{"verb":"resummon","reasoning":"r","failure_class":"c","destructive":true}`)
	pa, err := ParseProposedAction(stdout)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !pa.Destructive {
		t.Error("expected destructive=true from cleric override")
	}
}

func TestProposedAction_MarshalRoundTrip(t *testing.T) {
	src := ProposedAction{
		Verb:         "reset --to <step>",
		Args:         map[string]string{"step": "implement"},
		Reasoning:    "build flake on review",
		Confidence:   0.8,
		FailureClass: "build-error",
	}
	if err := src.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	enc, err := src.Marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	round, err := ParseProposedAction(enc)
	if err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	if round.Verb != src.Verb || round.Args["step"] != src.Args["step"] {
		t.Errorf("round-trip mismatch: %+v vs %+v", round, src)
	}
}
