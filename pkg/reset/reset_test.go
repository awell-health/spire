package reset

import (
	"context"
	"errors"
	"testing"

	"github.com/awell-health/spire/pkg/store"
)

// TestResetBead_NotWired verifies the package-level error that callers see
// when cmd/spire hasn't booted (so RunFunc is still nil). The gateway
// handler test relies on this being a stable, package-level sentinel.
func TestResetBead_NotWired(t *testing.T) {
	orig := RunFunc
	RunFunc = nil
	t.Cleanup(func() { RunFunc = orig })

	bead, err := ResetBead(context.Background(), Opts{BeadID: "spi-test"})
	if !errors.Is(err, ErrNotWired) {
		t.Errorf("ResetBead with nil RunFunc: err = %v, want ErrNotWired", err)
	}
	if bead != nil {
		t.Errorf("ResetBead with nil RunFunc: bead = %v, want nil", bead)
	}
}

// TestResetBead_DelegatesToRunFunc verifies that ResetBead is a thin
// passthrough to RunFunc and propagates Opts unchanged. This is the
// contract cmd/spire's wiring depends on.
func TestResetBead_DelegatesToRunFunc(t *testing.T) {
	orig := RunFunc
	t.Cleanup(func() { RunFunc = orig })

	want := &store.Bead{ID: "spi-test", Status: "open"}
	var gotOpts Opts
	RunFunc = func(_ context.Context, opts Opts) (*store.Bead, error) {
		gotOpts = opts
		return want, nil
	}

	in := Opts{
		BeadID: "spi-test",
		To:     "implement",
		Force:  true,
		Set:    map[string]string{"implement.outputs.outcome": "verified"},
	}
	got, err := ResetBead(context.Background(), in)
	if err != nil {
		t.Fatalf("ResetBead: %v", err)
	}
	if got != want {
		t.Errorf("returned bead = %v, want %v", got, want)
	}
	if gotOpts.BeadID != in.BeadID || gotOpts.To != in.To || gotOpts.Force != in.Force {
		t.Errorf("opts not propagated: got %+v, want %+v", gotOpts, in)
	}
	if got, want := gotOpts.Set["implement.outputs.outcome"], "verified"; got != want {
		t.Errorf("opts.Set[implement.outputs.outcome] = %q, want %q", got, want)
	}
}

// TestResetBead_PropagatesError verifies that ResetBead surfaces RunFunc
// errors verbatim. The gateway handler relies on the error message
// containing "not found" for 404 mapping.
func TestResetBead_PropagatesError(t *testing.T) {
	orig := RunFunc
	t.Cleanup(func() { RunFunc = orig })

	wantErr := errors.New("get bead spi-missing: not found")
	RunFunc = func(_ context.Context, _ Opts) (*store.Bead, error) {
		return nil, wantErr
	}

	bead, err := ResetBead(context.Background(), Opts{BeadID: "spi-missing"})
	if err != wantErr {
		t.Errorf("err = %v, want %v", err, wantErr)
	}
	if bead != nil {
		t.Errorf("bead = %v, want nil", bead)
	}
}
