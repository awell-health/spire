package runctx

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/awell-health/spire/pkg/logartifact"
	"github.com/awell-health/spire/pkg/runtime"
)

// EnvLogRoot is the env var pod builders set on agent containers to
// declare where artifact files should land. Local-native ignores this
// env (it resolves the root via DefaultLocalRoot); cluster pods set
// it to ClusterLogRoot below.
const EnvLogRoot = "SPIRE_LOG_ROOT"

// ClusterLogRoot is the in-pod mount path for the shared spire-logs
// emptyDir volume. Wired in pod_builder.go and wizard_sage_pod_builder.go
// so wizard, apprentice, sage, cleric, and arbiter pods all expose the
// same path. The exporter sidecar (spi-k1cnof) mounts the same volume
// at the same path.
const ClusterLogRoot = "/var/spire/logs"

// LegacyOperationalLogName is the filename of the wizard-side
// operational log under <agentResultDir> (the existing
// `<wizardName>.log` files that `spire logs` tails). Compatibility
// callers keep teeing into it.
const LegacyOperationalLogName = "operational.log"

// LogPaths is the path-derivation accessor for a RunContext + log root.
// All methods are pure: calling them twice with equal inputs yields the
// same output regardless of CWD, hostname, pod name, or wall clock.
//
// Use New to construct a LogPaths bound to a specific (RunContext,
// Root) pair, or Identity below to convert a RunContext into a
// logartifact.Identity for substrate calls.
type LogPaths struct {
	rc   runtime.RunContext
	root string
}

// New returns a LogPaths bound to rc and root. root may be empty —
// callers that just want to derive a logartifact.Identity for an
// in-process log writer can leave it unset; path methods on a
// rootless LogPaths will report an error.
func New(rc runtime.RunContext, root string) LogPaths {
	return LogPaths{rc: rc, root: root}
}

// Root returns the absolute log root the paths derive from. Empty
// when New was constructed without a root — the cluster exporter
// resolves the root from SPIRE_LOG_ROOT, but in-process tests can
// build paths against tempdirs without one.
func (p LogPaths) Root() string { return p.root }

// RunContext returns the identity bundle the paths derive from.
func (p LogPaths) RunContext() runtime.RunContext { return p.rc }

// Identity returns the canonical logartifact.Identity for a single
// (provider, stream) pair under this LogPaths. Provider may be empty
// for non-provider streams (wizard operational stdout/stderr) — the
// resulting Identity is well-formed because logartifact.BuildObjectKey
// elides the provider segment when it is unset.
//
// Identity validates that the underlying RunContext carries the seven
// always-required identity fields (tower, bead, attempt, run, agent,
// role, phase). Stream is required. Empty fields produce a typed error
// so callers can distinguish "no run identity yet" from "bad input".
func (p LogPaths) Identity(provider string, stream logartifact.Stream) (logartifact.Identity, error) {
	id := logartifact.Identity{
		Tower:     p.rc.TowerName,
		BeadID:    p.rc.BeadID,
		AttemptID: p.rc.AttemptID,
		RunID:     p.rc.RunID,
		AgentName: p.rc.AgentName,
		Role:      logartifact.Role(p.rc.Role),
		Phase:     p.rc.FormulaStep,
		Provider:  provider,
		Stream:    stream,
	}
	if err := id.Validate(); err != nil {
		return logartifact.Identity{}, err
	}
	return id, nil
}

// TranscriptFile returns the absolute filesystem path for a provider
// transcript stream rooted at p.Root(). The path matches
// logartifact.BuildObjectKey(p.root, identity, 0) so the local-store
// reconciler (logartifact.LocalStore.Reconcile) sees the same file the
// agent wrote — no second indirection layer.
//
// Returns an error when p.root is empty (no place to write) or when the
// derived identity is missing a required field.
func (p LogPaths) TranscriptFile(provider string, stream logartifact.Stream) (string, error) {
	if p.root == "" {
		return "", fmt.Errorf("runctx: TranscriptFile: log root is empty")
	}
	id, err := p.Identity(provider, stream)
	if err != nil {
		return "", err
	}
	key, err := logartifact.BuildObjectKey("", id, 0)
	if err != nil {
		return "", err
	}
	return filepath.Join(p.root, filepath.FromSlash(key)), nil
}

// Dir returns the per-run directory under which every artifact for this
// RunContext lands. Equivalent to the parent dir of the operational
// log; useful for callers that need to mkdir once and write multiple
// stream files without computing each path independently.
//
// Dir does not include the provider segment because providers are
// per-write, not per-run.
func (p LogPaths) Dir() (string, error) {
	if p.root == "" {
		return "", fmt.Errorf("runctx: Dir: log root is empty")
	}
	if err := requireIdentityFields(p.rc); err != nil {
		return "", err
	}
	return filepath.Join(p.root,
		p.rc.TowerName,
		p.rc.BeadID,
		p.rc.AttemptID,
		p.rc.RunID,
		p.rc.AgentName,
		string(p.rc.Role),
		p.rc.FormulaStep,
	), nil
}

// OperationalLog returns the absolute path for the wizard/apprentice/
// sage/cleric/arbiter operational log under this RunContext. The
// filename is fixed (operational.log) so the cluster exporter and the
// local artifact reconciler can find it without parsing the directory
// listing for a per-process glob.
//
// Operational logs sit at a sibling depth to provider transcripts:
//
//	<root>/<tower>/<bead>/<attempt>/<run>/<agent>/<role>/<phase>/operational.log
//	<root>/<tower>/<bead>/<attempt>/<run>/<agent>/<role>/<phase>/<provider>/<stream>.jsonl
//
// This keeps a single mkdir per run sufficient for both surfaces.
func (p LogPaths) OperationalLog() (string, error) {
	dir, err := p.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, LegacyOperationalLogName), nil
}

// MkdirAll ensures every parent directory of the artifact paths under
// this RunContext exists. Returns an error if Dir() fails or if the
// filesystem operation fails; callers can ignore the error and still
// attempt the write — the writer itself will surface the failure as a
// best-effort error rather than blocking.
func (p LogPaths) MkdirAll() error {
	dir, err := p.Dir()
	if err != nil {
		return err
	}
	return os.MkdirAll(dir, 0o755)
}

// requireIdentityFields rejects a RunContext that is missing one of the
// seven path-shaping segments (tower, bead, attempt, run, agent, role,
// phase). Stream is checked at TranscriptFile time because it is per-
// write, not per-run.
func requireIdentityFields(rc runtime.RunContext) error {
	required := map[string]string{
		"tower":        rc.TowerName,
		"bead_id":      rc.BeadID,
		"attempt_id":   rc.AttemptID,
		"run_id":       rc.RunID,
		"agent_name":   rc.AgentName,
		"role":         string(rc.Role),
		"formula_step": rc.FormulaStep,
	}
	for name, val := range required {
		if val == "" {
			return fmt.Errorf("runctx: RunContext field %s is required", name)
		}
	}
	return nil
}

// DefaultLocalRoot returns the local-native log root for a given Spire
// data directory. The conventional layout puts every per-run artifact
// under <dataDir>/logs so the existing wizards/<wizard>.log convention
// (handled by AgentResultDir) continues to live alongside the new
// per-identity tree without churning user-visible paths.
//
// Callers that already have a data directory string in hand should pass
// it through unchanged; callers wanting the global default should pass
// dolt.GlobalDir() — runctx does not import pkg/dolt to keep its
// import surface minimal.
func DefaultLocalRoot(dataDir string) string {
	if dataDir == "" {
		return ""
	}
	return filepath.Join(dataDir, "logs")
}

// ResolveLogRoot returns the in-pod log root that should be used for
// artifact writes. Cluster-native pods set SPIRE_LOG_ROOT explicitly;
// local-native callers fall through to localDefault (typically
// DefaultLocalRoot(dolt.GlobalDir())).
//
// The env var takes precedence so an operator can override the local
// default for testing without rebuilding.
func ResolveLogRoot(localDefault string) string {
	if r := os.Getenv(EnvLogRoot); r != "" {
		return r
	}
	return localDefault
}
