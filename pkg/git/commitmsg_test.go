package git

import "testing"

func TestBeadIDFromSubject(t *testing.T) {
	tests := []struct {
		name    string
		subject string
		want    string
	}{
		{"happy path spi prefix", "feat(spi-abc123): thing", "spi-abc123"},
		{"fix prefix", "fix(xserver-0hy): handle nil pointer", "xserver-0hy"},
		{"chore prefix", "chore(pan-b7d0): upgrade deps", "pan-b7d0"},
		{"docs prefix", "docs(web-9xx): update README", "web-9xx"},
		{"refactor prefix", "refactor(spi-82jzy): clean up", "spi-82jzy"},
		{"hierarchical sub-task id", "feat(spi-a3f8.1): sub-task under epic", "spi-a3f8.1"},
		{"deeply nested sub-task id", "feat(spi-a3f8.1.2): nested sub-task", "spi-a3f8.1.2"},
		{"multi-hyphen prefix (lower-kebab)", "feat(foo-bar-baz): yes", ""},

		// Negatives.
		{"no bead id (plain subject)", "thing", ""},
		{"merge commit", "Merge pull request #42 from foo/bar", ""},
		{"missing parens", "fix: missing bead id in parens", ""},
		{"bracket instead of parens", "[spi-abc12] wrong format", ""},
		{"no prefix", "no prefix here", ""},
		{"empty string", "", ""},
		{"spaces inside parens", "feat(not a bead): invalid id chars", ""},
		{"uppercase prefix", "feat(SPI-ABC12): uppercase prefix", ""},
		{"malformed parens (missing close)", "feat(spi-abc12: missing close paren", ""},
		{"malformed parens (missing open)", "feat spi-abc12): missing open paren", ""},
		{"trailing whitespace after message", "feat(spi-abc12): message   ", "spi-abc12"},
		{"leading whitespace disqualifies", "  feat(spi-abc12): leading space", ""},
		{"missing colon", "feat(spi-abc12) no colon", ""},
		{"missing space after colon still matches", "feat(spi-abc12):nomessage", "spi-abc12"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := BeadIDFromSubject(tt.subject)
			if got != tt.want {
				t.Errorf("BeadIDFromSubject(%q) = %q, want %q", tt.subject, got, tt.want)
			}
		})
	}
}
