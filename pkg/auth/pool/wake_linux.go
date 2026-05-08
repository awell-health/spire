//go:build linux

package pool

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

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
// file for pool, or ctx is cancelled.
//
// The inotify fd is opened non-blocking and polled with a short
// timeout so ctx cancellation is detected without closing the fd from
// another goroutine — closing a descriptor that another thread is
// reading is undefined on Linux and was the source of a hang on
// ctx-cancel where the in-flight Read never returned.
func (w *InotifyPoolWake) Wait(ctx context.Context, pool string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := w.ensureWakeFile(pool); err != nil {
		return fmt.Errorf("inotify pool wake: ensure wake file: %w", err)
	}

	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		return fmt.Errorf("inotify pool wake: init: %w", err)
	}
	defer unix.Close(fd)

	if _, err := unix.InotifyAddWatch(fd, w.wakeFilePath(pool), unix.IN_CLOSE_WRITE); err != nil {
		return fmt.Errorf("inotify pool wake: add watch: %w", err)
	}

	pfds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
	buf := make([]byte, 4096)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		// 100ms poll timeout bounds cancellation latency without
		// burning CPU. Real wake events fire well within this.
		n, perr := unix.Poll(pfds, 100)
		if perr != nil {
			if errors.Is(perr, unix.EINTR) {
				continue
			}
			return fmt.Errorf("inotify pool wake: poll: %w", perr)
		}
		if n == 0 || pfds[0].Revents&unix.POLLIN == 0 {
			continue
		}
		_, rerr := unix.Read(fd, buf)
		if rerr != nil {
			if errors.Is(rerr, unix.EAGAIN) {
				continue
			}
			return rerr
		}
		return nil
	}
}
