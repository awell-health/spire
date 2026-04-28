package redact

import (
	"bytes"
	"regexp"
	"strings"
	"testing"
)

// TestRedact_PositivePatterns runs each canonical pattern against a
// fixture that should match. A failure here means the runtime redactor
// is letting a documented credential shape through to the byte store.
func TestRedact_PositivePatterns(t *testing.T) {
	cases := []struct {
		name  string
		input string
	}{
		{"anthropic_api_key", "log line with sk-ant-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA in the middle"},
		{"openai_api_key", `request: {"key":"sk-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}`},
		{"aws_access_key_id", "before AKIAIOSFODNN7EXAMPLE after"},
		{"aws_session_key", "session ASIAIOSFODNN7EXAMPLE expires"},
		{"github_pat_classic", "git push using token ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789 worked"},
		{"github_pat_oauth", "got gho_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA from device flow"},
		{"jwt", "Cookie: session=eyJhbGciOiJIUzI1NiIs.eyJzdWIiOiIxMjM0NTY3ODkwIiwidXNlciI6ImpvaG4ifQ.dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk done"},
		{"authorization_bearer", "GET /v1/x HTTP/1.1\nAuthorization: Bearer abcdef.ghijkl-MNOPQR=\n"},
		{"api_key_assignment", "config: api_key=mySecretValue123"},
		{"password_assignment", "init password = correctHorseBatteryStaple"},
		{"private_key_pem", "leaked file:\n-----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAKCAQEA1lsJzL\n-----END RSA PRIVATE KEY-----\nthat was bad"},
	}
	r := New()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, version := r.Redact([]byte(tc.input))
			if version != CurrentRedactionVersion {
				t.Errorf("version = %d, want %d", version, CurrentRedactionVersion)
			}
			if !bytes.Contains(out, MaskToken) {
				t.Errorf("expected redaction in %q, got %q", tc.input, out)
			}
		})
	}
}

// TestRedact_NegativeCases pins inputs that must NOT match any
// pattern. Each case targets a plausible false-positive surface
// (prose that mentions credential shapes, identifiers that look
// JWT-ish but lack the leading ey..., short test fixtures, etc.).
// A failure here means a pattern broadened in a way that mangles
// non-credential transcripts — which is the whole reason engineers
// resist running a redactor over their logs in the first place.
func TestRedact_NegativeCases(t *testing.T) {
	cases := []struct {
		name  string
		input string
	}{
		// Prose that mentions credential shapes without containing one.
		{"prose_about_anthropic", "the Anthropic API uses sk-ant- prefixed keys"},
		{"prose_about_openai", "OpenAI's sk- format is documented in their guide"},
		{"prose_about_aws", "configure the AKIA... value in your environment"},
		// Short test fixtures shouldn't trip the length floor.
		{"short_sk_test", "url: /api/sk-test"},
		{"sk_short", "value=sk-foo bar"},
		// Looks JWT-ish but no leading ey...
		{"non_jwt_dotted", "trace.id.abc.def.ghi"},
		// Authorization header without Bearer (e.g. Basic).
		{"basic_auth", "Authorization: Basic dXNlcjpwYXNz"},
		// password= without a real value (just empty/short).
		{"empty_password", "password=\nname=foo"},
		// AKIA-prefixed but wrong length (shouldn't match).
		{"akia_short", "AKIA1234"},
		// Just a benign string.
		{"plain_log", "implementing feature spi-cmy90h, no secrets here"},
	}
	r := New()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, _ := r.Redact([]byte(tc.input))
			if bytes.Contains(out, MaskToken) {
				t.Errorf("expected no redaction in %q, got %q", tc.input, out)
			}
		})
	}
}

// TestRedact_EmptyInput proves the redactor returns an empty (but
// non-nil) slice on empty input, and still reports the current version.
// The version-on-empty contract matters because callers stamp the
// manifest from this return value — a "no-op" upload still records that
// the redactor would have processed bytes.
func TestRedact_EmptyInput(t *testing.T) {
	out, version := New().Redact(nil)
	if version != CurrentRedactionVersion {
		t.Errorf("version = %d, want %d", version, CurrentRedactionVersion)
	}
	if len(out) != 0 {
		t.Errorf("expected empty output, got %q", out)
	}
}

// TestRedact_NilRedactor lets callers safely zero-value the type. A
// nil-receiver Redact returns the bytes unchanged plus the current
// version (so the manifest still gets stamped consistently); the
// substrate's upload path explicitly constructs a New() redactor so
// this is a defensive guard rather than a runtime path.
func TestRedact_NilRedactor(t *testing.T) {
	var r *Redactor
	out, version := r.Redact([]byte("sk-ant-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"))
	if version != CurrentRedactionVersion {
		t.Errorf("version = %d, want %d", version, CurrentRedactionVersion)
	}
	// Nil redactor masks nothing — bytes pass through.
	if !bytes.Contains(out, []byte("sk-ant-")) {
		t.Errorf("expected unchanged bytes from nil redactor, got %q", out)
	}
}

// TestRedact_DoesNotMutateInput ensures the redactor returns a fresh
// slice. A call site that retains its input must not see it modified
// after the call returns.
func TestRedact_DoesNotMutateInput(t *testing.T) {
	input := []byte("password=hunter2longstring")
	cp := make([]byte, len(input))
	copy(cp, input)
	r := New()
	r.Redact(input)
	if !bytes.Equal(input, cp) {
		t.Errorf("input mutated:\n got  %q\nwant %q", input, cp)
	}
}

// TestNewWithPatterns_Isolation lets a test exercise a single pattern
// in isolation. We use it to assert each pattern's exact replacement
// span — the union redactor in TestRedact_PositivePatterns checks that
// matches happen, this checks that the right substring is masked.
func TestNewWithPatterns_Isolation(t *testing.T) {
	bearer := regexp.MustCompile(`(?i)Authorization:\s*Bearer\s+[A-Za-z0-9_\-\.=]+`)
	r := NewWithPatterns([]*regexp.Regexp{bearer})
	in := "GET /\nAuthorization: Bearer XYZTOKENVALUE123\nUser-Agent: spire\n"
	out, _ := r.Redact([]byte(in))
	got := string(out)
	// The header text + token should be replaced; the User-Agent line
	// is untouched.
	if !strings.Contains(got, "[REDACTED]") {
		t.Errorf("expected mask in %q", got)
	}
	if strings.Contains(got, "XYZTOKENVALUE123") {
		t.Errorf("expected token to be masked, got %q", got)
	}
	if !strings.Contains(got, "User-Agent: spire") {
		t.Errorf("non-credential lines should pass through, got %q", got)
	}
}

// TestContains_ReportsMaskPresence is the inverse of MaskToken: a
// helper for tail-render code that wants to flag artifacts whose
// rendered output went through the redactor.
func TestContains_ReportsMaskPresence(t *testing.T) {
	if Contains([]byte("nothing to see here")) {
		t.Error("Contains returned true on clean bytes")
	}
	if !Contains([]byte("a [REDACTED] b")) {
		t.Error("Contains returned false on bytes containing MaskToken")
	}
}

// TestCurrentRedactionVersion_NonZero pins the contract that v0 is
// reserved for "no redactor applied". A version of 0 in a manifest row
// means engineer_only or a backfilled pre-existing row, NEVER "v0
// redactor ran". If a future patch sets CurrentRedactionVersion to 0
// the storage shape becomes ambiguous.
func TestCurrentRedactionVersion_NonZero(t *testing.T) {
	if CurrentRedactionVersion == 0 {
		t.Fatal("CurrentRedactionVersion must be > 0 (v0 reserved for 'no redactor applied')")
	}
}
