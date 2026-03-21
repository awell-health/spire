package controllers

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/go-logr/logr"

	spirev1 "github.com/awell-health/spire/operator/api/v1alpha1"
)

// AgentMonitor tracks agent heartbeats and manages pods for managed agents.
type AgentMonitor struct {
	Client         client.Client
	Log            logr.Logger
	Namespace      string
	Interval       time.Duration
	OfflineTimeout time.Duration // how long before an agent is considered offline
	StewardImage     string        // default image for managed agent pods
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
	var agents spirev1.SpireAgentList
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
func (m *AgentMonitor) checkExternalAgent(ctx context.Context, agent *spirev1.SpireAgent) {
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
func (m *AgentMonitor) reconcileManagedAgent(ctx context.Context, agent *spirev1.SpireAgent, cfg *spirev1.SpireConfig) {
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

	// Reap completed/failed pods and remove their bead IDs from CurrentWork
	statusChanged := false
	for beadID, pod := range podsByBead {
		if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed || isPodFinished(pod) {
			// Remove bead from CurrentWork
			for i, id := range agent.Status.CurrentWork {
				if id == beadID {
					agent.Status.CurrentWork = append(agent.Status.CurrentWork[:i], agent.Status.CurrentWork[i+1:]...)
					delete(workSet, beadID)
					statusChanged = true
					m.Log.Info("reaped completed workload", "agent", agent.Name, "bead", beadID, "podPhase", pod.Status.Phase)
					break
				}
			}
			// Delete the finished pod
			if pod.DeletionTimestamp == nil {
				if err := m.Client.Delete(ctx, pod); err != nil {
					m.Log.Error(err, "failed to delete finished pod", "pod", pod.Name)
				}
			}
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

		// Epic beads get a workshop pod (artificer + sidecar).
		// Task/bug/feature/chore beads get a wizard pod (wizard + sidecar).
		var pod *corev1.Pod
		if wlType := m.getWorkloadType(ctx, beadID); wlType == "epic" {
			pod = m.buildEpicPod(agent, beadID, cfg)
		} else {
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
func (m *AgentMonitor) updateAgentPhase(ctx context.Context, agent *spirev1.SpireAgent, podsByBead map[string]*corev1.Pod) {
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

// buildWorkloadPod creates a pod spec for a single bead assignment.
// The pod has two containers:
//   - wizard: runs agent-entrypoint.sh (clone, claim, focus, execute, push)
//   - sidecar: runs spire-sidecar (inbox polling, health, control channel)
//
// They share /comms (filesystem-based coordination) and /workspace.
func (m *AgentMonitor) buildWorkloadPod(agent *spirev1.SpireAgent, beadID string, cfg *spirev1.SpireConfig) *corev1.Pod {
	image := agent.Spec.Image
	if image == "" {
		image = m.StewardImage
	}

	podName := fmt.Sprintf("spire-agent-%s-%s", agent.Name, sanitizeK8sName(beadID))
	// k8s pod names max 63 chars
	if len(podName) > 63 {
		podName = podName[:63]
	}

	branch := agent.Spec.RepoBranch
	if branch == "" {
		branch = "main"
	}

	// Wizard environment
	wizardEnv := []corev1.EnvVar{
		{Name: "SPIRE_AGENT_NAME", Value: agent.Name},
		{Name: "SPIRE_BEAD_ID", Value: beadID},
		{Name: "SPIRE_REPO_URL", Value: agent.Spec.Repo},
		{Name: "SPIRE_REPO_BRANCH", Value: branch},
		{Name: "SPIRE_COMMS_DIR", Value: "/comms"},
		{Name: "SPIRE_WORKSPACE_DIR", Value: "/workspace"},
		{Name: "SPIRE_STATE_DIR", Value: "/data"},
		{Name: "DOLT_HOST", Value: "spire-dolt.spire.svc"},
		{Name: "DOLT_PORT", Value: "3306"},
	}

	// Sidecar environment
	sidecarEnv := []corev1.EnvVar{
		{Name: "SPIRE_AGENT_NAME", Value: agent.Name},
		{Name: "DOLT_HOST", Value: "spire-dolt.spire.svc"},
		{Name: "DOLT_PORT", Value: "3306"},
	}

	// Inject secrets from SpireConfig
	if cfg != nil {
		// Dolt remote — in-cluster remotesapi for bd dolt pull/push
		wizardEnv = append(wizardEnv,
			corev1.EnvVar{Name: "DOLT_REMOTE_URL", Value: "http://spire-dolt:50051/spi"},
		)
		if cfg.Spec.DoltHub.CredentialsSecret != "" {
			wizardEnv = append(wizardEnv,
				envFromSecretOptional("DOLT_REMOTE_USER", cfg.Spec.DoltHub.CredentialsSecret, "DOLT_REMOTE_USER_WIZARD"),
				envFromSecretOptional("DOLT_REMOTE_PASSWORD", cfg.Spec.DoltHub.CredentialsSecret, "DOLT_REMOTE_PASSWORD_WIZARD"),
			)
		}

		// Anthropic API key — resolve token name from agent spec or default
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

		// GitHub token (if present in the credentials secret)
		if cfg.Spec.DoltHub.CredentialsSecret != "" {
			wizardEnv = append(wizardEnv,
				envFromSecretOptional("GITHUB_TOKEN", cfg.Spec.DoltHub.CredentialsSecret, "GITHUB_TOKEN"),
			)
		}
	}

	// Shared volumes
	volumes := []corev1.Volume{
		{Name: "comms", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		{Name: "workspace", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		{Name: "data", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		{Name: "dolt-creds", VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{
				SecretName: "dolt-creds",
				Optional:   boolPtr(true),
			},
		}},
	}

	sharedMounts := []corev1.VolumeMount{
		{Name: "comms", MountPath: "/comms"},
		{Name: "workspace", MountPath: "/workspace"},
		{Name: "data", MountPath: "/data"},
		{Name: "dolt-creds", MountPath: "/root/.dolt/creds", ReadOnly: true},
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: m.Namespace,
			Labels: map[string]string{
				"spire.awell.io/agent":   agent.Name,
				"spire.awell.io/bead":    beadID,
				"spire.awell.io/managed": "true",
				"app.kubernetes.io/name": "spire-agent",
			},
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever, // one-shot: do the work, exit
			Volumes:       volumes,
			Containers: []corev1.Container{
				{
					Name:         "wizard",
					Image:        image,
					Command:      []string{"/usr/local/bin/agent-entrypoint.sh"},
					Env:          wizardEnv,
					Resources:    buildResources(agent.Spec.Resources),
					VolumeMounts: sharedMounts,
					WorkingDir:   "/workspace",
				},
				{
					Name:  "sidecar",
					Image: image, // same image — contains spire-sidecar binary
					Command: []string{
						"spire-sidecar",
						"--comms-dir=/comms",
						"--poll-interval=10s",
						"--port=8080",
						fmt.Sprintf("--agent-name=%s", agent.Name),
					},
					Env: sidecarEnv,
					WorkingDir: "/data", // bd/spire find .beads by walking up from CWD
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

func buildResources(spec *spirev1.AgentResourceRequirements) corev1.ResourceRequirements {
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

// buildEpicPod creates a pod spec for an epic bead — the Workshop.
// Instead of a wizard container, epic pods run the Artificer (spire-artificer),
// which reviews child branches, creates PRs, and manages the merge queue.
func (m *AgentMonitor) buildEpicPod(agent *spirev1.SpireAgent, beadID string, cfg *spirev1.SpireConfig) *corev1.Pod {
	image := agent.Spec.Image
	if image == "" {
		image = m.StewardImage
	}

	podName := fmt.Sprintf("spire-workshop-%s", sanitizeK8sName(beadID))
	if len(podName) > 63 {
		podName = podName[:63]
	}

	branch := agent.Spec.RepoBranch
	if branch == "" {
		branch = "main"
	}

	// Artificer environment.
	artificerEnv := []corev1.EnvVar{
		{Name: "SPIRE_AGENT_NAME", Value: agent.Name},
		{Name: "SPIRE_EPIC_ID", Value: beadID},
		{Name: "SPIRE_REPO_URL", Value: agent.Spec.Repo},
		{Name: "SPIRE_REPO_BRANCH", Value: branch},
		{Name: "SPIRE_COMMS_DIR", Value: "/comms"},
		{Name: "SPIRE_WORKSPACE_DIR", Value: "/workspace"},
		{Name: "SPIRE_STATE_DIR", Value: "/data"},
		{Name: "DOLT_HOST", Value: "spire-dolt.spire.svc"},
		{Name: "DOLT_PORT", Value: "3306"},
		{Name: "ARTIFICER_MODEL", Value: "claude-opus-4-6"},
		{Name: "ARTIFICER_MAX_REVIEW_ROUNDS", Value: "3"},
	}

	// Sidecar environment.
	sidecarEnv := []corev1.EnvVar{
		{Name: "SPIRE_AGENT_NAME", Value: agent.Name},
		{Name: "DOLT_HOST", Value: "spire-dolt.spire.svc"},
		{Name: "DOLT_PORT", Value: "3306"},
	}

	// Inject secrets from SpireConfig.
	if cfg != nil {
		// Dolt remote — in-cluster remotesapi for bd dolt pull/push
		artificerEnv = append(artificerEnv,
			corev1.EnvVar{Name: "DOLT_REMOTE_URL", Value: "http://spire-dolt:50051/spi"},
		)
		if cfg.Spec.DoltHub.CredentialsSecret != "" {
			artificerEnv = append(artificerEnv,
				envFromSecretOptional("DOLT_REMOTE_USER", cfg.Spec.DoltHub.CredentialsSecret, "DOLT_REMOTE_USER_ARTIFICER"),
				envFromSecretOptional("DOLT_REMOTE_PASSWORD", cfg.Spec.DoltHub.CredentialsSecret, "DOLT_REMOTE_PASSWORD_ARTIFICER"),
			)
		}

		// Opus token — prefer "heavy" token for the artificer, fall back to default.
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
			artificerEnv = append(artificerEnv,
				envFromSecret("ANTHROPIC_API_KEY", tokenRef.Secret, tokenRef.Key),
			)
		}

		// GitHub token.
		if cfg.Spec.DoltHub.CredentialsSecret != "" {
			artificerEnv = append(artificerEnv,
				envFromSecretOptional("GITHUB_TOKEN", cfg.Spec.DoltHub.CredentialsSecret, "GITHUB_TOKEN"),
			)
		}
	}

	volumes := []corev1.Volume{
		{Name: "comms", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		{Name: "workspace", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		{Name: "data", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		{Name: "dolt-creds", VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{
				SecretName: "dolt-creds",
				Optional:   boolPtr(true),
			},
		}},
	}

	sharedMounts := []corev1.VolumeMount{
		{Name: "comms", MountPath: "/comms"},
		{Name: "workspace", MountPath: "/workspace"},
		{Name: "data", MountPath: "/data"},
		{Name: "dolt-creds", MountPath: "/root/.dolt/creds", ReadOnly: true},
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: m.Namespace,
			Labels: map[string]string{
				"spire.awell.io/agent":   agent.Name,
				"spire.awell.io/bead":    beadID,
				"spire.awell.io/managed": "true",
				"spire.awell.io/role":    "artificer",
				"app.kubernetes.io/name": "spire-workshop",
			},
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Volumes:       volumes,
			Containers: []corev1.Container{
				{
					Name:    "artificer",
					Image:   image,
					Command: []string{"spire-artificer", fmt.Sprintf("--epic-id=%s", beadID)},
					Env:     artificerEnv,
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

func boolPtr(b bool) *bool { return &b }

// isPodFinished checks if the main work container (wizard or artificer) has
// terminated, even if the pod phase is still Running (sidecar may still be alive).
func isPodFinished(pod *corev1.Pod) bool {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name == "sidecar" {
			continue // skip the sidecar
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
