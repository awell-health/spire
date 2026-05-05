//go:build linux

package pool

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"golang.org/x/sys/unix"
)

// InotifyPoolWake implements PoolWake using inotify on per-pool wake
// files under stateDir. Broadcast writes-then-syncs-then-closes the
// file; Wait watches IN_CLOSE_WRITE on the same file.
type InotifyPoolWake struct {
	stateDir string
}

// newInotifyPoolWake creates the state directory if missing and
// returns an InotifyPoolWake rooted at it.
func newInotifyPoolWake(stateDir string) (PoolWake, error) {
	if stateDir == "" {
		return nil, errors.New("inotify pool wake: empty stateDir")
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, fmt.Errorf("inotify pool wake: mkdir stateDir: %w", err)
	}
	return &InotifyPoolWake{stateDir: stateDir}, nil
}

func (w *InotifyPoolWake) wakeFilePath(pool string) string {
	return filepath.Join(w.stateDir, ".wake-"+pool)
}

func (w *InotifyPoolWake) ensureWakeFile(pool string) error {
	f, err := os.OpenFile(w.wakeFilePath(pool), os.O_RDONLY|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	return f.Close()
}

// Broadcast opens the wake file for write, writes one byte, fsyncs,
// and closes — generating an IN_CLOSE_WRITE event observed by Waiters.
func (w *InotifyPoolWake) Broadcast(pool string) error {
	f, err := os.OpenFile(w.wakeFilePath(pool), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("inotify pool wake: open wake file: %w", err)
	}
	if _, err := f.Write([]byte{'.'}); err != nil {
		_ = f.Close()
		return fmt.Errorf("inotify pool wake: write wake file: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("inotify pool wake: fsync wake file: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("inotify pool wake: close wake file: %w", err)
	}
	return nil
}

// Wait blocks until an IN_CLOSE_WRITE event is observed on the wake
// file for pool, or ctx is cancelled. On ctx cancellation the inotify
// fd is closed to unblock the read goroutine.
func (w *InotifyPoolWake) Wait(ctx context.Context, pool string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := w.ensureWakeFile(pool); err != nil {
		return fmt.Errorf("inotify pool wake: ensure wake file: %w", err)
	}

	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC)
	if err != nil {
		return fmt.Errorf("inotify pool wake: init: %w", err)
	}

	if _, err := unix.InotifyAddWatch(fd, w.wakeFilePath(pool), unix.IN_CLOSE_WRITE); err != nil {
		_ = unix.Close(fd)
		return fmt.Errorf("inotify pool wake: add watch: %w", err)
	}

	type readResult struct {
		ok  bool
		err error
	}
	rch := make(chan readResult, 1)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		buf := make([]byte, 4096)
		n, rerr := unix.Read(fd, buf)
		if rerr != nil {
			rch <- readResult{err: rerr}
			return
		}
		rch <- readResult{ok: n > 0}
	}()

	var result error
	select {
	case r := <-rch:
		if r.err != nil {
			if errors.Is(r.err, unix.EBADF) || errors.Is(r.err, unix.EINTR) {
				if cerr := ctx.Err(); cerr != nil {
					result = cerr
				} else {
					result = r.err
				}
			} else {
				result = r.err
			}
		} else if !r.ok {
			result = errors.New("inotify pool wake: empty read")
		}
	case <-ctx.Done():
		result = ctx.Err()
	}

	_ = unix.Close(fd)
	wg.Wait()
	return result
}
