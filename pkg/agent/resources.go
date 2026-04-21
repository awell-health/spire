package agent

import (
	"os"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// Wizard-tier resource defaults. Wizard pods orchestrate per-bead workflows
// and fan out apprentices, so they need more headroom than a basic executor.
const (
	wizardDefaultMemoryRequest = "1Gi"
	wizardDefaultMemoryLimit   = "2Gi"
	wizardDefaultCPURequest    = "250m"
	wizardDefaultCPULimit      = "1000m"
)

// Env-var names that override the wizard-tier defaults. Each must parse as a
// Kubernetes resource.Quantity; on parse error the default is used and no
// panic is raised.
const (
	EnvWizardMemoryRequest = "SPIRE_WIZARD_MEMORY_REQUEST"
	EnvWizardMemoryLimit   = "SPIRE_WIZARD_MEMORY_LIMIT"
	EnvWizardCPURequest    = "SPIRE_WIZARD_CPU_REQUEST"
	EnvWizardCPULimit      = "SPIRE_WIZARD_CPU_LIMIT"
)

// WizardResources returns the Kubernetes ResourceRequirements for a wizard
// pod. Callers (backend spawn paths) pass the returned value straight into
// container.Resources.
//
// Each field honours an env-var override, parsed with
// k8s.io/apimachinery/pkg/api/resource.ParseQuantity. On parse error the
// default is used — this function never panics on malformed overrides.
func WizardResources() corev1.ResourceRequirements {
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceMemory: quantityFromEnv(EnvWizardMemoryRequest, wizardDefaultMemoryRequest),
			corev1.ResourceCPU:    quantityFromEnv(EnvWizardCPURequest, wizardDefaultCPURequest),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceMemory: quantityFromEnv(EnvWizardMemoryLimit, wizardDefaultMemoryLimit),
			corev1.ResourceCPU:    quantityFromEnv(EnvWizardCPULimit, wizardDefaultCPULimit),
		},
	}
}

// quantityFromEnv returns the parsed quantity from envKey, or the parsed
// default if the env var is unset or unparseable. The default is assumed to
// be a valid quantity string.
func quantityFromEnv(envKey, defaultVal string) resource.Quantity {
	if v := os.Getenv(envKey); v != "" {
		if q, err := resource.ParseQuantity(v); err == nil {
			return q
		}
	}
	return resource.MustParse(defaultVal)
}
