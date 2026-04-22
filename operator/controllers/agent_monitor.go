package controllers

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/go-logr/logr"

	spirev1 "github.com/awell-health/spire/operator/api/v1alpha1"
	"github.com/awell-health/spire/pkg/agent"
	"github.com/awell-health/spire/pkg/repoconfig"
	"github.com/awell-health/spire/pkg/runtime"
	"github.com/awell-health/spire/pkg/steward/identity"
	"github.com/awell-health/spire/pkg/store"
)

// apprenticeSignalKeyPrefix is the metadata-key prefix written by
// `spire apprentice submit`. Its presence on a bead means at least one
// apprentice finished submission (bundle or no-op). See cmd/spire/apprentice.go.
const apprenticeSignalKeyPrefix = "apprentice_signal_"

// getBeadMetadataFn is overridable in tests.
var getBeadMetadataFn = store.GetBeadMetadata

// hasApprenticeSignal returns true if any apprentice_signal_* metadata key
// is present on the bead. Enumerating by prefix handles the multi-apprentice
// case (each idx writes its own key) without needing the index up front.
func hasApprenticeSignal(beadID string) (bool, error) {
	md, err := getBeadMetadataFn(beadID)
	if err != nil {
		return false, err
	}
	for k := range md {
		if strings.HasPrefix(k, apprenticeSignalKeyPrefix) {
			return true, nil
		}
	}
	return false, nil
}

// AgentMonitor tracks agent heartbeats and manages pods for managed agents.
type AgentMonitor struct {
	Client         client.Client
	Log            logr.Logger
	Namespace      string
	Interval       time.Duration
	OfflineTimeout time.Duration // how long before an agent is considered offline
	StewardImage   string        // default image for managed agent pods

	// Runtime-contract identity inputs (docs/design/spi-xplwy-runtime-contract.md §1.1).
	//
	// Set once at operator startup from --database/--prefix/--dolthub-remote
	// or the matching env vars (helm plumbs these). Pod building NEVER reads
	// these from process env on every spawn — that was the ambient-CWD
	// anti-pattern spi-ypoqx removed from pkg/executor, extended here so
	// operator-managed pods follow the same rule.
	//
	// Database is the dolt database name (== tower identity). Falls back to
	// Namespace in main.go when unset (helm convention).
	Database string
	// Prefix is the default bead prefix. For single-prefix guilds this is
	// the authoritative value; multi-prefix guilds may override it from
	// WizardGuild.Spec.Prefixes when a single guild-level prefix is present.
	Prefix string
	// DolthubRemote is the dolt remote URL for the tower-attach init container.
	DolthubRemote string

	// Resolver is the canonical source of cluster repo identity per
	// spi-njzmg. When wired, buildWorkloadPod treats
	// WizardGuild.Spec.Repo/RepoBranch/Prefixes as projection-only and
	// reconciles them to the resolver's output — a drift between CR
	// and resolver is logged and the resolver's values win.
	//
	// Nil disables the drift check: CR fields are used verbatim
	// (the pre-spi-njzmg behavior) so existing unit tests continue to
	// pass without requiring a live store.
	Resolver identity.ClusterIdentityResolver
}

// Start implements controller-runtime's Runnable interface.
func (m *AgentMonitor) Start(ctx context.Context) error {
	m.Run(ctx)
	return nil
}

func (m *AgentMonitor) Run(ctx context.Context) {
	m.Log.Info("agent monitor starting", "interval", m.Interval, "tower", m.Database, "prefix", m.Prefix, "backend", "operator-k8s")
	ticker := time.NewTicker(m.Interval)
	defer ticker.Stop()

	m.cycle(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.cycle(ctx)
		}
	}
}

func (m *AgentMonitor) cycle(ctx context.Context) {
	m.Log.Info("agent monitor cycle start", "tower", m.Database, "prefix", m.Prefix, "backend", "operator-k8s")
	var agents spirev1.WizardGuildList
	if err := m.Client.List(ctx, &agents, client.InNamespace(m.Namespace)); err != nil {
		m.Log.Error(err, "failed to list agents", "tower", m.Database, "prefix", m.Prefix, "backend", "operator-k8s")
		return
	}
	m.Log.Info("agent monitor found agents", "count", len(agents.Items), "tower", m.Database, "prefix", m.Prefix, "backend", "operator-k8s")

	// Load SpireConfig for token/DoltHub resolution
	cfg := m.loadConfig(ctx)

	for i := range agents.Items {
		agent := &agents.Items[i]

		switch agent.Spec.Mode {
		case "external":
			m.checkExternalAgent(ctx, agent)
		case "managed":
			m.reconcileManagedAgent(ctx, agent, cfg)
		}
	}
}

// loadConfig reads the "default" SpireConfig from the namespace.
// Returns nil if not found (pods will be created without token injection).
func (m *AgentMonitor) loadConfig(ctx context.Context) *spirev1.SpireConfig {
	var cfg spirev1.SpireConfig
	if err := m.Client.Get(ctx, client.ObjectKey{Namespace: m.Namespace, Name: "default"}, &cfg); err != nil {
		if !errors.IsNotFound(err) {
			m.Log.Error(err, "failed to read SpireConfig", "tower", m.Database, "prefix", m.Prefix, "backend", "operator-k8s")
		}
		return nil
	}
	return &cfg
}

// checkExternalAgent updates phase based on lastSeen heartbeat.
func (m *AgentMonitor) checkExternalAgent(ctx context.Context, agent *spirev1.WizardGuild) {
	if agent.Status.LastSeen == "" {
		if agent.Status.Phase != "Offline" {
			agent.Status.Phase = "Offline"
			agent.Status.Message = "Never seen — agent has not run spire collect"
			m.Client.Status().Update(ctx, agent) //nolint
		}
		return
	}

	lastSeen, err := time.Parse(time.RFC3339, agent.Status.LastSeen)
	if err != nil {
		return
	}

	age := time.Since(lastSeen)
	if age > m.OfflineTimeout {
		if agent.Status.Phase != "Offline" {
			agent.Status.Phase = "Offline"
			agent.Status.Message = fmt.Sprintf("Last seen %s ago", age.Round(time.Minute))
			m.Client.Status().Update(ctx, agent) //nolint
			m.Log.Info("agent went offline", "agent_name", agent.Name, "lastSeen", age, "tower", m.Database, "prefix", m.Prefix, "backend", "operator-k8s")
		}
	}
}

// reconcileManagedAgent creates one pod per assigned workload (bead),
// and cleans up pods when work is removed.
func (m *AgentMonitor) reconcileManagedAgent(ctx context.Context, agent *spirev1.WizardGuild, cfg *spirev1.SpireConfig) {
	// List existing pods for this agent
	var podList corev1.PodList
	if err := m.Client.List(ctx, &podList,
		client.InNamespace(m.Namespace),
		client.MatchingLabels{"spire.awell.io/agent": agent.Name, "spire.awell.io/managed": "true"},
	); err != nil {
		m.Log.Error(err, "failed to list agent pods", "agent_name", agent.Name, "tower", m.Database, "prefix", m.Prefix, "backend", "operator-k8s")
		return
	}

	// Build set of bead IDs that have running pods
	podsByBead := make(map[string]*corev1.Pod)
	for i := range podList.Items {
		pod := &podList.Items[i]
		beadID := pod.Labels["spire.awell.io/bead"]
		if beadID != "" {
			podsByBead[beadID] = pod
		}
	}

	// Build set of currently assigned work
	workSet := make(map[string]bool)
	for _, beadID := range agent.Status.CurrentWork {
		workSet[beadID] = true
	}

	// Reap loop: the reconciler is a pure function of (pods × signals).
	// Walk the union of pod beads and CurrentWork beads and decide per-bead:
	//   signal present                       → success: drop from CurrentWork,
	//                                          delete pod, KEEP origin/feat/<bead>
	//                                          (wizard consumes it on merge).
	//   no signal, pod terminated            → failure: drop from CurrentWork,
	//                                          delete pod, delete origin/feat/<bead>.
	//   no signal, pod active                → in progress; skip.
	//   no signal, no pod                    → leave CurrentWork alone; the
	//                                          create-pod loop re-provisions.
	// "Signal" = any apprentice_signal_* metadata key (bundle or no-op). The
	// apprentice's comment is human UX; reading it would be a layering violation.
	statusChanged := false
	reaped := make(map[string]bool)
	allBeads := make(map[string]bool, len(podsByBead)+len(workSet))
	for beadID := range podsByBead {
		allBeads[beadID] = true
	}
	for beadID := range workSet {
		allBeads[beadID] = true
	}

	for beadID := range allBeads {
		pod, havePod := podsByBead[beadID]

		signalPresent, err := hasApprenticeSignal(beadID)
		if err != nil {
			m.Log.Error(err, "failed to read bead metadata for reap; skipping this cycle",
				"agent_name", agent.Name, "bead_id", beadID, "tower", m.Database, "prefix", m.Prefix, "backend", "operator-k8s")
			continue
		}

		switch {
		case signalPresent:
			if removeFromCurrentWork(agent, beadID) {
				delete(workSet, beadID)
				statusChanged = true
			}
			if havePod && pod.DeletionTimestamp == nil {
				if err := m.Client.Delete(ctx, pod); err != nil {
					m.Log.Error(err, "failed to delete completed pod", "pod", pod.Name, "agent_name", agent.Name, "bead_id", beadID, "tower", m.Database, "prefix", m.Prefix, "backend", "operator-k8s")
				}
			}
			reaped[beadID] = true
			m.Log.Info("reaped completed workload",
				"agent_name", agent.Name, "bead_id", beadID, "tower", m.Database, "prefix", m.Prefix, "backend", "operator-k8s", "reason", "signal")

		case havePod && (pod.Status.Phase == corev1.PodSucceeded ||
			pod.Status.Phase == corev1.PodFailed || isPodFinished(pod)):
			if removeFromCurrentWork(agent, beadID) {
				delete(workSet, beadID)
				statusChanged = true
			}
			// Apprentice's checkpoint push runs unconditionally, so cleanup of
			// the leaked remote branch has to live here, not in the entrypoint.
			if err := m.deleteRemoteFeatBranch(ctx, agent, beadID, cfg); err != nil {
				m.Log.Error(err, "failed to delete remote feat branch after non-success reap",
					"agent_name", agent.Name, "bead_id", beadID, "tower", m.Database, "prefix", m.Prefix, "backend", "operator-k8s")
			}
			if pod.DeletionTimestamp == nil {
				if err := m.Client.Delete(ctx, pod); err != nil {
					m.Log.Error(err, "failed to delete finished pod", "pod", pod.Name, "agent_name", agent.Name, "bead_id", beadID, "tower", m.Database, "prefix", m.Prefix, "backend", "operator-k8s")
				}
			}
			reaped[beadID] = true
			m.Log.Info("reaped completed workload",
				"agent_name", agent.Name, "bead_id", beadID, "tower", m.Database, "prefix", m.Prefix, "backend", "operator-k8s", "reason", "pod-terminated-no-signal")
		}
	}
	if statusChanged {
		if err := m.Client.Status().Update(ctx, agent); err != nil {
			m.Log.Error(err, "failed to update agent CurrentWork after reaping", "agent_name", agent.Name, "tower", m.Database, "prefix", m.Prefix, "backend", "operator-k8s")
		}
	}

	// Create pods for new work
	for _, beadID := range agent.Status.CurrentWork {
		if _, exists := podsByBead[beadID]; exists {
			continue // pod already running for this bead
		}

		// All workload types (task/bug/feature/chore/epic) route through the
		// canonical single-container wizard pod via the shared pkg/agent
		// builder. `spire execute <bead-id>` dispatches on the bead's type
		// internally — the operator no longer has to fan out to per-type pod
		// shapes. The Model A epic/review paths (wizard + sidecar, /comms
		// volume, beads-seed ConfigMap) were removed as part of chunk 4 of
		// the runtime-contract migration (spi-fjt2t).
		pod := m.buildWorkloadPod(agent, beadID, cfg)
		if pod == nil {
			// buildWorkloadPod logs and returns nil when SpawnConfig validation
			// fails (e.g. the CR is missing Repo or RepoBranch). Skip the
			// bead this cycle; the next reconcile will retry once the CR is
			// fixed.
			continue
		}
		if err := m.Client.Create(ctx, pod); err != nil {
			m.Log.Error(err, "failed to create workload pod", "agent_name", agent.Name, "bead_id", beadID, "tower", m.Database, "prefix", m.Prefix, "backend", "operator-k8s")
			continue
		}

		m.Log.Info("created workload pod", "agent_name", agent.Name, "bead_id", beadID, "tower", m.Database, "prefix", m.Prefix, "backend", "operator-k8s", "pod", pod.Name, "role", pod.Labels["spire.awell.io/role"], "workspace_kind", pod.Labels["spire.awell.io/workspace-kind"], "workspace_name", pod.Labels["spire.awell.io/workspace-name"], "handoff_mode", pod.Labels["spire.awell.io/handoff-mode"])

		// After Create, pod.UID is populated by the client. Provision the
		// per-wizard shared-workspace PVC so the pod can transition out
		// of Pending. BlockOwnerDeletion ensures the PVC is kept around
		// until the pod fully terminates, so in-flight writes flush.
		if err := m.ensureOwningWizardPVC(ctx, agent, pod); err != nil {
			m.Log.Error(err, "failed to ensure owning-wizard PVC",
				"agent_name", agent.Name, "bead_id", beadID, "pod", pod.Name,
				"tower", m.Database, "prefix", m.Prefix, "backend", "operator-k8s")
		}
	}

	// Attach the owning-wizard PVC to any existing wizard pods that
	// don't yet have one. This handles the recovery case where the
	// operator restarted after creating the pod but before creating
	// the PVC, and the steady-state case where a CR flip from
	// sharedWorkspace=false → true needs to provision against pods
	// that already exist. Skip pods that were just reaped or are
	// being deleted — attaching a PVC to a terminating pod would
	// provision something that kube-gc tears down on the next cycle.
	for beadID, pod := range podsByBead {
		if reaped[beadID] || pod.DeletionTimestamp != nil {
			continue
		}
		if err := m.ensureOwningWizardPVC(ctx, agent, pod); err != nil {
			m.Log.Error(err, "failed to ensure owning-wizard PVC for existing pod",
				"agent_name", agent.Name, "pod", pod.Name,
				"tower", m.Database, "prefix", m.Prefix, "backend", "operator-k8s")
		}
	}

	// Delete pods for work that's no longer assigned
	for beadID, pod := range podsByBead {
		if workSet[beadID] {
			continue
		}
		if reaped[beadID] {
			continue // reap loop already issued Delete
		}
		if pod.DeletionTimestamp != nil {
			continue
		}
		if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue // already handled above
		}
		if err := m.Client.Delete(ctx, pod); err != nil {
			m.Log.Error(err, "failed to delete stale workload pod", "pod", pod.Name, "bead_id", beadID, "tower", m.Database, "prefix", m.Prefix, "backend", "operator-k8s")
		} else {
			m.Log.Info("deleted stale workload pod", "agent_name", agent.Name, "bead_id", beadID, "tower", m.Database, "prefix", m.Prefix, "backend", "operator-k8s")
		}
	}

	// Update agent phase based on pod states
	m.updateAgentPhase(ctx, agent, podsByBead)
}

// updateAgentPhase sets the agent phase based on its running pods.
func (m *AgentMonitor) updateAgentPhase(ctx context.Context, agent *spirev1.WizardGuild, podsByBead map[string]*corev1.Pod) {
	if len(agent.Status.CurrentWork) == 0 {
		if agent.Status.Phase != "Idle" {
			agent.Status.Phase = "Idle"
			agent.Status.PodName = ""
			agent.Status.Message = "No active work"
			m.Client.Status().Update(ctx, agent) //nolint
		}
		return
	}

	// Check if any pods are still provisioning
	anyProvisioning := false
	anyFailed := false
	for _, beadID := range agent.Status.CurrentWork {
		pod, exists := podsByBead[beadID]
		if !exists {
			anyProvisioning = true // pod not created yet
			continue
		}
		switch pod.Status.Phase {
		case corev1.PodPending:
			anyProvisioning = true
		case corev1.PodFailed:
			anyFailed = true
		}
	}

	newPhase := "Working"
	msg := fmt.Sprintf("%d active workload(s)", len(agent.Status.CurrentWork))
	if anyProvisioning {
		newPhase = "Provisioning"
		msg = "Waiting for pod(s) to start"
	} else if anyFailed {
		msg = "One or more workload pods failed"
	}

	if agent.Status.Phase != newPhase || agent.Status.Message != msg {
		agent.Status.Phase = newPhase
		agent.Status.Message = msg
		m.Client.Status().Update(ctx, agent) //nolint
	}
}

// buildWorkloadPod creates the canonical single-container wizard pod for a
// single bead assignment (task/bug/feature/chore workloads).
//
// As of spi-fjt2t (chunk 4 of docs/design/spi-xplwy-runtime-contract.md §4),
// pod construction delegates to the shared pkg/agent builder via
// agent.NewPodBuilder(...).BuildPod(cfg). The operator's role is to:
//   - translate WizardGuild CR + SpireConfig + tower identity into a
//     runtime.SpawnConfig (Identity / Workspace / Run are populated from
//     canonical sources — never from ambient env);
//   - call BuildPod to get the canonical pod shape;
//   - apply operator-specific overlays that the shared builder does not
//     know about (guild-scoped labels, SpireConfig-sourced secret refs,
//     MaxApprentices env, resource override, operator-flavored pod name).
//
// After the overlay the pod is byte-for-byte equivalent to what the shared
// builder would produce for the same SpawnConfig, plus the overlay fields.
// See TestBuildWorkloadPod_SharedBuilderParity for the enforced contract.
func (m *AgentMonitor) buildWorkloadPod(wg *spirev1.WizardGuild, beadID string, cfg *spirev1.SpireConfig) *corev1.Pod {
	image := wg.Spec.Image
	if image == "" {
		image = m.StewardImage
	}

	branch := wg.Spec.RepoBranch
	if branch == "" {
		branch = repoconfig.DefaultBranchBase
	}

	prefix := m.resolvePrefix(wg)
	db := m.resolveDatabase()

	// spi-njzmg: CR fields Repo/RepoBranch/Prefixes are projection-only.
	// When a ClusterIdentityResolver is wired, reconcile them to the
	// shared repo-registry output so scheduling decisions never treat
	// the CR as source of truth. Drift is logged; resolver wins.
	repoURL := wg.Spec.Repo
	if m.Resolver != nil && prefix != "" {
		if resolved, err := m.Resolver.Resolve(context.Background(), prefix); err == nil {
			if repoURL != "" && repoURL != resolved.URL {
				m.Log.Info("wizardguild Repo drifts from canonical resolver; using resolver",
					"agent_name", wg.Name, "prefix", prefix,
					"cr_url", repoURL, "canonical_url", resolved.URL, "backend", "operator-k8s")
			}
			if branch != "" && branch != resolved.BaseBranch {
				m.Log.Info("wizardguild RepoBranch drifts from canonical resolver; using resolver",
					"agent_name", wg.Name, "prefix", prefix,
					"cr_branch", branch, "canonical_branch", resolved.BaseBranch, "backend", "operator-k8s")
			}
			repoURL = resolved.URL
			branch = resolved.BaseBranch
		} else {
			m.Log.V(1).Info("resolver unable to resolve cluster repo identity; falling back to CR fields",
				"agent_name", wg.Name, "prefix", prefix, "error", err, "backend", "operator-k8s")
		}
	}

	sharedWorkspace := wg.Spec.SharedWorkspace != nil && *wg.Spec.SharedWorkspace

	// SpawnConfig for the shared builder. Identity fields come from
	// the operator's explicit startup config (Database / Prefix /
	// DolthubRemote) and, when a ClusterIdentityResolver is wired,
	// the shared repo-registry output (spi-njzmg). CR fields are
	// projection-only — they never become the source of truth for
	// scheduling.
	spawnCfg := agent.SpawnConfig{
		Name:       wg.Name, // operator uses the guild name as the agent name
		BeadID:     beadID,
		Role:       agent.RoleWizard,
		Tower:      db,
		Step:       "wizard",
		RepoURL:    repoURL,
		RepoBranch: branch,
		RepoPrefix: prefix,
		Identity: runtime.RepoIdentity{
			TowerName:  db,
			Prefix:     prefix,
			RepoURL:    repoURL,
			BaseBranch: branch,
		},
		Run: runtime.RunContext{
			TowerName:       db,
			Prefix:          prefix,
			BeadID:          beadID,
			Role:            runtime.RoleWizard,
			FormulaStep:     "wizard",
			Backend:         "operator-k8s",
			WorkspaceKind:   runtime.WorkspaceKindOwnedWorktree,
			WorkspaceName:   "wizard",
			WorkspaceOrigin: runtime.WorkspaceOriginOriginClone,
			HandoffMode:     runtime.HandoffNone,
		},
		// Wire the shared pod builder to swap /workspace from emptyDir
		// to the per-wizard PVC when the guild opts in. The PVC is
		// provisioned by the operator in ensureOwningWizardPVC after
		// the pod is created so the PVC's ownerReference can point at
		// the live pod UID.
		SharedWorkspace: sharedWorkspace,
	}

	// BuildPod uses the shared wizard-pod shape (tower-attach +
	// repo-bootstrap init containers, /data+/workspace volumes, WizardResources).
	// Client is nil because RoleWizard never hits resolveWorkspaceVolume
	// (that path is substrate-only). Secret name is "spire-credentials"
	// by default; the operator overrides the env refs below via CR-sourced
	// SpireConfig tokens.
	builder := agent.NewPodBuilder(nil, m.Namespace, image, "")
	pod, err := builder.BuildPod(spawnCfg)
	if err != nil {
		// Identity inputs are required for wizard pods. If we produced a
		// SpawnConfig with empty RepoURL / BaseBranch / Prefix, fail
		// visibly instead of silently returning a bad pod — the monitor
		// logs the error and skips this bead until the CR is fixed.
		m.Log.Error(err, "shared pod builder failed; skipping bead",
			"agent_name", wg.Name, "bead_id", beadID, "tower", m.Database, "prefix", m.Prefix, "backend", "operator-k8s")
		return nil
	}

	// Apply operator-specific overlays: guild-scoped labels, canonical
	// pod name, dolthub-remote on the init container, SpireConfig-sourced
	// secret refs, MaxApprentices env, resource override, pod-name
	// template. These are operator inputs the shared builder does not know
	// about; applying them post-build keeps the shared shape authoritative.
	m.applyOperatorOverlay(pod, wg, beadID, cfg, image, db, prefix)

	// Phase-2 cache overlay (spi-sn7o3): when the guild declares a cache,
	// rewire the pod so it boots from the guild-owned cache PVC instead
	// of cloning from origin on every pod start. The overlay replaces the
	// repo-bootstrap init container with cache-bootstrap, adds the cache
	// PVC volume, and repoints the workspace mount at WorkspaceMountPath
	// so the main container's runtime surface stays identical to phase-1
	// (writable repo substrate at a single well-known mount).
	if wg.Spec.Cache != nil {
		applyCacheOverlay(pod, wg.Name, prefix, image)
	}

	return pod
}

// applyOperatorOverlay mutates pod in place to carry operator-specific
// fields on top of the shared pkg/agent pod shape. Split out for test
// readability: callers can assert the shared shape and the overlay
// separately. The overlay is intentionally narrow — anything that could
// live on SpawnConfig should go there instead, not accrete here.
func (m *AgentMonitor) applyOperatorOverlay(
	pod *corev1.Pod,
	wg *spirev1.WizardGuild,
	beadID string,
	cfg *spirev1.SpireConfig,
	image, db, prefix string,
) {
	// Pod name template is "<guild>-wizard-<bead>" per the 2026-04-20 naming
	// decision on spi-kh2em (no "spire-" prefix; agent/guild name first). The
	// shared builder's sanitizePodName appends a timestamp suffix for
	// collision avoidance; operator-managed pods use a deterministic name
	// so controller-runtime's idempotent Create/Delete reconciler loop
	// works correctly.
	podName := fmt.Sprintf("%s-wizard-%s", sanitizeK8sName(wg.Name), sanitizeK8sName(beadID))
	if len(podName) > 63 {
		podName = podName[:63]
	}
	pod.Name = podName

	// When the shared builder wired /workspace to a PVC (SharedWorkspace
	// opt-in), its ClaimName was derived from the sanitized cfg.Name that
	// the builder saw. The operator has just overridden pod.Name to the
	// deterministic "<guild>-wizard-<bead>" form, so the PVC reference
	// must be re-derived from the final pod name — otherwise the pod
	// references a PVC that the reconciler never creates.
	if wg.Spec.SharedWorkspace != nil && *wg.Spec.SharedWorkspace {
		for i := range pod.Spec.Volumes {
			v := &pod.Spec.Volumes[i]
			if v.Name != "workspace" || v.PersistentVolumeClaim == nil {
				continue
			}
			v.PersistentVolumeClaim.ClaimName = agent.OwningWizardPVCName(podName)
		}
	}

	// spire.awell.io/* labels are load-bearing for reconcileManagedAgent's
	// list selector and the workload_assigner prefix-match path. The
	// shared builder's spire.* labels remain in place for
	// network-policy / backend-agent discovery; we add the operator-guild
	// labels on top.
	if pod.Labels == nil {
		pod.Labels = map[string]string{}
	}
	pod.Labels["spire.awell.io/agent"] = wg.Name
	pod.Labels["spire.awell.io/guild"] = wg.Name
	pod.Labels["spire.awell.io/bead"] = beadID
	pod.Labels["spire.awell.io/managed"] = "true"
	pod.Labels["spire.awell.io/role"] = "wizard"
	pod.Labels["app.kubernetes.io/name"] = "spire-wizard"

	// Update the main container: image override from guild CR (the shared
	// builder used m.StewardImage when the guild didn't override), resource
	// override from guild CR, and WorkingDir pointed at the cloned repo
	// checkout.
	//
	// WorkingDir must match the clone destination from repoBootstrapScript
	// in pkg/agent/backend_k8s.go, which resolves to
	// "/workspace/${SPIRE_REPO_PREFIX}". If we set WorkingDir=/workspace
	// (one level too high), the main container starts above the cloned
	// tree, spire.yaml resolution misses, and resolveMaxApprentices() plus
	// ResolveBackend("") silently fall back to env/process defaults — the
	// k8s backend path spi-wqax9 built is then never exercised. See
	// spi-vrzhf for the bug that made this explicit.
	if len(pod.Spec.Containers) > 0 {
		main := &pod.Spec.Containers[0]
		main.Image = image
		main.Resources = wizardResources(wg.Spec.Resources)
		main.WorkingDir = "/workspace/" + prefix
		// Rewrite the command to match the operator's wizard entrypoint
		// shape: `spire execute <bead-id> --name <agent-name>`. The shared
		// builder uses roleToSubcmd(RoleWizard) which is also "spire execute",
		// so the arg list is already the canonical form. The canonical
		// form from pkg/agent includes "--name <Name>" — we just keep it.
		_ = main
	}

	// Update the init container: override image, rewrite the command to
	// include operator-scoped --data-dir/--database/--prefix/--dolthub-remote.
	// The shared builder's init container uses the same flag set, but
	// with dolthub remote resolved from env — the operator passes it
	// explicitly via m.DolthubRemote (read once at startup).
	if len(pod.Spec.InitContainers) > 0 {
		for i := range pod.Spec.InitContainers {
			ic := &pod.Spec.InitContainers[i]
			ic.Image = image
			if ic.Name == "tower-attach" {
				ic.Command = []string{
					"spire", "tower", "attach-cluster",
					"--data-dir=/data/" + db,
					"--database=" + db,
					"--prefix=" + prefix,
					"--dolthub-remote=" + m.dolthubRemoteForCfg(cfg),
				}
			}
		}
	}

	// Env overlay: MaxApprentices from the guild CR, SpireConfig token
	// refs for ANTHROPIC_API_KEY and GITHUB_TOKEN, and (only when the
	// guild opts in via spec.sharedWorkspace) SPIRE_K8S_SHARED_WORKSPACE=1
	// so apprentice/sage children spawned by this wizard go through the
	// shared-workspace path. Applied to every container + init container
	// so both paths see the same values.
	overlayEnv := m.buildOverlayEnv(wg, cfg)
	for i := range pod.Spec.Containers {
		pod.Spec.Containers[i].Env = mergeEnv(pod.Spec.Containers[i].Env, overlayEnv)
	}
	for i := range pod.Spec.InitContainers {
		pod.Spec.InitContainers[i].Env = mergeEnv(pod.Spec.InitContainers[i].Env, overlayEnv)
	}
}

// applyCacheOverlay rewires the pod to boot from the guild-owned repo
// cache PVC instead of cloning from origin (phase-2 cluster repo-cache
// contract, spi-sn7o3). Mutates pod in place:
//
//   - Adds a cache PVC volume named "repo-cache" pointing at the
//     reconciler-managed <guild-name>-repo-cache PVC (pvcName() is the
//     shared helper defined in cache_reconciler.go).
//   - Replaces the shared builder's "repo-bootstrap" init container
//     with "cache-bootstrap", which invokes `spire cache-bootstrap`
//     (cmd/spire/cache_bootstrap.go) to call the pkg/agent helpers
//     MaterializeWorkspaceFromCache then BindLocalRepo.
//   - Mounts the cache PVC read-only at agent.CacheMountPath and the
//     writable workspace emptyDir at agent.WorkspaceMountPath, on both
//     the init container and the main container, so the main container
//     finds the local repo substrate at WorkspaceMountPath.
//   - Repoints the main container's WorkingDir to WorkspaceMountPath —
//     MaterializeWorkspaceFromCache clones the cache tree directly into
//     WorkspaceMountPath (no prefix subdirectory), so the repo root IS
//     WorkspaceMountPath.
//
// The env overlay (SPIRE_REPO_URL/BRANCH/PREFIX, DOLT_DATA_DIR,
// canonical observability identity from spi-xplwy) is already applied
// by the shared builder and the operator overlay before this runs —
// the cache overlay does not touch env, to keep pkg/executor and
// pkg/wizard's runtime surface identical to phase-1.
func applyCacheOverlay(pod *corev1.Pod, guildName, prefix, image string) {
	// Add the cache PVC volume. Read-only access at the volume level is
	// a belt-and-suspenders companion to the per-mount ReadOnly flag
	// below — the PVC itself is provisioned ReadOnlyMany by the cache
	// reconciler, so neither init nor main container can mutate it.
	cacheVolume := corev1.Volume{
		Name: "repo-cache",
		VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
				ClaimName: pvcName(guildName),
				ReadOnly:  true,
			},
		},
	}
	pod.Spec.Volumes = append(pod.Spec.Volumes, cacheVolume)

	cacheMount := corev1.VolumeMount{
		Name:      "repo-cache",
		MountPath: agent.CacheMountPath,
		ReadOnly:  true,
	}
	workspaceMount := corev1.VolumeMount{
		Name:      "workspace",
		MountPath: agent.WorkspaceMountPath,
	}
	dataMount := corev1.VolumeMount{Name: "data", MountPath: "/data"}

	// Replace the shared builder's repo-bootstrap init container with a
	// cache-bootstrap init container. tower-attach stays as-is — it
	// stages /data and does not care about the workspace/cache mounts.
	for i := range pod.Spec.InitContainers {
		ic := &pod.Spec.InitContainers[i]
		if ic.Name != "repo-bootstrap" {
			continue
		}
		ic.Name = "cache-bootstrap"
		ic.Image = image
		ic.Command = []string{
			"spire", "cache-bootstrap",
			"--cache-path=" + agent.CacheMountPath,
			"--workspace-path=" + agent.WorkspaceMountPath,
			"--prefix=" + prefix,
		}
		ic.VolumeMounts = []corev1.VolumeMount{dataMount, cacheMount, workspaceMount}
	}

	// Repoint the main container: mount the cache read-only at
	// CacheMountPath, remap the workspace mount from /workspace (shared
	// builder default) to WorkspaceMountPath, and set WorkingDir so
	// cwd-sensitive code (resolveBeadsDir, ResolveBackend("")) lands
	// inside the materialized repo tree.
	for i := range pod.Spec.Containers {
		main := &pod.Spec.Containers[i]
		remapped := false
		for j := range main.VolumeMounts {
			vm := &main.VolumeMounts[j]
			if vm.Name == "workspace" {
				vm.MountPath = agent.WorkspaceMountPath
				remapped = true
			}
		}
		if !remapped {
			main.VolumeMounts = append(main.VolumeMounts, workspaceMount)
		}
		main.VolumeMounts = append(main.VolumeMounts, cacheMount)
		main.WorkingDir = agent.WorkspaceMountPath
	}
}

// sharedWorkspaceDefaultSize is the deployment-time default when a
// guild's SharedWorkspaceSize is unset. Matched to the cache default so
// operators have one number to tune for baseline PVC sizing.
var sharedWorkspaceDefaultSize = resource.MustParse("5Gi")

// ensureOwningWizardPVC provisions the per-wizard shared-workspace PVC
// for pod, labeled spire.io/owning-wizard-pod=<pod.Name> and
// owner-referenced by the pod so k8s GC cascades the delete when the
// pod terminates. Idempotent: returns nil when the PVC already exists.
//
// Only runs when:
//   - the guild opts in via spec.sharedWorkspace=true, AND
//   - the pod is a wizard pod (the canonical operator-managed shape).
//
// BlockOwnerDeletion=true so the PVC sticks around until the pod fully
// terminates — without this, a fast pod delete can race the controller
// and orphan in-flight writes.
func (m *AgentMonitor) ensureOwningWizardPVC(
	ctx context.Context,
	wg *spirev1.WizardGuild,
	pod *corev1.Pod,
) error {
	if wg.Spec.SharedWorkspace == nil || !*wg.Spec.SharedWorkspace {
		return nil
	}
	if pod == nil || pod.Name == "" {
		return nil
	}

	pvcName := agent.OwningWizardPVCName(pod.Name)

	var existing corev1.PersistentVolumeClaim
	err := m.Client.Get(ctx, client.ObjectKey{Namespace: m.Namespace, Name: pvcName}, &existing)
	if err == nil {
		// PVC already exists. We intentionally do NOT patch the spec —
		// k8s rejects most mutations on a bound PVC (size changes
		// require storage class support; access mode changes are
		// refused). If the guild's SharedWorkspaceSize or
		// SharedWorkspaceStorageClass change, operators must delete the
		// pod (which GCs the old PVC via ownerRef) and let the next
		// reconcile create a fresh PVC with the new spec.
		return nil
	}
	if !errors.IsNotFound(err) {
		return fmt.Errorf("get PVC %s: %w", pvcName, err)
	}

	size := sharedWorkspaceDefaultSize
	if q := wg.Spec.SharedWorkspaceSize; q != nil && !q.IsZero() {
		size = *q
	}

	spec := corev1.PersistentVolumeClaimSpec{
		AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
		Resources: corev1.VolumeResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceStorage: size,
			},
		},
	}
	// StorageClass: nil = cluster default. Empty string has a different
	// meaning in k8s ("disable dynamic provisioning") so we deliberately
	// do NOT pass through an empty-string override.
	if sc := wg.Spec.SharedWorkspaceStorageClass; sc != nil && *sc != "" {
		v := *sc
		spec.StorageClassName = &v
	}

	block := true
	controller := true
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pvcName,
			Namespace: m.Namespace,
			Labels: map[string]string{
				agent.LabelOwningWizardPod: pod.Name,
				"spire.awell.io/guild":     wg.Name,
				"app.kubernetes.io/name":   "spire-wizard-workspace",
				"app.kubernetes.io/part-of": "spire",
			},
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion:         "v1",
					Kind:               "Pod",
					Name:               pod.Name,
					UID:                pod.UID,
					Controller:         &controller,
					BlockOwnerDeletion: &block,
				},
			},
		},
		Spec: spec,
	}
	if err := m.Client.Create(ctx, pvc); err != nil {
		if errors.IsAlreadyExists(err) {
			// Lost a race with a concurrent reconcile; treat as success.
			return nil
		}
		return fmt.Errorf("create PVC %s: %w", pvcName, err)
	}
	m.Log.Info("provisioned per-wizard shared-workspace PVC",
		"pvc", pvcName, "pod", pod.Name, "agent_name", wg.Name,
		"tower", m.Database, "prefix", m.Prefix, "backend", "operator-k8s")
	return nil
}

// buildOverlayEnv returns the operator-specific env vars applied on top
// of the shared pkg/agent env. Kept as a method on AgentMonitor so tests
// can build it in isolation.
func (m *AgentMonitor) buildOverlayEnv(wg *spirev1.WizardGuild, cfg *spirev1.SpireConfig) []corev1.EnvVar {
	env := []corev1.EnvVar{
		// SPIRE_AGENT_NAME — the operator uses the guild name as the
		// agent name. pkg/agent's buildEnvVars doesn't emit this because
		// the process/docker backends have no analog. Wizards read it
		// for logging and metric attribution.
		{Name: "SPIRE_AGENT_NAME", Value: wg.Name},
	}

	// SPIRE_K8S_SHARED_WORKSPACE is opt-in via the guild CR
	// (spec.sharedWorkspace). When the flag is on, the reconciler
	// provisions a per-wizard PVC labeled
	// `spire.io/owning-wizard-pod=<pod-name>` (see
	// ensureOwningWizardPVC), mounts it on the wizard pod itself, and
	// child apprentice/sage borrowed-worktree spawns discover it via
	// the same label selector (pkg/agent/backend_k8s.go
	// resolveWorkspaceVolume). Default stays off so clusters without
	// the new PVC-provisioning RBAC (spi-zpnyu, spi-cslm8) keep
	// today's emptyDir semantics.
	if wg.Spec.SharedWorkspace != nil && *wg.Spec.SharedWorkspace {
		env = append(env, corev1.EnvVar{Name: "SPIRE_K8S_SHARED_WORKSPACE", Value: "1"})
	}

	// MaxApprentices: CR > spire.yaml > default (built-in 3). Only
	// inject SPIRE_MAX_APPRENTICES when the CR sets it; otherwise the
	// wizard falls back to spire.yaml.
	if wg.Spec.MaxApprentices != nil {
		env = append(env, corev1.EnvVar{
			Name:  "SPIRE_MAX_APPRENTICES",
			Value: strconv.Itoa(*wg.Spec.MaxApprentices),
		})
	}

	if cfg != nil {
		// Anthropic API key.
		tokenName := wg.Spec.Token
		if tokenName == "" {
			tokenName = cfg.Spec.DefaultToken
		}
		if tokenName == "" {
			tokenName = "default"
		}
		if tokenRef, ok := cfg.Spec.Tokens[tokenName]; ok {
			env = append(env, envFromSecret("ANTHROPIC_API_KEY", tokenRef.Secret, tokenRef.Key))
		}

		// GitHub token — optional so installs without one don't block pod
		// creation.
		if cfg.Spec.DoltHub.CredentialsSecret != "" {
			env = append(env, envFromSecretOptional("GITHUB_TOKEN", cfg.Spec.DoltHub.CredentialsSecret, "GITHUB_TOKEN"))
		}
	}

	return env
}

// mergeEnv overlays b onto a: entries in b that share a Name with an
// entry in a replace the earlier value; new entries in b are appended.
// Order in a is preserved for entries that aren't overridden, keeping
// the shared-builder env stable across parity test runs.
func mergeEnv(a, b []corev1.EnvVar) []corev1.EnvVar {
	if len(b) == 0 {
		return a
	}
	out := make([]corev1.EnvVar, 0, len(a)+len(b))
	bIndex := make(map[string]int, len(b))
	for i, e := range b {
		bIndex[e.Name] = i
	}
	seen := make(map[string]bool, len(a))
	for _, e := range a {
		if idx, ok := bIndex[e.Name]; ok {
			out = append(out, b[idx])
		} else {
			out = append(out, e)
		}
		seen[e.Name] = true
	}
	for _, e := range b {
		if !seen[e.Name] {
			out = append(out, e)
		}
	}
	return out
}

// resolvePrefix picks the authoritative prefix for this guild's pods.
// Precedence: single-prefix guild CR > operator's default Prefix. A
// multi-prefix guild without an operator default yields the empty
// string; the init container then reads the authoritative value from
// the dolt TowerConfig at attach-cluster time.
func (m *AgentMonitor) resolvePrefix(wg *spirev1.WizardGuild) string {
	if len(wg.Spec.Prefixes) == 1 {
		return wg.Spec.Prefixes[0]
	}
	return m.Prefix
}

// resolveDatabase returns the dolt database name. Plumbed once at
// startup; never read from process env at pod-build time.
func (m *AgentMonitor) resolveDatabase() string {
	if m.Database != "" {
		return m.Database
	}
	// Helm convention: release-scoped database equals install namespace.
	return m.Namespace
}

// dolthubRemoteForCfg returns the dolthub remote used in the
// tower-attach init container. Precedence: operator startup config >
// SpireConfig CR > empty.
func (m *AgentMonitor) dolthubRemoteForCfg(cfg *spirev1.SpireConfig) string {
	if m.DolthubRemote != "" {
		return m.DolthubRemote
	}
	if cfg != nil {
		return cfg.Spec.DoltHub.Remote
	}
	return ""
}

// wizardResources returns the resource requirements for a wizard pod.
// Guild-level overrides (WizardGuild.Spec.Resources) win when set; otherwise
// we fall back to the canonical wizard-tier defaults from pkg/agent so the
// operator path matches pkg/agent/backend_k8s.go:buildWizardPod.
func wizardResources(spec *spirev1.GuildResourceRequirements) corev1.ResourceRequirements {
	if spec == nil {
		return agent.WizardResources()
	}
	return buildResources(spec)
}

func envFromSecret(envName, secretName, key string) corev1.EnvVar {
	return corev1.EnvVar{
		Name: envName,
		ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
				Key:                  key,
			},
		},
	}
}

func envFromSecretOptional(envName, secretName, key string) corev1.EnvVar {
	optional := true
	return corev1.EnvVar{
		Name: envName,
		ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
				Key:                  key,
				Optional:             &optional,
			},
		},
	}
}

func buildResources(spec *spirev1.GuildResourceRequirements) corev1.ResourceRequirements {
	reqs := corev1.ResourceRequirements{}
	if spec == nil {
		return reqs
	}
	if len(spec.Requests) > 0 {
		reqs.Requests = make(corev1.ResourceList)
		for k, v := range spec.Requests {
			reqs.Requests[corev1.ResourceName(k)] = resource.MustParse(v)
		}
	}
	if len(spec.Limits) > 0 {
		reqs.Limits = make(corev1.ResourceList)
		for k, v := range spec.Limits {
			reqs.Limits[corev1.ResourceName(k)] = resource.MustParse(v)
		}
	}
	return reqs
}


// removeFromCurrentWork drops beadID from agent.Status.CurrentWork in-place.
// Returns true if the slice was modified.
func removeFromCurrentWork(agent *spirev1.WizardGuild, beadID string) bool {
	for i, id := range agent.Status.CurrentWork {
		if id == beadID {
			agent.Status.CurrentWork = append(agent.Status.CurrentWork[:i], agent.Status.CurrentWork[i+1:]...)
			return true
		}
	}
	return false
}

// isPodFinished reports whether the main work container has terminated,
// even when the pod phase is still Running.
//
// Operator-managed pods use the canonical single-container wizard pod
// shape (one "agent" container; Model A's wizard+sidecar shape was
// deleted in spi-fjt2t). The "wizard"/"sidecar" fallbacks are preserved
// here because in-flight pods on clusters that have not yet been
// recreated may still carry the old container names — so reap still
// fires on upgrade without a pod-by-pod manual cleanup.
func isPodFinished(pod *corev1.Pod) bool {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name == "sidecar" {
			continue // legacy Model A sidecar — not present on new pods.
		}
		if cs.State.Terminated != nil {
			return true
		}
	}
	return false
}

func sanitizeK8sName(s string) string {
	// k8s names: lowercase, alphanumeric, hyphens
	result := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-':
			result = append(result, c)
		case c >= 'A' && c <= 'Z':
			result = append(result, c+32) // lowercase
		case c == '.' || c == '_':
			result = append(result, '-')
		}
	}
	return string(result)
}
