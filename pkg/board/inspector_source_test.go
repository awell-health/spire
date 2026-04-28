package board

import (
	"context"
	"strings"
	"testing"

	"github.com/awell-health/spire/pkg/board/logstream"
	"github.com/awell-health/spire/pkg/store"
)

// fakeLogSource is an in-memory logstream.Source for inspector tests.
// Returning a fixed slice + error lets a test cover empty / error /
// populated states without touching the filesystem.
type fakeLogSource struct {
	artifacts []logstream.Artifact
	err       error
}

func (f *fakeLogSource) List(_ context.Context, _ string) ([]logstream.Artifact, error) {
	return f.artifacts, f.err
}

// TestInspectorEmptyState_LogsTabRendersFriendlyMessage covers the
// spec's requirement that an empty Logs tab shows an explicit "no log
// artifacts yet" message rather than a blank panel. The copy must
// include the bead ID so users can tell the panel is intentionally
// empty rather than broken.
func TestInspectorEmptyState_LogsTabRendersFriendlyMessage(t *testing.T) {
	prev := logSourceFactory
	defer SetLogSourceFactory(prev)
	SetLogSourceFactory(func() logstream.Source { return &fakeLogSource{} })

	bead := store.BoardBead{ID: "spi-empty", Status: "in_progress", Type: "task"}
	data := InspectorData{Bead: bead}
	out := renderInspectorSnap(bead, &data, nil, 100, 200, 0,
		InspectorTabLogs, 0, LogModePretty, false, false)

	if !strings.Contains(out, "No log artifacts yet for spi-empty") {
		t.Fatalf("empty Logs tab missing friendly message; got:\n%s", out)
	}
	if !strings.Contains(out, "(logs appear here once an agent has produced output)") {
		t.Fatalf("empty Logs tab missing helper hint; got:\n%s", out)
	}
}

// TestArtifactsToLogViews_RoundTrip verifies the conversion that
// inspector.go uses to turn Source-produced Artifacts into LogViews:
// adapter parsing on Provider-bearing artifacts, KindStderr synthesis
// for sidecar lines, and pass-through for Path/StderrPath so cycle
// tagging still works.
func TestArtifactsToLogViews_RoundTrip(t *testing.T) {
	arts := []logstream.Artifact{
		{
			Name:     "wizard",
			Path:     "/tmp/wizard.log",
			Content:  "operational line\n",
			Provider: "",
		},
		{
			Name:     "implement (17:34)",
			Path:     "/tmp/implement.jsonl",
			Provider: "claude",
			Content:  `{"type":"system","subtype":"init","session_id":"x"}` + "\n",
			StderrPath:    "/tmp/implement.stderr.log",
			StderrContent: "stderr line A\nstderr line B\n",
		},
	}
	views := artifactsToLogViews(arts)
	if len(views) != 2 {
		t.Fatalf("got %d views, want 2", len(views))
	}

	// Operational artifact: adapter not invoked, no events.
	if views[0].Provider != "" {
		t.Errorf("views[0].Provider = %q, want empty", views[0].Provider)
	}
	if len(views[0].Events) != 0 {
		t.Errorf("views[0] should have no events; got %d", len(views[0].Events))
	}

	// Provider artifact: events parsed, plus 2 stderr lines from sidecar.
	if views[1].Provider != "claude" {
		t.Errorf("views[1].Provider = %q, want claude", views[1].Provider)
	}
	if len(views[1].Events) == 0 {
		t.Fatalf("views[1].Events is empty; expected adapter to parse claude transcript")
	}
	var stderrCount int
	for _, ev := range views[1].Events {
		if ev.Kind == logstream.KindStderr {
			stderrCount++
		}
	}
	if stderrCount != 2 {
		t.Errorf("expected 2 KindStderr events from sidecar, got %d", stderrCount)
	}
}

// TestSetLogSourceFactory_ResetsToDefault confirms that passing nil to
// SetLogSourceFactory restores the default local-source constructor.
// Tests that swap the factory rely on this so they can leave the
// global in a clean state for the next test.
func TestSetLogSourceFactory_ResetsToDefault(t *testing.T) {
	original := logSourceFactory
	SetLogSourceFactory(func() logstream.Source { return &fakeLogSource{} })
	if _, ok := logSourceFactory().(*fakeLogSource); !ok {
		t.Fatalf("custom factory did not install")
	}
	SetLogSourceFactory(nil)
	if _, ok := logSourceFactory().(*logstream.LocalSource); !ok {
		t.Fatalf("nil factory did not restore the default LocalSource")
	}
	logSourceFactory = original
}
