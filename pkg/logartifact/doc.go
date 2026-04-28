// Package logartifact provides the substrate for bead-scoped log
// artifacts: the byte store (local filesystem or GCS), a small Store
// interface that uniformly streams writes and reads, and the manifest
// shape that pkg/store persists alongside.
//
// Design context lives in design bead spi-7wzwk2 and substrate bead
// spi-b986in. The short version:
//
//   - In cluster mode, transcripts and wizard logs eventually land in
//     GCS so they survive pod eviction.
//   - In local mode, they continue to live under the wizard data
//     directory, but go through the same Store interface so callers
//     don't branch on deployment topology.
//   - Dolt holds the manifest (identity, pointers, checksums, bounded
//     summaries) — never the raw bytes. ZFC: tower state is small.
//
// This package is the substrate only. It does not wire callers
// (wizards, exporters, gateway, board, `spire logs pretty`); that
// happens in dependent beads spi-egw26j, spi-k1cnof, spi-j3r694.
//
// Object naming is a contract. BuildObjectKey is exported and pure so
// the local backend, the GCS backend, the exporter, and the gateway
// all derive the same key from the same Identity. Pod names, node
// names, replica indices, and wall-clock timestamps must never appear
// in the key — only stable Spire identity (tower, bead, attempt, run,
// agent, role, phase, provider, stream, sequence).
package logartifact
