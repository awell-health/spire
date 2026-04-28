// Package runctx is the path-derivation and best-effort writer layer for
// agent log artifacts. It is the contract that the cluster log exporter
// (spi-k1cnof) and gateway log API (spi-j3r694) consume.
//
// The package composes two existing types instead of duplicating fields:
//
//   - runtime.RunContext (pkg/runtime): the canonical worker identity
//     (tower, prefix, bead, attempt, run, agent name, role, formula
//     step, ...) propagated into every spawned agent via SPIRE_* env
//     vars and read back with runtime.RunContextFromEnv.
//   - logartifact.Identity (pkg/logartifact): the artifact identity
//     tuple persisted into the agent_log_artifacts manifest table.
//
// LogPaths takes a RunContext + a log root and derives:
//
//   - operational log path:  <root>/<tower>/<bead>/<attempt>/<run>/<agent>/<role>/<phase>/operational.log
//   - transcript file path:  <root>/<tower>/<bead>/<attempt>/<run>/<agent>/<role>/<phase>/<provider>/<stream>.jsonl
//
// The transcript shape mirrors logartifact.BuildObjectKey byte-for-byte
// so the same on-disk layout is the source for the local artifact
// backend (pkg/logartifact.LocalStore.Reconcile) and the cluster
// exporter (which uploads files at the same key into GCS). Sequence > 0
// chunked artifacts add a `-N` suffix the way BuildObjectKey does.
//
// AsyncFile returns an io.WriteCloser that buffers writes through a
// dedicated goroutine so a slow or failing log device never blocks the
// agent's critical path. Best-effort, not guaranteed: a sustained burst
// past the buffer drops bytes (visible as a counter on the writer) and
// the agent keeps going. This is the contract called out in the task
// description: "best-effort and non-blocking for agent execution, while
// preserving errors as visible operational events."
//
// Local-native uses runtime.LogRoot resolved from the active spire data
// directory (see DefaultLocalRoot below); cluster-native pods read the
// SPIRE_LOG_ROOT env var that the pod builders inject. The same path
// schema applies in both.
package runctx
