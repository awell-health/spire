package logexport

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/awell-health/spire/pkg/logartifact"
)

// fakeStore is a memory-only logartifact.Store implementation used by
// the exporter tests. It supports the Put/Finalize/Get/Stat/List
// surface and exposes failure-injection knobs so the upload-failure
// tests can assert manifest status transitions without a real DB.
//
// The fake intentionally mirrors the substrate's contract: a Put
// returns a Writer that buffers bytes; Finalize closes the writer and
// flips the manifest to finalized. Sequence > 0 is supported so the
// tailer's rotation path can be exercised.
type fakeStore struct {
	mu sync.Mutex

	// keyed by Identity + Sequence
	manifests map[fakeKey]*logartifact.Manifest
	bytes     map[fakeKey][]byte

	nextID int

	// failPut returns this error from Put when non-nil. Cleared after
	// each failed call to support "fail N times then succeed" patterns
	// — tests configure putFailures with a slice of errors; nil entries
	// represent successful attempts.
	putFailures      []error
	finalizeFailures []error
}

type fakeKey struct {
	identity logartifact.Identity
	sequence int
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		manifests: make(map[fakeKey]*logartifact.Manifest),
		bytes:     make(map[fakeKey][]byte),
	}
}

func (s *fakeStore) Put(ctx context.Context, identity logartifact.Identity, sequence int, visibility logartifact.Visibility) (logartifact.Writer, error) {
	if err := identity.Validate(); err != nil {
		return nil, err
	}
	if visibility == "" {
		return nil, errors.New("fakeStore: visibility required")
	}

	s.mu.Lock()
	if len(s.putFailures) > 0 {
		err := s.putFailures[0]
		s.putFailures = s.putFailures[1:]
		if err != nil {
			s.mu.Unlock()
			return nil, err
		}
	}
	key := fakeKey{identity: identity, sequence: sequence}
	if existing, ok := s.manifests[key]; ok && existing.Status == logartifact.StatusFinalized {
		s.mu.Unlock()
		return nil, errors.New("fakeStore: already finalized")
	}
	s.nextID++
	id := fmt.Sprintf("log-fake-%06d", s.nextID)
	manifest := &logartifact.Manifest{
		ID:         id,
		Identity:   identity,
		Sequence:   sequence,
		ObjectURI:  fmt.Sprintf("memory://%s", id),
		Status:     logartifact.StatusWriting,
		Visibility: visibility,
		StartedAt:  time.Now(),
		CreatedAt:  time.Now(),
		UpdatedAt:  time.Now(),
	}
	s.manifests[key] = manifest
	s.mu.Unlock()

	return &fakeWriter{
		store:      s,
		key:        key,
		manifestID: id,
		objectURI:  manifest.ObjectURI,
		visibility: visibility,
		identity:   identity,
		sequence:   sequence,
		hasher:     sha256.New(),
	}, nil
}

func (s *fakeStore) Finalize(ctx context.Context, w logartifact.Writer) (logartifact.Manifest, error) {
	fw, ok := w.(*fakeWriter)
	if !ok {
		return logartifact.Manifest{}, fmt.Errorf("fakeStore.Finalize: writer is %T", w)
	}
	s.mu.Lock()
	if len(s.finalizeFailures) > 0 {
		err := s.finalizeFailures[0]
		s.finalizeFailures = s.finalizeFailures[1:]
		if err != nil {
			s.mu.Unlock()
			return logartifact.Manifest{}, err
		}
	}
	manifest, ok := s.manifests[fw.key]
	if !ok {
		s.mu.Unlock()
		return logartifact.Manifest{}, logartifact.ErrNotFound
	}
	if manifest.Status == logartifact.StatusFinalized {
		s.mu.Unlock()
		return *manifest, nil
	}
	checksum := "sha256:" + hex.EncodeToString(fw.hasher.Sum(nil))
	manifest.ByteSize = int64(fw.size)
	manifest.Checksum = checksum
	manifest.Status = logartifact.StatusFinalized
	manifest.EndedAt = time.Now()
	manifest.UpdatedAt = time.Now()
	s.bytes[fw.key] = append([]byte(nil), fw.buf.Bytes()...)
	s.mu.Unlock()
	return *manifest, nil
}

func (s *fakeStore) Get(ctx context.Context, ref logartifact.ManifestRef) (io.ReadCloser, logartifact.Manifest, error) {
	if ref.ID == "" {
		return nil, logartifact.Manifest{}, errors.New("fakeStore: ref.ID required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, m := range s.manifests {
		if m.ID == ref.ID {
			data := s.bytes[k]
			return io.NopCloser(bytes.NewReader(data)), *m, nil
		}
	}
	return nil, logartifact.Manifest{}, logartifact.ErrNotFound
}

func (s *fakeStore) Stat(ctx context.Context, ref logartifact.ManifestRef) (logartifact.Manifest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.manifests {
		if m.ID == ref.ID {
			return *m, nil
		}
	}
	return logartifact.Manifest{}, logartifact.ErrNotFound
}

func (s *fakeStore) List(ctx context.Context, filter logartifact.Filter) ([]logartifact.Manifest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []logartifact.Manifest
	for _, m := range s.manifests {
		if filter.BeadID != "" && m.Identity.BeadID != filter.BeadID {
			continue
		}
		if filter.AttemptID != "" && m.Identity.AttemptID != filter.AttemptID {
			continue
		}
		if filter.RunID != "" && m.Identity.RunID != filter.RunID {
			continue
		}
		if filter.AgentName != "" && m.Identity.AgentName != filter.AgentName {
			continue
		}
		out = append(out, *m)
	}
	return out, nil
}

// markFailed flips the manifest row's status. Used by tests that
// simulate the in-store mark-failed path (the production code routes
// through pkgstore.UpdateLogArtifactStatus + a *sql.DB; the fake's
// in-memory tests assert on store state directly).
func (s *fakeStore) markFailed(identity logartifact.Identity, sequence int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := fakeKey{identity: identity, sequence: sequence}
	if m, ok := s.manifests[key]; ok {
		m.Status = logartifact.StatusFailed
	}
}

// fakeWriter implements logartifact.Writer over an in-memory buffer.
type fakeWriter struct {
	store      *fakeStore
	key        fakeKey
	manifestID string
	objectURI  string
	visibility logartifact.Visibility
	identity   logartifact.Identity
	sequence   int

	mu     sync.Mutex
	buf    bytes.Buffer
	hasher interface {
		Write([]byte) (int, error)
		Sum([]byte) []byte
	}
	size   int
	closed bool
}

func (w *fakeWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return 0, errors.New("fakeWriter: write on closed writer")
	}
	n, err := w.buf.Write(p)
	if n > 0 {
		w.hasher.Write(p[:n])
		w.size += n
	}
	return n, err
}

func (w *fakeWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.closed = true
	return nil
}

func (w *fakeWriter) Identity() logartifact.Identity { return w.identity }
func (w *fakeWriter) Sequence() int                  { return w.sequence }
func (w *fakeWriter) Size() int64                    { return int64(w.size) }
func (w *fakeWriter) ChecksumHex() string {
	return hex.EncodeToString(w.hasher.Sum(nil))
}
func (w *fakeWriter) ObjectURI() string                { return w.objectURI }
func (w *fakeWriter) ManifestID() string               { return w.manifestID }
func (w *fakeWriter) Visibility() logartifact.Visibility { return w.visibility }
