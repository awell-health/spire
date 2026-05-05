package pool

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// WithExclusiveLock acquires an exclusive (writer) POSIX advisory lock on
// path, calls fn, then releases the lock. The file is created with 0600
// perms if missing. Blocks until the lock is granted. Returns fn's error,
// or any setup/teardown error if fn itself succeeded.
func WithExclusiveLock(path string, fn func() error) error {
	return withLock(path, unix.LOCK_EX, fn)
}

// WithSharedLock acquires a shared (reader) POSIX advisory lock on path,
// calls fn, then releases the lock. The file is created with 0600 perms
// if missing. Multiple shared holders may overlap; an exclusive holder
// blocks all shared holders and vice versa.
func WithSharedLock(path string, fn func() error) error {
	return withLock(path, unix.LOCK_SH, fn)
}

// withLock is the shared implementation behind WithExclusiveLock and
// WithSharedLock. It opens (or creates) path, acquires the lock with
// flock(2), runs fn, then unlocks and closes — in that order, so the
// unlock is observable to other holders before the fd is dropped. Errors
// from unlock/close never shadow fn's error.
func withLock(path string, how int, fn func() error) (err error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open lock file %s: %w", path, err)
	}
	defer func() {
		if cerr := f.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("close lock file %s: %w", path, cerr)
		}
	}()
	if err := unix.Flock(int(f.Fd()), how); err != nil {
		return fmt.Errorf("flock %s: %w", path, err)
	}
	defer func() {
		if uerr := unix.Flock(int(f.Fd()), unix.LOCK_UN); uerr != nil && err == nil {
			err = fmt.Errorf("flock unlock %s: %w", path, uerr)
		}
	}()
	return fn()
}
