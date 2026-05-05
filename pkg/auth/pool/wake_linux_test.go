//go:build linux

package pool

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestInotifyPoolWake_WaitWakesOnBroadcast(t *testing.T) {
	dir := t.TempDir()
	w, err := newInotifyPoolWake(dir)
	if err != nil {
		t.Fatalf("newInotifyPoolWake: %v", err)
	}

	woken := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		woken <- w.Wait(ctx, "p")
	}()

	// Give the waiter a moment to set up the inotify watch.
	time.Sleep(100 * time.Millisecond)

	if err := w.Broadcast("p"); err != nil {
		t.Fatalf("Broadcast: %v", err)
	}

	select {
	case err := <-woken:
		if err != nil {
			t.Fatalf("Wait returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("inotify waiter did not wake within 2s of broadcast")
	}
}

func TestInotifyPoolWake_CtxCancelReturnsErr(t *testing.T) {
	dir := t.TempDir()
	w, err := newInotifyPoolWake(dir)
	if err != nil {
		t.Fatalf("newInotifyPoolWake: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- w.Wait(ctx, "p")
	}()

	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("expected context.Canceled, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("inotify Wait did not return after ctx cancel")
	}
}
