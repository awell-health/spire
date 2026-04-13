package steward

import (
	"context"
	"sync"
	"time"
)

// MergeRequest represents a completed wizard's branch ready to merge.
type MergeRequest struct {
	BeadID     string
	AgentName  string
	Branch     string    // wizard's feature branch
	BaseBranch string    // target branch (usually "main")
	RepoPath   string    // local repo path for git operations
	EnqueuedAt time.Time
}

// MergeResult is the outcome of a merge attempt.
type MergeResult struct {
	BeadID  string
	Success bool
	Error   error
	SHA     string // merge commit SHA on success
}

// MergeQueue serializes merge operations to prevent git push contention.
// Thread-safe. The steward enqueues, then calls ProcessNext each cycle.
type MergeQueue struct {
	mu         sync.Mutex
	queue      []MergeRequest
	active     *MergeRequest // currently processing (nil if idle)
	processing bool          // true while mergeFn is executing
}

// NewMergeQueue creates an empty merge queue.
func NewMergeQueue() *MergeQueue {
	return &MergeQueue{
		queue: make([]MergeRequest, 0),
	}
}

// Enqueue adds a merge request to the back of the queue.
func (mq *MergeQueue) Enqueue(req MergeRequest) {
	mq.mu.Lock()
	defer mq.mu.Unlock()
	mq.queue = append(mq.queue, req)
}

// Depth returns the number of pending requests (not including active).
func (mq *MergeQueue) Depth() int {
	mq.mu.Lock()
	defer mq.mu.Unlock()
	return len(mq.queue)
}

// Active returns the currently-processing request, or nil.
func (mq *MergeQueue) Active() *MergeRequest {
	mq.mu.Lock()
	defer mq.mu.Unlock()
	if mq.active == nil {
		return nil
	}
	cp := *mq.active
	return &cp
}

// Peek returns the next request without removing it, or nil if empty.
func (mq *MergeQueue) Peek() *MergeRequest {
	mq.mu.Lock()
	defer mq.mu.Unlock()
	if len(mq.queue) == 0 {
		return nil
	}
	r := mq.queue[0]
	return &r
}

// ProcessNext takes the next request from the queue and executes the
// merge sequence: git fetch, rebase onto base branch, run tests (optional),
// push. Returns nil if queue is empty. Sets active during processing.
//
// The mergeFn callback performs the actual git operations. This allows
// the steward to inject the real git commands while tests can use mocks.
// Signature: func(ctx context.Context, req MergeRequest) MergeResult
func (mq *MergeQueue) ProcessNext(ctx context.Context, mergeFn func(context.Context, MergeRequest) MergeResult) *MergeResult {
	mq.mu.Lock()
	if mq.processing || len(mq.queue) == 0 {
		mq.mu.Unlock()
		return nil
	}
	mq.processing = true
	req := mq.queue[0]
	mq.queue = mq.queue[1:]
	mq.active = &req
	mq.mu.Unlock()

	result := mergeFn(ctx, req)

	mq.mu.Lock()
	mq.active = nil
	mq.processing = false
	mq.mu.Unlock()

	return &result
}

// Remove removes a specific bead's request from the queue (e.g., on cancellation).
// Returns true if a request was found and removed.
func (mq *MergeQueue) Remove(beadID string) bool {
	mq.mu.Lock()
	defer mq.mu.Unlock()
	for i, req := range mq.queue {
		if req.BeadID == beadID {
			mq.queue = append(mq.queue[:i], mq.queue[i+1:]...)
			return true
		}
	}
	return false
}
