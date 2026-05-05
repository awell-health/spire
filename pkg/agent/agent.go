// Package agent provides backend-agnostic agent invocation: spawning, listing,
// log streaming, and lifecycle management. Implementations exist for local OS
// processes and Docker containers.
//
// The key abstractions:
//   - Backend  — unified adapter for any execution environment (process, docker, k8s)
//   - Handle   — lifecycle handle for a single running agent
//   - Spawner  — subset of Backend that only creates agents
//   - Registry — file-locked wizard registry for tracking local agents
package agent

import (
	"context"
	"errors"
	"io"
	"os"
	"time"

	"github.com/awell-health/spire/pkg/runtime"
)

// Handle represents a running agent, regardless of execution backend.
// Implementations exist for process (local) and Docker.
type Handle interface {
	// Wait blocks until the agent exits. Returns nil on clean exit.
	Wait() error

	// Signal sends a signal to the agent.
	// Process: os signal. Docker: stop/kill.
	Signal(os.Signal) error

	// Alive returns true if the agent is still running.
	Alive() bool

	// Name returns the agent's configured name.
	Name() string

	// Identifier returns a backend-specific opaque identifier.
	// Process: PID as string. Docker: container ID.
	// Callers should not parse this — use Alive/Signal for lifecycle.
	Identifier() string
}

// Spawner creates agent processes on a specific backend.
type Spawner interface {
	Spawn(cfg SpawnConfig) (Handle, error)
}

// Backend is the unified adapter for agent execution environments.
// Every consumer (steward, wizard, status, logs) programs against this
// interface. Implementations exist for process and docker.
//
// Backend is a superset of Spawner — any function accepting
// Spawner also accepts Backend.
type Backend interface {
	// Spawn starts an agent. Returns a handle for per-agent lifecycle.
	Spawn(cfg SpawnConfig) (Handle, error)

	// List returns all agents tracked by this backend.
	List() ([]Info, error)

	// Logs returns a log stream for the named agent.
	// Returns os.ErrNotExist if no logs are available.
	Logs(name string) (io.ReadCloser, error)

	// Kill force-stops an agent by name (when no handle is available).
	Kill(name string) error

	// TerminateBead stops every runtime worker the backend has spawned
	// for the given bead and reports an error if any survive.
	//
	// The contract is bead-scoped on purpose: reset / unsummon must reap
	// the parent wizard AND every nested apprentice / sage / cleric
	// worker AND any provider subprocess (claude, codex) descended from
	// them. Signalling only the registered parent PID lets detached
	// children survive — that's the spi-w65pr1 bug.
	//
	// Backend implementations:
	//   - ProcessBackend signals each registered entry's recorded PGID
	//     (SIGTERM, 5s grace, SIGKILL) so the whole process group goes
	//     down regardless of parent-child reparenting.
	//   - K8sBackend / cluster operator: deletes every pod owned by the
	//     bead/attempt label (filed as spd-1lu5; stub returns
	//     ErrTerminateBeadNotImplemented for now).
	//   - DockerBackend: not yet implemented; returns
	//     ErrTerminateBeadNotImplemented.
	//
	// Returns nil when no processes are owned by the bead (idempotent).
	// Returns a non-nil error when at least one bead-scoped worker
	// survived termination, so callers (reset) can fail closed and
	// surface a manual-cleanup message.
	TerminateBead(ctx context.Context, beadID string) error
}

// ErrTerminateBeadNotImplemented is returned by Backend.TerminateBead
// implementations that have not yet been wired (cluster mode is filed
// as spd-1lu5; docker has no current consumer that needs it). Callers
// that wrap multiple backends can errors.Is against this to decide
// whether to fall back or surface a hard error.
var ErrTerminateBeadNotImplemented = errors.New("agent: TerminateBead not implemented for this backend")

// Info is the backend-agnostic view of a running or recently-run agent.
type Info struct {
	Name       string    // agent name (e.g. "wizard-spi-abc")
	BeadID     string    // bead being worked on
	Phase      string    // current phase (e.g. "implement", "review")
	Alive      bool      // true if the agent is still running
	Identifier string    // opaque: PID, container ID, pod name
	StartedAt  time.Time // when the agent was started
	Tower      string    // tower this agent belongs to
}

// SpawnRole describes what kind of agent to run.
// Each backend maps this to its own execution mechanism.
//
// The canonical definition now lives in pkg/runtime so that
// runtime.RunContext.Role can reference it without forcing
// pkg/runtime → pkg/agent (which would re-open the cycle resolved by
// pkg/runtime). Existing agent.SpawnRole / agent.RoleX call sites
// continue to work unchanged through this alias + const re-export.
type SpawnRole = runtime.SpawnRole

const (
	// RoleApprentice is a per-subtask implementer (apprentice run).
	RoleApprentice = runtime.RoleApprentice

	// RoleSage is a per-review agent (sage review).
	RoleSage = runtime.RoleSage

	// RoleWizard is the executor — handles full workflow for all workload types.
	RoleWizard = runtime.RoleWizard

	// RoleExecutor is a formula-driven executor (execute).
	RoleExecutor = runtime.RoleExecutor

	// RoleCleric is a one-shot recovery agent — proposes a recovery
	// action as JSON for human review. Cleric runtime (spi-hhkozk).
	RoleCleric = runtime.RoleCleric
)

// SpawnConfig describes the intent for spawning an agent.
// Backend-agnostic — each spawner translates this to its own mechanism.
type SpawnConfig struct {
	Name          string    // Agent name (e.g. "apprentice-spi-1dl-0")
	BeadID        string    // Bead to work on — injected as SPIRE_BEAD_ID into subprocess env
	Role          SpawnRole // What kind of agent to run
	Tower         string    // Tower name — injected as SPIRE_TOWER into subprocess env
	InstanceID    string    // Instance identity of the spawning steward
	Provider      string    // AI provider override (claude, codex, cursor) — injected as SPIRE_PROVIDER into subprocess env
	Step          string    // Current step name (e.g. "implement", "review") — injected into OTEL_RESOURCE_ATTRIBUTES for log/trace correlation.
	ExtraArgs     []string  // Additional args (e.g. "--review-fix")
	LogPath       string    // Output destination (empty = inherit stderr)
	StartRef      string    // Git ref (SHA or branch) for the child worktree start point. Empty = use repo base branch.
	CustomPrompt  string    // Inline prompt from formula with.prompt — written to temp file and passed as --custom-prompt-file to subprocess.
	AttemptID     string    // Attempt bead ID created by the wizard for this spawn — injected as SPIRE_ATTEMPT_ID into subprocess env. Empty when the spawn is not a fresh attempt (e.g. review-fix re-engagement).
	ApprenticeIdx string    // Fan-out index of this apprentice within its wave (integer as string). Injected as SPIRE_APPRENTICE_IDX into subprocess env. "0" for single-apprentice spawns.

	// Repo bootstrap inputs — required by the k8s backend for RoleWizard pods
	// (see pkg/agent/backend_k8s buildWizardPod). A wizard pod has an empty
	// /workspace and no local instance binding; the repo-bootstrap init
	// container clones RepoURL@RepoBranch into /workspace/<RepoPrefix> and
	// invokes `spire repo bind-local` so wizard.ResolveRepo can find the
	// local path. Other backends (process, docker) and other roles ignore
	// these fields.
	RepoURL    string // git remote URL for the bead's repo prefix
	RepoBranch string // default branch to clone for bootstrap
	RepoPrefix string // bead prefix (e.g. "spi") — keys cfg.Instances[prefix]

	// Canonical runtime contract fields (docs/design/spi-xplwy-runtime-contract.md §1).
	//
	// These three fields replace the ad-hoc plumbing (RepoURL/RepoBranch/
	// RepoPrefix env reads, per-backend workspace mounting, and scattered
	// log/trace labels) as pkg/agent backends migrate to the canonical
	// contract. In this task (spi-b9tu3) the fields are populated by the
	// executor at dispatch but backends still read from the legacy fields
	// / env vars — later tasks in epic spi-xplwy migrate each backend to
	// read these authoritative values instead.
	Identity  runtime.RepoIdentity    // canonical repo identity for the run
	Workspace *runtime.WorkspaceHandle // materialized workspace substrate (nil if none)
	Run       runtime.RunContext      // observability identity (tower/prefix/bead/attempt/role/step/backend/workspace/handoff)

	// SharedWorkspace, when true, signals that the wizard pod's /workspace
	// mount must be backed by a per-wizard PersistentVolumeClaim (named by
	// OwningWizardPVCName(podName)) instead of an emptyDir. The PVC itself
	// is provisioned by the operator's reconciler — the shared pod builder
	// only wires the volume reference. Children (apprentice/sage)
	// borrowed-worktree spawns continue to discover the PVC via the
	// spire.io/owning-wizard-pod label selector (see resolveWorkspaceVolume).
	SharedWorkspace bool

	// DetachFromParent declares that the caller will NOT Handle.Wait() and
	// the spawned process must survive parent exit. Fire-and-forget callers
	// like `spire summon` set this true; synchronous callers (wizard pod
	// spawning apprentice/sage, which Wait on the subprocess) leave it false.
	//
	// When true, ProcessSpawner routes stdout/stderr directly to the log
	// *os.File (Go's exec.Cmd uses dup2 for *os.File sinks, so no parent-
	// side pipe or forwarder goroutine is created). It also sets
	// SysProcAttr.Setpgid so the child is not killed by SIGHUP when the
	// parent's controlling terminal closes.
	//
	// When false (default), ProcessSpawner keeps the tee to the log file +
	// parent stderr via an in-parent goroutine so cluster-native wizard
	// pods surface subprocess output in `kubectl logs` (spi-fxfq5f). That
	// goroutine is joined by Handle.Wait(); callers that don't Wait and
	// don't set DetachFromParent will truncate the log when the parent
	// exits.
	DetachFromParent bool

	// AuthEnv, when non-nil, overrides the inherited Anthropic credential
	// env vars on the spawned process. It is the env-var slice produced by
	// AuthContext.InjectEnv applied to a base environment, kept here as a
	// flat []string so pkg/agent backends can stay free of pkg/config
	// import cycles. Nil means "inherit whatever is in the parent process
	// env" (legacy behavior).
	AuthEnv []string

	// AuthSlot is the slot name (subscription | api-key) of AuthEnv. Used
	// by observability sites to tag agent_runs rows. Empty when AuthEnv is
	// nil.
	AuthSlot string

	// PoolStateDir is the directory holding the multi-token auth pool's
	// per-slot state JSON files. When set alongside AuthSlot, the spawned
	// subprocess receives SPIRE_AUTH_POOL_STATE_DIR + SPIRE_AUTH_SLOT in
	// its env so the in-process rate-limit-event sink can apply
	// rate_limit_event JSONL lines from the claude stream back to the
	// slot's <stateDir>/<slot>.json. Empty when no pool is configured —
	// the legacy single-token path leaves it untouched.
	PoolStateDir string
}

// NewSpawner returns a Spawner for the given backend.
// Supported: "process" (default), "docker".
//
// Deprecated: Use ResolveBackend instead, which returns the full Backend
// interface (superset of Spawner). NewSpawner now delegates to
// ResolveBackend internally.
func NewSpawner(backend string) Spawner {
	return ResolveBackend(backend)
}
