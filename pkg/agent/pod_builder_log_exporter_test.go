package agent

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// TestPodBuilder_NoLogExporterSidecarByDefault asserts the
// canonical-shape invariant: with LogExporterEnabled unset, the pod
// has exactly one container (the agent) and the legacy contract
// is byte-for-byte preserved. This guards against accidentally
// shipping the sidecar in installs that haven't opted in.
func TestPodBuilder_NoLogExporterSidecarByDefault(t *testing.T) {
	pod, err := BuildApprenticePod(canonicalPodSpec())
	if err != nil {
		t.Fatalf("BuildApprenticePod: %v", err)
	}
	if len(pod.Spec.Containers) != 1 {
		t.Fatalf("Containers = %d, want 1 (no sidecar by default)", len(pod.Spec.Containers))
	}
	if pod.Spec.Containers[0].Name != "agent" {
		t.Errorf("Containers[0].Name = %q, want agent", pod.Spec.Containers[0].Name)
	}
	if pod.Spec.TerminationGracePeriodSeconds != nil {
		t.Errorf("TerminationGracePeriodSeconds = %d, want unset (no exporter, no grace override)",
			*pod.Spec.TerminationGracePeriodSeconds)
	}
}

// TestPodBuilder_LogExporterSidecarShape pins the canonical sidecar
// shape when LogExporterEnabled is true: exactly two containers (agent +
// spire-log-exporter), the sidecar mounts the shared spire-logs volume,
// and TerminationGracePeriodSeconds is set so SIGTERM has time to drain.
func TestPodBuilder_LogExporterSidecarShape(t *testing.T) {
	spec := canonicalPodSpec()
	spec.LogExporterEnabled = true
	spec.LogExporterResources = corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("10m"),
			corev1.ResourceMemory: resource.MustParse("64Mi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("200m"),
			corev1.ResourceMemory: resource.MustParse("256Mi"),
		},
	}
	spec.LogStoreBackend = "gcs"
	spec.LogStoreGCSBucket = "spire-logs-prod"
	spec.LogStoreGCSPrefix = "spire/agent-logs"
	spec.GCSSecretName = "spire-gcp-sa"
	spec.GCSMountPath = "/var/run/secrets/gcp"
	spec.GCSKeyName = "key.json"

	pod, err := BuildApprenticePod(spec)
	if err != nil {
		t.Fatalf("BuildApprenticePod: %v", err)
	}

	if len(pod.Spec.Containers) != 2 {
		t.Fatalf("Containers = %d, want 2 (agent + exporter)", len(pod.Spec.Containers))
	}
	agent := pod.Spec.Containers[0]
	exporter := pod.Spec.Containers[1]
	if agent.Name != "agent" {
		t.Errorf("Containers[0].Name = %q, want agent", agent.Name)
	}
	if exporter.Name != LogExporterContainerName {
		t.Errorf("Containers[1].Name = %q, want %q", exporter.Name, LogExporterContainerName)
	}

	// Sidecar's command is the spire-log-exporter binary.
	if len(exporter.Command) != 1 || exporter.Command[0] != LogExporterCommand {
		t.Errorf("exporter Command = %v, want [%s]", exporter.Command, LogExporterCommand)
	}

	// Sidecar shares the spire-logs volume mount with the agent.
	mountPaths := mountByPath(exporter.VolumeMounts)
	if got, ok := mountPaths[LogsMountPath]; !ok || got.Name != LogsVolumeName {
		t.Errorf("exporter missing %s mount of %q; got %+v", LogsMountPath, LogsVolumeName, exporter.VolumeMounts)
	}

	// Sidecar reuses the same Image as the agent when LogExporterImage
	// is unset (single-image installs).
	if exporter.Image != spec.Image {
		t.Errorf("exporter Image = %q, want %q (default to agent image)", exporter.Image, spec.Image)
	}

	// Resources flow from spec.LogExporterResources.
	if got := exporter.Resources.Requests[corev1.ResourceCPU]; got.String() != "10m" {
		t.Errorf("exporter Requests[cpu] = %s, want 10m", got.String())
	}

	// TerminationGracePeriodSeconds defaults to 30 (paired with the
	// exporter's 25s drain default).
	if pod.Spec.TerminationGracePeriodSeconds == nil {
		t.Fatal("TerminationGracePeriodSeconds is nil; sidecar build must set a grace period")
	}
	if *pod.Spec.TerminationGracePeriodSeconds != LogExporterDefaultTerminationGrace {
		t.Errorf("TerminationGracePeriodSeconds = %d, want %d",
			*pod.Spec.TerminationGracePeriodSeconds, LogExporterDefaultTerminationGrace)
	}
}

// TestPodBuilder_LogExporterEnvForwarding asserts the sidecar carries
// the substrate-relevant env vars (SPIRE_LOG_ROOT, SPIRE_TOWER,
// LOGSTORE_*, BEADS_DOLT_*). Credential SecretKeyRefs (ANTHROPIC_API_KEY
// / GITHUB_TOKEN) are agent-only and must NOT leak into the sidecar.
func TestPodBuilder_LogExporterEnvForwarding(t *testing.T) {
	spec := canonicalPodSpec()
	spec.LogExporterEnabled = true
	spec.LogStoreBackend = "gcs"
	spec.LogStoreGCSBucket = "spire-logs-prod"
	spec.LogStoreGCSPrefix = "spire/agent-logs"
	spec.LogStoreRetentionDays = "30"
	spec.GCSSecretName = "spire-gcp-sa"
	spec.GCSMountPath = "/var/run/secrets/gcp"
	spec.GCSKeyName = "key.json"

	pod, err := BuildApprenticePod(spec)
	if err != nil {
		t.Fatalf("BuildApprenticePod: %v", err)
	}
	exporter := pod.Spec.Containers[1]
	env := envByName(exporter.Env)

	mustHave := map[string]string{
		"SPIRE_LOG_ROOT":          LogsMountPath,
		"SPIRE_TOWER":             "test-tower",
		"BEADS_DATABASE":          "test-tower",
		"BEADS_DOLT_SERVER_HOST":  "spire-dolt.spire-test.svc",
		"BEADS_DOLT_SERVER_PORT":  "3306",
		"DOLT_URL":                "spire-dolt.spire-test.svc:3306",
		"LOGSTORE_BACKEND":        "gcs",
		"LOGSTORE_GCS_BUCKET":     "spire-logs-prod",
		"LOGSTORE_GCS_PREFIX":     "spire/agent-logs",
		"LOGSTORE_RETENTION_DAYS": "30",
	}
	for k, want := range mustHave {
		got, ok := env[k]
		if !ok {
			t.Errorf("exporter missing env %q", k)
			continue
		}
		if got.Value != want {
			t.Errorf("exporter env %s = %q, want %q", k, got.Value, want)
		}
	}

	// GOOGLE_APPLICATION_CREDENTIALS is forwarded when GCS is wired so
	// the substrate's storage client can authenticate.
	if _, ok := env["GOOGLE_APPLICATION_CREDENTIALS"]; !ok {
		t.Error("exporter missing GOOGLE_APPLICATION_CREDENTIALS when GCSSecretName is set")
	}

	// Credential env vars must NOT appear on the sidecar.
	for _, k := range []string{"ANTHROPIC_API_KEY", "GITHUB_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN"} {
		if _, ok := env[k]; ok {
			t.Errorf("exporter env %q must not be forwarded — credentials are agent-only", k)
		}
	}
}

// TestPodBuilder_LogExporterMountsGCSCredentialWhenPresent verifies the
// sidecar gains a read-only mount of the GCS service-account secret
// when GCSSecretName is set, paralleling the agent container's mount
// shape so both containers can authenticate via Application Default
// Credentials at the same path.
func TestPodBuilder_LogExporterMountsGCSCredentialWhenPresent(t *testing.T) {
	spec := canonicalPodSpec()
	spec.LogExporterEnabled = true
	spec.GCSSecretName = "spire-gcp-sa"
	spec.GCSMountPath = "/var/run/secrets/gcp"
	spec.GCSKeyName = "key.json"

	pod, err := BuildApprenticePod(spec)
	if err != nil {
		t.Fatalf("BuildApprenticePod: %v", err)
	}
	exporter := pod.Spec.Containers[1]
	mountPaths := mountByPath(exporter.VolumeMounts)
	got, ok := mountPaths[spec.GCSMountPath]
	if !ok {
		t.Fatalf("exporter missing GCS credential mount at %s; got %+v", spec.GCSMountPath, exporter.VolumeMounts)
	}
	if got.Name != "gcp-sa" {
		t.Errorf("GCS mount name = %q, want gcp-sa", got.Name)
	}
	if !got.ReadOnly {
		t.Error("GCS mount must be ReadOnly")
	}
}

// TestPodBuilder_LogExporterImageOverride verifies operators can pin
// the sidecar at a different image (forward-fix scenario) without
// changing the agent's image.
func TestPodBuilder_LogExporterImageOverride(t *testing.T) {
	spec := canonicalPodSpec()
	spec.LogExporterEnabled = true
	spec.LogExporterImage = "spire-log-exporter:0.3.1"

	pod, err := BuildApprenticePod(spec)
	if err != nil {
		t.Fatalf("BuildApprenticePod: %v", err)
	}
	exporter := pod.Spec.Containers[1]
	if exporter.Image != "spire-log-exporter:0.3.1" {
		t.Errorf("exporter Image = %q, want %q", exporter.Image, "spire-log-exporter:0.3.1")
	}
	// Agent image must remain unchanged.
	if pod.Spec.Containers[0].Image != spec.Image {
		t.Errorf("agent Image = %q, want %q (override must not affect agent)",
			pod.Spec.Containers[0].Image, spec.Image)
	}
}

// TestPodBuilder_LogExporterTerminationGraceOverride pins the operator
// override path: a non-zero LogExporterTerminationGrace replaces the
// default.
func TestPodBuilder_LogExporterTerminationGraceOverride(t *testing.T) {
	spec := canonicalPodSpec()
	spec.LogExporterEnabled = true
	spec.LogExporterTerminationGrace = 90

	pod, err := BuildApprenticePod(spec)
	if err != nil {
		t.Fatalf("BuildApprenticePod: %v", err)
	}
	if pod.Spec.TerminationGracePeriodSeconds == nil {
		t.Fatal("TerminationGracePeriodSeconds is nil")
	}
	if *pod.Spec.TerminationGracePeriodSeconds != 90 {
		t.Errorf("TerminationGracePeriodSeconds = %d, want 90",
			*pod.Spec.TerminationGracePeriodSeconds)
	}
}

// TestBuildWizardPod_LogExporterSidecarShape mirrors the apprentice
// shape test against the wizard pod builder so a parity drift between
// pod_builder.go and wizard_sage_pod_builder.go surfaces immediately.
func TestBuildWizardPod_LogExporterSidecarShape(t *testing.T) {
	spec := canonicalPodSpec()
	spec.Name = "wizard-spi-abc"
	spec.AgentName = "wizard-spi-abc"
	spec.LogExporterEnabled = true

	pod, err := BuildWizardPod(spec)
	if err != nil {
		t.Fatalf("BuildWizardPod: %v", err)
	}
	if len(pod.Spec.Containers) != 2 {
		t.Fatalf("wizard Containers = %d, want 2 (parity with apprentice)", len(pod.Spec.Containers))
	}
	if pod.Spec.Containers[1].Name != LogExporterContainerName {
		t.Errorf("wizard Containers[1].Name = %q, want %q",
			pod.Spec.Containers[1].Name, LogExporterContainerName)
	}
}

// TestBuildSagePod_LogExporterSidecarShape covers the sage path.
func TestBuildSagePod_LogExporterSidecarShape(t *testing.T) {
	spec := canonicalPodSpec()
	spec.Name = "sage-spi-abc"
	spec.AgentName = "sage-spi-abc"
	spec.LogExporterEnabled = true

	pod, err := BuildSagePod(spec)
	if err != nil {
		t.Fatalf("BuildSagePod: %v", err)
	}
	if len(pod.Spec.Containers) != 2 {
		t.Fatalf("sage Containers = %d, want 2", len(pod.Spec.Containers))
	}
	if pod.Spec.Containers[1].Name != LogExporterContainerName {
		t.Errorf("sage Containers[1].Name = %q, want %q",
			pod.Spec.Containers[1].Name, LogExporterContainerName)
	}
}

// TestBuildClericPod_LogExporterSidecarShape covers the cleric path.
func TestBuildClericPod_LogExporterSidecarShape(t *testing.T) {
	spec := canonicalPodSpec()
	spec.Name = "cleric-spi-abc"
	spec.AgentName = "cleric-spi-abc"
	spec.LogExporterEnabled = true

	pod, err := BuildClericPod(spec)
	if err != nil {
		t.Fatalf("BuildClericPod: %v", err)
	}
	if len(pod.Spec.Containers) != 2 {
		t.Fatalf("cleric Containers = %d, want 2", len(pod.Spec.Containers))
	}
	if pod.Spec.Containers[1].Name != LogExporterContainerName {
		t.Errorf("cleric Containers[1].Name = %q, want %q",
			pod.Spec.Containers[1].Name, LogExporterContainerName)
	}
}

// TestPodBuilder_LogExporterDisabledLeavesPodUnchanged compares the
// full Spec between LogExporterEnabled=false (default) and an
// explicit-false override: both must produce identical containers,
// identical TerminationGracePeriodSeconds, and identical volumes.
func TestPodBuilder_LogExporterDisabledLeavesPodUnchanged(t *testing.T) {
	specOff := canonicalPodSpec()
	specOff.LogExporterEnabled = false

	defaultPod, err := BuildApprenticePod(canonicalPodSpec())
	if err != nil {
		t.Fatalf("default BuildApprenticePod: %v", err)
	}
	offPod, err := BuildApprenticePod(specOff)
	if err != nil {
		t.Fatalf("off BuildApprenticePod: %v", err)
	}

	if len(defaultPod.Spec.Containers) != len(offPod.Spec.Containers) {
		t.Errorf("Containers count differs: default=%d, off=%d",
			len(defaultPod.Spec.Containers), len(offPod.Spec.Containers))
	}
	if !equalGracePtr(defaultPod.Spec.TerminationGracePeriodSeconds,
		offPod.Spec.TerminationGracePeriodSeconds) {
		t.Errorf("TerminationGracePeriodSeconds differs: default=%v, off=%v",
			defaultPod.Spec.TerminationGracePeriodSeconds,
			offPod.Spec.TerminationGracePeriodSeconds)
	}
}

func equalGracePtr(a, b *int64) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
}
