package redact

import "regexp"

// DefaultPatterns returns the canonical pattern set the runtime
// redactor applies. Each pattern targets a high-confidence credential
// shape — keys with strong prefixes, well-defined character classes,
// and lengths that are unlikely to match prose by accident.
//
// Adding a pattern: bump CurrentRedactionVersion and add table-driven
// coverage in redact_test.go (positive AND negative cases). Negative
// cases are the discipline that keeps the set from drifting into
// false-positives — a transcript that talks about API keys without
// containing one must not be mangled.
//
// Removing or broadening a pattern: also bump
// CurrentRedactionVersion. Renderers re-apply the current set at read
// time so old artifacts pick up the new behavior without rewriting
// storage.
func DefaultPatterns() []*regexp.Regexp {
	return []*regexp.Regexp{
		// Anthropic API keys: documented prefix `sk-ant-` + base64url-ish
		// body of significant length. The 32-char floor avoids matching
		// short literals like "sk-ant-test" in code comments.
		regexp.MustCompile(`sk-ant-[A-Za-z0-9_\-]{32,}`),

		// OpenAI API keys: `sk-` followed by 32+ base64-ish chars. Tighter
		// than `sk-\w+` so the substring "sk-foo" in prose doesn't trip.
		// Anthropic-style sk-ant-* keys are matched by the first pattern
		// before this one runs.
		regexp.MustCompile(`sk-[A-Za-z0-9]{32,}`),

		// AWS access key IDs: 16-char canonical body, prefixes documented
		// by AWS. The character class matches AWS's documented shape and
		// the trailing word boundary keeps accidental concatenation from
		// extending the match.
		regexp.MustCompile(`\b(?:AKIA|ASIA|AROA|AIDA|AGPA|ANPA|ANVA|AIPA|ABIA|ACCA)[A-Z0-9]{16}\b`),

		// GitHub personal access tokens: ghp_/gho_/ghu_/ghs_/ghr_ + 36+
		// base62 chars. Prefix is mandatory so we don't accidentally
		// catch ssh fingerprints.
		regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{36,}`),

		// JWT-shaped strings: three base64url-ish segments separated by
		// dots, with a leading ey... so we don't catch arbitrary
		// dotted strings. JWT segments have no fixed length, but the
		// 16-char floor on each segment keeps the pattern from matching
		// short identifiers like "eyJ.foo.bar".
		regexp.MustCompile(`\beyJ[A-Za-z0-9_\-]{16,}\.[A-Za-z0-9_\-]{16,}\.[A-Za-z0-9_\-]{16,}\b`),

		// Authorization: Bearer ... — the header/value pair, anchored on
		// the token name so "...Bearer..." inside prose doesn't trip.
		// We replace the entire token after Bearer so the credential
		// itself is masked even if the header label survives in the log
		// shape elsewhere.
		regexp.MustCompile(`(?i)Authorization:\s*Bearer\s+[A-Za-z0-9_\-\.=]+`),

		// Generic api_key=... and password=... assignments. Tight
		// boundary on the value so "password=" alone (no value) is not
		// matched and a spaceless URL query value is.
		regexp.MustCompile(`(?i)\b(?:api[_\-]?key|password|secret|token)\s*[=:]\s*["']?[A-Za-z0-9_\-\.]{8,}["']?`),

		// GCP service-account-key JSON-fragment: the most distinctive
		// substring in a leaked SA key file is the BEGIN PRIVATE KEY
		// header. Catch it so a transcript that accidentally cats a
		// key file doesn't ship verbatim.
		regexp.MustCompile(`-----BEGIN (?:RSA |EC )?PRIVATE KEY-----[\s\S]*?-----END (?:RSA |EC )?PRIVATE KEY-----`),
	}
}
