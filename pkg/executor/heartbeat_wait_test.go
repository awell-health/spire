package executor

import (
	"os"
	"testing"
	"time"
)

type blockingHeartbeatHandle struct {
	release <-chan struct{}
	waitErr error
}

func (h *blockingHeartbeatHandle) Wait() error {
	<-h.release
	return h.waitErr
}

func (h *blockingHeartbeatHandle) Signal(os.Signal) error { return nil }
func (h *blockingHeartbeatHandle) Alive() bool            { return true }
func (h *blockingHeartbeatHandle) Name() string           { return "blocking-heartbeat" }
func (h *blockingHeartbeatHandle) Identifier() string     { return "blocking-heartbeat" }

func TestWaitHandleWithHeartbeat_TicksWhileBlocked(t *testing.T) {
	deps, _ := testGraphDeps(t)

	var heartbeats int
	deps.UpdateAttemptHeartbeat = func(attemptID string) error {
		heartbeats++
		return nil
	}

	exec := NewGraphForTest("spi-test", "wizard-test", nil, &GraphState{}, deps)
	exec.graphState.AttemptBeadID = "attempt-1"

	origInterval := attemptHeartbeatInterval
	attemptHeartbeatInterval = 5 * time.Millisecond
	defer func() { attemptHeartbeatInterval = origInterval }()

	release := make(chan struct{})
	go func() {
		time.Sleep(18 * time.Millisecond)
		close(release)
	}()

	err := exec.waitHandleWithHeartbeat(&blockingHeartbeatHandle{release: release})
	if err != nil {
		t.Fatalf("waitHandleWithHeartbeat returned error: %v", err)
	}

	if heartbeats < 2 {
		t.Fatalf("expected at least 2 heartbeat writes while blocked, got %d", heartbeats)
	}
}
