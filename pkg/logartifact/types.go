package logartifact

import (
	"errors"
	"fmt"
	"time"
)

// Status is the lifecycle state of a log artifact. It mirrors the values
// stored in agent_log_artifacts.status (pkg/store).
type Status string

const (
	// StatusWriting means the artifact has a manifest row and bytes are
	// being streamed. Size and checksum are not yet finalized.
	StatusWriting Status = "writing"

	// StatusFinalized means the artifact's bytes are committed, the size
	// and checksum are recorded, and the row is the canonical manifest
	// for that identity.
	StatusFinalized Status = "finalized"

	// StatusFailed means the writer crashed or the upload failed and the
	// artifact is not safe to read. Failed rows still occupy the unique
	// identity slot so a subsequent retry must use a fresh sequence.
	StatusFailed Status = "failed"
)

// Valid reports whether s is one of the known status values.
func (s Status) Valid() bool {
	switch s {
	case StatusWriting, StatusFinalized, StatusFailed:
		return true
	default:
		return false
	}
}

// Stream is the kind of byte stream the artifact captures.
type Stream string

const (
	StreamStdout     Stream = "stdout"
	StreamStderr     Stream = "stderr"
	StreamTranscript Stream = "transcript"
)

// Valid reports whether s is one of the known stream values.
func (s Stream) Valid() bool {
	switch s {
	case StreamStdout, StreamStderr, StreamTranscript:
		return true
	default:
		return false
	}
}

// Role is the agent role producing the log artifact.
type Role string

const (
	RoleWizard     Role = "wizard"
	RoleApprentice Role = "apprentice"
	RoleSage       Role = "sage"
	RoleCleric     Role = "cleric"
	RoleArbiter    Role = "arbiter"
)

// Valid reports whether r is one of the known role values.
func (r Role) Valid() bool {
	switch r {
	case RoleWizard, RoleApprentice, RoleSage, RoleCleric, RoleArbiter:
		return true
	default:
		return false
	}
}

// Identity is the stable, deployment-independent address of a single log
// artifact's identity tuple. The (tower, bead, attempt, run, agent, role,
// phase, provider, stream) tuple plus the sequence index pin the artifact
// to a specific Spire execution. Pod names, node names, and wall-clock
// timestamps must never appear here — those are deployment artefacts and
// would break the contract with the gateway and exporter (spi-j3r694,
// spi-k1cnof) that compute the same object key independently.
//
// Provider may be empty for non-provider streams (e.g. wizard operational
// stdout/stderr). All other fields are required for a canonical artifact;
// callers that produce an artifact tied to no specific bead/attempt
// should not use this package.
type Identity struct {
	Tower     string
	BeadID    string
	AttemptID string
	RunID     string
	AgentName string
	Role      Role
	Phase     string
	Provider  string
	Stream    Stream
}

// Validate rejects identities with empty required fields or values that
// would not survive serialization into an object key. It does not check
// for unknown roles/streams — Role.Valid and Stream.Valid handle that
// when callers want strict checking.
func (id Identity) Validate() error {
	required := map[string]string{
		"tower":      id.Tower,
		"bead_id":    id.BeadID,
		"attempt_id": id.AttemptID,
		"run_id":     id.RunID,
		"agent_name": id.AgentName,
		"role":       string(id.Role),
		"phase":      id.Phase,
		"stream":     string(id.Stream),
	}
	for name, value := range required {
		if value == "" {
			return fmt.Errorf("logartifact: identity field %s is required", name)
		}
	}
	return nil
}

// Manifest is the in-memory shape of a single agent_log_artifacts row.
// pkg/store owns the persistence shape (LogArtifactRecord); this is the
// domain projection the Store interface returns. Conversions between the
// two live in store.go to keep pkg/logartifact's import surface small.
type Manifest struct {
	ID               string
	Identity         Identity
	Sequence         int
	ObjectURI        string
	ByteSize         int64
	Checksum         string // sha256:<lowercase-hex>; empty until finalized
	Status           Status
	StartedAt        time.Time
	EndedAt          time.Time
	CreatedAt        time.Time
	UpdatedAt        time.Time
	RedactionVersion int
	Summary          string
	Tail             string
}

// ManifestRef is the pointer callers pass to Get/Stat. ID is preferred —
// it uniquely addresses the manifest row and survives object renames at
// the byte-store layer. ObjectURI is accepted for callers that already
// hold a URI from a previous Put; backends resolve it back to the
// underlying object.
type ManifestRef struct {
	ID        string
	ObjectURI string
}

// Filter narrows List queries by Spire identity. Empty fields mean "any".
// Backends translate these into manifest table queries; they never list
// the byte store directly because the manifest is the index of record
// (see design spi-7wzwk2).
type Filter struct {
	BeadID    string
	AttemptID string
	RunID     string
	AgentName string
}

// ErrNotFound is returned when a Get/Stat call resolves to no manifest
// row. Backends mirror this so callers don't need to discriminate
// between local-vs-GCS error shapes.
var ErrNotFound = errors.New("logartifact: artifact not found")
