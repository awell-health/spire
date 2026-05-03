package gateway

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/awell-health/spire/pkg/store"
)

func TestLegacyStatusFor(t *testing.T) {
	cases := []struct {
		status string
		want   string
	}{
		// Landing-3 statuses collapse to their pre-Landing-3 buckets.
		{"awaiting_review", "in_progress"},
		{"needs_changes", "in_progress"},
		{"merge_pending", "in_progress"},
		{"awaiting_human", "open"},

		// Pre-Landing-3 statuses are emitted unchanged so existing
		// consumers continue to read the same value off the
		// legacy_status field.
		{"open", "open"},
		{"ready", "ready"},
		{"in_progress", "in_progress"},
		{"dispatched", "dispatched"},
		{"blocked", "blocked"},
		{"deferred", "deferred"},
		{"closed", "closed"},

		// Empty / unknown values pass through unchanged — the shim is
		// not in the business of validating status taxonomy.
		{"", ""},
		{"some-other-string", "some-other-string"},
	}

	for _, c := range cases {
		t.Run(c.status, func(t *testing.T) {
			got := LegacyStatusFor(c.status)
			if got != c.want {
				t.Errorf("LegacyStatusFor(%q) = %q, want %q", c.status, got, c.want)
			}
		})
	}
}

func TestWrapBead_EmitsLegacyStatusAlongsideStatus(t *testing.T) {
	b := store.Bead{
		ID:     "spi-test1",
		Title:  "review work",
		Status: "awaiting_review",
		Type:   "task",
	}

	resp := wrapBead(b)
	if resp.Status != "awaiting_review" {
		t.Fatalf("wrapBead preserved Status incorrectly: got %q want %q", resp.Status, "awaiting_review")
	}
	if resp.LegacyStatus != "in_progress" {
		t.Fatalf("wrapBead.LegacyStatus = %q, want %q", resp.LegacyStatus, "in_progress")
	}

	// The marshalled JSON must carry both fields so external consumers
	// can pick whichever they speak: new-taxonomy clients read `status`,
	// legacy clients read `legacy_status`.
	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal beadResponse: %v", err)
	}
	body := string(data)
	if !strings.Contains(body, `"status":"awaiting_review"`) {
		t.Errorf("expected new status in JSON, got: %s", body)
	}
	if !strings.Contains(body, `"legacy_status":"in_progress"`) {
		t.Errorf("expected legacy_status in JSON, got: %s", body)
	}

	// And no `id`/`title` regression — embedding store.Bead must keep
	// every existing field reachable via the same JSON tags.
	if !strings.Contains(body, `"id":"spi-test1"`) {
		t.Errorf("expected id field in JSON, got: %s", body)
	}
}

func TestWrapBeads_PerStatusLegacyMapping(t *testing.T) {
	in := []store.Bead{
		{ID: "spi-a", Status: "awaiting_review"},
		{ID: "spi-b", Status: "needs_changes"},
		{ID: "spi-c", Status: "merge_pending"},
		{ID: "spi-d", Status: "awaiting_human"},
		{ID: "spi-e", Status: "in_progress"},
		{ID: "spi-f", Status: "open"},
		{ID: "spi-g", Status: "closed"},
	}
	wantLegacy := map[string]string{
		"spi-a": "in_progress",
		"spi-b": "in_progress",
		"spi-c": "in_progress",
		"spi-d": "open",
		"spi-e": "in_progress",
		"spi-f": "open",
		"spi-g": "closed",
	}

	got := wrapBeads(in)
	if len(got) != len(in) {
		t.Fatalf("wrapBeads len = %d, want %d", len(got), len(in))
	}
	for _, r := range got {
		want := wantLegacy[r.ID]
		if r.LegacyStatus != want {
			t.Errorf("wrapBeads[%s].LegacyStatus = %q, want %q", r.ID, r.LegacyStatus, want)
		}
	}
}

func TestWrapBeadPtr_NilSafe(t *testing.T) {
	if got := wrapBeadPtr(nil); got != nil {
		t.Fatalf("wrapBeadPtr(nil) = %v, want nil", got)
	}

	b := &store.Bead{ID: "spi-x", Status: "merge_pending"}
	got := wrapBeadPtr(b)
	if got == nil {
		t.Fatalf("wrapBeadPtr(non-nil) returned nil")
	}
	if got.LegacyStatus != "in_progress" {
		t.Errorf("wrapBeadPtr.LegacyStatus = %q, want %q", got.LegacyStatus, "in_progress")
	}
	if got.ID != "spi-x" {
		t.Errorf("wrapBeadPtr did not preserve embedded fields")
	}
}
