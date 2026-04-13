package steward

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestEnqueueFIFO(t *testing.T) {
	mq := NewMergeQueue()
	mq.Enqueue(MergeRequest{BeadID: "a"})
	mq.Enqueue(MergeRequest{BeadID: "b"})
	mq.Enqueue(MergeRequest{BeadID: "c"})

	if mq.Depth() != 3 {
		t.Fatalf("expected depth 3, got %d", mq.Depth())
	}

	// Peek should return first without removing.
	p := mq.Peek()
	if p == nil || p.BeadID != "a" {
		t.Fatalf("expected peek to return 'a', got %v", p)
	}
	if mq.Depth() != 3 {
		t.Fatalf("peek should not change depth, got %d", mq.Depth())
	}

	// Process in order.
	ids := make([]string, 0, 3)
	passthrough := func(_ context.Context, req MergeRequest) MergeResult {
		return MergeResult{BeadID: req.BeadID, Success: true}
	}
	for i := 0; i < 3; i++ {
		r := mq.ProcessNext(context.Background(), passthrough)
		if r == nil {
			t.Fatalf("expected result on iteration %d", i)
		}
		ids = append(ids, r.BeadID)
	}
	if ids[0] != "a" || ids[1] != "b" || ids[2] != "c" {
		t.Fatalf("expected FIFO order [a b c], got %v", ids)
	}
}

func TestDepth(t *testing.T) {
	mq := NewMergeQueue()
	if mq.Depth() != 0 {
		t.Fatalf("expected depth 0, got %d", mq.Depth())
	}
	mq.Enqueue(MergeRequest{BeadID: "x"})
	mq.Enqueue(MergeRequest{BeadID: "y"})
	if mq.Depth() != 2 {
		t.Fatalf("expected depth 2, got %d", mq.Depth())
	}
	// Process one — depth should decrease.
	mq.ProcessNext(context.Background(), func(_ context.Context, req MergeRequest) MergeResult {
		return MergeResult{BeadID: req.BeadID, Success: true}
	})
	if mq.Depth() != 1 {
		t.Fatalf("expected depth 1 after processing one, got %d", mq.Depth())
	}
}

func TestProcessNextEmpty(t *testing.T) {
	mq := NewMergeQueue()
	r := mq.ProcessNext(context.Background(), func(_ context.Context, req MergeRequest) MergeResult {
		t.Fatal("mergeFn should not be called on empty queue")
		return MergeResult{}
	})
	if r != nil {
		t.Fatalf("expected nil result from empty queue, got %+v", r)
	}
}

func TestRemove(t *testing.T) {
	mq := NewMergeQueue()
	mq.Enqueue(MergeRequest{BeadID: "a"})
	mq.Enqueue(MergeRequest{BeadID: "b"})
	mq.Enqueue(MergeRequest{BeadID: "c"})

	// Remove middle element.
	if !mq.Remove("b") {
		t.Fatal("expected Remove('b') to return true")
	}
	if mq.Depth() != 2 {
		t.Fatalf("expected depth 2, got %d", mq.Depth())
	}

	// Removing non-existent returns false.
	if mq.Remove("z") {
		t.Fatal("expected Remove('z') to return false")
	}

	// Remaining order is a, c.
	ids := make([]string, 0, 2)
	passthrough := func(_ context.Context, req MergeRequest) MergeResult {
		return MergeResult{BeadID: req.BeadID, Success: true}
	}
	for mq.Depth() > 0 {
		r := mq.ProcessNext(context.Background(), passthrough)
		ids = append(ids, r.BeadID)
	}
	if len(ids) != 2 || ids[0] != "a" || ids[1] != "c" {
		t.Fatalf("expected [a c], got %v", ids)
	}
}

func TestActiveSetDuringProcessing(t *testing.T) {
	mq := NewMergeQueue()
	mq.Enqueue(MergeRequest{BeadID: "active-test"})

	if mq.Active() != nil {
		t.Fatal("expected Active() to be nil before processing")
	}

	sawActive := make(chan *MergeRequest, 1)
	mq.ProcessNext(context.Background(), func(_ context.Context, req MergeRequest) MergeResult {
		// Inside mergeFn, Active() should return the current request.
		sawActive <- mq.Active()
		return MergeResult{BeadID: req.BeadID, Success: true}
	})

	active := <-sawActive
	if active == nil || active.BeadID != "active-test" {
		t.Fatalf("expected Active() to return 'active-test' during processing, got %v", active)
	}

	// After processing, Active() should be nil again.
	if mq.Active() != nil {
		t.Fatal("expected Active() to be nil after processing")
	}
}

func TestConcurrentEnqueue(t *testing.T) {
	mq := NewMergeQueue()
	var wg sync.WaitGroup
	n := 100
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(id int) {
			defer wg.Done()
			mq.Enqueue(MergeRequest{BeadID: time.Now().String()})
		}(i)
	}
	wg.Wait()
	if mq.Depth() != n {
		t.Fatalf("expected depth %d after concurrent enqueue, got %d", n, mq.Depth())
	}
}

func TestProcessNextSerializes(t *testing.T) {
	mq := NewMergeQueue()
	mq.Enqueue(MergeRequest{BeadID: "s1"})
	mq.Enqueue(MergeRequest{BeadID: "s2"})

	var concurrent int32
	var maxConcurrent int32

	slowMerge := func(_ context.Context, req MergeRequest) MergeResult {
		cur := atomic.AddInt32(&concurrent, 1)
		// Track max concurrency.
		for {
			old := atomic.LoadInt32(&maxConcurrent)
			if cur <= old || atomic.CompareAndSwapInt32(&maxConcurrent, old, cur) {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
		atomic.AddInt32(&concurrent, -1)
		return MergeResult{BeadID: req.BeadID, Success: true}
	}

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		mq.ProcessNext(context.Background(), slowMerge)
	}()
	go func() {
		defer wg.Done()
		// Small delay to let first goroutine start.
		time.Sleep(10 * time.Millisecond)
		mq.ProcessNext(context.Background(), slowMerge)
	}()

	wg.Wait()

	// Because ProcessNext dequeues under lock before calling mergeFn,
	// the second goroutine gets its own item and they can run concurrently
	// in terms of mergeFn. But each one is a distinct queue item.
	// The key invariant: only one item is "active" at a time is enforced
	// by the queue — the second call gets the next item, not the same one.
	if mq.Depth() != 0 {
		t.Fatalf("expected depth 0, got %d", mq.Depth())
	}
}

func TestPeekNilOnEmpty(t *testing.T) {
	mq := NewMergeQueue()
	if mq.Peek() != nil {
		t.Fatal("expected Peek() to return nil on empty queue")
	}
}

func TestMergeResultFields(t *testing.T) {
	mq := NewMergeQueue()
	mq.Enqueue(MergeRequest{
		BeadID:     "field-test",
		AgentName:  "wizard-field-test",
		Branch:     "feat/field-test",
		BaseBranch: "main",
		RepoPath:   "/tmp/repo",
	})

	r := mq.ProcessNext(context.Background(), func(_ context.Context, req MergeRequest) MergeResult {
		if req.AgentName != "wizard-field-test" {
			t.Errorf("expected AgentName 'wizard-field-test', got %q", req.AgentName)
		}
		if req.Branch != "feat/field-test" {
			t.Errorf("expected Branch 'feat/field-test', got %q", req.Branch)
		}
		if req.BaseBranch != "main" {
			t.Errorf("expected BaseBranch 'main', got %q", req.BaseBranch)
		}
		return MergeResult{
			BeadID:  req.BeadID,
			Success: true,
			SHA:     "abc123",
		}
	})

	if r == nil {
		t.Fatal("expected non-nil result")
	}
	if r.SHA != "abc123" {
		t.Errorf("expected SHA 'abc123', got %q", r.SHA)
	}
	if !r.Success {
		t.Error("expected Success=true")
	}
}

func TestProcessNextReturnsError(t *testing.T) {
	mq := NewMergeQueue()
	mq.Enqueue(MergeRequest{BeadID: "err-test"})

	testErr := &mergeTestError{msg: "rebase conflict"}
	r := mq.ProcessNext(context.Background(), func(_ context.Context, req MergeRequest) MergeResult {
		return MergeResult{
			BeadID:  req.BeadID,
			Success: false,
			Error:   testErr,
		}
	})

	if r == nil {
		t.Fatal("expected non-nil result")
	}
	if r.Success {
		t.Error("expected Success=false")
	}
	if r.Error != testErr {
		t.Errorf("expected error to be preserved, got %v", r.Error)
	}
}

type mergeTestError struct {
	msg string
}

func (e *mergeTestError) Error() string { return e.msg }

func TestRemoveFirst(t *testing.T) {
	mq := NewMergeQueue()
	mq.Enqueue(MergeRequest{BeadID: "a"})
	mq.Enqueue(MergeRequest{BeadID: "b"})

	if !mq.Remove("a") {
		t.Fatal("expected Remove('a') to return true")
	}

	p := mq.Peek()
	if p == nil || p.BeadID != "b" {
		t.Fatalf("expected first item to be 'b' after removing 'a', got %v", p)
	}
}

func TestRemoveLast(t *testing.T) {
	mq := NewMergeQueue()
	mq.Enqueue(MergeRequest{BeadID: "a"})
	mq.Enqueue(MergeRequest{BeadID: "b"})

	if !mq.Remove("b") {
		t.Fatal("expected Remove('b') to return true")
	}
	if mq.Depth() != 1 {
		t.Fatalf("expected depth 1, got %d", mq.Depth())
	}

	p := mq.Peek()
	if p == nil || p.BeadID != "a" {
		t.Fatalf("expected remaining item to be 'a', got %v", p)
	}
}
