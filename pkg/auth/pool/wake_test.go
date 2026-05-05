package pool

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestLocalPoolWake_WaitWakesOnBroadcast(t *testing.T) {
	w := NewLocalPoolWake()

	woken := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		woken <- w.Wait(ctx, "subscription")
	}()

	// Give the waiter a moment to enter cond.Wait. This isn't strictly
	// required for correctness (Broadcast bumps gen unconditionally),
	// but it exercises the typical "waiter blocked first" path.
	time.Sleep(50 * time.Millisecond)

	if err := w.Broadcast("subscription"); err != nil {
		t.Fatalf("Broadcast: %v", err)
	}

	select {
	case err := <-woken:
		if err != nil {
			t.Fatalf("Wait returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waiter did not wake within 2s of broadcast")
	}
}

func TestLocalPoolWake_AllWaitersWakeOnOneBroadcast(t *testing.T) {
	w := NewLocalPoolWake()
	const n = 10

	var wg sync.WaitGroup
	wg.Add(n)
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := w.Wait(ctx, "p"); err != nil {
				errs <- err
			}
		}()
	}

	time.Sleep(100 * time.Millisecond)
	if err := w.Broadcast("p"); err != nil {
		t.Fatalf("Broadcast: %v", err)
	}

	allDone := make(chan struct{})
	go func() {
		wg.Wait()
		close(allDone)
	}()

	select {
	case <-allDone:
	case <-time.After(2 * time.Second):
		t.Fatal("not all waiters woke from a single broadcast")
	}
	close(errs)
	for err := range errs {
		t.Errorf("waiter error: %v", err)
	}
}

func TestLocalPoolWake_CtxCancelReturnsErr(t *testing.T) {
	w := NewLocalPoolWake()

	ctx, cancel := context.WithCancel(context.Background())

	errCh := make(chan error, 1)
	go func() {
		errCh <- w.Wait(ctx, "p")
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("expected context.Canceled, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Wait did not return after ctx cancel")
	}
}

func TestLocalPoolWake_AlreadyCancelledCtx(t *testing.T) {
	w := NewLocalPoolWake()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := w.Wait(ctx, "p")
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}

func TestLocalPoolWake_DoubleBroadcastSafe(t *testing.T) {
	w := NewLocalPoolWake()

	if err := w.Broadcast("p"); err != nil {
		t.Fatalf("first Broadcast: %v", err)
	}
	if err := w.Broadcast("p"); err != nil {
		t.Fatalf("second Broadcast: %v", err)
	}

	woken := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		woken <- w.Wait(ctx, "p")
	}()

	time.Sleep(50 * time.Millisecond)
	if err := w.Broadcast("p"); err != nil {
		t.Fatalf("third Broadcast: %v", err)
	}
	if err := w.Broadcast("p"); err != nil {
		t.Fatalf("fourth Broadcast: %v", err)
	}

	select {
	case err := <-woken:
		if err != nil {
			t.Fatalf("Wait returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waiter did not wake")
	}
}

func TestLocalPoolWake_PoolIsolation(t *testing.T) {
	w := NewLocalPoolWake()

	bDone := make(chan error, 1)
	bCtx, bCancel := context.WithCancel(context.Background())
	defer bCancel()
	go func() {
		bDone <- w.Wait(bCtx, "B")
	}()

	time.Sleep(50 * time.Millisecond)
	if err := w.Broadcast("A"); err != nil {
		t.Fatalf("Broadcast A: %v", err)
	}

	select {
	case err := <-bDone:
		t.Fatalf("waiter on pool B woke from broadcast on pool A (err=%v)", err)
	case <-time.After(150 * time.Millisecond):
		// expected: B is still blocked
	}

	bCancel()
	select {
	case <-bDone:
	case <-time.After(2 * time.Second):
		t.Fatal("waiter on pool B did not exit after ctx cancel")
	}
}

func TestNewPoolWake_DefaultsToLocal(t *testing.T) {
	t.Setenv("SPIRE_POOL_WAKE", "")
	w := NewPoolWake(t.TempDir())
	if _, ok := w.(*LocalPoolWake); !ok {
		t.Errorf("expected *LocalPoolWake, got %T", w)
	}
}

func TestNewPoolWake_UnknownValueFallsBackToLocal(t *testing.T) {
	t.Setenv("SPIRE_POOL_WAKE", "bogus")
	w := NewPoolWake(t.TempDir())
	if _, ok := w.(*LocalPoolWake); !ok {
		t.Errorf("expected *LocalPoolWake, got %T", w)
	}
}
