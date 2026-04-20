package bundlestore

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"
)

// fakeStore is a test double that records calls and lets cases inject
// List / Stat / Delete behavior without touching the filesystem.
type fakeStore struct {
	mu      sync.Mutex
	handles []BundleHandle
	infos   map[string]BundleInfo // keyed by handle.Key
	deletes []BundleHandle
	listErr error
}

func (f *fakeStore) Put(context.Context, PutRequest, io.Reader) (BundleHandle, error) {
	return BundleHandle{}, errors.New("Put not used in janitor tests")
}
func (f *fakeStore) Get(context.Context, BundleHandle) (io.ReadCloser, error) {
	return nil, errors.New("Get not used in janitor tests")
}
func (f *fakeStore) Delete(_ context.Context, h BundleHandle) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deletes = append(f.deletes, h)
	return nil
}
func (f *fakeStore) List(context.Context) ([]BundleHandle, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := make([]BundleHandle, len(f.handles))
	copy(out, f.handles)
	return out, nil
}
func (f *fakeStore) Stat(_ context.Context, h BundleHandle) (BundleInfo, error) {
	if info, ok := f.infos[h.Key]; ok {
		return info, nil
	}
	return BundleInfo{}, ErrNotFound
}

func (f *fakeStore) deletedKeys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	keys := make([]string, len(f.deletes))
	for i, h := range f.deletes {
		keys[i] = h.Key
	}
	return keys
}

func TestJanitor_ReapsClosedSealed(t *testing.T) {
	now := time.Date(2026, 4, 20, 12, 0, 0, 0, time.UTC)
	store := &fakeStore{
		handles: []BundleHandle{{BeadID: "spi-old", Key: "spi-old/a-0.bundle"}},
	}
	lookup := BeadLookupFunc(func(_ context.Context, id string) (BeadInfo, error) {
		return BeadInfo{
			Status:   "closed",
			SealedAt: now.Add(-45 * time.Minute), // past the 30m grace
		}, nil
	})
	j := &Janitor{
		Store:       store,
		Lookup:      lookup,
		SealedGrace: 30 * time.Minute,
		OrphanAge:   7 * 24 * time.Hour,
		Now:         func() time.Time { return now },
	}
	j.Sweep(context.Background())

	got := store.deletedKeys()
	if len(got) != 1 || got[0] != "spi-old/a-0.bundle" {
		t.Errorf("deleted = %v, want [spi-old/a-0.bundle]", got)
	}
}

func TestJanitor_KeepsClosedWithinGrace(t *testing.T) {
	now := time.Date(2026, 4, 20, 12, 0, 0, 0, time.UTC)
	store := &fakeStore{
		handles: []BundleHandle{{BeadID: "spi-new", Key: "spi-new/a-0.bundle"}},
	}
	lookup := BeadLookupFunc(func(_ context.Context, id string) (BeadInfo, error) {
		return BeadInfo{
			Status:   "closed",
			SealedAt: now.Add(-5 * time.Minute), // inside grace window
		}, nil
	})
	j := &Janitor{
		Store:       store,
		Lookup:      lookup,
		SealedGrace: 30 * time.Minute,
		Now:         func() time.Time { return now },
	}
	j.Sweep(context.Background())

	if got := store.deletedKeys(); len(got) != 0 {
		t.Errorf("deleted = %v, want []", got)
	}
}

func TestJanitor_KeepsOpenBeads(t *testing.T) {
	now := time.Date(2026, 4, 20, 12, 0, 0, 0, time.UTC)
	store := &fakeStore{
		handles: []BundleHandle{{BeadID: "spi-open", Key: "spi-open/a-0.bundle"}},
	}
	lookup := BeadLookupFunc(func(_ context.Context, id string) (BeadInfo, error) {
		// Open bead with no seal — this is the "work in progress" case.
		return BeadInfo{Status: "in_progress"}, nil
	})
	j := &Janitor{Store: store, Lookup: lookup, Now: func() time.Time { return now }}
	j.Sweep(context.Background())

	if got := store.deletedKeys(); len(got) != 0 {
		t.Errorf("deleted = %v, want []", got)
	}
}

func TestJanitor_KeepsClosedUnsealed(t *testing.T) {
	// Until spi-rfee2 populates sealed_at, every closed bead's SealedAt
	// is zero — and we intentionally DO NOT delete in that case. The
	// janitor becomes a no-op for this path until the schema catches up.
	now := time.Date(2026, 4, 20, 12, 0, 0, 0, time.UTC)
	store := &fakeStore{
		handles: []BundleHandle{{BeadID: "spi-closed", Key: "spi-closed/a-0.bundle"}},
	}
	lookup := BeadLookupFunc(func(_ context.Context, id string) (BeadInfo, error) {
		return BeadInfo{Status: "closed"}, nil // SealedAt zero
	})
	j := &Janitor{Store: store, Lookup: lookup, Now: func() time.Time { return now }}
	j.Sweep(context.Background())

	if got := store.deletedKeys(); len(got) != 0 {
		t.Errorf("deleted = %v, want []", got)
	}
}

func TestJanitor_ReapsOrphansAfterOrphanAge(t *testing.T) {
	now := time.Date(2026, 4, 20, 12, 0, 0, 0, time.UTC)
	store := &fakeStore{
		handles: []BundleHandle{{BeadID: "spi-gone", Key: "spi-gone/a-0.bundle"}},
		infos: map[string]BundleInfo{
			"spi-gone/a-0.bundle": {ModTime: now.Add(-8 * 24 * time.Hour)},
		},
	}
	lookup := BeadLookupFunc(func(_ context.Context, id string) (BeadInfo, error) {
		return BeadInfo{}, ErrBeadNotFound
	})
	j := &Janitor{
		Store:     store,
		Lookup:    lookup,
		OrphanAge: 7 * 24 * time.Hour,
		Now:       func() time.Time { return now },
	}
	j.Sweep(context.Background())

	got := store.deletedKeys()
	if len(got) != 1 || got[0] != "spi-gone/a-0.bundle" {
		t.Errorf("deleted = %v, want [spi-gone/a-0.bundle]", got)
	}
}

func TestJanitor_KeepsFreshOrphans(t *testing.T) {
	now := time.Date(2026, 4, 20, 12, 0, 0, 0, time.UTC)
	store := &fakeStore{
		handles: []BundleHandle{{BeadID: "spi-gone", Key: "spi-gone/a-0.bundle"}},
		infos: map[string]BundleInfo{
			"spi-gone/a-0.bundle": {ModTime: now.Add(-1 * time.Hour)},
		},
	}
	lookup := BeadLookupFunc(func(_ context.Context, id string) (BeadInfo, error) {
		return BeadInfo{}, ErrBeadNotFound
	})
	j := &Janitor{
		Store:     store,
		Lookup:    lookup,
		OrphanAge: 7 * 24 * time.Hour,
		Now:       func() time.Time { return now },
	}
	j.Sweep(context.Background())

	if got := store.deletedKeys(); len(got) != 0 {
		t.Errorf("deleted = %v, want []", got)
	}
}

func TestJanitor_ContinuesOnLookupError(t *testing.T) {
	now := time.Date(2026, 4, 20, 12, 0, 0, 0, time.UTC)
	store := &fakeStore{
		handles: []BundleHandle{
			{BeadID: "spi-bad", Key: "spi-bad/a-0.bundle"},
			{BeadID: "spi-ok", Key: "spi-ok/a-0.bundle"},
		},
	}
	lookup := BeadLookupFunc(func(_ context.Context, id string) (BeadInfo, error) {
		if id == "spi-bad" {
			return BeadInfo{}, errors.New("transient failure")
		}
		return BeadInfo{Status: "closed", SealedAt: now.Add(-45 * time.Minute)}, nil
	})
	j := &Janitor{Store: store, Lookup: lookup, SealedGrace: 30 * time.Minute, Now: func() time.Time { return now }}
	j.Sweep(context.Background())

	// The good handle must still get reaped; the bad one stays.
	got := store.deletedKeys()
	if len(got) != 1 || got[0] != "spi-ok/a-0.bundle" {
		t.Errorf("deleted = %v, want [spi-ok/a-0.bundle]", got)
	}
}

func TestJanitor_RunStopsOnContextCancel(t *testing.T) {
	store := &fakeStore{}
	lookup := BeadLookupFunc(func(context.Context, string) (BeadInfo, error) {
		return BeadInfo{}, ErrBeadNotFound
	})
	j := &Janitor{Store: store, Lookup: lookup, Interval: 10 * time.Millisecond}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		j.Run(ctx)
		close(done)
	}()
	// Let at least one tick fire.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not exit on context cancel")
	}
}
