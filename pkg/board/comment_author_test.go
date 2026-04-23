package board

import "testing"

func TestParseCommentAuthor(t *testing.T) {
	cases := []struct {
		name      string
		in        string
		wantName  string
		wantEmail string
		wantOK    bool
	}{
		{
			name:      "canonical",
			in:        "JB <jb@example.com>",
			wantName:  "JB",
			wantEmail: "jb@example.com",
			wantOK:    true,
		},
		{
			name:   "bare name",
			in:     "JB",
			wantOK: false,
		},
		{
			name:   "bare agent-style name",
			in:     "wizard-spi-abc123",
			wantOK: false,
		},
		{
			name:   "empty email",
			in:     "JB <>",
			wantOK: false,
		},
		{
			name:      "nested angle brackets — non-greedy name, greedy email",
			in:        "A<B> <a@b.com>",
			wantName:  "A<B>",
			wantEmail: "a@b.com",
			wantOK:    true,
		},
		{
			name:      "unicode name",
			in:        "山田 <yamada@example.com>",
			wantName:  "山田",
			wantEmail: "yamada@example.com",
			wantOK:    true,
		},
		{
			name:      "leading and trailing whitespace is trimmed",
			in:        "  JB <jb@example.com>  ",
			wantName:  "JB",
			wantEmail: "jb@example.com",
			wantOK:    true,
		},
		{
			name:      "multi-word name",
			in:        "Jane Doe <jane@example.com>",
			wantName:  "Jane Doe",
			wantEmail: "jane@example.com",
			wantOK:    true,
		},
		{
			name:   "empty string",
			in:     "",
			wantOK: false,
		},
		{
			name:   "only angle brackets",
			in:     "<a@b.com>",
			wantOK: false,
		},
		{
			name:   "missing closing bracket",
			in:     "JB <jb@example.com",
			wantOK: false,
		},
		{
			name:   "missing opening bracket",
			in:     "JB jb@example.com>",
			wantOK: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotName, gotEmail, gotOK := parseCommentAuthor(tc.in)
			if gotOK != tc.wantOK {
				t.Fatalf("parseCommentAuthor(%q) ok = %v, want %v", tc.in, gotOK, tc.wantOK)
			}
			if !tc.wantOK {
				return
			}
			if gotName != tc.wantName {
				t.Errorf("parseCommentAuthor(%q) name = %q, want %q", tc.in, gotName, tc.wantName)
			}
			if gotEmail != tc.wantEmail {
				t.Errorf("parseCommentAuthor(%q) email = %q, want %q", tc.in, gotEmail, tc.wantEmail)
			}
		})
	}
}
