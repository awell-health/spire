package process

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAcquireLock_Success(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.lock")

	lock, err := AcquireLock(path)
	if err != nil {
		t.Fatalf("AcquireLock failed: %v", err)
	}
	defer lock.Release()

	// Lock file should have been created
	if _, err := os.Stat(path); os.IsNotExist(err) {
		t.Fatal("lock file was not created")
	}
}

func TestAcquireLock_FailsWhenAlreadyLocked(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.lock")

	lock1, err := AcquireLock(path)
	if err != nil {
		t.Fatalf("first AcquireLock failed: %v", err)
	}
	defer lock1.Release()

	// Second acquire should fail
	lock2, err := AcquireLock(path)
	if err == nil {
		lock2.Release()
		t.Fatal("second AcquireLock should have failed but succeeded")
	}
}

func TestRelease_AllowsReacquire(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.lock")

	lock1, err := AcquireLock(path)
	if err != nil {
		t.Fatalf("first AcquireLock failed: %v", err)
	}

	if err := lock1.Release(); err != nil {
		t.Fatalf("Release failed: %v", err)
	}

	// Should be able to re-acquire after release
	lock2, err := AcquireLock(path)
	if err != nil {
		t.Fatalf("second AcquireLock after release failed: %v", err)
	}
	defer lock2.Release()
}

func TestIsLocked(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.lock")

	// Not locked (file doesn't exist)
	if IsLocked(path) {
		t.Fatal("IsLocked should return false for nonexistent file")
	}

	lock, err := AcquireLock(path)
	if err != nil {
		t.Fatalf("AcquireLock failed: %v", err)
	}

	// Should be locked
	if !IsLocked(path) {
		t.Fatal("IsLocked should return true when lock is held")
	}

	lock.Release()

	// Should be unlocked after release
	if IsLocked(path) {
		t.Fatal("IsLocked should return false after release")
	}
}

func TestLockFileCreated(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "subdir", "test.lock")

	// Parent dir doesn't exist — should fail
	_, err := AcquireLock(path)
	if err == nil {
		t.Fatal("AcquireLock should fail when parent dir doesn't exist")
	}

	// Create parent dir and retry
	os.MkdirAll(filepath.Dir(path), 0700)
	lock, err := AcquireLock(path)
	if err != nil {
		t.Fatalf("AcquireLock failed after creating parent dir: %v", err)
	}
	defer lock.Release()

	if _, err := os.Stat(path); os.IsNotExist(err) {
		t.Fatal("lock file should exist after AcquireLock")
	}
}

func TestLockSurvivesUntilRelease(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.lock")

	lock, err := AcquireLock(path)
	if err != nil {
		t.Fatalf("AcquireLock failed: %v", err)
	}

	// Lock should still be held — second acquire must fail
	for i := 0; i < 3; i++ {
		_, err := AcquireLock(path)
		if err == nil {
			t.Fatalf("lock should still be held on attempt %d", i)
		}
	}

	lock.Release()

	// Now it should succeed
	lock2, err := AcquireLock(path)
	if err != nil {
		t.Fatalf("AcquireLock should succeed after release: %v", err)
	}
	lock2.Release()
}

func TestRelease_Idempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.lock")

	lock, err := AcquireLock(path)
	if err != nil {
		t.Fatalf("AcquireLock failed: %v", err)
	}

	if err := lock.Release(); err != nil {
		t.Fatalf("first Release failed: %v", err)
	}

	// Second release should be a no-op, not panic or error
	if err := lock.Release(); err != nil {
		t.Fatalf("second Release should be no-op but got: %v", err)
	}
}
