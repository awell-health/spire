package pool

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// slotStateExt is the suffix of per-slot state JSON files. ListSlotStates
// uses it to filter out lock files and any leftover atomic-write temps.
const slotStateExt = ".json"

// slotLockExt is the suffix of the sibling lock file used to serialize
// access to a slot's state JSON. We lock a sibling rather than the data
// file itself because atomic writes (temp + rename) replace the data
// file's inode, which would invalidate flock held against the previous
// inode and let concurrent writers each lock a different inode.
const slotLockExt = ".lock"

// SlotStatePath returns the canonical path of slotName's cached state
// JSON file under stateDir.
func SlotStatePath(stateDir, slotName string) string {
	return filepath.Join(stateDir, slotName+slotStateExt)
}

// slotLockPath returns the sibling lock file used to serialize access to
// slotName's state JSON.
func slotLockPath(stateDir, slotName string) string {
	return filepath.Join(stateDir, slotName+slotLockExt)
}

// ReadSlotState returns the cached state for slotName under stateDir. If
// the file does not exist, returns a zero-value &SlotState{Slot: slotName}
// with no error so callers can treat "missing" identically to "empty". A
// shared lock is held for the duration of the read, so concurrent readers
// don't block each other but writers do block readers.
func ReadSlotState(stateDir, slotName string) (*SlotState, error) {
	if err := ensureStateDir(stateDir); err != nil {
		return nil, err
	}
	var state *SlotState
	err := WithSharedLock(slotLockPath(stateDir, slotName), func() error {
		s, err := readSlotStateLocked(stateDir, slotName)
		if err != nil {
			return err
		}
		state = s
		return nil
	})
	if err != nil {
		return nil, err
	}
	return state, nil
}

// WriteSlotState atomically writes state to <stateDir>/<state.Slot>.json
// under an exclusive lock. The write goes to a temp file, fsync'd, closed,
// and renamed into place; perms are 0600.
func WriteSlotState(stateDir string, state *SlotState) error {
	if state == nil {
		return errors.New("pool.WriteSlotState: nil state")
	}
	if state.Slot == "" {
		return errors.New("pool.WriteSlotState: state.Slot is empty")
	}
	if err := ensureStateDir(stateDir); err != nil {
		return err
	}
	return WithExclusiveLock(slotLockPath(stateDir, state.Slot), func() error {
		return writeSlotStateLocked(stateDir, state)
	})
}

// MutateSlotState reads slotName's state (zero-value if missing), passes
// it to fn, and writes the result back if fn returns nil. The exclusive
// lock is held for the entire read-modify-write so concurrent callers
// cannot lose updates. If fn returns a non-nil error, the state is not
// written and that error is returned to the caller.
func MutateSlotState(stateDir, slotName string, fn func(*SlotState) error) error {
	if fn == nil {
		return errors.New("pool.MutateSlotState: nil fn")
	}
	if err := ensureStateDir(stateDir); err != nil {
		return err
	}
	return WithExclusiveLock(slotLockPath(stateDir, slotName), func() error {
		state, err := readSlotStateLocked(stateDir, slotName)
		if err != nil {
			return err
		}
		if err := fn(state); err != nil {
			return err
		}
		// Defensive: a caller may have cleared Slot inside fn.
		if state.Slot == "" {
			state.Slot = slotName
		}
		return writeSlotStateLocked(stateDir, state)
	})
}

// ListSlotStates returns every cached state JSON under stateDir keyed by
// slot name. A missing stateDir yields an empty map (no error). Each file
// is read under its own shared lock; lock files and partial temp files
// are ignored. Callers iterating to gather pool state should treat the
// snapshot as point-in-time — a concurrent writer may modify any slot
// after this returns.
func ListSlotStates(stateDir string) (map[string]*SlotState, error) {
	entries, err := os.ReadDir(stateDir)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]*SlotState{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("pool: read state dir %s: %w", stateDir, err)
	}
	out := make(map[string]*SlotState)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, slotStateExt) {
			continue
		}
		slotName := strings.TrimSuffix(name, slotStateExt)
		state, err := ReadSlotState(stateDir, slotName)
		if err != nil {
			return nil, err
		}
		out[slotName] = state
	}
	return out, nil
}

// readSlotStateLocked decodes the slot state JSON without acquiring a
// lock; callers must already hold a shared or exclusive flock on
// slotLockPath. Missing file is treated as a zero-value state.
func readSlotStateLocked(stateDir, slotName string) (*SlotState, error) {
	path := SlotStatePath(stateDir, slotName)
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return &SlotState{Slot: slotName}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("pool: read %s: %w", path, err)
	}
	state := &SlotState{}
	if err := json.Unmarshal(data, state); err != nil {
		return nil, fmt.Errorf("pool: decode %s: %w", path, err)
	}
	if state.Slot == "" {
		state.Slot = slotName
	}
	return state, nil
}

// writeSlotStateLocked atomically writes state to its canonical path
// without acquiring a lock; callers must already hold an exclusive flock
// on slotLockPath. The sequence is: write to a temp file in stateDir,
// chmod 0600, fsync, close, rename into place. Same-directory rename
// guarantees same-filesystem atomicity.
func writeSlotStateLocked(stateDir string, state *SlotState) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("pool: encode slot %q: %w", state.Slot, err)
	}

	finalPath := SlotStatePath(stateDir, state.Slot)
	tmp, err := os.CreateTemp(stateDir, state.Slot+".*.tmp")
	if err != nil {
		return fmt.Errorf("pool: create temp for %s: %w", finalPath, err)
	}
	tmpPath := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpPath) }

	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("pool: chmod %s: %w", tmpPath, err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("pool: write %s: %w", tmpPath, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("pool: fsync %s: %w", tmpPath, err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("pool: close %s: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, finalPath); err != nil {
		cleanup()
		return fmt.Errorf("pool: rename %s -> %s: %w", tmpPath, finalPath, err)
	}
	return nil
}

// ensureStateDir creates stateDir with 0700 perms if it does not exist.
// The pool state contains tokens-adjacent metadata (slot names, claims),
// so the directory is owner-only.
func ensureStateDir(stateDir string) error {
	if stateDir == "" {
		return errors.New("pool: stateDir is empty")
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return fmt.Errorf("pool: create state dir %s: %w", stateDir, err)
	}
	return nil
}
