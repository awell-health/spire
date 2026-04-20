package git

import "regexp"

// beadIDFromCommitSubject matches the project convention `<type>(<bead-id>):`
// at the start of a commit subject (e.g. "feat(spi-abc12):", "fix(web-9xx.1):").
// Bead IDs are prefix-lowercase-dash-hex-dot-digits per the `spi-<hex>` scheme
// documented in CLAUDE.md.
var beadIDFromCommitSubject = regexp.MustCompile(`^[a-z]+\(([a-z]+-[a-z0-9]+(?:\.\d+)*)\):`)

// BeadIDFromSubject returns the bead ID from a commit subject of the form
// `<type>(<bead-id>): <message>`, or "" if the subject doesn't match the
// convention.
func BeadIDFromSubject(subject string) string {
	m := beadIDFromCommitSubject.FindStringSubmatch(subject)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}
