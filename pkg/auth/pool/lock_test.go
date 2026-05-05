package pool

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// envLockChild, when set to a path, makes the test binary act as a helper
// process: it acquires an exclusive lock at that path, prints "READY\n" to
// stdout, sleeps briefly, releases, and exits. The parent test launches the
// same binary with this env var set to verify cross-process flock semantics.
const envLockChild = "POOL_LOCK_CHILD"

// childHoldDuration is how long the helper process holds the lock before
// releasing. The parent test asserts its blocked-acquisition takes at least
// some fraction of this; keep it well above OS scheduling jitter.
const childHoldDuration = 500 * time.Millisecond

func TestMain(m *testing.M) {
	if path := os.Getenv(envLockChild); path != "" {
		runLockChild(path)
		return
	}
	os.Exit(m.Run())
}

func runLockChild(path string) {
	err := WithExclusiveLock(path, func() error {
		// fmt.Println on os.Stdout writes directly to the pipe fd in one
		// syscall — no Sync needed (and Sync errors on pipes anyway).
		if _, err := fmt.Println("READY"); err != nil {
			return err
		}
		time.Sleep(childHoldDuration)
		return nil
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

func TestExclusiveBlocksExclusive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock")

	held := make(chan struct{})
	release := make(chan struct{})

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := WithExclusiveLock(path, func() error {
			close(held)
			<-release
			return nil
		}); err != nil {
			t.Errorf("first exclusive lock: %v", err)
		}
	}()
	<-held

	acquired := make(chan struct{})
	go func() {
		if err := WithExclusiveLock(path, func() error { return nil }); err != nil {
			t.Errorf("second exclusive lock: %v", err)
		}
		close(acquired)
	}()

	select {
	case <-acquired:
		t.Fatal("second exclusive lock acquired while first still held")
	case <-time.After(150 * time.Millisecond):
	}

	close(release)

	select {
	case <-acquired:
	case <-time.After(2 * time.Second):
		t.Fatal("second exclusive lock never acquired after first released")
	}

	wg.Wait()
}

func TestSharedAllowsShared(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock")

	firstHeld := make(chan struct{})
	bothHeld := make(chan struct{})

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		err := WithSharedLock(path, func() error {
			close(firstHeld)
			select {
			case <-bothHeld:
			case <-time.After(2 * time.Second):
				return fmt.Errorf("timed out waiting for second shared lock")
			}
			return nil
		})
		if err != nil {
			t.Errorf("first shared lock: %v", err)
		}
	}()
	<-firstHeld

	if err := WithSharedLock(path, func() error {
		close(bothHeld)
		return nil
	}); err != nil {
		t.Fatalf("second shared lock: %v", err)
	}

	wg.Wait()
}

func TestExclusiveBlocksAcrossProcesses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock")

	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), envLockChild+"="+path)
	cmd.Stderr = os.Stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}

	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		t.Fatalf("read READY from helper: %v", err)
	}
	if !strings.HasPrefix(line, "READY") {
		t.Fatalf("expected READY from helper, got %q", line)
	}

	start := time.Now()
	if err := WithExclusiveLock(path, func() error { return nil }); err != nil {
		t.Fatalf("parent exclusive lock: %v", err)
	}
	elapsed := time.Since(start)

	// Helper holds for childHoldDuration. We want to assert the parent
	// genuinely blocked, so require a substantial fraction of that —
	// 40% gives plenty of margin against OS scheduling jitter.
	min := childHoldDuration * 4 / 10
	if elapsed < min {
		t.Fatalf("parent acquired exclusive lock after only %v; expected >= %v (helper held for %v)", elapsed, min, childHoldDuration)
	}

	if err := cmd.Wait(); err != nil {
		t.Fatalf("helper exited with error: %v", err)
	}
}
