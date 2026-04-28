// Package agent — wizard / sage / cleric pod builders.
//
// BuildWizardPod, BuildSagePod, and BuildClericPod are siblings of
// BuildApprenticePod (pod_builder.go). The cluster-native dispatch
// path emits one WorkloadIntent per claimed bead carrying explicit
// (Role, Phase, Runtime) — see pkg/steward/intent.WorkloadIntent and
// the contract.go enum/allowlist. The operator validates the intent
// and selects a builder via SelectBuilder(role, phase) which routes:
//
//   - (wizard, implement)               → BuildWizardPod
//   - (apprentice, implement|fix|review-fix) → BuildApprenticePod
//   - (sage, review)                    → BuildSagePod
//   - (cleric, recovery)                → BuildClericPod
//
// All four builders share PodSpec, validate(), withDefaults(), and
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

// BuildClericPod returns the canonical cleric pod for spec. The cleric
// pod runs `spire cleric diagnose <bead-id> --name <agent>` and is the
// failure-recovery driver dispatched against a recovery bead.
//
// Cluster routing comes through the Role=cleric, Phase=recovery
// allowlist entry — the operator no longer routes cleric work via
// formula_phase=recovery (which the operator does not recognize as a
// formula step name).
//
// PodSpec.Role is set to RoleApprentice as a sentinel because the
// runtime SpawnRole vocabulary in pkg/runtime does not currently
// declare a cleric role; the canonical "cleric" identity travels via
// the cluster intent contract (intent.RoleCleric) and the per-pod
// agent name + command. A future refactor that adds a cleric SpawnRole
// to pkg/runtime should switch this to that role.
func BuildClericPod(spec PodSpec) (*corev1.Pod, error) {
	spec.Role = RoleApprentice
	if err := spec.validate(); err != nil {
		return nil, err
	}
	spec = spec.withDefaults()

	args := []string{"cleric", "diagnose", spec.BeadID, "--name", spec.effectiveAgentName()}
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
	logsMount := corev1.VolumeMount{Name: LogsVolumeName, MountPath: LogsMountPath}

	mainMounts := []corev1.VolumeMount{dataMount, workspaceMount, logsMount}
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
