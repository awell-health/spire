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
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/go-logr/logr"

	spirev1 "github.com/awell-health/spire/operator/api/v1alpha1"
	"github.com/awell-health/spire/pkg/agent"
	"github.com/awell-health/spire/pkg/repoconfig"
	"github.com/awell-health/spire/pkg/runtime"
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
}

// Start implements controller-runtime's Runnable interface.
func (m *AgentMonitor) Start(ctx context.Context) error {
	m.Run(ctx)
	return nil
}

func (m *AgentMonitor) Run(ctx context.Context) {
	m.Log.Info("agent monitor starting", "interval", m.Interval)
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
	m.Log.Info("agent monitor cycle start")
	var agents spirev1.WizardGuildList
	if err := m.Client.List(ctx, &agents, client.InNamespace(m.Namespace)); err != nil {
		m.Log.Error(err, "failed to list agents")
		return
	}
	m.Log.Info("agent monitor found agents", "count", len(agents.Items))

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
			m.Log.Error(err, "failed to read SpireConfig")
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
			m.Log.Info("agent went offline", "agent", agent.Name, "lastSeen", age)
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
		m.Log.Error(err, "failed to list agent pods", "agent", agent.Name)
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
				"agent", agent.Name, "bead", beadID)
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
					m.Log.Error(err, "failed to delete completed pod", "pod", pod.Name)
				}
			}
			reaped[beadID] = true
			m.Log.Info("reaped completed workload",
				"agent", agent.Name, "bead", beadID, "reason", "signal")

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
					"agent", agent.Name, "bead", beadID)
			}
			if pod.DeletionTimestamp == nil {
				if err := m.Client.Delete(ctx, pod); err != nil {
					m.Log.Error(err, "failed to delete finished pod", "pod", pod.Name)
				}
			}
			reaped[beadID] = true
			m.Log.Info("reaped completed workload",
				"agent", agent.Name, "bead", beadID, "reason", "pod-terminated-no-signal")
		}
	}
	if statusChanged {
		if err := m.Client.Status().Update(ctx, agent); err != nil {
			m.Log.Error(err, "failed to update agent CurrentWork after reaping")
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
			m.Log.Error(err, "failed to create workload pod", "agent", agent.Name, "bead", beadID)
			continue
		}

		m.Log.Info("created workload pod", "agent", agent.Name, "bead", beadID, "pod", pod.Name, "role", pod.Labels["spire.awell.io/role"])
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
			m.Log.Error(err, "failed to delete stale workload pod", "pod", pod.Name, "bead", beadID)
		} else {
			m.Log.Info("deleted stale workload pod", "agent", agent.Name, "bead", beadID)
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

	// SpawnConfig for the shared builder. Identity fields come from the
	// operator's explicit startup config (Database / Prefix / DolthubRemote)
	// and the WizardGuild CR — never from pod-building-time env reads.
	spawnCfg := agent.SpawnConfig{
		Name:       wg.Name, // operator uses the guild name as the agent name
		BeadID:     beadID,
		Role:       agent.RoleWizard,
		Tower:      db,
		Step:       "wizard",
		RepoURL:    wg.Spec.Repo,
		RepoBranch: branch,
		RepoPrefix: prefix,
		Identity: runtime.RepoIdentity{
			TowerName:  db,
			Prefix:     prefix,
			RepoURL:    wg.Spec.Repo,
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
			"agent", wg.Name, "bead", beadID)
		return nil
	}

	// Apply operator-specific overlays: guild-scoped labels, canonical
	// pod name, dolthub-remote on the init container, SpireConfig-sourced
	// secret refs, MaxApprentices env, resource override, pod-name
	// template. These are operator inputs the shared builder does not know
	// about; applying them post-build keeps the shared shape authoritative.
	m.applyOperatorOverlay(pod, wg, beadID, cfg, image, db, prefix)

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
	// override from guild CR, working dir at /workspace for the wizard
	// subprocess (the shared builder doesn't set WorkingDir by default).
	if len(pod.Spec.Containers) > 0 {
		main := &pod.Spec.Containers[0]
		main.Image = image
		main.Resources = wizardResources(wg.Spec.Resources)
		main.WorkingDir = "/workspace"
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
	// refs for ANTHROPIC_API_KEY and GITHUB_TOKEN, SPIRE_K8S_SHARED_WORKSPACE=1
	// so apprentice/sage children spawned by this wizard go through the
	// shared-workspace path (chunk 2 of the runtime-contract migration).
	// Applied to every container + init container so both paths see the
	// same values.
	overlayEnv := m.buildOverlayEnv(wg, cfg)
	for i := range pod.Spec.Containers {
		pod.Spec.Containers[i].Env = mergeEnv(pod.Spec.Containers[i].Env, overlayEnv)
	}
	for i := range pod.Spec.InitContainers {
		pod.Spec.InitContainers[i].Env = mergeEnv(pod.Spec.InitContainers[i].Env, overlayEnv)
	}
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

		// Flip the shared-workspace gate on for operator-managed pods.
		// This is the first production surface turning on the new path
		// (spi-fjt2t, per design §7.2). Child apprentice/sage pods
		// spawned by the wizard inherit this env via SPIRE_ROLE-aware
		// dispatch; the wizard itself never reads it.
		{Name: "SPIRE_K8S_SHARED_WORKSPACE", Value: "1"},
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
