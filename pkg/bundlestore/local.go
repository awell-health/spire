package bundlestore

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// tmpSuffix marks in-flight bundle writes. List/Stat must skip these; a
// crashed writer leaves a *.tmp behind that the janitor can eventually
// reap (orphan path) — but a half-written file must never be mistaken
// for a complete bundle.
const tmpSuffix = ".tmp"

// bundleSuffix is the filename extension for a completed bundle.
const bundleSuffix = ".bundle"

// LocalStore is a filesystem-backed BundleStore. Bundles are stored at
//
//	<root>/<beadID>/<attemptID>-<idx>.bundle
//
// with atomic tmpfile+rename to guarantee crash-safety.
type LocalStore struct {
	root     string
	maxBytes int64
}

// NewLocalStore constructs a LocalStore rooted at cfg.LocalRoot (or the
// platform default when empty). The root directory is created on demand.
func NewLocalStore(cfg Config) (*LocalStore, error) {
	cfg = cfg.WithDefaults()
	root := cfg.LocalRoot
	if root == "" {
		def, err := defaultLocalRoot()
		if err != nil {
			return nil, fmt.Errorf("resolve default local root: %w", err)
		}
		root = def
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir %s: %w", root, err)
	}
	return &LocalStore{root: root, maxBytes: cfg.MaxBytes}, nil
}

// Root returns the filesystem path the store writes to. Exposed for
// tests and operational tooling; callers must NOT read the filesystem
// directly in production code.
func (s *LocalStore) Root() string { return s.root }

// Put implements BundleStore.
func (s *LocalStore) Put(ctx context.Context, req PutRequest, bundle io.Reader) (BundleHandle, error) {
	if err := req.Validate(); err != nil {
		return BundleHandle{}, err
	}

	relKey := keyFor(req)
	finalPath := filepath.Join(s.root, relKey)
	dir := filepath.Dir(finalPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return BundleHandle{}, fmt.Errorf("mkdir %s: %w", dir, err)
	}

	// Reject-on-duplicate: silent overwrite would mask a bug where two
	// apprentices collide on the same triple.
	if _, err := os.Stat(finalPath); err == nil {
		return BundleHandle{}, ErrDuplicate
	} else if !os.IsNotExist(err) {
		return BundleHandle{}, fmt.Errorf("stat %s: %w", finalPath, err)
	}

	tmp, err := os.CreateTemp(dir, filepath.Base(finalPath)+".*"+tmpSuffix)
	if err != nil {
		return BundleHandle{}, fmt.Errorf("create tmpfile: %w", err)
	}
	tmpPath := tmp.Name()
	// Ensure tmpfile is removed if we don't rename it into place.
	committed := false
	defer func() {
		tmp.Close()
		if !committed {
			os.Remove(tmpPath)
		}
	}()

	// LimitReader(max+1) lets us detect oversize: if we read max+1 bytes,
	// the caller provided more than allowed.
	lr := io.LimitReader(bundle, s.maxBytes+1)
	n, err := io.Copy(tmp, lr)
	if err != nil {
		return BundleHandle{}, fmt.Errorf("write bundle: %w", err)
	}
	if n > s.maxBytes {
		return BundleHandle{}, ErrTooLarge
	}
	if err := tmp.Sync(); err != nil {
		return BundleHandle{}, fmt.Errorf("fsync tmpfile: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return BundleHandle{}, fmt.Errorf("close tmpfile: %w", err)
	}
	if err := os.Rename(tmpPath, finalPath); err != nil {
		return BundleHandle{}, fmt.Errorf("rename into place: %w", err)
	}
	committed = true

	return BundleHandle{BeadID: req.BeadID, Key: relKey}, nil
}

// Get implements BundleStore.
func (s *LocalStore) Get(ctx context.Context, h BundleHandle) (io.ReadCloser, error) {
	path, err := s.resolve(h)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	return f, nil
}

// Delete implements BundleStore. Idempotent.
func (s *LocalStore) Delete(ctx context.Context, h BundleHandle) error {
	path, err := s.resolve(h)
	if err != nil {
		// An invalid handle deletes nothing — treat as success so callers
		// can't loop forever on a bad entry from List.
		if err == ErrNotFound {
			return nil
		}
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove %s: %w", path, err)
	}
	// Best-effort prune of the bead-level directory when it becomes empty.
	// Errors here are not meaningful to the caller.
	os.Remove(filepath.Dir(path))
	return nil
}

// List implements BundleStore. Walks the store root and returns every
// completed bundle; tmpfiles are skipped.
func (s *LocalStore) List(ctx context.Context) ([]BundleHandle, error) {
	var out []BundleHandle
	err := filepath.WalkDir(s.root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			// Don't abort the whole walk on a single unreadable dir.
			return nil
		}
		if d.IsDir() {
			return nil
		}
		name := d.Name()
		if strings.HasSuffix(name, tmpSuffix) || !strings.HasSuffix(name, bundleSuffix) {
			return nil
		}
		rel, err := filepath.Rel(s.root, path)
		if err != nil {
			return nil
		}
		// Normalize to forward slashes so handles are portable across OSes.
		rel = filepath.ToSlash(rel)
		parts := strings.SplitN(rel, "/", 2)
		if len(parts) != 2 {
			return nil
		}
		out = append(out, BundleHandle{BeadID: parts[0], Key: rel})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk %s: %w", s.root, err)
	}
	return out, nil
}

// Stat implements BundleStore.
func (s *LocalStore) Stat(ctx context.Context, h BundleHandle) (BundleInfo, error) {
	path, err := s.resolve(h)
	if err != nil {
		return BundleInfo{}, err
	}
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return BundleInfo{}, ErrNotFound
		}
		return BundleInfo{}, fmt.Errorf("stat %s: %w", path, err)
	}
	return BundleInfo{Size: info.Size(), ModTime: info.ModTime()}, nil
}

// resolve validates h.Key (path-traversal guard) and returns the
// absolute filesystem path it refers to.
func (s *LocalStore) resolve(h BundleHandle) (string, error) {
	if h.Key == "" {
		return "", ErrNotFound
	}
	// Normalize and reject anything that tries to escape the root.
	clean := filepath.ToSlash(filepath.Clean(h.Key))
	if strings.HasPrefix(clean, "../") || strings.Contains(clean, "/../") ||
		clean == ".." || strings.HasPrefix(clean, "/") {
		return "", ErrNotFound
	}
	return filepath.Join(s.root, filepath.FromSlash(clean)), nil
}

// keyFor is the local backend's key scheme. Exported as an
// implementation detail for tests — callers must treat BundleHandle.Key
// as opaque in production code.
func keyFor(req PutRequest) string {
	return req.BeadID + "/" + req.AttemptID + "-" + strconv.Itoa(req.ApprenticeIdx) + bundleSuffix
}

// defaultLocalRoot computes the default bundle root under XDG_DATA_HOME
// (or ~/.local/share on platforms where XDG_DATA_HOME isn't set).
func defaultLocalRoot() (string, error) {
	if x := os.Getenv("XDG_DATA_HOME"); x != "" {
		return filepath.Join(x, "spire", "bundles"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "share", "spire", "bundles"), nil
}

