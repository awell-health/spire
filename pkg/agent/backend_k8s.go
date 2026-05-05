package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/awell-health/spire/pkg/runtime"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// K8sBackend implements Backend for Kubernetes pod execution.
// Each agent runs as a one-shot Pod with labels for discovery and
// secret references for credentials.
type K8sBackend struct {
	client     kubernetes.Interface
	namespace  string
	image      string // agent container image
	secretName string // k8s Secret holding ANTHROPIC_API_KEY_DEFAULT / GITHUB_TOKEN
}

// Typed errors returned by Spawn for missing runtime contract inputs.
// Callers can errors.Is against these to route failures (e.g. the
// executor may retry on a missing-workspace error by re-materializing
// the substrate before re-dispatching).
var (
	// ErrWorkspaceRequired is returned when cfg.Workspace is nil but the
	// role/config combination requires a materialized substrate.
	ErrWorkspaceRequired = errors.New("k8s backend: cfg.Workspace is required")
	// ErrIdentityRequired is returned when cfg.Identity (plus legacy
	// cfg.Tower / cfg.RepoURL / cfg.RepoBranch / cfg.RepoPrefix) are
	// all unset for a role that requires canonical identity to stage
	// tower data or bootstrap a repo.
	ErrIdentityRequired = errors.New("k8s backend: cfg.Identity is required")
	// ErrSharedWorkspacePVCNotFound is returned when the shared-workspace
	// gate is on, cfg.Workspace.Kind==WorkspaceKindBorrowedWorktree, and
	// no PVC with the expected owner-label is found in the namespace.
	ErrSharedWorkspacePVCNotFound = errors.New("k8s backend: owning-wizard PVC not found for borrowed workspace")
)

// NewK8sBackend creates a K8sBackend using in-cluster config with
// kubeconfig fallback. Reads SPIRE_K8S_NAMESPACE (default: namespace
// from serviceaccount token), SPIRE_AGENT_IMAGE (required), and
// SPIRE_CREDENTIALS_SECRET (optional; falls back to "spire-credentials"
// for backward compat with installs that pre-date the helm chart's
// release-scoped secret naming).
//
// As of spi-wqax9, backend construction no longer reads
// BEADS_DATABASE / BEADS_PREFIX / DOLTHUB_REMOTE from process env.
// Those values are now sourced from cfg.Identity on every Spawn so one
// backend instance can serve multiple towers/prefixes without process
// restart. Legacy SpawnConfig fields (cfg.Tower, cfg.RepoURL,
// cfg.RepoBranch, cfg.RepoPrefix) are used as a fallback for the
// migration window until every dispatch site populates cfg.Identity —
// once they all do, the legacy fallback becomes dead code and can be
// removed in a later cleanup bead.
func NewK8sBackend() (*K8sBackend, error) {
	image := os.Getenv("SPIRE_AGENT_IMAGE")
	if image == "" {
		return nil, fmt.Errorf("SPIRE_AGENT_IMAGE env is required for k8s backend")
	}
	secretName := os.Getenv("SPIRE_CREDENTIALS_SECRET")
	if secretName == "" {
		secretName = "spire-credentials"
	}

	config, err := rest.InClusterConfig()
	if err != nil {
		// Fallback to kubeconfig for local development.
		rules := clientcmd.NewDefaultClientConfigLoadingRules()
		config, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
			rules, &clientcmd.ConfigOverrides{},
		).ClientConfig()
		if err != nil {
			return nil, fmt.Errorf("k8s config: %w", err)
		}
	}

	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("k8s client: %w", err)
	}

	ns := os.Getenv("SPIRE_K8S_NAMESPACE")
	if ns == "" {
		// Try to read the namespace from the serviceaccount mount.
		if data, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace"); err == nil {
			ns = strings.TrimSpace(string(data))
		}
	}
	if ns == "" {
		ns = "default"
	}

	return &K8sBackend{
		client:     client,
		namespace:  ns,
		image:      image,
		secretName: secretName,
	}, nil
}

// NewK8sBackendFromClient creates a K8sBackend with an injected client.
// Used for testing with the k8s fake client.
func NewK8sBackendFromClient(client kubernetes.Interface, namespace, image string) *K8sBackend {
	return &K8sBackend{
		client:     client,
		namespace:  namespace,
		image:      image,
		secretName: "spire-credentials",
	}
}

// Spawn creates a one-shot k8s Pod for the given agent config.
//
// The pod shape is selected by buildRolePod keyed on cfg.Role ×
// cfg.Workspace.Kind:
//
//   - Wizard role → the canonical wizard pod (tower-attach +
//     repo-bootstrap init containers, /data+/workspace volumes, wizard
//     resources).
//   - Any role whose cfg.Workspace.Kind is non-repo → the same two
//     init containers, staging the substrate before the main container
//     starts. This closes the apprentice/sage-in-k8s gap.
//   - Everything else (apprentice/sage without a workspace handle, or
//     Kind==WorkspaceKindRepo) → the flat executor pod (byte-for-byte
//     identical to the pre-shared-builder code path).
func (b *K8sBackend) Spawn(cfg SpawnConfig) (Handle, error) {
	pod, err := b.buildRolePod(cfg)
	if err != nil {
		return nil, err
	}

	created, err := b.client.CoreV1().Pods(b.namespace).Create(
		context.Background(), pod, metav1.CreateOptions{},
	)
	if err != nil {
		return nil, fmt.Errorf("create pod %s: %w", pod.Name, err)
	}

	return &K8sHandle{
		client:    b.client,
		namespace: b.namespace,
		podName:   created.Name,
		name:      cfg.Name,
	}, nil
}

// buildRolePod is the role-agnostic shared pod builder. It routes on
// cfg.Role × cfg.Workspace.Kind and returns a ready-to-create Pod
// (with labels, annotations, volumes, init containers, and main
// container) without calling out to the k8s API.
//
// The routing rules (spi-wqax9 §4):
//
//  1. Wizard: always gets tower-attach + repo-bootstrap init containers.
//     A wizard pod without a materialized workspace is invalid —
//     buildWizardPod fails fast on empty identity/bootstrap inputs.
//  2. Non-wizard role with cfg.Workspace != nil and
//     cfg.Workspace.Kind != WorkspaceKindRepo: gets the same two init
//     containers. When SPIRE_K8S_SHARED_WORKSPACE=1 and the kind is
//     borrowed-worktree, the workspace is mounted from the parent
//     wizard's PVC rather than an emptyDir.
//  3. Otherwise (non-wizard with nil Workspace, or Kind==repo): the
//     legacy flat pod. This is the gate-OFF baseline.
func (b *K8sBackend) buildRolePod(cfg SpawnConfig) (*corev1.Pod, error) {
	ident := resolveIdentity(cfg)
	podName := sanitizePodName(cfg.Name)

	// Determine the pod shape before we start allocating objects so we
	// fail fast when the contract is violated (cfg.Identity missing for
	// a role that needs substrate, cfg.Workspace missing, etc.).
	shape := selectPodShape(cfg)

	subcmd, err := roleToSubcmd(cfg.Role)
	if err != nil {
		return nil, err
	}
	args := append([]string{}, subcmd...)
	args = append(args, cfg.BeadID, "--name", cfg.Name)
	args = append(args, cfg.ExtraArgs...)

	env := b.buildEnvVars(cfg, ident)
	if cfg.CustomPrompt != "" {
		env = append(env, corev1.EnvVar{Name: "SPIRE_CUSTOM_PROMPT", Value: cfg.CustomPrompt})
	}
	env = append(env, secretEnvRefs(b.secretName)...)

	switch shape {
	case podShapeWizard:
		return b.buildWizardPod(cfg, ident, podName, args, env)
	case podShapeSubstrate:
		return b.buildSubstratePod(cfg, ident, podName, args, env)
	default:
		return b.buildFlatPod(cfg, podName, args, env), nil
	}
}

// podShape selects which builder buildRolePod dispatches to.
type podShape int

const (
	// podShapeFlat is the legacy flat executor pod (no init containers,
	// no workspace volumes). Used when the caller has not populated
	// cfg.Workspace (gate-OFF baseline).
	podShapeFlat podShape = iota
	// podShapeWizard is the wizard pod with tower-attach and
	// repo-bootstrap init containers and a /data+/workspace volume pair.
	podShapeWizard
	// podShapeSubstrate is the apprentice/sage/cleric equivalent of the
	// wizard pod: same init containers staging substrate, but keyed on
	// cfg.Workspace rather than the hard-coded wizard branch. When
	// SPIRE_K8S_SHARED_WORKSPACE=1, a borrowed_worktree workspace
	// mounts the parent wizard's PVC instead of an emptyDir.
	podShapeSubstrate
)

// selectPodShape picks the builder for cfg. Wizard always goes to the
// wizard shape; non-wizard roles go to substrate when their workspace
// handle carries a non-repo kind.
func selectPodShape(cfg SpawnConfig) podShape {
	if cfg.Role == RoleWizard {
		return podShapeWizard
	}
	if cfg.Workspace != nil && cfg.Workspace.Kind != runtime.WorkspaceKindRepo {
		return podShapeSubstrate
	}
	return podShapeFlat
}

// resolveIdentity returns the canonical RepoIdentity for cfg. When
// cfg.Identity is populated (by the executor's dispatch site), it is
// used as-is. Otherwise a best-effort identity is synthesized from the
// legacy SpawnConfig fields (cfg.Tower, cfg.RepoURL, cfg.RepoBranch,
// cfg.RepoPrefix) to preserve behavior during the migration window.
// Once every dispatch site populates cfg.Identity this fallback becomes
// dead code and can be removed.
func resolveIdentity(cfg SpawnConfig) runtime.RepoIdentity {
	ident := cfg.Identity
	if ident.TowerName == "" {
		ident.TowerName = cfg.Tower
	}
	if ident.Prefix == "" {
		ident.Prefix = cfg.RepoPrefix
	}
	if ident.RepoURL == "" {
		ident.RepoURL = cfg.RepoURL
	}
	if ident.BaseBranch == "" {
		ident.BaseBranch = cfg.RepoBranch
	}
	return ident
}

// resolveDolthubRemote returns the dolthub remote for the tower-attach
// init container. The canonical source is RepoIdentity — but the
// identity type does not (yet) carry DolthubRemote as a first-class
// field; until it does, we read from cfg.Run (if populated by the
// executor) or fall back to the DOLTHUB_REMOTE env. This is the one
// remaining env read in the backend and is documented in the package
// README as transitional.
func resolveDolthubRemote(cfg SpawnConfig) string {
	// DOLTHUB_REMOTE is not on RepoIdentity today; fall through to the
	// env for now. This is the only process-env read that remains in
	// the backend, and it mirrors the steward's own resolution path so
	// init containers across the cluster agree on one source.
	return os.Getenv("DOLTHUB_REMOTE")
}

// isIdentityZero reports whether an identity is missing the three
// pieces needed to stage tower data + bootstrap a repo.
func isIdentityZero(ident runtime.RepoIdentity) bool {
	return ident.TowerName == "" && ident.Prefix == "" && ident.RepoURL == ""
}

// buildWizardPod produces the canonical wizard pod: a tower-attach init
// container that stages .beads and tower config onto emptyDir /data, a
// repo-bootstrap init container that clones the bead's repo into
// /workspace/<prefix> and binds it locally (spi-fopwn), a /workspace
// emptyDir shared with the main container, matching volume mounts on
// the main container, DOLT_DATA_DIR and SPIRE_CONFIG_DIR env vars so
// resolveBeadsDir() finds the staged store, SPIRE_REPO_PREFIX so
// wizard.ResolveRepo deterministically keys cfg.Instances, and
// WizardResources() for the main container.
//
// Repo bootstrap inputs (RepoURL, RepoBranch, RepoPrefix) come from
// cfg.Identity if populated, else from the legacy cfg.RepoURL /
// cfg.RepoBranch / cfg.RepoPrefix fields (see resolveIdentity). Any
// empty input surfaces as a typed error at Spawn time rather than
// producing a pod that fails at ResolveRepo with an opaque "no local
// repo registered" message.
func (b *K8sBackend) buildWizardPod(cfg SpawnConfig, ident runtime.RepoIdentity, podName string, args []string, env []corev1.EnvVar) (*corev1.Pod, error) {
	// Error messages reference the legacy SpawnConfig field names
	// (RepoURL/RepoBranch/RepoPrefix) because that is what callers see
	// at dispatch sites today; Identity is populated by resolveIdentity
	// from those fields during the migration window. Once every
	// dispatch site populates Identity directly, the messages can move
	// to the canonical names (RepoURL/BaseBranch/Prefix).
	if ident.RepoURL == "" {
		return nil, fmt.Errorf("%w: wizard pod spec: RepoURL is required (bead %s)", ErrIdentityRequired, cfg.BeadID)
	}
	if ident.BaseBranch == "" {
		return nil, fmt.Errorf("%w: wizard pod spec: RepoBranch is required (bead %s)", ErrIdentityRequired, cfg.BeadID)
	}
	if ident.Prefix == "" {
		return nil, fmt.Errorf("%w: wizard pod spec: RepoPrefix is required (bead %s)", ErrIdentityRequired, cfg.BeadID)
	}

	db := ident.TowerName
	if db == "" {
		// Tower name == database name (per pkg/tower/attach-cluster).
		// Fall back to cfg.Tower for dispatch sites that set Tower
		// without populating Identity.
		db = cfg.Tower
	}

	env = append(env, substrateEnv(ident)...)
	env = append(env, logsEnv(cfg)...)

	dataMount := corev1.VolumeMount{Name: "data", MountPath: "/data"}
	workspaceMount := corev1.VolumeMount{Name: "workspace", MountPath: "/workspace"}
	logsMount := corev1.VolumeMount{Name: LogsVolumeName, MountPath: LogsMountPath}

	// Workspace volume source: emptyDir by default. When the caller opts
	// in via cfg.SharedWorkspace, back /workspace with a per-wizard PVC
	// instead so child apprentice/sage pods that mount the same PVC by
	// label-selector see the wizard's clone. The PVC itself is
	// provisioned out-of-band by the operator reconciler — this code path
	// only wires the mount. A pod that references a missing PVC stays
	// Pending until the operator creates it, which is the intended
	// two-step (create pod → attach PVC with ownerRef=pod).
	workspaceVol := corev1.Volume{
		Name:         "workspace",
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
	}
	if cfg.SharedWorkspace {
		workspaceVol.VolumeSource = corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
				ClaimName: OwningWizardPVCName(podName),
			},
		}
	}

	volumes := []corev1.Volume{
		{
			Name:         "data",
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		},
		workspaceVol,
		{
			Name:         LogsVolumeName,
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		},
	}

	initContainers := []corev1.Container{
		b.towerAttachInit(db, ident, env, []corev1.VolumeMount{dataMount}),
		b.repoBootstrapInit(env, []corev1.VolumeMount{dataMount, workspaceMount}),
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: b.namespace,
			Labels:    b.podLabels(cfg, ident),
			Annotations: b.podAnnotations(cfg),
		},
		Spec: corev1.PodSpec{
			RestartPolicy:     corev1.RestartPolicyNever,
			PriorityClassName: "spire-agent-default",
			Volumes:           volumes,
			InitContainers:    initContainers,
			Containers: []corev1.Container{
				{
					Name:         "agent",
					Image:        b.image,
					Command:      append([]string{"spire"}, args...),
					Env:          env,
					Resources:    WizardResources(),
					VolumeMounts: []corev1.VolumeMount{dataMount, workspaceMount, logsMount},
				},
			},
		},
	}

	return pod, nil
}

// OwningWizardPVCName returns the deterministic name of the per-wizard
// shared-workspace PVC for a given wizard pod. Kept as a shared helper
// so the shared pod builder, the operator PVC reconciler, and any test
// that asserts wiring end-to-end all agree on the same name.
//
// The pod name (not the agent/guild name) is the anchor because a
// wizard pod that gets recreated must get a fresh PVC — the old PVC is
// GC'd via the previous pod's ownerRef. If we anchored on the guild or
// agent name, a restart would reuse a potentially-corrupt PVC from the
// prior run.
func OwningWizardPVCName(podName string) string {
	return podName + "-workspace"
}

// buildSubstratePod produces an apprentice/sage/cleric pod that needs
// a materialized workspace substrate — the same contract as the wizard
// pod, but keyed off cfg.Workspace rather than hard-coded. Closes the
// apprentice/sage-in-k8s gap from the test matrix (design §5).
//
// Volumes:
//   - /data emptyDir (tower-attach target)
//   - /workspace: emptyDir by default. When SPIRE_K8S_SHARED_WORKSPACE=1
//     and cfg.Workspace.Kind == WorkspaceKindBorrowedWorktree, the
//     workspace is mounted from the parent wizard pod's PVC (located by
//     the spire.io/owning-wizard-pod label selector).
func (b *K8sBackend) buildSubstratePod(cfg SpawnConfig, ident runtime.RepoIdentity, podName string, args []string, env []corev1.EnvVar) (*corev1.Pod, error) {
	if cfg.Workspace == nil {
		return nil, fmt.Errorf("%w: role %q requires cfg.Workspace (bead %s)", ErrWorkspaceRequired, cfg.Role, cfg.BeadID)
	}
	if isIdentityZero(ident) {
		return nil, fmt.Errorf("%w: role %q requires cfg.Identity (bead %s)", ErrIdentityRequired, cfg.Role, cfg.BeadID)
	}
	if ident.RepoURL == "" {
		return nil, fmt.Errorf("%w: role %q requires RepoURL (bead %s)", ErrIdentityRequired, cfg.Role, cfg.BeadID)
	}
	if ident.BaseBranch == "" {
		return nil, fmt.Errorf("%w: role %q requires BaseBranch (bead %s)", ErrIdentityRequired, cfg.Role, cfg.BeadID)
	}
	if ident.Prefix == "" {
		return nil, fmt.Errorf("%w: role %q requires Prefix (bead %s)", ErrIdentityRequired, cfg.Role, cfg.BeadID)
	}

	db := ident.TowerName
	if db == "" {
		db = cfg.Tower
	}

	env = append(env, substrateEnv(ident)...)
	env = append(env, logsEnv(cfg)...)

	dataMount := corev1.VolumeMount{Name: "data", MountPath: "/data"}
	workspaceMount := corev1.VolumeMount{Name: "workspace", MountPath: "/workspace"}
	logsMount := corev1.VolumeMount{Name: LogsVolumeName, MountPath: LogsMountPath}

	// Pick the workspace volume source. Default: emptyDir (fresh
	// per-pod substrate). Shared-PVC: when the gate is on and the kind
	// signals borrowed-worktree continuation, mount the parent
	// wizard's PVC so the child sees the wizard-owned checkout.
	workspaceVol, err := b.resolveWorkspaceVolume(cfg)
	if err != nil {
		return nil, err
	}

	volumes := []corev1.Volume{
		{
			Name:         "data",
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		},
		{
			Name:         "workspace",
			VolumeSource: workspaceVol,
		},
		{
			Name:         LogsVolumeName,
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		},
	}

	initContainers := []corev1.Container{
		b.towerAttachInit(db, ident, env, []corev1.VolumeMount{dataMount}),
		b.repoBootstrapInit(env, []corev1.VolumeMount{dataMount, workspaceMount}),
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: b.namespace,
			Labels:    b.podLabels(cfg, ident),
			Annotations: b.podAnnotations(cfg),
		},
		Spec: corev1.PodSpec{
			RestartPolicy:     corev1.RestartPolicyNever,
			PriorityClassName: "spire-agent-default",
			Volumes:           volumes,
			InitContainers:    initContainers,
			Containers: []corev1.Container{
				{
					Name:         "agent",
					Image:        b.image,
					Command:      append([]string{"spire"}, args...),
					Env:          env,
					Resources:    resourcesForRole(cfg.Role),
					VolumeMounts: []corev1.VolumeMount{dataMount, workspaceMount, logsMount},
				},
			},
		},
	}

	return pod, nil
}

// buildFlatPod produces the legacy flat executor pod (no init
// containers, no volumes). This is the gate-OFF baseline for
// apprentice/sage spawns that arrive without cfg.Workspace — the
// existing byte-for-byte output before the shared-builder refactor.
func (b *K8sBackend) buildFlatPod(cfg SpawnConfig, podName string, args []string, env []corev1.EnvVar) *corev1.Pod {
	ident := resolveIdentity(cfg)
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: b.namespace,
			Labels:    b.podLabels(cfg, ident),
			Annotations: b.podAnnotations(cfg),
		},
		Spec: corev1.PodSpec{
			RestartPolicy:     corev1.RestartPolicyNever,
			PriorityClassName: "spire-agent-default",
			Containers: []corev1.Container{
				{
					Name:      "agent",
					Image:     b.image,
					Command:   append([]string{"spire"}, args...),
					Env:       env,
					Resources: resourcesForRole(cfg.Role),
				},
			},
		},
	}
}

// resolveWorkspaceVolume returns the VolumeSource for the pod's
// /workspace mount. Default: emptyDir. When SPIRE_K8S_SHARED_WORKSPACE=1
// and cfg.Workspace.Kind == WorkspaceKindBorrowedWorktree, the volume
// is backed by the parent wizard pod's PVC, located via the
// `spire.io/owning-wizard-pod=<name>` label selector. A missing PVC
// surfaces as ErrSharedWorkspacePVCNotFound — no silent fallback to
// emptyDir.
func (b *K8sBackend) resolveWorkspaceVolume(cfg SpawnConfig) (corev1.VolumeSource, error) {
	gateOn := os.Getenv("SPIRE_K8S_SHARED_WORKSPACE") == "1"
	if !gateOn || cfg.Workspace == nil || cfg.Workspace.Kind != runtime.WorkspaceKindBorrowedWorktree {
		return corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}, nil
	}

	parent := parentWizardPodName(cfg)
	if parent == "" {
		return corev1.VolumeSource{}, fmt.Errorf("%w: cannot derive parent wizard pod name (bead %s)", ErrSharedWorkspacePVCNotFound, cfg.BeadID)
	}

	pvcs, err := b.client.CoreV1().PersistentVolumeClaims(b.namespace).List(
		context.Background(),
		metav1.ListOptions{LabelSelector: fmt.Sprintf("%s=%s", LabelOwningWizardPod, parent)},
	)
	if err != nil {
		return corev1.VolumeSource{}, fmt.Errorf("%w: list PVCs: %v", ErrSharedWorkspacePVCNotFound, err)
	}
	if len(pvcs.Items) == 0 {
		return corev1.VolumeSource{}, fmt.Errorf("%w: no PVC with %s=%s (bead %s)",
			ErrSharedWorkspacePVCNotFound, LabelOwningWizardPod, parent, cfg.BeadID)
	}

	return corev1.VolumeSource{
		PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
			ClaimName: pvcs.Items[0].Name,
		},
	}, nil
}

// parentWizardPodName returns the name of the parent wizard pod that
// owns the shared PVC for a borrowed-worktree child. It is derived
// from cfg.Run.RunID (the wizard pod's name is RunID when the wizard
// was itself spawned by the operator or a parent executor) or from the
// agent name's conventional prefix (e.g. "apprentice-spi-abc-0" has
// parent "wizard-spi-abc"). The heuristic is intentionally simple —
// later work can plumb an explicit ParentPod field onto SpawnConfig if
// the agent-name convention proves fragile.
//
// The returned name is normalized via podNameLookupKey (no timestamp
// suffix) because it is used as a label selector against existing
// PVCs — sanitizePodName's collision-avoidance suffix is only valid
// for freshly-created pods.
func parentWizardPodName(cfg SpawnConfig) string {
	if cfg.Run.RunID != "" {
		return podNameLookupKey(cfg.Run.RunID)
	}
	// Convention: apprentice-spi-abc-0 → wizard-spi-abc (drop the
	// last "-N" fan-out index). Works for the canonical wave names
	// produced by action_dispatch.go.
	name := cfg.Name
	if idx := strings.LastIndexByte(name, '-'); idx > 0 {
		name = name[:idx]
	}
	if strings.HasPrefix(name, "apprentice-") {
		return podNameLookupKey("wizard-" + strings.TrimPrefix(name, "apprentice-"))
	}
	if strings.HasPrefix(name, "sage-") {
		return podNameLookupKey("wizard-" + strings.TrimPrefix(name, "sage-"))
	}
	return ""
}

// podNameLookupKey normalizes a name to the same lowercase / charset
// rules as sanitizePodName but WITHOUT the timestamp suffix —
// suitable for label-selector lookups that need to match an existing
// pod's stable name.
func podNameLookupKey(name string) string {
	name = strings.ToLower(name)
	name = strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			return r
		}
		return '-'
	}, name)
	name = strings.Trim(name, "-")
	if len(name) > 253 {
		name = name[:253]
	}
	return name
}

// towerAttachInit builds the tower-attach init container that stages
// /data/<db>/.beads and tower config onto the shared emptyDir. Shared
// env plus the supplied volume mounts so the init container has the
// same dolt cluster pointers as the main container — without this the
// init container falls back to laptop localhost defaults and
// attach-cluster times out.
func (b *K8sBackend) towerAttachInit(db string, ident runtime.RepoIdentity, env []corev1.EnvVar, mounts []corev1.VolumeMount) corev1.Container {
	return corev1.Container{
		Name:  "tower-attach",
		Image: b.image,
		Command: []string{
			"spire", "tower", "attach-cluster",
			"--data-dir=/data/" + db,
			"--database=" + db,
			"--prefix=" + ident.Prefix,
			"--dolthub-remote=" + resolveDolthubRemote(SpawnConfig{Identity: ident}),
		},
		Env:          env,
		VolumeMounts: mounts,
	}
}

// repoBootstrapInit builds the repo-bootstrap init container that
// clones SPIRE_REPO_URL@SPIRE_REPO_BRANCH into /workspace/<prefix> and
// binds it locally so wizard.ResolveRepo succeeds when the main
// container starts.
func (b *K8sBackend) repoBootstrapInit(env []corev1.EnvVar, mounts []corev1.VolumeMount) corev1.Container {
	return corev1.Container{
		Name:  "repo-bootstrap",
		Image: b.image,
		// Fail-fast validation in shell covers the case where some
		// future wiring strips the env vars before they reach the
		// pod. Without this, git clone would emit a confusing
		// "fatal: repository '' not found" and the bind would silently
		// skip the mount instead of surfacing a clear config error.
		Command:      []string{"sh", "-c", repoBootstrapScript},
		Env:          env,
		VolumeMounts: mounts,
	}
}

// logsEnv builds the SPIRE_AGENT_NAME / SPIRE_LOG_ROOT env vars that
// the in-pod log writers (pkg/runctx) read to locate the canonical
// per-run artifact directory. SPIRE_LOG_ROOT mirrors LogsMountPath —
// both backend paths emit it the same way so the eventual log
// exporter (spi-k1cnof) tails the same files regardless of which pod
// builder produced the pod.
//
// SPIRE_AGENT_NAME falls back to cfg.Name when cfg.Run.AgentName is
// empty. The backend-process path (backend_process.go) uses the same
// fallback so the canonical RunContext.AgentName resolves to the
// agent name the executor dispatched against, even on legacy call
// sites that have not yet populated cfg.Run.AgentName explicitly.
func logsEnv(cfg SpawnConfig) []corev1.EnvVar {
	agentName := cfg.Run.AgentName
	if agentName == "" {
		agentName = cfg.Name
	}
	return []corev1.EnvVar{
		{Name: "SPIRE_AGENT_NAME", Value: agentName},
		{Name: "SPIRE_LOG_ROOT", Value: LogsMountPath},
	}
}

// substrateEnv builds the canonical env vars a substrate-enabled pod
// must carry: the SPIRE_REPO_* trio consumed by the repo-bootstrap
// init container's shell script, plus the DOLT_DATA_DIR /
// SPIRE_CONFIG_DIR pair consumed by resolveBeadsDir() and the
// BEADS_DATABASE / BEADS_PREFIX / DOLTHUB_REMOTE vars that downstream
// dolt tooling expects.
func substrateEnv(ident runtime.RepoIdentity) []corev1.EnvVar {
	env := []corev1.EnvVar{
		{Name: "DOLT_DATA_DIR", Value: "/data"},
		{Name: "SPIRE_CONFIG_DIR", Value: "/data/spire-config"},
		{Name: "SPIRE_REPO_URL", Value: ident.RepoURL},
		{Name: "SPIRE_REPO_BRANCH", Value: ident.BaseBranch},
		{Name: "SPIRE_REPO_PREFIX", Value: ident.Prefix},
	}
	if ident.TowerName != "" {
		env = append(env, corev1.EnvVar{Name: "BEADS_DATABASE", Value: ident.TowerName})
	}
	if ident.Prefix != "" {
		env = append(env, corev1.EnvVar{Name: "BEADS_PREFIX", Value: ident.Prefix})
	}
	if r := resolveDolthubRemote(SpawnConfig{Identity: ident}); r != "" {
		env = append(env, corev1.EnvVar{Name: "DOLTHUB_REMOTE", Value: r})
	}
	return env
}

// secretEnvRefs builds the ANTHROPIC_API_KEY and GITHUB_TOKEN secret
// references. Key names match what `helm/spire/templates/secret.yaml`
// writes. GITHUB_TOKEN is optional so installs without a github token
// (e.g. smoke tests that don't push) don't block pod creation on a
// missing key.
func secretEnvRefs(secretName string) []corev1.EnvVar {
	optional := true
	return []corev1.EnvVar{
		{
			Name: "ANTHROPIC_API_KEY",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
					Key:                  "ANTHROPIC_API_KEY_DEFAULT",
				},
			},
		},
		{
			Name: "GITHUB_TOKEN",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
					Key:                  "GITHUB_TOKEN",
					Optional:             &optional,
				},
			},
		},
	}
}

// Canonical label keys written to every pod created by this backend
// (spi-wqax9). The spire.* labels below preserve the pre-existing
// network-policy / discovery surface; the spire.io/* labels are the
// runtime-contract vocabulary from docs/design/spi-xplwy-runtime-contract.md
// §1. High-cardinality identifiers (attempt_id, run_id) are emitted as
// annotations, not labels, so metric/selector cardinality stays bounded.
const (
	LabelTower           = "spire.io/tower"
	LabelPrefix          = "spire.io/prefix"
	LabelBead            = "spire.io/bead"
	LabelRole            = "spire.io/role"
	LabelFormulaStep     = "spire.io/formula-step"
	LabelWorkspaceKind   = "spire.io/workspace-kind"
	LabelWorkspaceName   = "spire.io/workspace-name"
	LabelWorkspaceOrigin = "spire.io/workspace-origin"
	LabelHandoffMode     = "spire.io/handoff-mode"
	LabelBackend         = "spire.io/backend"
	LabelOwningWizardPod = "spire.io/owning-wizard-pod"

	AnnotationAttemptID = "spire.io/attempt-id"
	AnnotationRunID     = "spire.io/run-id"
)

// podLabels constructs the full canonical label set for a pod. The
// legacy spire.agent / spire.bead / spire.role / spire.tower labels
// are preserved unchanged because network policies and the List() /
// findPod() code paths still match on them.
func (b *K8sBackend) podLabels(cfg SpawnConfig, ident runtime.RepoIdentity) map[string]string {
	labels := map[string]string{
		// Legacy labels — preserved byte-for-byte so discovery and
		// network policies do not regress.
		"spire.agent":      "true",
		"spire.agent.name": cfg.Name,
		"spire.bead":       cfg.BeadID,
		"spire.role":       string(cfg.Role),
		"spire.tower":      cfg.Tower,
	}

	// Canonical spire.io/* label vocabulary. Low-cardinality fields
	// only; attempt/run go on annotations below.
	setLabel(labels, LabelBackend, "k8s")
	setLabel(labels, LabelTower, ident.TowerName)
	setLabel(labels, LabelPrefix, ident.Prefix)
	setLabel(labels, LabelBead, cfg.BeadID)
	setLabel(labels, LabelRole, string(cfg.Role))
	setLabel(labels, LabelFormulaStep, cfg.Run.FormulaStep)
	if cfg.Run.FormulaStep == "" && cfg.Step != "" {
		// Legacy Step field — keep the label non-empty for pre-Run
		// dispatch sites that only set cfg.Step.
		setLabel(labels, LabelFormulaStep, cfg.Step)
	}
	if cfg.Workspace != nil {
		setLabel(labels, LabelWorkspaceKind, string(cfg.Workspace.Kind))
		setLabel(labels, LabelWorkspaceName, cfg.Workspace.Name)
		setLabel(labels, LabelWorkspaceOrigin, string(cfg.Workspace.Origin))
	} else {
		setLabel(labels, LabelWorkspaceKind, string(cfg.Run.WorkspaceKind))
		setLabel(labels, LabelWorkspaceName, cfg.Run.WorkspaceName)
		setLabel(labels, LabelWorkspaceOrigin, string(cfg.Run.WorkspaceOrigin))
	}
	setLabel(labels, LabelHandoffMode, string(cfg.Run.HandoffMode))

	return labels
}

// podAnnotations writes high-cardinality RunContext fields (attempt,
// run id) as annotations, not labels, so metric / selector
// cardinality stays bounded.
func (b *K8sBackend) podAnnotations(cfg SpawnConfig) map[string]string {
	annotations := map[string]string{}
	if cfg.Run.AttemptID != "" {
		annotations[AnnotationAttemptID] = cfg.Run.AttemptID
	} else if cfg.AttemptID != "" {
		annotations[AnnotationAttemptID] = cfg.AttemptID
	}
	if cfg.Run.RunID != "" {
		annotations[AnnotationRunID] = cfg.Run.RunID
	}
	if len(annotations) == 0 {
		return nil
	}
	return annotations
}

// setLabel writes key=val only when val is non-empty. Empty labels are
// dropped so discovery selectors do not accidentally match the wrong
// pod on an unset field.
func setLabel(labels map[string]string, key, val string) {
	if val == "" {
		return
	}
	labels[key] = val
}

// repoBootstrapScript is the shell command run by the repo-bootstrap
// init container. It performs three steps:
//
//  1. Validate that SPIRE_REPO_URL / SPIRE_REPO_BRANCH / SPIRE_REPO_PREFIX
//     are all set. Without this check, git clone and bind-local would emit
//     opaque errors downstream.
//  2. Clone SPIRE_REPO_URL@SPIRE_REPO_BRANCH into /workspace/<prefix>.
//  3. Invoke `spire repo bind-local` with the same values to populate
//     tower.LocalBindings[prefix] and cfg.Instances[prefix] on the shared
//     /data volume so wizard.ResolveRepo succeeds when the main container
//     starts. bind-local (not bind) is used deliberately: bind reads from
//     the shared dolt repos table and would need the prefix to already
//     be registered, which is the case in production but not in the
//     local-test flow where this init container runs before the steward
//     has reconciled repos.
const repoBootstrapScript = `set -e
: "${SPIRE_REPO_URL:?SPIRE_REPO_URL required}"
: "${SPIRE_REPO_BRANCH:?SPIRE_REPO_BRANCH required}"
: "${SPIRE_REPO_PREFIX:?SPIRE_REPO_PREFIX required}"
dest="/workspace/${SPIRE_REPO_PREFIX}"
if [ ! -d "${dest}/.git" ]; then
  git clone --branch "${SPIRE_REPO_BRANCH}" "${SPIRE_REPO_URL}" "${dest}"
fi
spire repo bind-local \
  --prefix "${SPIRE_REPO_PREFIX}" \
  --path "${dest}" \
  --repo-url "${SPIRE_REPO_URL}" \
  --branch "${SPIRE_REPO_BRANCH}"
`

// List returns Info for all Spire agent pods in the namespace.
func (b *K8sBackend) List() ([]Info, error) {
	pods, err := b.client.CoreV1().Pods(b.namespace).List(
		context.Background(),
		metav1.ListOptions{LabelSelector: "spire.agent=true"},
	)
	if err != nil {
		return nil, fmt.Errorf("list agent pods: %w", err)
	}

	infos := make([]Info, 0, len(pods.Items))
	for _, pod := range pods.Items {
		alive := pod.Status.Phase == corev1.PodRunning || pod.Status.Phase == corev1.PodPending
		infos = append(infos, Info{
			Name:       pod.Labels["spire.agent.name"],
			BeadID:     pod.Labels["spire.bead"],
			Phase:      pod.Labels["spire.role"],
			Alive:      alive,
			Identifier: pod.Name,
			StartedAt:  pod.CreationTimestamp.Time,
			Tower:      pod.Labels["spire.tower"],
		})
	}
	return infos, nil
}

// Logs returns a follow-stream of logs for the named agent's pod.
// Returns os.ErrNotExist if no pod is found.
func (b *K8sBackend) Logs(name string) (io.ReadCloser, error) {
	podName, err := b.findPod(name)
	if err != nil {
		return nil, err
	}

	follow := true
	stream, err := b.client.CoreV1().Pods(b.namespace).GetLogs(podName, &corev1.PodLogOptions{
		Follow: follow,
	}).Stream(context.Background())
	if err != nil {
		return nil, fmt.Errorf("get logs for pod %s: %w", podName, err)
	}

	return stream, nil
}

// Kill deletes the named agent's pod with a 10-second grace period.
func (b *K8sBackend) Kill(name string) error {
	podName, err := b.findPod(name)
	if err != nil {
		return err
	}

	grace := int64(10)
	return b.client.CoreV1().Pods(b.namespace).Delete(
		context.Background(), podName,
		metav1.DeleteOptions{GracePeriodSeconds: &grace},
	)
}

// TerminateBead is the cluster-mode equivalent of process-group
// termination: delete every pod owned by the bead/attempt label.
// Filed as a follow-up bead (spd-1lu5); the gateway reset handler
// short-circuits to 501 in TowerModeGateway today, so this method
// is unreachable from the v1 desktop reset path. Returns
// ErrTerminateBeadNotImplemented until the operator-driven
// termination intent lands.
func (b *K8sBackend) TerminateBead(ctx context.Context, beadID string) error {
	return ErrTerminateBeadNotImplemented
}

// findPod locates a pod by the spire.agent=<name> label.
// Returns os.ErrNotExist if not found.
func (b *K8sBackend) findPod(name string) (string, error) {
	pods, err := b.client.CoreV1().Pods(b.namespace).List(
		context.Background(),
		metav1.ListOptions{
			LabelSelector: fmt.Sprintf("spire.agent.name=%s", name),
		},
	)
	if err != nil {
		return "", fmt.Errorf("find pod for agent %s: %w", name, err)
	}
	if len(pods.Items) == 0 {
		return "", os.ErrNotExist
	}
	return pods.Items[0].Name, nil
}

// buildEnvVars constructs the standard environment variables for an agent pod,
// mirroring the process spawner's env setup. The returned slice is the
// main container's env (init containers reuse the same list).
func (b *K8sBackend) buildEnvVars(cfg SpawnConfig, ident runtime.RepoIdentity) []corev1.EnvVar {
	env := []corev1.EnvVar{
		{Name: "OTEL_EXPORTER_OTLP_ENDPOINT", Value: fmt.Sprintf("http://spire-steward.%s.svc:4317", b.namespace)},
		{Name: "CLAUDE_CODE_ENABLE_TELEMETRY", Value: "1"},
		{Name: "CLAUDE_CODE_ENHANCED_TELEMETRY_BETA", Value: "1"},
		{Name: "OTEL_TRACES_EXPORTER", Value: "otlp"},
		{Name: "OTEL_LOGS_EXPORTER", Value: "otlp"},
		{Name: "OTEL_EXPORTER_OTLP_PROTOCOL", Value: "grpc"},
		{Name: "BEADS_DOLT_SERVER_HOST", Value: fmt.Sprintf("spire-dolt.%s.svc", b.namespace)},
		// Cluster dolt service listens on 3306 (the chart default,
		// `.Values.dolt.port` in `helm/spire/values.yaml`); 3307 is the
		// laptop local port only. Hardcoded here because the backend
		// process doesn't have access to chart values at runtime.
		{Name: "BEADS_DOLT_SERVER_PORT", Value: "3306"},
	}

	tower := cfg.Tower
	if tower == "" {
		tower = ident.TowerName
	}
	if tower != "" {
		env = append(env, corev1.EnvVar{Name: "SPIRE_TOWER", Value: tower})
	}
	if cfg.Provider != "" {
		env = append(env, corev1.EnvVar{Name: "SPIRE_PROVIDER", Value: cfg.Provider})
	}
	if cfg.Role != "" {
		env = append(env, corev1.EnvVar{Name: "SPIRE_ROLE", Value: string(cfg.Role)})
	}

	// Apprentice identity env vars. Transport-agnostic: the apprentice reads
	// them to resolve which bead to write to and what role to claim at
	// submit time.
	if cfg.BeadID != "" {
		env = append(env, corev1.EnvVar{Name: "SPIRE_BEAD_ID", Value: cfg.BeadID})
	}
	if cfg.AttemptID != "" {
		env = append(env, corev1.EnvVar{Name: "SPIRE_ATTEMPT_ID", Value: cfg.AttemptID})
	}
	if cfg.ApprenticeIdx != "" {
		env = append(env, corev1.EnvVar{Name: "SPIRE_APPRENTICE_IDX", Value: cfg.ApprenticeIdx})
	}

	// SPIRE_AGENT_NAME / SPIRE_LOG_ROOT — pkg/runctx in the pod reads
	// these via runtime.RunContextFromEnv() to derive canonical artifact
	// paths. SPIRE_AGENT_NAME falls back to cfg.Name when the executor
	// has not populated cfg.Run.AgentName.
	env = append(env, logsEnv(cfg)...)

	// Canonical RunContext env (docs/design/spi-xplwy-runtime-contract.md
	// §1.4). Every canonical log-field value flows into the pod so the
	// in-pod worker's runtime.RunContextFromEnv() reconstructs the full
	// identity set and stamps it on every structured log line. Missing
	// fields are omitted so unset values do not leak as blank env; the
	// consumer treats absence as empty-string, matching the log-surface
	// contract. BEADS_PREFIX is already emitted via the tower/identity
	// surface; SPIRE_REPO_PREFIX carries the same value under the
	// canonical observability name.
	prefix := ident.Prefix
	if prefix == "" {
		prefix = cfg.RepoPrefix
	}
	if prefix != "" {
		env = append(env, corev1.EnvVar{Name: "SPIRE_REPO_PREFIX", Value: prefix})
	}
	if cfg.Run.RunID != "" {
		env = append(env, corev1.EnvVar{Name: "SPIRE_RUN_ID", Value: cfg.Run.RunID})
	}
	step := cfg.Run.FormulaStep
	if step == "" {
		step = cfg.Step
	}
	if step != "" {
		env = append(env, corev1.EnvVar{Name: "SPIRE_FORMULA_STEP", Value: step})
	}
	backend := cfg.Run.Backend
	if backend == "" {
		backend = "k8s"
	}
	env = append(env, corev1.EnvVar{Name: "SPIRE_BACKEND", Value: backend})
	if cfg.Run.WorkspaceKind != "" {
		env = append(env, corev1.EnvVar{Name: "SPIRE_WORKSPACE_KIND", Value: string(cfg.Run.WorkspaceKind)})
	} else if cfg.Workspace != nil && cfg.Workspace.Kind != "" {
		env = append(env, corev1.EnvVar{Name: "SPIRE_WORKSPACE_KIND", Value: string(cfg.Workspace.Kind)})
	}
	if cfg.Run.WorkspaceName != "" {
		env = append(env, corev1.EnvVar{Name: "SPIRE_WORKSPACE_NAME", Value: cfg.Run.WorkspaceName})
	} else if cfg.Workspace != nil && cfg.Workspace.Name != "" {
		env = append(env, corev1.EnvVar{Name: "SPIRE_WORKSPACE_NAME", Value: cfg.Workspace.Name})
	}
	if cfg.Run.WorkspaceOrigin != "" {
		env = append(env, corev1.EnvVar{Name: "SPIRE_WORKSPACE_ORIGIN", Value: string(cfg.Run.WorkspaceOrigin)})
	} else if cfg.Workspace != nil && cfg.Workspace.Origin != "" {
		env = append(env, corev1.EnvVar{Name: "SPIRE_WORKSPACE_ORIGIN", Value: string(cfg.Workspace.Origin)})
	}
	if cfg.Run.HandoffMode != "" {
		env = append(env, corev1.EnvVar{Name: "SPIRE_HANDOFF_MODE", Value: string(cfg.Run.HandoffMode)})
	}

	// Multi-token auth pool: surface the slot identity + state dir so
	// the in-pod claude subprocess can apply rate_limit_event lines
	// from the JSONL stream back to the slot's cached state. Mirrors
	// the process-spawn path; absent vars mean the legacy single-token
	// flow.
	if cfg.AuthSlot != "" {
		env = append(env, corev1.EnvVar{Name: "SPIRE_AUTH_SLOT", Value: cfg.AuthSlot})
	}
	if cfg.PoolStateDir != "" {
		env = append(env, corev1.EnvVar{Name: "SPIRE_AUTH_POOL_STATE_DIR", Value: cfg.PoolStateDir})
	}

	// OTEL resource attributes carry the canonical RunContext vocabulary
	// (docs/design/spi-xplwy-runtime-contract.md §1.4) so every
	// trace/log/metric correlates to the same identity set the wizard
	// stamps on its structured logs. Missing fields are omitted rather
	// than emitted blank to keep the attribute set compact.
	var resAttrs []string
	addAttr := func(k, v string) {
		if v == "" {
			return
		}
		resAttrs = append(resAttrs, k+"="+v)
	}
	if cfg.Name != "" {
		// agent.name retained for back-compat with existing alerts.
		resAttrs = append(resAttrs, "agent.name="+cfg.Name)
	}
	addAttr("tower", tower)
	addAttr("prefix", prefix)
	addAttr("bead_id", cfg.BeadID)
	attemptID := cfg.AttemptID
	if attemptID == "" {
		attemptID = cfg.Run.AttemptID
	}
	addAttr("attempt_id", attemptID)
	addAttr("run_id", cfg.Run.RunID)
	addAttr("role", string(cfg.Role))
	addAttr("formula_step", step)
	addAttr("backend", backend)
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
		env = append(env, corev1.EnvVar{
			Name:  "OTEL_RESOURCE_ATTRIBUTES",
			Value: strings.Join(resAttrs, ","),
		})
	}

	return env
}

// resourcesForRole returns CPU/memory requests and limits based on agent role.
func resourcesForRole(role SpawnRole) corev1.ResourceRequirements {
	switch role {
	case RoleApprentice:
		return corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceMemory: resource.MustParse("1Gi"),
				corev1.ResourceCPU:    resource.MustParse("500m"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceMemory: resource.MustParse("4Gi"),
				corev1.ResourceCPU:    resource.MustParse("2000m"),
			},
		}
	case RoleSage:
		return corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceMemory: resource.MustParse("256Mi"),
				corev1.ResourceCPU:    resource.MustParse("250m"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceMemory: resource.MustParse("1Gi"),
				corev1.ResourceCPU:    resource.MustParse("1000m"),
			},
		}
	case RoleWizard, RoleExecutor:
		return corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceMemory: resource.MustParse("256Mi"),
				corev1.ResourceCPU:    resource.MustParse("250m"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceMemory: resource.MustParse("512Mi"),
				corev1.ResourceCPU:    resource.MustParse("500m"),
			},
		}
	case RoleCleric:
		// Cleric pods are one-shot Claude invocations: spawn, think,
		// emit JSON, exit. They do not check out the workspace, do not
		// invoke the gateway directly (gateway action endpoints run
		// server-side), and do not run validation. Memory/CPU envelope
		// matches the wizard/executor tier with extra headroom for the
		// Claude invocation. Cleric runtime (spi-hhkozk).
		return corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceMemory: resource.MustParse("256Mi"),
				corev1.ResourceCPU:    resource.MustParse("250m"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceMemory: resource.MustParse("1Gi"),
				corev1.ResourceCPU:    resource.MustParse("500m"),
			},
		}
	default:
		// Fallback to wizard-tier resources.
		return corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceMemory: resource.MustParse("256Mi"),
				corev1.ResourceCPU:    resource.MustParse("250m"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceMemory: resource.MustParse("512Mi"),
				corev1.ResourceCPU:    resource.MustParse("500m"),
			},
		}
	}
}

// sanitizePodName converts an agent name to a valid k8s pod name.
// Pod names must be lowercase, alphanumeric, or '-', max 253 chars.
func sanitizePodName(name string) string {
	name = strings.ToLower(name)
	name = strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			return r
		}
		return '-'
	}, name)
	// Trim leading/trailing dashes.
	name = strings.Trim(name, "-")
	if len(name) > 253 {
		name = name[:253]
	}
	if name == "" {
		name = "spire-agent"
	}

	// Add a timestamp suffix to avoid name collisions.
	suffix := fmt.Sprintf("-%d", time.Now().UnixMilli()%100000)
	if len(name)+len(suffix) > 253 {
		name = name[:253-len(suffix)]
	}
	return name + suffix
}
