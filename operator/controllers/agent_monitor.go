package controllers

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/go-logr/logr"

	spirev1 "github.com/awell-health/spire/operator/api/v1alpha1"
)

// AgentMonitor tracks agent heartbeats and manages pods for managed agents.
type AgentMonitor struct {
	Client          client.Client
	Log             logr.Logger
	Namespace       string
	Interval        time.Duration
	OfflineTimeout  time.Duration // how long before an agent is considered offline
	MayorImage      string        // image to use for managed agent pods
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
	var agents spirev1.SpireAgentList
	if err := m.Client.List(ctx, &agents, client.InNamespace(m.Namespace)); err != nil {
		m.Log.Error(err, "failed to list agents")
		return
	}

	for i := range agents.Items {
		agent := &agents.Items[i]

		switch agent.Spec.Mode {
		case "external":
			m.checkExternalAgent(ctx, agent)
		case "managed":
			m.reconcileManagedAgent(ctx, agent)
		}
	}
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

// reconcileManagedAgent ensures a pod exists when there's work, cleans up when idle.
func (m *AgentMonitor) reconcileManagedAgent(ctx context.Context, agent *spirev1.SpireAgent) {
	podName := fmt.Sprintf("spire-agent-%s", agent.Name)

	// Check if pod exists
	var existingPod corev1.Pod
	podExists := true
	if err := m.Client.Get(ctx, client.ObjectKey{Namespace: m.Namespace, Name: podName}, &existingPod); err != nil {
		if errors.IsNotFound(err) {
			podExists = false
		} else {
			m.Log.Error(err, "failed to check pod", "pod", podName)
			return
		}
	}

	hasWork := len(agent.Status.CurrentWork) > 0

	if hasWork && !podExists {
		// Create pod
		pod := m.buildAgentPod(agent, podName)
		if err := m.Client.Create(ctx, pod); err != nil {
			m.Log.Error(err, "failed to create agent pod", "agent", agent.Name)
			return
		}

		agent.Status.Phase = "Provisioning"
		agent.Status.PodName = podName
		agent.Status.Message = "Pod created, waiting for startup"
		m.Client.Status().Update(ctx, agent) //nolint
		m.Log.Info("created agent pod", "agent", agent.Name, "pod", podName)

	} else if !hasWork && podExists && existingPod.DeletionTimestamp == nil {
		// No work — delete the pod
		if err := m.Client.Delete(ctx, &existingPod); err != nil {
			m.Log.Error(err, "failed to delete idle agent pod", "pod", podName)
			return
		}

		agent.Status.Phase = "Idle"
		agent.Status.PodName = ""
		agent.Status.Message = "Pod deleted (no work)"
		m.Client.Status().Update(ctx, agent) //nolint
		m.Log.Info("deleted idle agent pod", "agent", agent.Name)

	} else if podExists {
		// Pod exists — check its phase
		switch existingPod.Status.Phase {
		case corev1.PodRunning:
			if agent.Status.Phase == "Provisioning" {
				agent.Status.Phase = "Working"
				agent.Status.Message = "Pod running"
				m.Client.Status().Update(ctx, agent) //nolint
			}
		case corev1.PodFailed:
			agent.Status.Phase = "Offline"
			agent.Status.Message = fmt.Sprintf("Pod failed: %s", existingPod.Status.Message)
			m.Client.Status().Update(ctx, agent) //nolint
		}
	}
}

func (m *AgentMonitor) buildAgentPod(agent *spirev1.SpireAgent, podName string) *corev1.Pod {
	image := agent.Spec.Image
	if image == "" {
		image = m.MayorImage
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: m.Namespace,
			Labels: map[string]string{
				"spire.awell.io/agent":   agent.Name,
				"spire.awell.io/managed": "true",
				"app.kubernetes.io/name": "spire-agent",
			},
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyOnFailure,
			Containers: []corev1.Container{
				{
					Name:  "agent",
					Image: image,
					Env: []corev1.EnvVar{
						{Name: "SPIRE_AGENT_NAME", Value: agent.Name},
						{Name: "SPIRE_AGENT_MODE", Value: "managed"},
					},
					Resources: buildResources(agent.Spec.Resources),
				},
			},
		},
	}

	// Add repo clone init container if repo is specified
	if agent.Spec.Repo != "" {
		branch := agent.Spec.RepoBranch
		if branch == "" {
			branch = "main"
		}
		pod.Spec.InitContainers = []corev1.Container{
			{
				Name:  "clone",
				Image: "alpine/git:latest",
				Command: []string{
					"git", "clone", "--depth=1", "--branch", branch,
					agent.Spec.Repo, "/workspace",
				},
				VolumeMounts: []corev1.VolumeMount{
					{Name: "workspace", MountPath: "/workspace"},
				},
			},
		}
		pod.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{
			{Name: "workspace", MountPath: "/workspace"},
		}
		pod.Spec.Containers[0].WorkingDir = "/workspace"
		pod.Spec.Volumes = []corev1.Volume{
			{
				Name: "workspace",
				VolumeSource: corev1.VolumeSource{
					EmptyDir: &corev1.EmptyDirVolumeSource{},
				},
			},
		}
	}

	return pod
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
