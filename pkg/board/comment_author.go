package board

import (
	"regexp"
	"strings"

	"charm.land/lipgloss/v2"
)

// commentAuthorRe parses the "Name <email>" author format written by
// `spire comment`. The name capture is non-greedy and the email capture is
// greedy up to the trailing '>', so nested brackets in the name portion
// (e.g. "A<B> <a@b.com>") resolve to name="A<B>", email="a@b.com".
var commentAuthorRe = regexp.MustCompile(`^(.+?)\s+<(.+)>$`)

// parseCommentAuthor extracts the name and email from a "Name <email>"
// author string. Returns ok=false on parse miss (bare names, empty email,
// malformed input) so the caller can fall back to rendering the raw string.
func parseCommentAuthor(s string) (name, email string, ok bool) {
	s = strings.TrimSpace(s)
	m := commentAuthorRe.FindStringSubmatch(s)
	if m == nil {
		return "", "", false
	}
	return m[1], m[2], true
}

// renderCommentAuthor returns the inspector's display string for a comment
// author. On parse match it renders the name in bold with the email dimmed;
// on miss it renders the raw string bold so legacy bare-name authors
// ("JB", "spire", "wizard-…") continue to work unchanged.
func renderCommentAuthor(author string) string {
	bold := lipgloss.NewStyle().Bold(true)
	if author == "" {
		return bold.Render("unknown")
	}
	name, email, ok := parseCommentAuthor(author)
	if !ok {
		return bold.Render(author)
	}
	dim := lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	return bold.Render(name) + " " + dim.Render("<"+email+">")
}
