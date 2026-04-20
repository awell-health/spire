package bundlestore

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestStore(t *testing.T, max int64) *LocalStore {
	t.Helper()
	s, err := NewLocalStore(Config{LocalRoot: t.TempDir(), MaxBytes: max})
	if err != nil {
		t.Fatalf("NewLocalStore: %v", err)
	}
	return s
}

func defaultReq() PutRequest {
	return PutRequest{BeadID: "spi-abc", AttemptID: "spi-att1", ApprenticeIdx: 0}
}

func TestLocalStore_PutGetRoundTrip(t *testing.T) {
	s := newTestStore(t, DefaultMaxBytes)
	ctx := context.Background()

	payload := []byte("fake git bundle bytes")
	h, err := s.Put(ctx, defaultReq(), bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if h.BeadID != "spi-abc" {
		t.Errorf("BeadID = %q, want spi-abc", h.BeadID)
	}
	if h.Key == "" {
		t.Fatal("empty key")
	}

	rc, err := s.Get(ctx, h)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	got, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("got %q, want %q", got, payload)
	}
}

func TestLocalStore_RejectsDuplicate(t *testing.T) {
	s := newTestStore(t, DefaultMaxBytes)
	ctx := context.Background()

	if _, err := s.Put(ctx, defaultReq(), bytes.NewReader([]byte("one"))); err != nil {
		t.Fatalf("first Put: %v", err)
	}
	_, err := s.Put(ctx, defaultReq(), bytes.NewReader([]byte("two")))
	if !errors.Is(err, ErrDuplicate) {
		t.Fatalf("second Put err = %v, want ErrDuplicate", err)
	}
}

func TestLocalStore_EnforcesSizeLimit(t *testing.T) {
	s := newTestStore(t, 16)
	ctx := context.Background()

	// Exactly at the limit should succeed.
	if _, err := s.Put(ctx, defaultReq(), bytes.NewReader(bytes.Repeat([]byte("x"), 16))); err != nil {
		t.Fatalf("at-limit Put: %v", err)
	}

	// One byte over should fail and leave no tmpfile behind.
	over := PutRequest{BeadID: "spi-abc", AttemptID: "spi-att2", ApprenticeIdx: 0}
	_, err := s.Put(ctx, over, bytes.NewReader(bytes.Repeat([]byte("x"), 17)))
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("over-limit Put err = %v, want ErrTooLarge", err)
	}

	// No leftover tmpfile from the failed Put.
	entries, err := os.ReadDir(filepath.Join(s.Root(), "spi-abc"))
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), tmpSuffix) {
			t.Errorf("leftover tmpfile: %s", e.Name())
		}
	}
}

func TestLocalStore_DeleteIdempotent(t *testing.T) {
	s := newTestStore(t, DefaultMaxBytes)
	ctx := context.Background()

	h, err := s.Put(ctx, defaultReq(), bytes.NewReader([]byte("bytes")))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := s.Delete(ctx, h); err != nil {
		t.Fatalf("first Delete: %v", err)
	}
	if err := s.Delete(ctx, h); err != nil {
		t.Fatalf("second Delete: %v", err)
	}
	if _, err := s.Get(ctx, h); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after Delete err = %v, want ErrNotFound", err)
	}
}

func TestLocalStore_GetMissing(t *testing.T) {
	s := newTestStore(t, DefaultMaxBytes)
	ctx := context.Background()
	_, err := s.Get(ctx, BundleHandle{BeadID: "spi-abc", Key: "spi-abc/missing-0.bundle"})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get missing err = %v, want ErrNotFound", err)
	}
}

func TestLocalStore_ListSkipsTmpfiles(t *testing.T) {
	s := newTestStore(t, DefaultMaxBytes)
	ctx := context.Background()

	if _, err := s.Put(ctx, defaultReq(), bytes.NewReader([]byte("ok"))); err != nil {
		t.Fatalf("Put: %v", err)
	}
	// Drop a stray tmpfile that mimics a crashed Put.
	beadDir := filepath.Join(s.Root(), "spi-abc")
	if err := os.WriteFile(filepath.Join(beadDir, "spi-att1-0.bundle.stuck"+tmpSuffix), []byte("junk"), 0o644); err != nil {
		t.Fatalf("write stray tmpfile: %v", err)
	}

	handles, err := s.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(handles) != 1 {
		t.Fatalf("List = %d handles, want 1 (%v)", len(handles), handles)
	}
	if !strings.HasSuffix(handles[0].Key, ".bundle") {
		t.Errorf("unexpected handle key: %q", handles[0].Key)
	}
}

func TestLocalStore_ValidatesIDs(t *testing.T) {
	s := newTestStore(t, DefaultMaxBytes)
	ctx := context.Background()

	bad := []PutRequest{
		{BeadID: "", AttemptID: "spi-a", ApprenticeIdx: 0},
		{BeadID: "spi-a", AttemptID: "", ApprenticeIdx: 0},
		{BeadID: "../etc/passwd", AttemptID: "spi-a", ApprenticeIdx: 0},
		{BeadID: "spi-a", AttemptID: "sp/../b", ApprenticeIdx: 0},
		{BeadID: "spi-a", AttemptID: "spi-a", ApprenticeIdx: -1},
		{BeadID: "SPI-UPPER", AttemptID: "spi-a", ApprenticeIdx: 0},
	}
	for _, req := range bad {
		if _, err := s.Put(ctx, req, bytes.NewReader([]byte("x"))); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("Put(%+v) err = %v, want ErrInvalidRequest", req, err)
		}
	}
}

func TestLocalStore_ResolveRejectsTraversal(t *testing.T) {
	s := newTestStore(t, DefaultMaxBytes)
	ctx := context.Background()

	bad := []BundleHandle{
		{BeadID: "spi-a", Key: "../escape.bundle"},
		{BeadID: "spi-a", Key: "/etc/passwd"},
		{BeadID: "spi-a", Key: "spi-a/../../etc"},
		{BeadID: "spi-a", Key: ""},
	}
	for _, h := range bad {
		if _, err := s.Get(ctx, h); !errors.Is(err, ErrNotFound) {
			t.Errorf("Get(%+v) err = %v, want ErrNotFound", h, err)
		}
	}
}

func TestLocalStore_Stat(t *testing.T) {
	s := newTestStore(t, DefaultMaxBytes)
	ctx := context.Background()

	payload := []byte("twelve bytes")
	h, err := s.Put(ctx, defaultReq(), bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	info, err := s.Stat(ctx, h)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Size != int64(len(payload)) {
		t.Errorf("Size = %d, want %d", info.Size, len(payload))
	}
	if info.ModTime.IsZero() {
		t.Error("ModTime is zero")
	}
}

func TestLocalStore_MultipleApprentices(t *testing.T) {
	s := newTestStore(t, DefaultMaxBytes)
	ctx := context.Background()

	// Fan-out: same bead, same attempt, different apprentice indices.
	reqs := []PutRequest{
		{BeadID: "spi-epic", AttemptID: "spi-att", ApprenticeIdx: 0},
		{BeadID: "spi-epic", AttemptID: "spi-att", ApprenticeIdx: 1},
		{BeadID: "spi-epic", AttemptID: "spi-att", ApprenticeIdx: 2},
	}
	for _, r := range reqs {
		if _, err := s.Put(ctx, r, bytes.NewReader([]byte("b"))); err != nil {
			t.Fatalf("Put(idx=%d): %v", r.ApprenticeIdx, err)
		}
	}
	handles, err := s.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(handles) != 3 {
		t.Fatalf("List = %d handles, want 3", len(handles))
	}
}
