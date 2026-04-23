package agent

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
)

// ErrWorkspacePathMissing is returned by the process backend when
// cfg.Workspace.Path is set but the directory does not exist on disk.
// The executor should re-materialize the workspace before re-dispatching.
var ErrWorkspacePathMissing = errors.New("process backend: cfg.Workspace.Path does not exist")

// ProcessSpawner spawns agents as local OS processes.
type ProcessSpawner struct{}

// ProcessHandle tracks a locally-spawned agent process.
type ProcessHandle struct {
	name    string
	cmd     *exec.Cmd
	logFile *os.File
	exited  atomic.Bool
}

// NewProcessHandle creates a ProcessHandle wrapping an already-started exec.Cmd.
// Used by tests that need to control the underlying process directly.
func NewProcessHandle(name string, cmd *exec.Cmd) *ProcessHandle {
	return &ProcessHandle{name: name, cmd: cmd}
}

func (s *ProcessSpawner) Spawn(cfg SpawnConfig) (Handle, error) {
	// Validate the workspace substrate exists on disk before spawning.
	// The worker will --worktree-dir into this path; launching a
	// process whose workspace does not exist produces confusing
	// downstream errors. cfg.Workspace is optional during the
	// migration window (not every dispatch site populates it yet);
	// when nil we preserve the pre-spi-wqax9 behavior.
	if cfg.Workspace != nil && cfg.Workspace.Path != "" {
		if stat, err := os.Stat(cfg.Workspace.Path); err != nil || !stat.IsDir() {
			return nil, fmt.Errorf("%w: %s (bead %s)", ErrWorkspacePathMissing, cfg.Workspace.Path, cfg.BeadID)
		}
	}

	spireBin, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("find spire binary: %w", err)
	}

	// Map role to spire subcommand.
	subcmd, err := roleToSubcmd(cfg.Role)
	if err != nil {
		return nil, err
	}

	args := append([]string{}, subcmd...)
	args = append(args, cfg.BeadID, "--name", cfg.Name)
	if cfg.StartRef != "" {
		args = append(args, "--start-ref", cfg.StartRef)
	}

	// Write custom prompt to a temp file to avoid arg-length limits and shell
	// escaping issues with multi-line prompts. The wizard subprocess reads and
	// removes the file after parsing.
	if cfg.CustomPrompt != "" {
		f, err := os.CreateTemp("", "spire-prompt-*.txt")
		if err != nil {
			return nil, fmt.Errorf("write custom prompt temp file: %w", err)
		}
		if _, err := f.WriteString(cfg.CustomPrompt); err != nil {
			f.Close()
			os.Remove(f.Name())
			return nil, fmt.Errorf("write custom prompt: %w", err)
		}
		f.Close()
		args = append(args, "--custom-prompt-file", f.Name())
	}

	args = append(args, cfg.ExtraArgs...)

	cmd := exec.Command(spireBin, args...)
	cmd.Env = os.Environ()
	applyProcessEnv(cmd, cfg)

	logFile := teeSpawnOutput(cmd, cfg.LogPath, os.Stderr, cfg.DetachFromParent)
	if cfg.DetachFromParent {
		applyDetachAttrs(cmd)
	}

	if err := cmd.Start(); err != nil {
		if logFile != nil {
			logFile.Close()
		}
		return nil, err
	}

	// When cmd.Stdout/Stderr is an io.Writer (not an *os.File), exec.Cmd
	// spawns a copy goroutine that forwards the child's pipe to our
	// writer, and that goroutine is only joined inside cmd.Wait(). We
	// therefore cannot close logFile until Wait() returns — do it in
	// ProcessHandle.Wait().
	return &ProcessHandle{name: cfg.Name, cmd: cmd, logFile: logFile}, nil
}

// Wait blocks until the process exits.
func (h *ProcessHandle) Wait() error {
	err := h.cmd.Wait()
	h.exited.Store(true)
	if h.logFile != nil {
		h.logFile.Close()
	}
	return err
}

// Signal sends a signal to the process.
func (h *ProcessHandle) Signal(sig os.Signal) error {
	if h.exited.Load() {
		return fmt.Errorf("process already exited")
	}
	if h.cmd.Process == nil {
		return fmt.Errorf("process not started")
	}
	return h.cmd.Process.Signal(sig)
}

// Alive returns true if the process is still running.
func (h *ProcessHandle) Alive() bool {
	if h.exited.Load() {
		return false
	}
	if h.cmd.Process == nil {
		return false
	}
	return h.cmd.Process.Signal(syscall.Signal(0)) == nil
}

// Name returns the agent name.
func (h *ProcessHandle) Name() string { return h.name }

// Identifier returns the PID as a string.
func (h *ProcessHandle) Identifier() string {
	if h.cmd.Process != nil {
		return strconv.Itoa(h.cmd.Process.Pid)
	}
	return ""
}

// teeSpawnOutput wires cmd.Stdout and cmd.Stderr so subprocess output
// lands on both the LogPath file (preserves the local-native artifact
// pairing used by the wizard plan inspector) and stderrSink (becomes
// PID 1's stderr in-cluster, which kubelet retains via `kubectl logs`
// even after the wizard pod is GC'd).
//
// Returns the opened log file so the caller can close it after
// cmd.Wait() joins the copy goroutine that exec.Cmd spawns when Stdout
// is a generic io.Writer. Returns nil if logPath is empty or the file
// could not be opened; in either case output still reaches stderrSink.
// teeSpawnOutput wires cmd.Stdout/Stderr for a spawned subprocess.
//
// Two shapes depending on the caller's lifetime contract:
//
//   - detach=false (default, synchronous callers that will Handle.Wait()):
//     stdout/stderr are tee'd to both the log file and stderrSink via an
//     in-parent io.Writer. exec.Cmd creates a pipe per stream and a
//     forwarder goroutine that io.Copy's pipe→writer. The goroutine is
//     joined inside cmd.Wait(). This surfaces subprocess output on the
//     parent's stderr so cluster-native wizard pods get kubectl-logs
//     visibility (spi-fxfq5f).
//
//   - detach=true (fire-and-forget callers like `spire summon`):
//     stdout/stderr point directly at the log *os.File. exec.Cmd
//     recognizes the concrete *os.File type and uses dup2 at fork time
//     to give the child its own fd — no pipe, no forwarder goroutine, no
//     dependency on the parent's lifetime. stderrSink is not tee'd; the
//     parent exits immediately after Start and there would be nothing to
//     tee through. A caller that sets DetachFromParent but still wants
//     parent-stderr tee should keep Waiting instead.
//
// Returns the opened log file so the caller can close it after
// cmd.Wait() joins the forwarder goroutine (synchronous path) or
// after Start (detach path — the fd has already been dup'd into the
// child, closing our handle does not affect the child's copy).
func teeSpawnOutput(cmd *exec.Cmd, logPath string, stderrSink io.Writer, detach bool) *os.File {
	var logFile *os.File
	if logPath != "" {
		os.MkdirAll(filepath.Dir(logPath), 0755)
		f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
		if err != nil {
			fmt.Fprintf(stderrSink, "spawn: open log %s failed, falling back to stderr-only: %v\n", logPath, err)
		} else {
			logFile = f
		}
	}

	if detach {
		// Detach path: dup the log fd directly into the child. When
		// logFile is nil (logPath empty or open failed) we cannot tee
		// to a parent-side writer either — the parent will exit
		// immediately, so there's nothing to keep alive for stderrSink.
		// Fall through to stderrSink only as a last resort; the
		// subprocess's stderr may survive long enough to capture a
		// startup error if Start is slow.
		if logFile != nil {
			cmd.Stdout = logFile
			cmd.Stderr = logFile
		} else {
			cmd.Stdout = stderrSink
			cmd.Stderr = stderrSink
		}
		return logFile
	}

	// Attached path: tee to logFile + stderrSink via a parent-side
	// forwarder goroutine. io.MultiWriter is appropriate here because
	// both sinks are alive for the subprocess's lifetime.
	var w io.Writer = stderrSink
	if logFile != nil {
		w = io.MultiWriter(logFile, stderrSink)
	}
	cmd.Stdout = w
	cmd.Stderr = w
	return logFile
}

// applyProcessEnv injects all SpawnConfig-derived env vars into cmd.Env.
// Extracted from Spawn so tests can verify the config-to-env translation
// without actually starting a process.
//
// Resolution order when both a legacy field (cfg.Tower, cfg.Step) and
// the canonical equivalent on cfg.Identity / cfg.Run carry the same
// value, cfg values take precedence: the spawn-time config is the
// source of truth and overrides whatever the caller inherited from its
// own process env. During the migration window, either shape is
// acceptable; once every dispatch site populates Identity/Run the
// legacy fields can be removed.
func applyProcessEnv(cmd *exec.Cmd, cfg SpawnConfig) {
	tower := cfg.Tower
	if tower == "" {
		tower = cfg.Identity.TowerName
	}
	if tower == "" {
		tower = cfg.Run.TowerName
	}
	if tower != "" {
		setEnv(cmd, "SPIRE_TOWER", tower)
	}
	if cfg.Provider != "" {
		setEnv(cmd, "SPIRE_PROVIDER", cfg.Provider)
	}
	if cfg.Role != "" {
		setEnv(cmd, "SPIRE_ROLE", string(cfg.Role))
	}

	// Canonical repo identity — written unconditionally when cfg.Identity
	// carries the value. Local wizards running in process mode do not need
	// SPIRE_REPO_* for bootstrap (there is no init container) but pkg/wizard
	// and the apprentice read these to resolve which repo/prefix they are
	// working on. Writing them here keeps the contract uniform across
	// backends.
	if cfg.Identity.RepoURL != "" {
		setEnv(cmd, "SPIRE_REPO_URL", cfg.Identity.RepoURL)
	}
	prefix := cfg.Identity.Prefix
	if prefix == "" {
		prefix = cfg.RepoPrefix
	}
	if prefix != "" {
		setEnv(cmd, "SPIRE_REPO_PREFIX", prefix)
	}
	baseBranch := cfg.Identity.BaseBranch
	if baseBranch == "" {
		baseBranch = cfg.RepoBranch
	}
	if baseBranch != "" {
		setEnv(cmd, "SPIRE_REPO_BRANCH", baseBranch)
	}
	if cfg.Identity.TowerName != "" {
		setEnv(cmd, "BEADS_DATABASE", cfg.Identity.TowerName)
	}
	if prefix != "" {
		setEnv(cmd, "BEADS_PREFIX", prefix)
	}

	// Workspace handle — surfaces the path the spawned worker should
	// use. The existing ExtraArgs=["--worktree-dir", ...] flow remains
	// the authoritative plumbing; SPIRE_WORKSPACE_PATH duplicates it
	// for consumers that want an env read.
	if cfg.Workspace != nil && cfg.Workspace.Path != "" {
		setEnv(cmd, "SPIRE_WORKSPACE_PATH", cfg.Workspace.Path)
	}

	// RunContext fields (docs/design/spi-xplwy-runtime-contract.md §1.4).
	// Every canonical log-field value flows through env so the spawned
	// worker's runtime.RunContextFromEnv() reconstructs the full identity
	// set and stamps it on every log line. Missing values remain unset
	// (empty-string env) — RunContextFromEnv() treats absence as empty,
	// matching the "never drop the field" log-surface rule.
	if cfg.Run.FormulaStep != "" {
		setEnv(cmd, "SPIRE_FORMULA_STEP", cfg.Run.FormulaStep)
	} else if cfg.Step != "" {
		setEnv(cmd, "SPIRE_FORMULA_STEP", cfg.Step)
	}
	if cfg.Run.Backend != "" {
		setEnv(cmd, "SPIRE_BACKEND", cfg.Run.Backend)
	}
	if cfg.Run.WorkspaceKind != "" {
		setEnv(cmd, "SPIRE_WORKSPACE_KIND", string(cfg.Run.WorkspaceKind))
	} else if cfg.Workspace != nil && cfg.Workspace.Kind != "" {
		setEnv(cmd, "SPIRE_WORKSPACE_KIND", string(cfg.Workspace.Kind))
	}
	if cfg.Run.WorkspaceName != "" {
		setEnv(cmd, "SPIRE_WORKSPACE_NAME", cfg.Run.WorkspaceName)
	} else if cfg.Workspace != nil && cfg.Workspace.Name != "" {
		setEnv(cmd, "SPIRE_WORKSPACE_NAME", cfg.Workspace.Name)
	}
	if cfg.Run.WorkspaceOrigin != "" {
		setEnv(cmd, "SPIRE_WORKSPACE_ORIGIN", string(cfg.Run.WorkspaceOrigin))
	} else if cfg.Workspace != nil && cfg.Workspace.Origin != "" {
		setEnv(cmd, "SPIRE_WORKSPACE_ORIGIN", string(cfg.Workspace.Origin))
	}
	if cfg.Run.HandoffMode != "" {
		setEnv(cmd, "SPIRE_HANDOFF_MODE", string(cfg.Run.HandoffMode))
	}

	// Apprentice identity env vars. Transport-agnostic: the apprentice reads
	// them to resolve which bead to write to and what role to claim at
	// submit time.
	if cfg.BeadID != "" {
		setEnv(cmd, "SPIRE_BEAD_ID", cfg.BeadID)
	}
	attemptID := cfg.AttemptID
	if attemptID == "" {
		attemptID = cfg.Run.AttemptID
	}
	if attemptID != "" {
		setEnv(cmd, "SPIRE_ATTEMPT_ID", attemptID)
	}
	if cfg.ApprenticeIdx != "" {
		setEnv(cmd, "SPIRE_APPRENTICE_IDX", cfg.ApprenticeIdx)
	}
	if cfg.Run.RunID != "" {
		setEnv(cmd, "SPIRE_RUN_ID", cfg.Run.RunID)
	}

	// OTLP telemetry. The daemon's OTLP receiver listens on localhost:4317
	// (or SPIRE_OTLP_PORT).
	otlpPort := os.Getenv("SPIRE_OTLP_PORT")
	if otlpPort == "" {
		otlpPort = "4317"
	}
	setEnv(cmd, "OTEL_EXPORTER_OTLP_ENDPOINT", "http://localhost:"+otlpPort)

	// Resource attributes carry the canonical RunContext vocabulary
	// (docs/design/spi-xplwy-runtime-contract.md §1.4) so the OTLP
	// receiver can correlate spans/logs/metrics to beads without
	// post-hoc matching. The set mirrors runtime.LogFieldOrder — every
	// canonical log field also rides on every emitted span/log as a
	// resource attribute. Missing fields are omitted rather than emitted
	// blank to keep the attribute set compact on the wire.
	step := cfg.Step
	if step == "" {
		step = cfg.Run.FormulaStep
	}
	prefix = cfg.Identity.Prefix
	if prefix == "" {
		prefix = cfg.RepoPrefix
	}
	if prefix == "" {
		prefix = cfg.Run.Prefix
	}
	var resAttrs []string
	addAttr := func(k, v string) {
		if v == "" {
			return
		}
		resAttrs = append(resAttrs, k+"="+v)
	}
	// agent.name is kept for back-compat with existing alerts; the rest
	// are the canonical RunContext field names.
	if cfg.Name != "" {
		resAttrs = append(resAttrs, "agent.name="+cfg.Name)
	}
	addAttr("tower", tower)
	addAttr("prefix", prefix)
	addAttr("bead_id", cfg.BeadID)
	addAttr("attempt_id", attemptID)
	addAttr("run_id", cfg.Run.RunID)
	addAttr("role", string(cfg.Role))
	addAttr("formula_step", step)
	addAttr("backend", cfg.Run.Backend)
	if cfg.Run.WorkspaceKind != "" {
		addAttr("workspace_kind", string(cfg.Run.WorkspaceKind))
	} else if cfg.Workspace != nil {
		addAttr("workspace_kind", string(cfg.Workspace.Kind))
	}
	if cfg.Run.WorkspaceName != "" {
		addAttr("workspace_name", cfg.Run.WorkspaceName)
	} else if cfg.Workspace != nil {
		addAttr("workspace_name", cfg.Workspace.Name)
	}
	if cfg.Run.WorkspaceOrigin != "" {
		addAttr("workspace_origin", string(cfg.Run.WorkspaceOrigin))
	} else if cfg.Workspace != nil {
		addAttr("workspace_origin", string(cfg.Workspace.Origin))
	}
	addAttr("handoff_mode", string(cfg.Run.HandoffMode))
	if len(resAttrs) > 0 {
		setEnv(cmd, "OTEL_RESOURCE_ATTRIBUTES", strings.Join(resAttrs, ","))
	}

	// Claude Code: enable built-in OTel telemetry with trace export.
	if cfg.Provider == "" || cfg.Provider == "claude" {
		setEnv(cmd, "CLAUDE_CODE_ENABLE_TELEMETRY", "1")
		setEnv(cmd, "CLAUDE_CODE_ENHANCED_TELEMETRY_BETA", "1")
		setEnv(cmd, "OTEL_TRACES_EXPORTER", "otlp")
		setEnv(cmd, "OTEL_LOGS_EXPORTER", "otlp")
		setEnv(cmd, "OTEL_EXPORTER_OTLP_PROTOCOL", "grpc")
	}
}

// ApplyProcessEnvForTest is the exported entry point test code uses to
// exercise applyProcessEnv without actually starting a process. The
// test/parity suite asserts the full canonical SPIRE_* env vocabulary
// is present on cmd.Env after this call.
//
// Production callers MUST NOT use this — they get the env automatically
// via Spawn(). Exported only because the parity test lives in
// test/parity (outside pkg/agent) and Go does not export unexported
// identifiers across package boundaries.
func ApplyProcessEnvForTest(cmd *exec.Cmd, cfg SpawnConfig) {
	applyProcessEnv(cmd, cfg)
}

// setEnv sets or replaces an environment variable in cmd.Env.
func setEnv(cmd *exec.Cmd, key, value string) {
	prefix := key + "="
	for i, e := range cmd.Env {
		if strings.HasPrefix(e, prefix) {
			cmd.Env[i] = prefix + value
			return
		}
	}
	cmd.Env = append(cmd.Env, prefix+value)
}

// roleToSubcmd maps a SpawnRole to the spire subcommand argv tokens.
// Returns a slice so multi-word role-scoped subcommands (e.g. "apprentice run")
// can be spliced into the command line by each backend.
//
// RoleWizard and RoleExecutor both map to "execute": the in-pod command is
// `spire execute`, and the wizard identity lives in the enum (surfaced via the
// SPIRE_ROLE env var and role-specific pod spec / resources).
func roleToSubcmd(role SpawnRole) ([]string, error) {
	switch role {
	case RoleApprentice:
		return []string{"apprentice", "run"}, nil
	case RoleSage:
		return []string{"sage", "review"}, nil
	case RoleWizard:
		return []string{"execute"}, nil
	case RoleExecutor:
		return []string{"execute"}, nil
	default:
		return nil, fmt.Errorf("unknown spawn role: %q", role)
	}
}
