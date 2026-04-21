package controllers

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/go-logr/logr"

	spirev1 "github.com/awell-health/spire/operator/api/v1alpha1"
	"github.com/awell-health/spire/pkg/agent"
	"github.com/awell-health/spire/pkg/repoconfig"
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

		// Route by workload type:
		//   epic   → epic pod (wizard + sidecar, Model A — out of scope for spi-9wo3a)
		//   review → review pod (wizard --mode=review, Model A — out of scope for spi-9wo3a)
		//   *      → canonical single-container wizard pod (see buildWorkloadPod)
		var pod *corev1.Pod
		switch wlType := m.getWorkloadType(ctx, beadID); wlType {
		case "epic":
			pod = m.buildEpicPod(agent, beadID, cfg)
		case "review":
			pod = m.buildReviewPod(agent, beadID, cfg)
		default:
			pod = m.buildWorkloadPod(agent, beadID, cfg)
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
// Source of truth for the pod shape: pkg/agent/backend_k8s.go:buildWizardPod.
// This path duplicates the canonical structure inline because the operator
// sources image, labels, secrets, and resource overrides from the
// WizardGuild CR and SpireConfig — inputs the pkg/agent backend does not
// know about. A follow-up bead tracks unifying the two builders; until
// then, changes to the canonical shape must be mirrored here.
//
// The pod runs:
//   - One init container ("tower-attach") that runs
//     `spire tower attach-cluster` to stage .beads/ and spire config into
//     /data.
//   - One main container ("agent") that runs
//     `spire execute <bead-id> --name <agent-name>` against the cluster
//     dolt service.
//
// Volumes are two emptyDirs: /data (beads + spire config) and /workspace
// (apprentice bundle staging). There is no sidecar, no /comms volume, and
// no beads-seed ConfigMap — those belong to the removed Model A contract
// (see docs/k8s-operator-reference.md → Deprecated: agent-entrypoint.sh).
func (m *AgentMonitor) buildWorkloadPod(agent *spirev1.WizardGuild, beadID string, cfg *spirev1.SpireConfig) *corev1.Pod {
	image := agent.Spec.Image
	if image == "" {
		image = m.StewardImage
	}

	// Pod name template is "<guild>-wizard-<bead>" per the 2026-04-20 naming
	// decision on spi-kh2em (no "spire-" prefix; agent/guild name first). The
	// agent name is the guild name today; that will diverge when the
	// WizardGuild CRD lands (separate bead).
	podName := fmt.Sprintf("%s-wizard-%s", sanitizeK8sName(agent.Name), sanitizeK8sName(beadID))
	// k8s pod names max 63 chars
	if len(podName) > 63 {
		podName = podName[:63]
	}

	branch := agent.Spec.RepoBranch
	if branch == "" {
		branch = repoconfig.DefaultBranchBase
	}

	// attach-cluster inputs. Database/prefix/dolthub-remote are read from
	// the operator's ambient env (plumbed via helm) and SpireConfig; we do
	// not invent new CRD fields here. Database falls back to the operator's
	// namespace to match the helm default `spire.database` which is
	// derived from the release-scoped bead prefix.
	db := os.Getenv("BEADS_DATABASE")
	if db == "" {
		db = m.Namespace
	}
	prefix := os.Getenv("BEADS_PREFIX")
	if prefix == "" && len(agent.Spec.Prefixes) == 1 {
		// Single-prefix guild: use it as the attach-cluster fallback so
		// the init container gets a sensible value even when the operator
		// wasn't deployed with BEADS_PREFIX set. Multi-prefix guilds
		// leave --prefix empty; attach-cluster reads the authoritative
		// value from the dolt TowerConfig and logs a mismatch warning.
		prefix = agent.Spec.Prefixes[0]
	}
	dolthubRemote := os.Getenv("DOLTHUB_REMOTE")
	if dolthubRemote == "" && cfg != nil {
		dolthubRemote = cfg.Spec.DoltHub.Remote
	}

	// Main-container env. Matches the canonical shape: the init container
	// stages beads at /data/<db>/.beads and spire config at /data/spire-config;
	// DOLT_DATA_DIR + SPIRE_CONFIG_DIR tell resolveBeadsDir() where to find
	// them. BEADS_DOLT_SERVER_{HOST,PORT} point at the cluster dolt so bd
	// doesn't auto-start an embedded server.
	wizardEnv := []corev1.EnvVar{
		{Name: "DOLT_DATA_DIR", Value: "/data"},
		{Name: "SPIRE_CONFIG_DIR", Value: "/data/spire-config"},
		{Name: "BEADS_DOLT_SERVER_HOST", Value: fmt.Sprintf("spire-dolt.%s.svc", m.Namespace)},
		{Name: "BEADS_DOLT_SERVER_PORT", Value: "3306"},

		// Identity
		{Name: "SPIRE_AGENT_NAME", Value: agent.Name},
		{Name: "SPIRE_BEAD_ID", Value: beadID},
		{Name: "SPIRE_ROLE", Value: "wizard"},

		// Repo — still consumed by the wizard for apprentice bundle production.
		{Name: "SPIRE_REPO_URL", Value: agent.Spec.Repo},
		{Name: "SPIRE_REPO_BRANCH", Value: branch},

		// OTel wiring — matches pkg/agent/backend_k8s.go so traces/logs from
		// operator-managed wizards land in the same steward collector as
		// steward-spawned pods.
		{Name: "OTEL_EXPORTER_OTLP_ENDPOINT", Value: fmt.Sprintf("http://spire-steward.%s.svc:4317", m.Namespace)},
		{Name: "OTEL_EXPORTER_OTLP_PROTOCOL", Value: "grpc"},
		{Name: "OTEL_TRACES_EXPORTER", Value: "otlp"},
		{Name: "OTEL_LOGS_EXPORTER", Value: "otlp"},
		{Name: "OTEL_RESOURCE_ATTRIBUTES", Value: fmt.Sprintf("bead.id=%s,agent.name=%s", beadID, agent.Name)},
	}

	// Cluster-level cap on concurrent apprentice subprocesses. When unset
	// on the CR we leave SPIRE_MAX_APPRENTICES absent so the wizard falls
	// back to spire.yaml (or the built-in default). This keeps the
	// layering precedence correct: CR > spire.yaml > default.
	if agent.Spec.MaxApprentices != nil {
		wizardEnv = append(wizardEnv, corev1.EnvVar{
			Name:  "SPIRE_MAX_APPRENTICES",
			Value: strconv.Itoa(*agent.Spec.MaxApprentices),
		})
	}

	// Inject secrets from SpireConfig.
	if cfg != nil {
		// Anthropic API key
		tokenName := agent.Spec.Token
		if tokenName == "" {
			tokenName = cfg.Spec.DefaultToken
		}
		if tokenName == "" {
			tokenName = "default"
		}
		if tokenRef, ok := cfg.Spec.Tokens[tokenName]; ok {
			wizardEnv = append(wizardEnv,
				envFromSecret("ANTHROPIC_API_KEY", tokenRef.Secret, tokenRef.Key),
			)
		}

		// GitHub token — optional so installs without one don't block pod creation.
		if cfg.Spec.DoltHub.CredentialsSecret != "" {
			wizardEnv = append(wizardEnv,
				envFromSecretOptional("GITHUB_TOKEN", cfg.Spec.DoltHub.CredentialsSecret, "GITHUB_TOKEN"),
			)
		}
	}

	dataMount := corev1.VolumeMount{Name: "data", MountPath: "/data"}
	workspaceMount := corev1.VolumeMount{Name: "workspace", MountPath: "/workspace"}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: m.Namespace,
			// spire.awell.io/* are load-bearing — reconcileManagedAgent's list
			// selector uses {agent, managed}, and the controller rename PRs
			// migrate these keys atomically, not one pod at a time. "guild" is
			// forward-compat for the WizardGuild CRD rename (separate bead).
			Labels: map[string]string{
				"spire.awell.io/agent":   agent.Name,
				"spire.awell.io/guild":   agent.Name,
				"spire.awell.io/bead":    beadID,
				"spire.awell.io/managed": "true",
				"spire.awell.io/role":    "wizard",
				"app.kubernetes.io/name": "spire-wizard",
			},
		},
		Spec: corev1.PodSpec{
			RestartPolicy:     corev1.RestartPolicyNever,
			PriorityClassName: "spire-agent-default",
			Volumes: []corev1.Volume{
				{Name: "data", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
				{Name: "workspace", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
			},
			InitContainers: []corev1.Container{{
				Name:  "tower-attach",
				Image: image,
				Command: []string{
					"spire", "tower", "attach-cluster",
					"--data-dir=/data/" + db,
					"--database=" + db,
					"--prefix=" + prefix,
					"--dolthub-remote=" + dolthubRemote,
				},
				// Share the main container's env so BEADS_DOLT_SERVER_HOST/PORT
				// point at the cluster dolt service — otherwise attach-cluster
				// falls through to laptop defaults (127.0.0.1:3307).
				Env:          wizardEnv,
				VolumeMounts: []corev1.VolumeMount{dataMount},
			}},
			Containers: []corev1.Container{
				{
					Name:  "agent",
					Image: image,
					Command: []string{
						"spire", "execute", beadID, "--name", agent.Name,
					},
					Env:          wizardEnv,
					Resources:    wizardResources(agent.Spec.Resources),
					VolumeMounts: []corev1.VolumeMount{dataMount, workspaceMount},
					WorkingDir:   "/workspace",
				},
			},
		},
	}

	return pod
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

func intstr8080() intstr.IntOrString {
	return intstr.FromInt32(8080)
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

// getWorkloadType looks up the SpireWorkload CR for a bead ID and returns its type.
// Returns empty string if not found.
func (m *AgentMonitor) getWorkloadType(ctx context.Context, beadID string) string {
	var workloads spirev1.SpireWorkloadList
	if err := m.Client.List(ctx, &workloads, client.InNamespace(m.Namespace)); err != nil {
		return ""
	}
	for _, wl := range workloads.Items {
		if wl.Spec.BeadID == beadID {
			return wl.Spec.Type
		}
	}
	return ""
}

// buildEpicPod creates a pod spec for an epic bead.
// Epic pods run the wizard binary with epic-specific args,
// which reviews child branches, creates PRs, and manages the merge queue.
func (m *AgentMonitor) buildEpicPod(agent *spirev1.WizardGuild, beadID string, cfg *spirev1.SpireConfig) *corev1.Pod {
	image := agent.Spec.Image
	if image == "" {
		image = m.StewardImage
	}

	podName := fmt.Sprintf("spire-wizard-%s", sanitizeK8sName(beadID))
	if len(podName) > 63 {
		podName = podName[:63]
	}

	branch := agent.Spec.RepoBranch
	if branch == "" {
		branch = repoconfig.DefaultBranchBase
	}

	// Wizard environment.
	wizardEnv := []corev1.EnvVar{
		{Name: "SPIRE_AGENT_NAME", Value: agent.Name},
		{Name: "SPIRE_EPIC_ID", Value: beadID},
		{Name: "SPIRE_REPO_URL", Value: agent.Spec.Repo},
		{Name: "SPIRE_REPO_BRANCH", Value: branch},
		{Name: "SPIRE_COMMS_DIR", Value: "/comms"},
		{Name: "SPIRE_WORKSPACE_DIR", Value: "/workspace"},
		{Name: "SPIRE_STATE_DIR", Value: "/data"},
		{Name: "DOLT_HOST", Value: fmt.Sprintf("spire-dolt.%s.svc", m.Namespace)},
		{Name: "DOLT_PORT", Value: "3306"},
		{Name: "WIZARD_MAX_REVIEW_ROUNDS", Value: "3"},
		{Name: "SPIRE_BD_LOG", Value: "1"},
	}

	// Sidecar environment.
	sidecarEnv := []corev1.EnvVar{
		{Name: "SPIRE_AGENT_NAME", Value: agent.Name},
		{Name: "DOLT_HOST", Value: fmt.Sprintf("spire-dolt.%s.svc", m.Namespace)},
		{Name: "DOLT_PORT", Value: "3306"},
		{Name: "SPIRE_BD_LOG", Value: "1"},
	}

	// Inject secrets from SpireConfig.
	if cfg != nil {
		// Opus token — prefer "heavy" token for the wizard, fall back to default.
		tokenName := "heavy"
		if _, ok := cfg.Spec.Tokens[tokenName]; !ok {
			tokenName = agent.Spec.Token
			if tokenName == "" {
				tokenName = cfg.Spec.DefaultToken
			}
			if tokenName == "" {
				tokenName = "default"
			}
		}
		if tokenRef, ok := cfg.Spec.Tokens[tokenName]; ok {
			wizardEnv = append(wizardEnv,
				envFromSecret("ANTHROPIC_API_KEY", tokenRef.Secret, tokenRef.Key),
			)
		}

		// GitHub token.
		if cfg.Spec.DoltHub.CredentialsSecret != "" {
			wizardEnv = append(wizardEnv,
				envFromSecretOptional("GITHUB_TOKEN", cfg.Spec.DoltHub.CredentialsSecret, "GITHUB_TOKEN"),
			)
		}
	}

	volumes := []corev1.Volume{
		{Name: "comms", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		{Name: "workspace", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		{Name: "data", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		beadsSeedVolume(),
	}

	sharedMounts := []corev1.VolumeMount{
		{Name: "comms", MountPath: "/comms"},
		{Name: "workspace", MountPath: "/workspace"},
		{Name: "data", MountPath: "/data"},
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: m.Namespace,
			Labels: map[string]string{
				"spire.awell.io/agent":   agent.Name,
				"spire.awell.io/bead":    beadID,
				"spire.awell.io/managed": "true",
				"spire.awell.io/role":    "wizard",
				"app.kubernetes.io/name": "spire-wizard",
			},
		},
		Spec: corev1.PodSpec{
			RestartPolicy:  corev1.RestartPolicyNever,
			InitContainers: []corev1.Container{beadsSeedInitContainer()},
			Volumes:        volumes,
			Containers: []corev1.Container{
				{
					Name:    "wizard",
					Image:   image,
					Command: []string{"spire", "wizard", fmt.Sprintf("--epic-id=%s", beadID)},
					Env:     wizardEnv,
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("100m"),
							corev1.ResourceMemory: resource.MustParse("256Mi"),
						},
					},
					VolumeMounts: sharedMounts,
					WorkingDir:   "/workspace",
				},
				{
					Name:  "sidecar",
					Image: image,
					Command: []string{
						"spire-sidecar",
						"--comms-dir=/comms",
						"--poll-interval=10s",
						"--port=8080",
						fmt.Sprintf("--agent-name=%s", agent.Name),
					},
					Env:        sidecarEnv,
					WorkingDir: "/data",
					VolumeMounts: []corev1.VolumeMount{
						{Name: "comms", MountPath: "/comms"},
						{Name: "data", MountPath: "/data"},
					},
					ReadinessProbe: &corev1.Probe{
						ProbeHandler: corev1.ProbeHandler{
							HTTPGet: &corev1.HTTPGetAction{
								Path: "/readyz",
								Port: intstr8080(),
							},
						},
						InitialDelaySeconds: 5,
						PeriodSeconds:       10,
					},
					LivenessProbe: &corev1.Probe{
						ProbeHandler: corev1.ProbeHandler{
							HTTPGet: &corev1.HTTPGetAction{
								Path: "/healthz",
								Port: intstr8080(),
							},
						},
						InitialDelaySeconds: 10,
						PeriodSeconds:       30,
					},
				},
			},
		},
	}

	return pod
}

// buildReviewPod creates a one-shot pod for standalone task review.
// Similar to buildEpicPod but runs the wizard in review mode (--mode=review --bead-id=X).
func (m *AgentMonitor) buildReviewPod(agent *spirev1.WizardGuild, beadID string, cfg *spirev1.SpireConfig) *corev1.Pod {
	image := agent.Spec.Image
	if image == "" {
		image = m.StewardImage
	}

	podName := fmt.Sprintf("spire-review-%s", sanitizeK8sName(beadID))
	if len(podName) > 63 {
		podName = podName[:63]
	}

	branch := agent.Spec.RepoBranch
	if branch == "" {
		branch = repoconfig.DefaultBranchBase
	}

	// Wizard environment.
	wizardEnv := []corev1.EnvVar{
		{Name: "SPIRE_AGENT_NAME", Value: agent.Name},
		{Name: "SPIRE_BEAD_ID", Value: beadID},
		{Name: "SPIRE_REPO_URL", Value: agent.Spec.Repo},
		{Name: "SPIRE_REPO_BRANCH", Value: branch},
		{Name: "SPIRE_COMMS_DIR", Value: "/comms"},
		{Name: "SPIRE_WORKSPACE_DIR", Value: "/workspace"},
		{Name: "SPIRE_STATE_DIR", Value: "/data"},
		{Name: "DOLT_HOST", Value: fmt.Sprintf("spire-dolt.%s.svc", m.Namespace)},
		{Name: "DOLT_PORT", Value: "3306"},
		{Name: "SPIRE_BD_LOG", Value: "1"},
	}

	// Sidecar environment.
	sidecarEnv := []corev1.EnvVar{
		{Name: "SPIRE_AGENT_NAME", Value: agent.Name},
		{Name: "DOLT_HOST", Value: fmt.Sprintf("spire-dolt.%s.svc", m.Namespace)},
		{Name: "DOLT_PORT", Value: "3306"},
		{Name: "SPIRE_BD_LOG", Value: "1"},
	}

	// Inject secrets from SpireConfig.
	if cfg != nil {
		// Opus token.
		tokenName := "heavy"
		if _, ok := cfg.Spec.Tokens[tokenName]; !ok {
			tokenName = agent.Spec.Token
			if tokenName == "" {
				tokenName = cfg.Spec.DefaultToken
			}
			if tokenName == "" {
				tokenName = "default"
			}
		}
		if tokenRef, ok := cfg.Spec.Tokens[tokenName]; ok {
			wizardEnv = append(wizardEnv,
				envFromSecret("ANTHROPIC_API_KEY", tokenRef.Secret, tokenRef.Key),
			)
		}

		if cfg.Spec.DoltHub.CredentialsSecret != "" {
			wizardEnv = append(wizardEnv,
				envFromSecretOptional("GITHUB_TOKEN", cfg.Spec.DoltHub.CredentialsSecret, "GITHUB_TOKEN"),
			)
		}
	}

	volumes := []corev1.Volume{
		{Name: "comms", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		{Name: "workspace", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		{Name: "data", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		beadsSeedVolume(),
	}

	sharedMounts := []corev1.VolumeMount{
		{Name: "comms", MountPath: "/comms"},
		{Name: "workspace", MountPath: "/workspace"},
		{Name: "data", MountPath: "/data"},
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: m.Namespace,
			Labels: map[string]string{
				"spire.awell.io/agent":   agent.Name,
				"spire.awell.io/bead":    beadID,
				"spire.awell.io/managed": "true",
				"spire.awell.io/role":    "reviewer",
				"app.kubernetes.io/name": "spire-review",
			},
		},
		Spec: corev1.PodSpec{
			RestartPolicy:  corev1.RestartPolicyNever,
			InitContainers: []corev1.Container{beadsSeedInitContainer()},
			Volumes:        volumes,
			Containers: []corev1.Container{
				{
					Name:  "wizard",
					Image: image,
					Command: []string{
						"spire",
						"wizard",
						fmt.Sprintf("--bead-id=%s", beadID),
						"--mode=review",
						"--once",
					},
					Env: wizardEnv,
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("100m"),
							corev1.ResourceMemory: resource.MustParse("256Mi"),
						},
					},
					VolumeMounts: sharedMounts,
					WorkingDir:   "/workspace",
				},
				{
					Name:  "sidecar",
					Image: image,
					Command: []string{
						"spire-sidecar",
						"--comms-dir=/comms",
						"--poll-interval=10s",
						"--port=8080",
						fmt.Sprintf("--agent-name=%s", agent.Name),
					},
					Env:        sidecarEnv,
					WorkingDir: "/data",
					VolumeMounts: []corev1.VolumeMount{
						{Name: "comms", MountPath: "/comms"},
						{Name: "data", MountPath: "/data"},
					},
					ReadinessProbe: &corev1.Probe{
						ProbeHandler: corev1.ProbeHandler{
							HTTPGet: &corev1.HTTPGetAction{
								Path: "/readyz",
								Port: intstr8080(),
							},
						},
						InitialDelaySeconds: 5,
						PeriodSeconds:       10,
					},
					LivenessProbe: &corev1.Probe{
						ProbeHandler: corev1.ProbeHandler{
							HTTPGet: &corev1.HTTPGetAction{
								Path: "/healthz",
								Port: intstr8080(),
							},
						},
						InitialDelaySeconds: 10,
						PeriodSeconds:       30,
					},
				},
			},
		},
	}

	return pod
}

// beadsSeedInitContainer returns an initContainer that copies .beads/ config
// from the beads-seed ConfigMap into /data/.beads/.
func beadsSeedInitContainer() corev1.Container {
	return corev1.Container{
		Name:    "seed-beads",
		Image:   "alpine:3.20",
		Command: []string{"sh", "-c"},
		Args: []string{
			// `|| true` on git init: alpine:3.20 has no git; failing there blocked pods (spi-5pgqy).
			// chown at end: wizard runs as UID 1000 (non-root); otherwise can't write to /data/.beads/dolt-server.lock, bd-created files, etc.
			// `|| true` on config.yaml cp: keep going even when the ConfigMap omits it (makes behavior consistent with the routes.jsonl line which uses &&).
			`mkdir -p /data/.beads && cp /seed/metadata.json /data/.beads/metadata.json && cp /seed/routes.jsonl /data/.beads/routes.jsonl && ([ -f /seed/config.yaml ] && cp /seed/config.yaml /data/.beads/config.yaml || true); if [ ! -d /data/.git ]; then cd /data && git init -q 2>/dev/null || true; fi; chown -R 1000:1000 /data`,
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: "data", MountPath: "/data"},
			{Name: "beads-seed", MountPath: "/seed", ReadOnly: true},
		},
	}
}

// beadsSeedVolume returns the volume definition for the beads-seed ConfigMap.
func beadsSeedVolume() corev1.Volume {
	return corev1.Volume{
		Name: "beads-seed",
		VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: "beads-seed"},
			},
		},
	}
}

func boolPtr(b bool) *bool { return &b }

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

// isPodFinished reports whether the main work container has terminated, even
// when the pod phase is still Running.
//
// Used across all three pod paths:
//   - Canonical workload pods (spi-9wo3a): one container named "agent"; any
//     termination signals completion.
//   - Epic/review pods (Model A, out of scope for spi-9wo3a): wizard + sidecar;
//     the sidecar may keep the pod in Running while the wizard has exited, so
//     we explicitly skip it.
func isPodFinished(pod *corev1.Pod) bool {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name == "sidecar" {
			continue // skip the sidecar (epic/review pods only)
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
