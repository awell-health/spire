package logartifact

import (
	"fmt"
	"strings"
)

// BuildObjectKey returns the canonical, deployment-independent object
// key for an Identity. The shape comes directly from the design bead
// spi-7wzwk2:
//
//	<prefix>/<tower>/<bead>/<attempt>/<run>/<agent>/<role>/<phase>/<provider>/<stream>[-<seq>].jsonl
//
// Sequence is suffixed only when nonzero; sequence=0 yields
// `<stream>.jsonl`. Provider is included when non-empty (claude, codex,
// …) and skipped otherwise — wizard operational streams have no
// provider segment. Each identity segment is sanitized so a malformed
// ID can't produce a key that escapes the prefix or collides with a
// neighbor; non-empty fields are required (see Identity.Validate).
//
// The function is pure and deterministic: same Identity → same key,
// across processes, hosts, and cluster restarts. Other beads (the
// exporter spi-k1cnof, the gateway spi-j3r694) compute the same key
// from the same Identity to find an artifact on GCS without needing a
// LIST call.
func BuildObjectKey(prefix string, identity Identity, sequence int) (string, error) {
	if err := identity.Validate(); err != nil {
		return "", err
	}
	if sequence < 0 {
		return "", fmt.Errorf("logartifact: sequence must be >= 0 (got %d)", sequence)
	}

	segments := []string{
		identity.Tower,
		identity.BeadID,
		identity.AttemptID,
		identity.RunID,
		identity.AgentName,
		string(identity.Role),
		identity.Phase,
	}
	if identity.Provider != "" {
		segments = append(segments, identity.Provider)
	}

	for i, seg := range segments {
		clean, err := sanitizeSegment(seg)
		if err != nil {
			return "", fmt.Errorf("logartifact: segment %d (%q): %w", i, seg, err)
		}
		segments[i] = clean
	}

	streamSeg, err := sanitizeSegment(string(identity.Stream))
	if err != nil {
		return "", fmt.Errorf("logartifact: stream segment (%q): %w", identity.Stream, err)
	}
	var leaf string
	if sequence == 0 {
		leaf = streamSeg + ".jsonl"
	} else {
		leaf = fmt.Sprintf("%s-%d.jsonl", streamSeg, sequence)
	}
	segments = append(segments, leaf)

	cleanPrefix := strings.Trim(prefix, "/")
	if cleanPrefix != "" {
		segments = append([]string{cleanPrefix}, segments...)
	}
	return strings.Join(segments, "/"), nil
}

// sanitizeSegment rejects path-traversal sequences and characters that
// would break the canonical key shape on either filesystem or GCS:
//
//   - Empty strings are rejected (an empty segment would produce
//     consecutive slashes that some backends collapse and some don't).
//   - "..", leading/trailing dots, and embedded slashes are rejected
//     so a malformed identity can't escape the per-tower prefix.
//   - Backslashes and null bytes are rejected because they round-trip
//     ambiguously through path/filepath on Windows-aware tools.
//
// Any control character below ASCII space is rejected. Whitespace
// inside an identity is allowed but discouraged — agent names with
// spaces are pathological, but rejecting them outright would create a
// dependency on uppercase rules we don't want to enforce here.
func sanitizeSegment(seg string) (string, error) {
	if seg == "" {
		return "", fmt.Errorf("empty segment")
	}
	if seg == "." || seg == ".." {
		return "", fmt.Errorf("dot segment %q", seg)
	}
	if strings.ContainsAny(seg, "/\\\x00") {
		return "", fmt.Errorf("contains forbidden character")
	}
	for _, r := range seg {
		if r < 0x20 {
			return "", fmt.Errorf("contains control character")
		}
	}
	return seg, nil
}
