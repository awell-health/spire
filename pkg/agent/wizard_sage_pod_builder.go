// Package agent — wizard / sage pod builders.
//
// BuildWizardPod and BuildSagePod are siblings of BuildApprenticePod
// (pod_builder.go). The cluster-native dispatch path emits one
// WorkloadIntent per claimed bead at the bead-level phase
// (intent.IsBeadLevelPhase) and the operator routes that to a wizard
// pod via BuildWizardPod. The wizard then emits step-level intents
// from inside the pod; the operator routes review/arbiter phases to a
// sage pod via BuildSagePod and implement/fix phases to an apprentice
// pod via BuildApprenticePod.
//
// All three builders share PodSpec, validate(), withDefaults(), and
// the volume / init-container / env / label helpers. The only thing
// that differs is the spawn role and the main container's command.
package agent

import (
	corev1 "k8s.io/api/core/v1"
)

// BuildWizardPod returns the canonical wizard pod for spec. The
// wizard pod runs `spire execute <bead-id> --name <agent>` and is the
// per-bead orchestrator: it owns formula state and dispatches
// step-level work via WorkloadIntent emits to the shared outbox.
//
// PodSpec.Role is set to RoleWizard before validation and pod
// construction; callers do not need to populate it themselves.
//
// Required PodSpec fields are the same as BuildApprenticePod (Name,
// Namespace, Image, BeadID, Identity.{TowerName,RepoURL,BaseBranch,
// Prefix}); see ErrPodSpec* vars for the typed errors.
func BuildWizardPod(spec PodSpec) (*corev1.Pod, error) {
	spec.Role = RoleWizard
	if err := spec.validate(); err != nil {
		return nil, err
	}
	spec = spec.withDefaults()

	args := []string{"execute", spec.BeadID, "--name", spec.effectiveAgentName()}
	args = append(args, spec.ExtraArgs...)
	return buildRolePodFromSpec(spec, args)
}

// BuildSagePod returns the canonical sage pod for spec. The sage pod
// runs `spire sage review <bead-id> --name <agent>` and is the
// one-shot review worker dispatched by the wizard for review or
// arbiter phases.
//
// PodSpec.Role is set to RoleSage before validation and pod
// construction.
func BuildSagePod(spec PodSpec) (*corev1.Pod, error) {
	spec.Role = RoleSage
	if err := spec.validate(); err != nil {
		return nil, err
	}
	spec = spec.withDefaults()

	args := []string{"sage", "review", spec.BeadID, "--name", spec.effectiveAgentName()}
	args = append(args, spec.ExtraArgs...)
	return buildRolePodFromSpec(spec, args)
}

// buildRolePodFromSpec is the shared pod-construction tail used by
// BuildWizardPod and BuildSagePod. It mirrors BuildApprenticePod's
// post-validate tail — same volumes, init containers, env, labels,
// annotations — only the main container's command differs.
//
// Apprentice pods do NOT call this helper; BuildApprenticePod retains
// its own inlined construction so the apprentice path stays
// byte-for-byte identical with the parity tests.
func buildRolePodFromSpec(spec PodSpec, args []string) (*corev1.Pod, error) {
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
