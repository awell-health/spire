package bundlestore

import (
	"errors"
	"strings"
	"testing"
)

func TestPutRequestValidate(t *testing.T) {
	const goodID = "spi-abc"

	cases := []struct {
		name string
		req  PutRequest
		want error
	}{
		// Accept: hierarchical and plain IDs.
		{"hierarchical-one-level", PutRequest{BeadID: "spi-5bzu9r.1", AttemptID: "spi-31i26o"}, nil},
		{"hierarchical-two-levels", PutRequest{BeadID: "spi-foo.1.2", AttemptID: goodID}, nil},
		{"all-letters-with-dots", PutRequest{BeadID: "a.b.c", AttemptID: goodID}, nil},
		{"plain-id", PutRequest{BeadID: "spi-5bzu9r", AttemptID: goodID}, nil},
		{"all-digit", PutRequest{BeadID: "12345", AttemptID: goodID}, nil},
		{"single-char", PutRequest{BeadID: "a", AttemptID: goodID}, nil},
		{"hierarchical-attempt-id", PutRequest{BeadID: goodID, AttemptID: "spi-foo.1.2"}, nil},
		{"max-length", PutRequest{BeadID: strings.Repeat("a", 64), AttemptID: goodID}, nil},
		{"apprentice-idx-positive", PutRequest{BeadID: goodID, AttemptID: goodID, ApprenticeIdx: 7}, nil},

		// Reject: path-traversal sequences.
		{"traversal-double-dot", PutRequest{BeadID: "..", AttemptID: goodID}, ErrInvalidRequest},
		{"traversal-embedded-double-dot", PutRequest{BeadID: "a..b", AttemptID: goodID}, ErrInvalidRequest},
		{"traversal-attempt-double-dot", PutRequest{BeadID: goodID, AttemptID: "a..b"}, ErrInvalidRequest},

		// Reject: leading / trailing / lone dot.
		{"single-dot", PutRequest{BeadID: ".", AttemptID: goodID}, ErrInvalidRequest},
		{"leading-dot", PutRequest{BeadID: ".foo", AttemptID: goodID}, ErrInvalidRequest},
		{"trailing-dot", PutRequest{BeadID: "foo.", AttemptID: goodID}, ErrInvalidRequest},
		{"leading-dot-attempt", PutRequest{BeadID: goodID, AttemptID: ".foo"}, ErrInvalidRequest},
		{"trailing-dot-attempt", PutRequest{BeadID: goodID, AttemptID: "foo."}, ErrInvalidRequest},

		// Reject: separators and other path-injection chars.
		{"forward-slash", PutRequest{BeadID: "foo/bar", AttemptID: goodID}, ErrInvalidRequest},
		{"backslash", PutRequest{BeadID: "foo\\bar", AttemptID: goodID}, ErrInvalidRequest},
		{"forward-slash-attempt", PutRequest{BeadID: goodID, AttemptID: "foo/bar"}, ErrInvalidRequest},

		// Reject: empty / oversize / wrong case.
		{"empty-bead-id", PutRequest{BeadID: "", AttemptID: goodID}, ErrInvalidRequest},
		{"empty-attempt-id", PutRequest{BeadID: goodID, AttemptID: ""}, ErrInvalidRequest},
		{"too-long", PutRequest{BeadID: strings.Repeat("a", 65), AttemptID: goodID}, ErrInvalidRequest},
		{"uppercase", PutRequest{BeadID: "Foo", AttemptID: goodID}, ErrInvalidRequest},

		// Reject: negative apprentice index even with valid IDs.
		{"negative-apprentice-idx", PutRequest{BeadID: goodID, AttemptID: goodID, ApprenticeIdx: -1}, ErrInvalidRequest},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.req.Validate()
			if tc.want == nil {
				if got != nil {
					t.Fatalf("Validate(%+v) = %v, want nil", tc.req, got)
				}
				return
			}
			if !errors.Is(got, tc.want) {
				t.Fatalf("Validate(%+v) = %v, want %v", tc.req, got, tc.want)
			}
		})
	}
}
