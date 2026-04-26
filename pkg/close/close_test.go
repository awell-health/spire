package close

import (
	"errors"
	"testing"
)

// TestRunLifecycle_NotWired verifies the package-level error that callers
// see when cmd/spire hasn't booted (so RunFunc is still nil). The gateway
// handler test relies on this being a stable, package-level sentinel.
func TestRunLifecycle_NotWired(t *testing.T) {
	orig := RunFunc
	RunFunc = nil
	t.Cleanup(func() { RunFunc = orig })

	err := RunLifecycle("spi-test")
	if !errors.Is(err, ErrNotWired) {
		t.Errorf("RunLifecycle with nil RunFunc: err = %v, want ErrNotWired", err)
	}
}

// TestRunLifecycle_DelegatesToRunFunc verifies that RunLifecycle is a thin
// passthrough to RunFunc and propagates the bead ID unchanged. This is the
// contract cmd/spire's wiring depends on.
func TestRunLifecycle_DelegatesToRunFunc(t *testing.T) {
	orig := RunFunc
	t.Cleanup(func() { RunFunc = orig })

	var gotID string
	RunFunc = func(id string) error {
		gotID = id
		return nil
	}

	if err := RunLifecycle("spi-test"); err != nil {
		t.Fatalf("RunLifecycle: %v", err)
	}
	if gotID != "spi-test" {
		t.Errorf("forwarded id = %q, want spi-test", gotID)
	}
}

// TestRunLifecycle_PropagatesError verifies that RunLifecycle surfaces
// RunFunc errors verbatim. The gateway handler relies on the error message
// containing "not found" for 404 mapping.
func TestRunLifecycle_PropagatesError(t *testing.T) {
	orig := RunFunc
	t.Cleanup(func() { RunFunc = orig })

	wantErr := errors.New("get bead spi-missing: not found")
	RunFunc = func(string) error {
		return wantErr
	}

	if err := RunLifecycle("spi-missing"); err != wantErr {
		t.Errorf("err = %v, want %v", err, wantErr)
	}
}
