package process

import (
	"fmt"
	"os"
	"syscall"
)

// FileLock is an exclusive advisory lock on a file. Only one process
// can hold the lock at a time. Used to prevent multiple spire up/daemon
// instances from racing.
type FileLock struct {
	path string
	file *os.File
}

// AcquireLock tries to take an exclusive lock on the given path.
// Non-blocking: returns immediately with an error if another process holds it.
// The lock file is created if it doesn't exist.
func AcquireLock(path string) (*FileLock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("cannot open lock file %s: %w", path, err)
	}
	err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err != nil {
		f.Close()
		if err == syscall.EWOULDBLOCK {
			return nil, fmt.Errorf("another instance is running (lock: %s)", path)
		}
		return nil, fmt.Errorf("flock %s: %w", path, err)
	}
	return &FileLock{path: path, file: f}, nil
}

// Release releases the lock and closes the file.
func (fl *FileLock) Release() error {
	if fl.file == nil {
		return nil
	}
	err := syscall.Flock(int(fl.file.Fd()), syscall.LOCK_UN)
	closeErr := fl.file.Close()
	fl.file = nil
	if err != nil {
		return err
	}
	return closeErr
}

// IsLocked checks if the given path is currently locked by another process.
// Does not acquire the lock. Returns false if the file doesn't exist.
func IsLocked(path string) bool {
	f, err := os.OpenFile(path, os.O_RDWR, 0600)
	if err != nil {
		return false
	}
	defer f.Close()
	err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err != nil {
		return true
	}
	syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return false
}
