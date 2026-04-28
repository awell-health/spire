package logartifact

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	pkgstore "github.com/awell-health/spire/pkg/store"

	"github.com/awell-health/spire/pkg/logartifact/redact"
)

// LocalStore is a filesystem-backed log artifact store. The byte stream
// for each artifact lands at a deterministic path under root; the
// manifest row goes into the agent_log_artifacts table accessed via the
// supplied *sql.DB.
//
// The on-disk layout mirrors the GCS object key shape:
//
//	<root>/<tower>/<bead>/<attempt>/<run>/<agent>/<role>/<phase>/<provider>/<stream>[-<seq>].jsonl
//
// (with the provider segment omitted when Identity.Provider is empty).
// Writes are crash-safe via tmpfile + rename in Finalize so a partial
// write never appears under the canonical path.
type LocalStore struct {
	root string
	db   *sql.DB
}

// NewLocal constructs a LocalStore rooted at rootDir. The directory is
// created on demand. db must be a valid *sql.DB pointing at the tower
// holding the agent_log_artifacts table — callers usually obtain it via
// pkg/store.ActiveDB.
//
// Passing an empty rootDir is rejected; callers should resolve the
// wizard data directory via the existing config helper (typically
// `dolt.GlobalDir() + "/wizards"` for local-native) and pass an
// absolute path. This keeps pkg/logartifact substrate-only — it does
// not own the wizard log directory convention.
func NewLocal(rootDir string, db *sql.DB) (*LocalStore, error) {
	if rootDir == "" {
		return nil, errors.New("logartifact: NewLocal: rootDir must not be empty")
	}
	if db == nil {
		return nil, errors.New("logartifact: NewLocal: db must not be nil")
	}
	if !filepath.IsAbs(rootDir) {
		return nil, fmt.Errorf("logartifact: NewLocal: rootDir %q must be absolute", rootDir)
	}
	if err := os.MkdirAll(rootDir, 0o755); err != nil {
		return nil, fmt.Errorf("logartifact: mkdir %s: %w", rootDir, err)
	}
	return &LocalStore{root: rootDir, db: db}, nil
}

// Root returns the filesystem path the store writes to. Exposed for
// tests and operational tooling.
func (s *LocalStore) Root() string { return s.root }

// localWriter is the LocalStore-side Writer. It owns a tmpfile; the
// final filename is locked at Put time so Finalize's rename is a single
// system call.
//
// When visibility is desktop_safe or public, bytes flow through the
// redactor before they reach the tmpfile. The substrate buffers the
// full payload, redacts it on Close/Finalize, and writes the redacted
// version to disk; we don't redact streaming chunks because the
// redactor's regex set must see token boundaries (a token split across
// two writes would slip through). For artifacts at desktop_safe scale
// (provider transcripts measured in MB at most) the buffer cost is
// acceptable; engineer_only artifacts skip the buffer entirely.
type localWriter struct {
	identity      Identity
	sequence      int
	finalPath     string
	tmpFile       *os.File
	tmpPath       string
	objectURI     string
	manifestID    string
	visibility    Visibility
	redactBuf     *bytes.Buffer // non-nil iff visibility.RedactsAtUpload()
	chunked       *chunkedHash
	closed        bool
	redactionDone int // generation that ran; 0 = engineer_only/no redactor
}

func (w *localWriter) Identity() Identity { return w.identity }
func (w *localWriter) Sequence() int      { return w.sequence }
func (w *localWriter) Size() int64        { return w.chunked.size }
func (w *localWriter) ChecksumHex() string {
	return hex.EncodeToString(w.chunked.hasher.Sum(nil))
}
func (w *localWriter) ObjectURI() string    { return w.objectURI }
func (w *localWriter) ManifestID() string   { return w.manifestID }
func (w *localWriter) Visibility() Visibility { return w.visibility }

func (w *localWriter) Write(p []byte) (int, error) {
	if w.closed {
		return 0, fmt.Errorf("logartifact: write on closed writer")
	}
	return w.chunked.Write(p)
}

// Close releases the underlying tmpfile without finalizing the
// manifest. Callers that want the artifact to be visible must call
// Store.Finalize, which renames the tmpfile into place. Calling Close
// after Finalize is a no-op.
func (w *localWriter) Close() error {
	if w.closed {
		return nil
	}
	w.closed = true
	if w.tmpFile == nil {
		return nil
	}
	closeErr := w.tmpFile.Close()
	// Tmpfile cleanup: if the writer is closed without Finalize, the
	// half-written bytes are not the canonical artifact and must be
	// removed so the next Put doesn't trip on stale state.
	if w.tmpPath != "" {
		_ = os.Remove(w.tmpPath)
	}
	return closeErr
}

// Put implements Store.
func (s *LocalStore) Put(ctx context.Context, identity Identity, sequence int, visibility Visibility) (Writer, error) {
	if err := identity.Validate(); err != nil {
		return nil, err
	}
	if sequence < 0 {
		return nil, fmt.Errorf("logartifact: sequence must be >= 0 (got %d)", sequence)
	}
	if visibility == "" {
		return nil, errors.New("logartifact: Put requires visibility (engineer_only/desktop_safe/public)")
	}
	if !visibility.Valid() {
		return nil, fmt.Errorf("logartifact: invalid visibility %q", visibility)
	}

	// Build the canonical relative key (no prefix; the backend
	// supplies its own root). BuildObjectKey performs all the
	// path-traversal sanitization we care about.
	relKey, err := BuildObjectKey("", identity, sequence)
	if err != nil {
		return nil, err
	}

	finalPath := filepath.Join(s.root, filepath.FromSlash(relKey))
	dir := filepath.Dir(finalPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("logartifact: mkdir %s: %w", dir, err)
	}

	tmp, err := os.CreateTemp(dir, filepath.Base(finalPath)+".*.tmp")
	if err != nil {
		return nil, fmt.Errorf("logartifact: create tmpfile: %w", err)
	}

	objectURI := "file://" + filepath.ToSlash(finalPath)

	manifestID, err := insertOrFetchManifestWithVisibility(ctx, s.db, identity, sequence, objectURI, visibility)
	if err != nil {
		// Clean up the tmpfile we just created — the manifest insert
		// failure means no caller will hold this writer.
		tmp.Close()
		os.Remove(tmp.Name())
		return nil, err
	}

	w := &localWriter{
		identity:   identity,
		sequence:   sequence,
		finalPath:  finalPath,
		tmpFile:    tmp,
		tmpPath:    tmp.Name(),
		objectURI:  objectURI,
		manifestID: manifestID,
		visibility: visibility,
	}
	if visibility.RedactsAtUpload() {
		// Buffer in memory so the redactor sees full token boundaries.
		// A token split across two streaming chunks would slip the
		// pattern set; buffering trades some memory for that
		// correctness. The bytes go through the hasher AFTER redaction
		// in Finalize, so the on-disk checksum matches the on-disk
		// content.
		w.redactBuf = &bytes.Buffer{}
		w.chunked = &chunkedHash{dst: w.redactBuf, hasher: sha256.New()}
	} else {
		w.chunked = &chunkedHash{dst: tmp, hasher: sha256.New()}
	}
	return w, nil
}

// Finalize implements Store.
func (s *LocalStore) Finalize(ctx context.Context, w Writer) (Manifest, error) {
	lw, ok := w.(*localWriter)
	if !ok {
		return Manifest{}, fmt.Errorf("logartifact: LocalStore.Finalize: writer is %T, expected *localWriter", w)
	}

	// Idempotent: if the manifest is already finalized, treat as no-op.
	rec, err := pkgstore.GetLogArtifact(ctx, s.db, lw.manifestID)
	if err != nil {
		return Manifest{}, fmt.Errorf("logartifact: lookup manifest: %w", err)
	}
	if rec != nil && rec.Status == pkgstore.LogArtifactStatusFinalized {
		// Drop the tmpfile — the canonical bytes are already on disk
		// and we don't want to overwrite them.
		if lw.tmpFile != nil {
			lw.tmpFile.Close()
			os.Remove(lw.tmpPath)
			lw.tmpFile = nil
			lw.closed = true
		}
		return recordToManifest(*rec), nil
	}

	if lw.tmpFile == nil {
		return Manifest{}, fmt.Errorf("logartifact: Finalize on already-closed writer (id=%s)", lw.manifestID)
	}

	// Redact-at-upload path: the bytes Write accepted are buffered in
	// redactBuf. Run the redactor over the full buffer (so a token
	// straddling two chunks isn't missed), recompute size+checksum on
	// the redacted output, write it to the tmpfile, and stamp the
	// redaction_version on the manifest row.
	if lw.redactBuf != nil {
		redacted, version := redact.New().Redact(lw.redactBuf.Bytes())
		hasher := sha256.New()
		hasher.Write(redacted)
		// Replace the chunked accounting so Size()/ChecksumHex() now
		// describe what is actually on disk after the rename below,
		// not the unredacted buffer the caller wrote.
		lw.chunked = &chunkedHash{
			dst:    io.Discard,
			hasher: hasher,
			size:   int64(len(redacted)),
		}
		lw.redactionDone = version
		if _, werr := lw.tmpFile.Write(redacted); werr != nil {
			lw.tmpFile.Close()
			os.Remove(lw.tmpPath)
			_ = pkgstore.UpdateLogArtifactStatus(ctx, s.db, lw.manifestID, pkgstore.LogArtifactStatusFailed)
			return Manifest{}, fmt.Errorf("logartifact: write redacted: %w", werr)
		}
	}

	// fsync + close the tmpfile, then rename into place. After this
	// point a crash leaves the canonical artifact on disk in its
	// finalized form.
	if err := lw.tmpFile.Sync(); err != nil {
		lw.tmpFile.Close()
		os.Remove(lw.tmpPath)
		_ = pkgstore.UpdateLogArtifactStatus(ctx, s.db, lw.manifestID, pkgstore.LogArtifactStatusFailed)
		return Manifest{}, fmt.Errorf("logartifact: fsync %s: %w", lw.tmpPath, err)
	}
	if err := lw.tmpFile.Close(); err != nil {
		os.Remove(lw.tmpPath)
		_ = pkgstore.UpdateLogArtifactStatus(ctx, s.db, lw.manifestID, pkgstore.LogArtifactStatusFailed)
		return Manifest{}, fmt.Errorf("logartifact: close tmpfile: %w", err)
	}
	if err := os.Rename(lw.tmpPath, lw.finalPath); err != nil {
		os.Remove(lw.tmpPath)
		_ = pkgstore.UpdateLogArtifactStatus(ctx, s.db, lw.manifestID, pkgstore.LogArtifactStatusFailed)
		return Manifest{}, fmt.Errorf("logartifact: rename %s → %s: %w", lw.tmpPath, lw.finalPath, err)
	}
	lw.closed = true
	lw.tmpFile = nil

	checksum := "sha256:" + lw.ChecksumHex()
	if err := pkgstore.FinalizeLogArtifact(ctx, s.db, lw.manifestID, lw.Size(), checksum, "", ""); err != nil {
		return Manifest{}, fmt.Errorf("logartifact: finalize manifest: %w", err)
	}
	if lw.redactionDone > 0 {
		if err := pkgstore.SetLogArtifactRedaction(ctx, s.db, lw.manifestID, lw.redactionDone); err != nil {
			return Manifest{}, fmt.Errorf("logartifact: stamp redaction version: %w", err)
		}
	}

	rec, err = pkgstore.GetLogArtifact(ctx, s.db, lw.manifestID)
	if err != nil {
		return Manifest{}, fmt.Errorf("logartifact: re-fetch manifest: %w", err)
	}
	if rec == nil {
		return Manifest{}, fmt.Errorf("logartifact: manifest %s vanished after finalize", lw.manifestID)
	}
	return recordToManifest(*rec), nil
}

// Get implements Store.
func (s *LocalStore) Get(ctx context.Context, ref ManifestRef) (io.ReadCloser, Manifest, error) {
	rec, err := resolveManifest(ctx, s.db, ref)
	if err != nil {
		return nil, Manifest{}, err
	}
	path, err := localPathFromURI(rec.ObjectURI)
	if err != nil {
		return nil, Manifest{}, err
	}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, Manifest{}, ErrNotFound
		}
		return nil, Manifest{}, fmt.Errorf("logartifact: open %s: %w", path, err)
	}
	return f, recordToManifest(*rec), nil
}

// Stat implements Store.
func (s *LocalStore) Stat(ctx context.Context, ref ManifestRef) (Manifest, error) {
	rec, err := resolveManifest(ctx, s.db, ref)
	if err != nil {
		return Manifest{}, err
	}
	return recordToManifest(*rec), nil
}

// List implements Store.
func (s *LocalStore) List(ctx context.Context, filter Filter) ([]Manifest, error) {
	return listManifests(ctx, s.db, filter)
}

// Reconcile walks the local artifact root for the given bead and
// inserts manifest rows for any byte-store files that have no
// corresponding row yet. The walk infers identity from the on-disk
// path layout (which mirrors BuildObjectKey), hashes each file to fill
// in the byte_size and checksum columns, and marks the resulting rows
// StatusFinalized.
//
// Reconcile is opt-in: List never invokes it implicitly. Use Reconcile
// once when migrating an existing wizard log directory into the
// manifest, or after an external process drops files into the tree
// that need to be made visible to the gateway.
//
// The walk is best-effort. Files whose path doesn't decode into a
// valid Identity are skipped silently; backends that need stricter
// guarantees should use the canonical write path through Put/Finalize.
//
// beadID may be empty; an empty value reconciles every bead under the
// root for the configured tower.
func (s *LocalStore) Reconcile(ctx context.Context, tower, beadID string) ([]Manifest, error) {
	if tower == "" {
		return nil, errors.New("logartifact: Reconcile: tower is required")
	}
	towerRoot := filepath.Join(s.root, tower)
	if beadID != "" {
		towerRoot = filepath.Join(towerRoot, beadID)
	}
	if _, err := os.Stat(towerRoot); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}

	var out []Manifest
	walkErr := filepath.WalkDir(towerRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			// Don't abort on a single unreadable dir; log via error
			// chain only when the walk root itself fails.
			return nil
		}
		if d.IsDir() {
			return nil
		}
		// Only canonical .jsonl artifacts are reconciled; tmpfiles
		// and stray files (.log, .stderr.log) are ignored. This is
		// where we'd later widen if we want to bring legacy wizard
		// .log files into the manifest, but for now the design
		// reserves .jsonl as the artifact extension.
		if !strings.HasSuffix(path, ".jsonl") {
			return nil
		}
		rel, err := filepath.Rel(s.root, path)
		if err != nil {
			return nil
		}
		identity, sequence, ok := parseLocalPath(filepath.ToSlash(rel))
		if !ok {
			return nil
		}
		if identity.Tower != tower {
			return nil
		}
		if beadID != "" && identity.BeadID != beadID {
			return nil
		}

		uri := "file://" + filepath.ToSlash(path)
		// Look up first — Reconcile is idempotent. If the manifest
		// row already exists, leave it alone.
		existing, err := pkgstore.GetLogArtifactByIdentity(ctx, s.db,
			identity.BeadID, identity.AttemptID, identity.RunID,
			identity.AgentName, string(identity.Role), identity.Phase,
			identity.Provider, string(identity.Stream), sequence,
		)
		if err != nil {
			return nil
		}
		if existing != nil {
			out = append(out, recordToManifest(*existing))
			return nil
		}

		// Hash the file to fill in size/checksum.
		size, sum, hashErr := hashFile(path)
		if hashErr != nil {
			return nil
		}

		rec := pkgstore.LogArtifactRecord{
			Tower:     identity.Tower,
			BeadID:    identity.BeadID,
			AttemptID: identity.AttemptID,
			RunID:     identity.RunID,
			AgentName: identity.AgentName,
			Role:      string(identity.Role),
			Phase:     identity.Phase,
			Provider:  identity.Provider,
			Stream:    string(identity.Stream),
			Sequence:  sequence,
			ObjectURI: uri,
			ByteSize:  &size,
			Checksum:  "sha256:" + sum,
			Status:    pkgstore.LogArtifactStatusFinalized,
		}
		id, insErr := pkgstore.InsertLogArtifact(ctx, s.db, rec)
		if insErr != nil {
			// Race with a parallel writer; pull the now-existing row.
			if errors.Is(insErr, pkgstore.ErrLogArtifactExists) {
				existing, _ = pkgstore.GetLogArtifactByIdentity(ctx, s.db,
					identity.BeadID, identity.AttemptID, identity.RunID,
					identity.AgentName, string(identity.Role), identity.Phase,
					identity.Provider, string(identity.Stream), sequence,
				)
				if existing != nil {
					out = append(out, recordToManifest(*existing))
				}
				return nil
			}
			return nil
		}
		rec.ID = id
		out = append(out, recordToManifest(rec))
		return nil
	})
	if walkErr != nil {
		return out, fmt.Errorf("logartifact: walk %s: %w", towerRoot, walkErr)
	}
	return out, nil
}

// hashFile streams a file through sha256 and returns its size and hex
// checksum. Used by Reconcile.
func hashFile(path string) (int64, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, "", err
	}
	defer f.Close()
	hasher := sha256.New()
	n, err := io.Copy(hasher, f)
	if err != nil {
		return 0, "", err
	}
	return n, hex.EncodeToString(hasher.Sum(nil)), nil
}

// parseLocalPath inverts BuildObjectKey for the local backend. The
// relative path produced by BuildObjectKey("",...) has the shape
//
//	<tower>/<bead>/<attempt>/<run>/<agent>/<role>/<phase>[/<provider>]/<stream>[-<seq>].jsonl
//
// with 8 or 9 segments depending on whether a provider was set. The
// parser returns ok=false on any structural mismatch; callers treat
// that as "not a canonical artifact".
func parseLocalPath(rel string) (Identity, int, bool) {
	parts := strings.Split(rel, "/")
	if len(parts) != 8 && len(parts) != 9 {
		return Identity{}, 0, false
	}
	leaf := parts[len(parts)-1]
	if !strings.HasSuffix(leaf, ".jsonl") {
		return Identity{}, 0, false
	}
	stem := strings.TrimSuffix(leaf, ".jsonl")
	stream := stem
	sequence := 0
	if dash := strings.LastIndex(stem, "-"); dash >= 0 {
		// stream-N is only a valid chunked artifact when the suffix
		// is purely numeric. Stream values like "stderr" never have
		// a dash, so a non-numeric suffix means we mis-parsed.
		if seq, err := parsePositiveInt(stem[dash+1:]); err == nil {
			stream = stem[:dash]
			sequence = seq
		}
	}

	id := Identity{
		Tower:     parts[0],
		BeadID:    parts[1],
		AttemptID: parts[2],
		RunID:     parts[3],
		AgentName: parts[4],
		Role:      Role(parts[5]),
		Phase:     parts[6],
		Stream:    Stream(stream),
	}
	if len(parts) == 9 {
		id.Provider = parts[7]
	}
	if id.Validate() != nil {
		return Identity{}, 0, false
	}
	return id, sequence, true
}

// parsePositiveInt returns n when s is a non-empty decimal string with a
// non-negative value. Used by parseLocalPath to parse the optional
// `-N` chunk suffix.
func parsePositiveInt(s string) (int, error) {
	if s == "" {
		return 0, errors.New("empty")
	}
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("non-digit %q", r)
		}
		n = n*10 + int(r-'0')
	}
	return n, nil
}

// localPathFromURI converts a file://... URI to an absolute filesystem
// path. Returns ErrNotFound for URIs that don't parse as file:// — the
// manifest stores `gs://` for GCS-managed artifacts and the local
// backend can't resolve those.
func localPathFromURI(uri string) (string, error) {
	if !strings.HasPrefix(uri, "file://") {
		return "", fmt.Errorf("logartifact: not a local URI: %s", uri)
	}
	parsed, err := url.Parse(uri)
	if err != nil {
		return "", fmt.Errorf("logartifact: parse uri %q: %w", uri, err)
	}
	return filepath.FromSlash(parsed.Path), nil
}

// insertOrFetchManifest writes a writing-status row for the identity
// tuple at the safe-default visibility (engineer_only), or returns the
// ID of the existing row if one already exists and is not yet finalized.
// Finalized rows can't be re-Put without going through Reconcile / a
// new sequence.
//
// Kept for callers (Reconcile) that have no visibility to declare; the
// upload path uses insertOrFetchManifestWithVisibility so the manifest
// records the caller's intent.
func insertOrFetchManifest(ctx context.Context, db *sql.DB, identity Identity, sequence int, objectURI string) (string, error) {
	return insertOrFetchManifestWithVisibility(ctx, db, identity, sequence, objectURI, VisibilityEngineerOnly)
}

// insertOrFetchManifestWithVisibility is the visibility-aware variant
// used by the Put path. The visibility is recorded on the manifest row
// at insert time so a parallel reader can see the caller's intent
// without waiting for Finalize to complete.
func insertOrFetchManifestWithVisibility(ctx context.Context, db *sql.DB, identity Identity, sequence int, objectURI string, visibility Visibility) (string, error) {
	rec := pkgstore.LogArtifactRecord{
		Tower:      identity.Tower,
		BeadID:     identity.BeadID,
		AttemptID:  identity.AttemptID,
		RunID:      identity.RunID,
		AgentName:  identity.AgentName,
		Role:       string(identity.Role),
		Phase:      identity.Phase,
		Provider:   identity.Provider,
		Stream:     string(identity.Stream),
		Sequence:   sequence,
		ObjectURI:  objectURI,
		Status:     pkgstore.LogArtifactStatusWriting,
		Visibility: string(visibility),
	}
	id, err := pkgstore.InsertLogArtifact(ctx, db, rec)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, pkgstore.ErrLogArtifactExists) {
		return "", fmt.Errorf("logartifact: insert manifest: %w", err)
	}
	existing, lookupErr := pkgstore.GetLogArtifactByIdentity(ctx, db,
		identity.BeadID, identity.AttemptID, identity.RunID,
		identity.AgentName, string(identity.Role), identity.Phase,
		identity.Provider, string(identity.Stream), sequence,
	)
	if lookupErr != nil {
		return "", fmt.Errorf("logartifact: lookup existing manifest: %w", lookupErr)
	}
	if existing == nil {
		return "", fmt.Errorf("logartifact: manifest exists but lookup returned nil")
	}
	if existing.Status == pkgstore.LogArtifactStatusFinalized {
		return "", pkgstore.ErrLogArtifactExists
	}
	return existing.ID, nil
}

// resolveManifest fetches the manifest row for ref, preferring ref.ID
// when present and falling back to (object_uri lookup via list+filter)
// when only ObjectURI is set. Returns ErrNotFound when neither field
// resolves to a row.
func resolveManifest(ctx context.Context, db *sql.DB, ref ManifestRef) (*pkgstore.LogArtifactRecord, error) {
	if ref.ID != "" {
		rec, err := pkgstore.GetLogArtifact(ctx, db, ref.ID)
		if err != nil {
			return nil, fmt.Errorf("logartifact: get manifest %s: %w", ref.ID, err)
		}
		if rec == nil {
			return nil, ErrNotFound
		}
		return rec, nil
	}
	if ref.ObjectURI == "" {
		return nil, fmt.Errorf("logartifact: ManifestRef requires ID or ObjectURI")
	}
	// Object-URI lookups are intentionally rare; the manifest row's
	// canonical address is its ID. We don't add a SQL helper for URI
	// lookup because callers should track the ID returned from Put or
	// from a previous Stat.
	return nil, fmt.Errorf("logartifact: ManifestRef.ID is required (got ObjectURI=%q)", ref.ObjectURI)
}

// listManifests dispatches a Filter to the appropriate pkg/store helper
// and converts the returned rows. The narrowest filter wins so we don't
// pull more rows than necessary: AttemptID > BeadID. Callers passing
// neither get an empty result rather than a full-table scan.
func listManifests(ctx context.Context, db *sql.DB, filter Filter) ([]Manifest, error) {
	switch {
	case filter.AttemptID != "":
		rows, err := pkgstore.ListLogArtifactsForAttempt(ctx, db, filter.AttemptID)
		if err != nil {
			return nil, err
		}
		return filterManifestRows(recordsToManifests(rows), filter), nil
	case filter.BeadID != "":
		rows, err := pkgstore.ListLogArtifactsForBead(ctx, db, filter.BeadID)
		if err != nil {
			return nil, err
		}
		return filterManifestRows(recordsToManifests(rows), filter), nil
	default:
		return nil, fmt.Errorf("logartifact: Filter requires BeadID or AttemptID")
	}
}

// filterManifestRows applies in-memory secondary filters (run, agent)
// after pkg/store has narrowed by bead/attempt. The expected dataset
// per bead is small (handful of artifacts per attempt), so doing this
// in memory is cheaper than SQL-level fan-out.
func filterManifestRows(rows []Manifest, filter Filter) []Manifest {
	if filter.RunID == "" && filter.AgentName == "" && filter.BeadID == "" {
		return rows
	}
	out := rows[:0]
	for _, r := range rows {
		if filter.BeadID != "" && r.Identity.BeadID != filter.BeadID {
			continue
		}
		if filter.RunID != "" && r.Identity.RunID != filter.RunID {
			continue
		}
		if filter.AgentName != "" && r.Identity.AgentName != filter.AgentName {
			continue
		}
		out = append(out, r)
	}
	return out
}
