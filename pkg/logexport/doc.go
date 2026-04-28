// Package logexport is the passive cluster log exporter chosen by design
// spi-7wzwk2 and built by bead spi-k1cnof.
//
// The exporter has a narrow contract: tail the shared log directory under
// SPIRE_LOG_ROOT, emit one structured JSON line per record to stdout for
// Cloud Logging, and upload completed artifacts via pkg/logartifact.Store
// (local filesystem or GCS) while recording manifest rows in the tower's
// agent_log_artifacts table.
//
// The exporter is passive. It does NOT process messages, dispatch work,
// create or close beads, mutate workflow state, or run any control-plane
// loop. The only state it touches is the tower's log artifact manifest
// table — the same write surface the in-process logartifact.Store uses.
// This is the explicit guardrail from the design: a fatter sidecar would
// resurrect the old "familiar" control sidecar that the cluster
// architecture removed.
//
// Two implementations sit behind the Exporter interface:
//
//   - Sidecar binary (cmd/spire-log-exporter): runs in its own container
//     inside wizard / apprentice / sage / cleric / arbiter pods, sharing
//     a spire-logs emptyDir with the agent. Preferred for cluster-grade
//     deployments because it isolates exporter failures from the agent
//     process.
//   - In-process exporter (pkg/logexport.RunInProcess): runs as a
//     goroutine inside the agent process. Used in local-native installs
//     and any cluster-as-truth deployment that prefers single-container
//     pods.
//
// Both run the same Tailer, StdoutSink, and Uploader stack.
//
// File-to-Identity inference: the exporter parses Identity from the
// shared log root path layout established by the capture contract
// (spi-egw26j) and pkg/runctx:
//
//	<root>/<tower>/<bead>/<attempt>/<run>/<agent>/<role>/<phase>/<provider>/<stream>.jsonl
//	<root>/<tower>/<bead>/<attempt>/<run>/<agent>/<role>/<phase>/operational.log
//
// Provider transcripts emit at the top schema; wizard/apprentice
// operational logs at the bottom schema. The exporter never invents
// identity from pod names, hostnames, or wall-clock time — the path is
// the single source of truth.
//
// Failure independence: a manifest insert error or upload failure marks
// the affected artifact's row with status=failed and emits a structured
// stderr log at severity=ERROR. The exporter process itself exits 0; the
// agent's success/failure verdict is unaffected. This is the explicit
// acceptance criterion from spi-k1cnof.
package logexport
