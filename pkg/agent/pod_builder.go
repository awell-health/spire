// Package agent — canonical apprentice pod shape.
//
// This file exposes the single source of truth for how an apprentice pod
// looks in the cluster: BuildApprenticePod(spec PodSpec). Callers (the
// steward's cluster-native dispatch path, the operator reconciler)
// translate their own state into a PodSpec and call BuildApprenticePod
// to obtain the canonical *corev1.Pod. Every identity, workspace,
// handoff, and resource input is an explicit field on PodSpec — no
// opaque maps, no process-env reads, no backend-receiver state.
//
// Pod shape (keep in sync with docs/design/spi-xplwy-runtime-contract.md):
//
//   - Two init containers, executed in order:
//       tower-attach    → stages /data/<db>/.beads + tower config onto
//                         the shared emptyDir.
//       repo-bootstrap  → clones RepoURL@BaseBranch into
//                         /workspace/<prefix> and binds it locally so
//                         resolveBeadsDir / wizard.ResolveRepo find it.
//   - Main container: `spire apprentice run <bead-id> --name <agent>`.
//       - Canonical env: DOLT_URL, SPIRE_BEAD_ID, SPIRE_REPO_PREFIX,
//         SPIRE_HANDOFF_MODE, and the full RunContext vocabulary
//         (tower, attempt, run, role, formula-step, backend, workspace
//         kind/name/origin).
//       - Credentials mounted via SecretKeyRef against CredentialsSecret
//         (ANTHROPIC_API_KEY, GITHUB_TOKEN).
//       - OTLP telemetry env for the trace/log pipeline, plus
//         OTEL_RESOURCE_ATTRIBUTES carrying the same RunContext.
//   - /data (emptyDir) and /workspace volumes. /workspace is a fresh
//     emptyDir by default; callers can plumb SharedWorkspacePVCName to
//     mount a pre-provisioned PVC instead.
//   - Optional /spire/cache read-only mount when CachePVCName is set.
//   - Labels: low-cardinality canonical spire.io/* vocabulary plus
//     legacy spire.* labels for network-policy discovery. Attempt and
//     run IDs live on annotations to keep label cardinality bounded.
//
// The legacy (*K8sBackend).BuildPod / buildRolePod path in
// backend_k8s.go still exists for the wizard pod shape and for the
// spawner lifecycle — migrating steward's cluster-native path and the
// operator reconciler onto BuildApprenticePod is wave-1 work on the
// spi-sj18k epic.
package agent

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/awell-health/spire/pkg/runtime"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
)

// DefaultCredentialsSecret is the k8s Secret name BuildApprenticePod
// uses when PodSpec.CredentialsSecret is left empty. Matches the
// helm-chart default for installs that pre-date release-scoped secret
// naming.
const DefaultCredentialsSecret = "spire-credentials"

// DefaultPriorityClassName is the PriorityClass assigned to apprentice
// pods when PodSpec.PriorityClassName is empty. Matches the chart's
// per-agent priority class used for eviction ordering.
const DefaultPriorityClassName = "spire-agent-default"

// DefaultApprenticeBackend is the SPIRE_BACKEND / labels backend
// identifier used when PodSpec.Backend is empty. Callers dispatched
// from the operator should override this to "operator-k8s" so
// observability surfaces can distinguish the two schedulers.
const DefaultApprenticeBackend = "k8s"

// DataMountPath is the in-pod path for the tower-attach substrate
// (BEADS_DIR, tower config). It is also the value of DOLT_DATA_DIR on
// every container.
const DataMountPath = "/data"

// DefaultWorkspaceMountPath is the in-pod path for the repo-bootstrap
// workspace when BuildApprenticePod is not composed with a cache
// overlay. The path mirrors what the legacy substrate pod used so
// existing in-pod tooling (spire.yaml resolution, bind-local) keeps
// working byte-for-byte.
const DefaultWorkspaceMountPath = "/workspace"

// PodSpec is the explicit, apprentice-scoped input to
// BuildApprenticePod. Every field is intentional and the struct has no
// opaque maps — this keeps dispatch sites honest about what they plumb
// through from their own configuration surfaces.
//
// Required fields: Name, Namespace, Image, BeadID, Identity.RepoURL,
// Identity.BaseBranch, Identity.Prefix. Missing required fields surface
// as typed errors from BuildApprenticePod (see ErrPodSpec* vars).
type PodSpec struct {
	// Name is the k8s pod name. Callers pass an already-sanitized name;
	// BuildApprenticePod does not mutate it so deterministic
	// operator-scoped names survive intact.
	Name string

	// Namespace is the k8s namespace the pod lives in.
	Namespace string

	// Image is the agent container image reference. Used for both the
	// main container and the init containers.
	Image string

	// ServiceAccountName is the k8s ServiceAccount the pod runs as.
	// Empty defers to the namespace default.
	ServiceAccountName string

	// AgentName is the logical agent name (e.g. "apprentice-spi-abc-0").
	// Flows into the main container's `--name` argument, the legacy
	// spire.agent.name label, and OTEL_RESOURCE_ATTRIBUTES.
	AgentName string

	// BeadID is the bead the apprentice is working on. Required.
	BeadID string

	// AttemptID is the attempt bead ID the wizard created for this
	// dispatch. The canonical ownership seam; empty means "not a fresh
	// attempt" (e.g. review-fix re-engagement).
	AttemptID string

	// RunID is the wizard-assigned correlation id that ties child runs
	// to their parent invocation.
	RunID string

	// ApprenticeIdx is the fan-out index of this apprentice within its
	// wave, as a decimal string. "0" for single-apprentice dispatches.
	ApprenticeIdx string

	// FormulaStep is the current formula step name (e.g. "implement").
	FormulaStep string

	// Role is the SpawnRole the pod runs as (apprentice / wizard /
	// sage). Empty defaults to RoleApprentice for backward
	// compatibility — every existing call site builds apprentice
	// pods. BuildWizardPod and BuildSagePod set this explicitly so
	// the canonical labels, env, and command vary by role.
	Role runtime.SpawnRole

	// Identity is the canonical repo identity. TowerName, Prefix,
	// RepoURL, and BaseBranch are all required for an apprentice pod
	// because the repo-bootstrap init container clones from them.
	Identity runtime.RepoIdentity

	// DolthubRemote is the dolt remote URL passed to the tower-attach
	// init container. Comes from the caller's explicit configuration,
	// never from process env.
	DolthubRemote string

	// Workspace is the materialized workspace handle the apprentice
	// consumes. Kind+Name+Origin flow onto labels/env; Path is passed
	// through as SPIRE_WORKSPACE_PATH so in-pod tooling can resolve it
	// without re-parsing the workspace name.
	Workspace runtime.WorkspaceHandle

	// HandoffMode is the handoff protocol the wizard selected for this
	// role transition. Emitted as SPIRE_HANDOFF_MODE and as a label.
	HandoffMode runtime.HandoffMode

	// Backend identifies the execution environment. Defaults to
	// "k8s"; the operator path should override to "operator-k8s" so
	// observability surfaces disambiguate.
	Backend string

	// Provider is the AI provider override (claude, codex, cursor).
	Provider string

	// DoltURL is the full in-cluster dolt server URL the in-pod worker
	// connects to (e.g. "spire-dolt.spire.svc:3306"). Emitted as
	// DOLT_URL; when parseable as host:port it also populates the
	// legacy BEADS_DOLT_SERVER_HOST / BEADS_DOLT_SERVER_PORT pair that
	// pkg/dolt clients still read.
	DoltURL string

	// CredentialsSecret is the k8s Secret holding
	// ANTHROPIC_API_KEY_DEFAULT and GITHUB_TOKEN. Empty defaults to
	// DefaultCredentialsSecret.
	CredentialsSecret string

	// Resources is the main container's resource requests/limits.
	// Callers pass explicit values; BuildApprenticePod never falls
	// back to env-var-based overrides the way the legacy spawner did.
	Resources corev1.ResourceRequirements

	// RestartPolicy for the pod. Empty defaults to
	// corev1.RestartPolicyNever (one-shot apprentices).
	RestartPolicy corev1.RestartPolicy

	// PriorityClassName for the pod. Empty defaults to
	// DefaultPriorityClassName.
	PriorityClassName string

	// CachePVCName, if non-empty, adds a read-only mount of that PVC at
	// CacheMountPath so the repo-bootstrap init container (or a caller
	// overlay) can materialize a workspace from a pre-populated guild
	// cache. BuildApprenticePod only wires the mount — the cache
	// protocol is the caller's concern.
	CachePVCName string

	// SharedWorkspacePVCName, if non-empty, backs /workspace with that
	// PVC instead of an emptyDir. This is the borrowed-worktree path
	// for apprentices that continue a parent wizard's checkout.
	SharedWorkspacePVCName string

	// GCSSecretName, if non-empty, mounts that k8s Secret at
	// GCSMountPath on the main container and sets
	// GOOGLE_APPLICATION_CREDENTIALS so the in-pod GCS BundleStore
	// client can authenticate via the mounted service-account JSON.
	// Callers populate this when the tower's BundleStore backend is
	// "gcs". Empty leaves the pod unchanged — local-backend apprentices
	// have no GCS volume, no env, nothing.
	GCSSecretName string

	// GCSMountPath is the in-pod directory the GCS Secret is mounted
	// under. Only consulted when GCSSecretName is non-empty. Matches the
	// chart's .Values.gcp.mountPath so one path works across every
	// GCP-consuming feature.
	GCSMountPath string

	// GCSKeyName is the filename of the service-account JSON inside the
	// mount (the Secret's data key). Joined with GCSMountPath to form
	// GOOGLE_APPLICATION_CREDENTIALS.
	GCSKeyName string

	// OTLPEndpoint is the destination for OTEL traces/logs
	// (e.g. "http://spire-steward.spire.svc:4317"). Empty disables the
	// OTLP env block so local test fixtures do not emit OTLP.
	OTLPEndpoint string

	// OLAPBackend, when non-empty, is emitted as SPIRE_OLAP_BACKEND so
	// the in-pod worker's olap.Config picks the cluster analytics
	// backend (typically "clickhouse") instead of falling through to
	// DuckDB — which fails at runtime in the CGO-off agent image.
	// Empty leaves the env unset; the worker keeps the laptop default.
	OLAPBackend string

	// OLAPDSN, when non-empty, is emitted as SPIRE_CLICKHOUSE_DSN so the
	// ClickHouse driver knows where to connect. Only meaningful when
	// OLAPBackend="clickhouse". Callers plumb the in-cluster DNS name
	// produced by helm's `spire.clickhouseDSN` helper (native protocol
	// port, e.g. "clickhouse://spire-clickhouse.spire.svc:9000/spire").
	OLAPDSN string

	// CustomPrompt, if non-empty, is emitted as SPIRE_CUSTOM_PROMPT on
	// the main container so the apprentice runs with a formula-supplied
	// prompt override.
	CustomPrompt string

	// ExtraArgs are additional args appended to the main container's
	// command (after the canonical `apprentice run <bead> --name
	// <agent>` prefix). Used for formula flags like `--review-fix`.
	ExtraArgs []string
}

// Typed errors returned by BuildApprenticePod for missing required
// inputs. Callers can errors.Is against these to route misconfiguration
// diagnostics.
var (
	// ErrPodSpecImage is returned when PodSpec.Image is empty.
	ErrPodSpecImage = fmt.Errorf("agent: PodSpec.Image is required")
	// ErrPodSpecName is returned when PodSpec.Name is empty.
	ErrPodSpecName = fmt.Errorf("agent: PodSpec.Name is required")
	// ErrPodSpecNamespace is returned when PodSpec.Namespace is empty.
	ErrPodSpecNamespace = fmt.Errorf("agent: PodSpec.Namespace is required")
	// ErrPodSpecBeadID is returned when PodSpec.BeadID is empty.
	ErrPodSpecBeadID = fmt.Errorf("agent: PodSpec.BeadID is required")
	// ErrPodSpecIdentity is returned when any of the three repo-bootstrap
	// inputs on PodSpec.Identity (RepoURL, BaseBranch, Prefix) is empty.
	// TowerName is checked separately (ErrPodSpecTower).
	ErrPodSpecIdentity = fmt.Errorf("agent: PodSpec.Identity.{RepoURL,BaseBranch,Prefix} are required")
	// ErrPodSpecTower is returned when PodSpec.Identity.TowerName is
	// empty — the tower-attach init container's --database/--data-dir
	// cannot be synthesized without it.
	ErrPodSpecTower = fmt.Errorf("agent: PodSpec.Identity.TowerName is required")
)

// BuildApprenticePod returns the canonical apprentice pod for spec.
// The returned pod is ready for controller-runtime's client.Create; no
// live k8s API calls happen here.
//
// All required PodSpec fields must be populated (see ErrPodSpec* vars).
// BuildApprenticePod never reads process env, never falls back to
// ambient CWD, and never hides missing identity behind a default — a
// misconfigured dispatch site surfaces as a typed error at build time
// rather than a pod that dies in the init container with an opaque
// shell message.
func BuildApprenticePod(spec PodSpec) (*corev1.Pod, error) {
	if err := spec.validate(); err != nil {
		return nil, err
	}

	spec = spec.withDefaults()

	args := []string{"apprentice", "run", spec.BeadID, "--name", spec.effectiveAgentName()}
	args = append(args, spec.ExtraArgs...)

	env := spec.buildEnv()
	env = append(env, spec.secretEnvRefs()...)

	dataMount := corev1.VolumeMount{Name: "data", MountPath: DataMountPath}
	workspaceMount := corev1.VolumeMount{Name: "workspace", MountPath: DefaultWorkspaceMountPath}

	mainMounts := []corev1.VolumeMount{dataMount, workspaceMount}
	if spec.CachePVCName != "" {
		mainMounts = append(mainMounts, corev1.VolumeMount{
			Name:      "repo-cache",
			MountPath: CacheMountPath,
			ReadOnly:  true,
		})
	}
	if spec.GCSSecretName != "" {
		mainMounts = append(mainMounts, corev1.VolumeMount{
			Name:      "gcp-sa",
			MountPath: spec.GCSMountPath,
			ReadOnly:  true,
		})
	}

	volumes := spec.buildVolumes()
	initContainers := spec.buildInitContainers(env, dataMount, workspaceMount)

	pod := &corev1.Pod{}
	pod.Name = spec.Name
	pod.Namespace = spec.Namespace
	pod.Labels = spec.buildLabels()
	pod.Annotations = spec.buildAnnotations()
	pod.Spec.RestartPolicy = spec.RestartPolicy
	pod.Spec.PriorityClassName = spec.PriorityClassName
	pod.Spec.ServiceAccountName = spec.ServiceAccountName
	pod.Spec.Volumes = volumes
	pod.Spec.InitContainers = initContainers
	pod.Spec.Containers = []corev1.Container{
		{
			Name:         "agent",
			Image:        spec.Image,
			Command:      append([]string{"spire"}, args...),
			Env:          env,
			Resources:    spec.Resources,
			VolumeMounts: mainMounts,
		},
	}

	return pod, nil
}

// validate returns a typed error for the first missing required field.
// Runs before any defaulting so the caller sees errors that reference
// the real input state, not defaulted values.
func (s PodSpec) validate() error {
	if s.Name == "" {
		return ErrPodSpecName
	}
	if s.Namespace == "" {
		return ErrPodSpecNamespace
	}
	if s.Image == "" {
		return ErrPodSpecImage
	}
	if s.BeadID == "" {
		return ErrPodSpecBeadID
	}
	if s.Identity.TowerName == "" {
		return fmt.Errorf("%w (bead %s)", ErrPodSpecTower, s.BeadID)
	}
	if s.Identity.RepoURL == "" || s.Identity.BaseBranch == "" || s.Identity.Prefix == "" {
		return fmt.Errorf("%w (bead %s, got %+v)", ErrPodSpecIdentity, s.BeadID, s.Identity)
	}
	return nil
}

// withDefaults returns a copy of s with empty optional fields replaced
// by their canonical default values.
func (s PodSpec) withDefaults() PodSpec {
	if s.CredentialsSecret == "" {
		s.CredentialsSecret = DefaultCredentialsSecret
	}
	if s.RestartPolicy == "" {
		s.RestartPolicy = corev1.RestartPolicyNever
	}
	if s.PriorityClassName == "" {
		s.PriorityClassName = DefaultPriorityClassName
	}
	if s.Backend == "" {
		s.Backend = DefaultApprenticeBackend
	}
	return s
}

// effectiveAgentName returns the logical agent name, falling back to
// the pod name when the caller left AgentName unset.
func (s PodSpec) effectiveAgentName() string {
	if s.AgentName != "" {
		return s.AgentName
	}
	return s.Name
}

// effectiveRole returns the spawn role for the pod. Empty defaults to
// RoleApprentice so existing call sites that built apprentice pods
// without setting Role continue to work unchanged.
func (s PodSpec) effectiveRole() runtime.SpawnRole {
	if s.Role == "" {
		return RoleApprentice
	}
	return s.Role
}

// buildEnv builds the canonical env set for the main container. Init
// containers reuse the same list so shell scripts and CLI invocations
// see identical values across container boundaries.
func (s PodSpec) buildEnv() []corev1.EnvVar {
	env := []corev1.EnvVar{
		// Substrate: tower-attach writes /data/<db>/.beads; the in-pod
		// worker reads DOLT_DATA_DIR + SPIRE_CONFIG_DIR to find it.
		{Name: "DOLT_DATA_DIR", Value: DataMountPath},
		{Name: "SPIRE_CONFIG_DIR", Value: DataMountPath + "/spire-config"},
		// Repo bootstrap inputs consumed by the repo-bootstrap init
		// container's shell script.
		{Name: "SPIRE_REPO_URL", Value: s.Identity.RepoURL},
		{Name: "SPIRE_REPO_BRANCH", Value: s.Identity.BaseBranch},
		{Name: "SPIRE_REPO_PREFIX", Value: s.Identity.Prefix},
		// Dolt client env — BEADS_DATABASE/BEADS_PREFIX are canonical
		// across the tree.
		{Name: "BEADS_DATABASE", Value: s.Identity.TowerName},
		{Name: "BEADS_PREFIX", Value: s.Identity.Prefix},
	}

	if s.DolthubRemote != "" {
		env = append(env, corev1.EnvVar{Name: "DOLTHUB_REMOTE", Value: s.DolthubRemote})
	}

	// DOLT_URL plus the split host/port form for legacy in-pod
	// consumers that have not yet migrated to the URL-based vocab.
	if s.DoltURL != "" {
		env = append(env, corev1.EnvVar{Name: "DOLT_URL", Value: s.DoltURL})
		if host, port, ok := splitHostPort(s.DoltURL); ok {
			env = append(env,
				corev1.EnvVar{Name: "BEADS_DOLT_SERVER_HOST", Value: host},
				corev1.EnvVar{Name: "BEADS_DOLT_SERVER_PORT", Value: port},
			)
		}
	}

	// Canonical RunContext env vocabulary.
	env = append(env,
		corev1.EnvVar{Name: "SPIRE_TOWER", Value: s.Identity.TowerName},
		corev1.EnvVar{Name: "SPIRE_ROLE", Value: string(s.effectiveRole())},
		corev1.EnvVar{Name: "SPIRE_BEAD_ID", Value: s.BeadID},
		corev1.EnvVar{Name: "SPIRE_BACKEND", Value: s.Backend},
	)
	if s.AttemptID != "" {
		env = append(env, corev1.EnvVar{Name: "SPIRE_ATTEMPT_ID", Value: s.AttemptID})
	}
	if s.RunID != "" {
		env = append(env, corev1.EnvVar{Name: "SPIRE_RUN_ID", Value: s.RunID})
	}
	if s.ApprenticeIdx != "" {
		env = append(env, corev1.EnvVar{Name: "SPIRE_APPRENTICE_IDX", Value: s.ApprenticeIdx})
	}
	if s.FormulaStep != "" {
		env = append(env, corev1.EnvVar{Name: "SPIRE_FORMULA_STEP", Value: s.FormulaStep})
	}
	if s.Provider != "" {
		env = append(env, corev1.EnvVar{Name: "SPIRE_PROVIDER", Value: s.Provider})
	}

	// Workspace vocabulary. All three fields go through when the
	// caller populated the handle; none of them are synthesized from
	// process env.
	if s.Workspace.Kind != "" {
		env = append(env, corev1.EnvVar{Name: "SPIRE_WORKSPACE_KIND", Value: string(s.Workspace.Kind)})
	}
	if s.Workspace.Name != "" {
		env = append(env, corev1.EnvVar{Name: "SPIRE_WORKSPACE_NAME", Value: s.Workspace.Name})
	}
	if s.Workspace.Origin != "" {
		env = append(env, corev1.EnvVar{Name: "SPIRE_WORKSPACE_ORIGIN", Value: string(s.Workspace.Origin)})
	}
	if s.Workspace.Path != "" {
		env = append(env, corev1.EnvVar{Name: "SPIRE_WORKSPACE_PATH", Value: s.Workspace.Path})
	}
	if s.HandoffMode != "" {
		env = append(env, corev1.EnvVar{Name: "SPIRE_HANDOFF_MODE", Value: string(s.HandoffMode)})
	}

	if s.CustomPrompt != "" {
		env = append(env, corev1.EnvVar{Name: "SPIRE_CUSTOM_PROMPT", Value: s.CustomPrompt})
	}

	// GOOGLE_APPLICATION_CREDENTIALS — only emitted when the caller
	// opts in via GCSSecretName. Points at the mounted service-account
	// JSON so the in-pod GCS client (pkg/bundlestore.gcsStore) can
	// authenticate. Same env var the dolt backup path sets when it's on.
	if s.GCSSecretName != "" {
		env = append(env, corev1.EnvVar{
			Name:  "GOOGLE_APPLICATION_CREDENTIALS",
			Value: s.GCSMountPath + "/" + s.GCSKeyName,
		})
	}

	// OLAP backend selection. Emit SPIRE_OLAP_BACKEND + SPIRE_CLICKHOUSE_DSN
	// when the caller opts in so the in-pod worker's olap.Config picks
	// the cluster backend. Empty OLAPBackend leaves the pod on the laptop
	// default (DuckDB via CGO) — correct for local test fixtures and
	// incorrect for CGO-off cluster images, which is why cluster callers
	// (operator IntentWorkloadReconciler, AgentMonitor) plumb this
	// explicitly from the helm chart's clickhouse block.
	if s.OLAPBackend != "" {
		env = append(env, corev1.EnvVar{Name: "SPIRE_OLAP_BACKEND", Value: s.OLAPBackend})
		if s.OLAPDSN != "" {
			env = append(env, corev1.EnvVar{Name: "SPIRE_CLICKHOUSE_DSN", Value: s.OLAPDSN})
		}
	}

	// OTLP telemetry. When the caller leaves the endpoint empty, we
	// emit nothing — local test fixtures should not ship traces.
	if s.OTLPEndpoint != "" {
		env = append(env,
			corev1.EnvVar{Name: "OTEL_EXPORTER_OTLP_ENDPOINT", Value: s.OTLPEndpoint},
			corev1.EnvVar{Name: "CLAUDE_CODE_ENABLE_TELEMETRY", Value: "1"},
			corev1.EnvVar{Name: "CLAUDE_CODE_ENHANCED_TELEMETRY_BETA", Value: "1"},
			corev1.EnvVar{Name: "OTEL_TRACES_EXPORTER", Value: "otlp"},
			corev1.EnvVar{Name: "OTEL_LOGS_EXPORTER", Value: "otlp"},
			corev1.EnvVar{Name: "OTEL_EXPORTER_OTLP_PROTOCOL", Value: "grpc"},
		)
		if attrs := s.otelResourceAttrs(); attrs != "" {
			env = append(env, corev1.EnvVar{Name: "OTEL_RESOURCE_ATTRIBUTES", Value: attrs})
		}
	}

	return env
}

// secretEnvRefs builds the ANTHROPIC_API_KEY + GITHUB_TOKEN SecretKeyRef
// env entries. GITHUB_TOKEN is Optional so installs without a GitHub
// token do not block pod creation.
func (s PodSpec) secretEnvRefs() []corev1.EnvVar {
	optional := true
	return []corev1.EnvVar{
		{
			Name: "ANTHROPIC_API_KEY",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: s.CredentialsSecret},
					Key:                  "ANTHROPIC_API_KEY_DEFAULT",
				},
			},
		},
		{
			Name: "GITHUB_TOKEN",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: s.CredentialsSecret},
					Key:                  "GITHUB_TOKEN",
					Optional:             &optional,
				},
			},
		},
	}
}

// buildVolumes returns the pod-level volume list. /data is always a
// fresh emptyDir. /workspace is emptyDir by default; the caller opts
// into a PVC by setting SharedWorkspacePVCName. CachePVCName, when
// non-empty, appends a read-only cache PVC volume.
func (s PodSpec) buildVolumes() []corev1.Volume {
	workspaceVol := corev1.Volume{
		Name:         "workspace",
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
	}
	if s.SharedWorkspacePVCName != "" {
		workspaceVol.VolumeSource = corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
				ClaimName: s.SharedWorkspacePVCName,
			},
		}
	}

	volumes := []corev1.Volume{
		{
			Name:         "data",
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		},
		workspaceVol,
	}

	if s.CachePVCName != "" {
		readOnly := true
		volumes = append(volumes, corev1.Volume{
			Name: "repo-cache",
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: s.CachePVCName,
					ReadOnly:  readOnly,
				},
			},
		})
	}

	if s.GCSSecretName != "" {
		mode := int32(0400)
		volumes = append(volumes, corev1.Volume{
			Name: "gcp-sa",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName:  s.GCSSecretName,
					DefaultMode: &mode,
				},
			},
		})
	}

	return volumes
}

// buildInitContainers returns the canonical tower-attach +
// repo-bootstrap init container pair. env is the shared main-container
// env (init containers reuse it verbatim so shell scripts see the same
// values as the main process).
func (s PodSpec) buildInitContainers(env []corev1.EnvVar, dataMount, workspaceMount corev1.VolumeMount) []corev1.Container {
	return []corev1.Container{
		{
			Name:  "tower-attach",
			Image: s.Image,
			Command: []string{
				"spire", "tower", "attach-cluster",
				"--data-dir=" + DataMountPath + "/" + s.Identity.TowerName,
				"--database=" + s.Identity.TowerName,
				"--prefix=" + s.Identity.Prefix,
				"--dolthub-remote=" + s.DolthubRemote,
			},
			Env:          env,
			VolumeMounts: []corev1.VolumeMount{dataMount},
		},
		{
			Name:         "repo-bootstrap",
			Image:        s.Image,
			Command:      []string{"sh", "-c", repoBootstrapScript},
			Env:          env,
			VolumeMounts: []corev1.VolumeMount{dataMount, workspaceMount},
		},
	}
}

// buildLabels returns the canonical spire.io/* label vocabulary plus
// the legacy spire.* labels that network policies and discovery code
// still select on. High-cardinality identifiers (attempt, run) live on
// annotations — see buildAnnotations.
func (s PodSpec) buildLabels() map[string]string {
	labels := map[string]string{
		// Legacy labels — preserved byte-for-byte so network policies
		// and List-selector code paths do not regress.
		"spire.agent":      "true",
		"spire.agent.name": s.effectiveAgentName(),
		"spire.bead":       s.BeadID,
		"spire.role":       string(s.effectiveRole()),
		"spire.tower":      s.Identity.TowerName,
	}

	setLabel(labels, LabelBackend, s.Backend)
	setLabel(labels, LabelTower, s.Identity.TowerName)
	setLabel(labels, LabelPrefix, s.Identity.Prefix)
	setLabel(labels, LabelBead, s.BeadID)
	setLabel(labels, LabelRole, string(s.effectiveRole()))
	setLabel(labels, LabelFormulaStep, s.FormulaStep)
	setLabel(labels, LabelWorkspaceKind, string(s.Workspace.Kind))
	setLabel(labels, LabelWorkspaceName, s.Workspace.Name)
	setLabel(labels, LabelWorkspaceOrigin, string(s.Workspace.Origin))
	setLabel(labels, LabelHandoffMode, string(s.HandoffMode))

	return labels
}

// buildAnnotations returns the high-cardinality identifier set for the
// pod: attempt and run IDs. Returns nil when nothing is populated so
// the pod does not carry an empty annotations map.
func (s PodSpec) buildAnnotations() map[string]string {
	annotations := map[string]string{}
	if s.AttemptID != "" {
		annotations[AnnotationAttemptID] = s.AttemptID
	}
	if s.RunID != "" {
		annotations[AnnotationRunID] = s.RunID
	}
	if len(annotations) == 0 {
		return nil
	}
	return annotations
}

// otelResourceAttrs builds the OTEL_RESOURCE_ATTRIBUTES payload. Uses
// the canonical RunContext underscore vocabulary (bead_id, formula_step
// etc.) from docs/design/spi-xplwy-runtime-contract.md §1.4.
func (s PodSpec) otelResourceAttrs() string {
	var parts []string
	add := func(k, v string) {
		if v == "" {
			return
		}
		parts = append(parts, k+"="+v)
	}

	if name := s.effectiveAgentName(); name != "" {
		parts = append(parts, "agent.name="+name)
	}
	add("tower", s.Identity.TowerName)
	add("prefix", s.Identity.Prefix)
	add("bead_id", s.BeadID)
	add("attempt_id", s.AttemptID)
	add("run_id", s.RunID)
	add("role", string(s.effectiveRole()))
	add("formula_step", s.FormulaStep)
	add("backend", s.Backend)
	add("workspace_kind", string(s.Workspace.Kind))
	add("workspace_name", s.Workspace.Name)
	add("workspace_origin", string(s.Workspace.Origin))
	add("handoff_mode", string(s.HandoffMode))

	return strings.Join(parts, ",")
}

// splitHostPort extracts host and port strings from a DOLT_URL. The
// canonical form is "host:port" (no scheme) but we also accept
// "scheme://host:port" forms for robustness. Returns ok=false when the
// URL has no port component; the caller then emits DOLT_URL alone.
func splitHostPort(raw string) (host, port string, ok bool) {
	// Try URL parse first so "mysql://spire-dolt.svc:3306" round-trips.
	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err == nil && u.Host != "" {
			if h, p, split := hostAndPort(u.Host); split {
				return h, p, true
			}
		}
	}
	return hostAndPort(raw)
}

func hostAndPort(s string) (string, string, bool) {
	if idx := strings.LastIndex(s, ":"); idx > 0 && idx < len(s)-1 {
		// Reject an IPv6 literal that forgot its brackets — more than
		// one colon and the remaining suffix is not a simple port.
		if strings.Contains(s[:idx], ":") && !strings.HasPrefix(s, "[") {
			return "", "", false
		}
		return s[:idx], s[idx+1:], true
	}
	return s, "", false
}

// ---------------------------------------------------------------------
// Legacy (*K8sBackend) surface — unchanged for back-compat.
//
// The BuildPod method remains the entry point for the spawner and the
// operator's current wizard-pod path. Wave-1 work on spi-sj18k migrates
// callers onto BuildApprenticePod and retires the receiver-based
// builder for the apprentice shape.
// ---------------------------------------------------------------------

// NewPodBuilder returns a K8sBackend configured as a pure pod builder
// (no live k8s API calls during BuildPod unless the caller asks for a
// shared-workspace substrate pod, in which case the client is used to
// locate the parent wizard's PVC). Callers that only build pods for
// non-substrate roles (e.g. the operator's wizard pods) may pass a nil
// client — any unexpected client use will then panic loudly, which is
// the desired fail-fast behavior under parity testing.
//
// The namespace, image, and secretName fields set here are used
// verbatim by buildRolePod, so operator-managed pods carry whatever
// settings the operator configured at startup rather than reading from
// process env on every spawn. This is the runtime-contract rule: no
// ambient env for identity inputs.
func NewPodBuilder(client kubernetes.Interface, namespace, image, secretName string) *K8sBackend {
	if secretName == "" {
		secretName = DefaultCredentialsSecret
	}
	return &K8sBackend{
		client:     client,
		namespace:  namespace,
		image:      image,
		secretName: secretName,
	}
}

// BuildPod returns the canonical pod for a SpawnConfig WITHOUT creating
// it in the cluster. This is the operator-facing entry point for the
// wizard pod shape: the operator applies the returned *corev1.Pod
// through controller-runtime's client (which lets it attach owner
// references and merge guild-level overrides).
//
// Apprentice callers should use the free function BuildApprenticePod
// instead — it accepts an explicit PodSpec rather than routing through
// the wider SpawnConfig/role-switch surface.
func (b *K8sBackend) BuildPod(cfg SpawnConfig) (*corev1.Pod, error) {
	return b.buildRolePod(cfg)
}
