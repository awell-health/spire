package agent

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

const testNamespace = "spire-test"
const testImage = "spire-agent:dev"

func newTestBackend() (*K8sBackend, *fake.Clientset) {
	client := fake.NewSimpleClientset()
	b := NewK8sBackendFromClient(client, testNamespace, testImage)
	return b, client
}

func TestK8sBackend_Spawn_CreatesCorrectPod(t *testing.T) {
	b, client := newTestBackend()

	cfg := SpawnConfig{
		Name:         "apprentice-spi-abc-0",
		BeadID:       "spi-abc",
		Role:         RoleApprentice,
		Tower:        "my-tower",
		Provider:     "claude",
		Step:         "implement",
		ExtraArgs:    []string{"--review-fix"},
		CustomPrompt: "do the thing",
	}

	handle, err := b.Spawn(cfg)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	if handle.Name() != cfg.Name {
		t.Errorf("handle.Name() = %q, want %q", handle.Name(), cfg.Name)
	}

	// Verify pod was created.
	pods, err := client.CoreV1().Pods(testNamespace).List(
		context.Background(), metav1.ListOptions{},
	)
	if err != nil {
		t.Fatalf("list pods: %v", err)
	}
	if len(pods.Items) != 1 {
		t.Fatalf("expected 1 pod, got %d", len(pods.Items))
	}

	pod := pods.Items[0]

	// Check labels.
	wantLabels := map[string]string{
		"spire.agent":      "true",
		"spire.agent.name": cfg.Name,
		"spire.bead":       cfg.BeadID,
		"spire.role":       string(cfg.Role),
		"spire.tower":      cfg.Tower,
	}
	for k, want := range wantLabels {
		if got := pod.Labels[k]; got != want {
			t.Errorf("label %s = %q, want %q", k, got, want)
		}
	}

	// Check restart policy.
	if pod.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("RestartPolicy = %q, want Never", pod.Spec.RestartPolicy)
	}

	// Check priority class.
	if pod.Spec.PriorityClassName != "spire-agent-default" {
		t.Errorf("PriorityClassName = %q, want spire-agent-default", pod.Spec.PriorityClassName)
	}

	// Check container basics.
	if len(pod.Spec.Containers) != 1 {
		t.Fatalf("expected 1 container, got %d", len(pod.Spec.Containers))
	}
	c := pod.Spec.Containers[0]

	if c.Image != testImage {
		t.Errorf("Image = %q, want %q", c.Image, testImage)
	}

	// Check command includes the role subcmd tokens, bead ID, and name.
	if len(c.Command) < 5 {
		t.Fatalf("Command too short: %v", c.Command)
	}
	if c.Command[0] != "spire" {
		t.Errorf("Command[0] = %q, want spire", c.Command[0])
	}
	// RoleApprentice -> "apprentice run"
	if c.Command[1] != "apprentice" || c.Command[2] != "run" {
		t.Errorf("Command[1:3] = %v, want [apprentice run]", c.Command[1:3])
	}
	if c.Command[3] != cfg.BeadID {
		t.Errorf("Command[3] = %q, want %q", c.Command[3], cfg.BeadID)
	}

	// Check env vars.
	envMap := make(map[string]corev1.EnvVar)
	for _, e := range c.Env {
		envMap[e.Name] = e
	}

	wantEnv := map[string]string{
		"SPIRE_TOWER":                      "my-tower",
		"SPIRE_PROVIDER":                   "claude",
		"OTEL_EXPORTER_OTLP_ENDPOINT":      "http://spire-steward.spire-test.svc:4317",
		"CLAUDE_CODE_ENABLE_TELEMETRY":      "1",
		"CLAUDE_CODE_ENHANCED_TELEMETRY_BETA": "1",
		"OTEL_TRACES_EXPORTER":              "otlp",
		"OTEL_LOGS_EXPORTER":                "otlp",
		"OTEL_EXPORTER_OTLP_PROTOCOL":       "grpc",
		"BEADS_DOLT_SERVER_HOST":            "spire-dolt.spire-test.svc",
		"BEADS_DOLT_SERVER_PORT":            "3307",
		"SPIRE_CUSTOM_PROMPT":               "do the thing",
	}
	for k, want := range wantEnv {
		got, ok := envMap[k]
		if !ok {
			t.Errorf("missing env var %s", k)
			continue
		}
		if got.Value != want {
			t.Errorf("env %s = %q, want %q", k, got.Value, want)
		}
	}

	// Check OTEL_RESOURCE_ATTRIBUTES contains expected fields.
	ra, ok := envMap["OTEL_RESOURCE_ATTRIBUTES"]
	if !ok {
		t.Error("missing OTEL_RESOURCE_ATTRIBUTES")
	} else {
		for _, want := range []string{"bead.id=spi-abc", "agent.name=apprentice-spi-abc-0", "step=implement", "tower=my-tower"} {
			if !contains(ra.Value, want) {
				t.Errorf("OTEL_RESOURCE_ATTRIBUTES %q missing %q", ra.Value, want)
			}
		}
	}

	// Check secret refs.
	apiKey, ok := envMap["ANTHROPIC_API_KEY"]
	if !ok {
		t.Error("missing ANTHROPIC_API_KEY env var")
	} else if apiKey.ValueFrom == nil || apiKey.ValueFrom.SecretKeyRef == nil {
		t.Error("ANTHROPIC_API_KEY should use secretKeyRef")
	} else {
		if apiKey.ValueFrom.SecretKeyRef.Name != "spire-credentials" {
			t.Errorf("ANTHROPIC_API_KEY secret name = %q, want spire-credentials", apiKey.ValueFrom.SecretKeyRef.Name)
		}
		if apiKey.ValueFrom.SecretKeyRef.Key != "ANTHROPIC_API_KEY_DEFAULT" {
			t.Errorf("ANTHROPIC_API_KEY secret key = %q, want ANTHROPIC_API_KEY_DEFAULT", apiKey.ValueFrom.SecretKeyRef.Key)
		}
	}

	ghToken, ok := envMap["GITHUB_TOKEN"]
	if !ok {
		t.Error("missing GITHUB_TOKEN env var")
	} else if ghToken.ValueFrom == nil || ghToken.ValueFrom.SecretKeyRef == nil {
		t.Error("GITHUB_TOKEN should use secretKeyRef")
	} else {
		if ghToken.ValueFrom.SecretKeyRef.Name != "spire-credentials" {
			t.Errorf("GITHUB_TOKEN secret name = %q, want spire-credentials", ghToken.ValueFrom.SecretKeyRef.Name)
		}
		if ghToken.ValueFrom.SecretKeyRef.Key != "GITHUB_TOKEN" {
			t.Errorf("GITHUB_TOKEN secret key = %q, want GITHUB_TOKEN", ghToken.ValueFrom.SecretKeyRef.Key)
		}
		if ghToken.ValueFrom.SecretKeyRef.Optional == nil || !*ghToken.ValueFrom.SecretKeyRef.Optional {
			t.Error("GITHUB_TOKEN should be Optional=true so installs without a github token don't block pod creation")
		}
	}
}

// TestK8sBackend_Spawn_ApprenticeIdentity verifies the three apprentice
// identity env vars are injected into the spawned pod when populated on
// SpawnConfig.
func TestK8sBackend_Spawn_ApprenticeIdentity(t *testing.T) {
	b, client := newTestBackend()

	cfg := SpawnConfig{
		Name:          "apprentice-spi-xyz-2",
		BeadID:        "spi-xyz",
		Role:          RoleApprentice,
		Tower:         "t",
		AttemptID:     "spi-att9",
		ApprenticeIdx: "2",
	}

	if _, err := b.Spawn(cfg); err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	pods, err := client.CoreV1().Pods(testNamespace).List(
		context.Background(), metav1.ListOptions{},
	)
	if err != nil {
		t.Fatalf("list pods: %v", err)
	}
	if len(pods.Items) != 1 {
		t.Fatalf("expected 1 pod, got %d", len(pods.Items))
	}

	envMap := make(map[string]corev1.EnvVar)
	for _, e := range pods.Items[0].Spec.Containers[0].Env {
		envMap[e.Name] = e
	}

	wantEnv := map[string]string{
		"SPIRE_BEAD_ID":        "spi-xyz",
		"SPIRE_ATTEMPT_ID":     "spi-att9",
		"SPIRE_APPRENTICE_IDX": "2",
	}
	for k, want := range wantEnv {
		got, ok := envMap[k]
		if !ok {
			t.Errorf("missing env var %s", k)
			continue
		}
		if got.Value != want {
			t.Errorf("env %s = %q, want %q", k, got.Value, want)
		}
	}
}

// TestK8sBackend_Spawn_SetsSpireRole verifies SPIRE_ROLE is injected into
// the pod env for each role so the SubagentStart hook can emit the
// correct per-role command catalog.
func TestK8sBackend_Spawn_SetsSpireRole(t *testing.T) {
	roles := []SpawnRole{RoleApprentice, RoleSage, RoleWizard, RoleExecutor}
	for _, role := range roles {
		t.Run(string(role), func(t *testing.T) {
			b, client := newTestBackend()

			cfg := SpawnConfig{
				Name:   "agent-" + string(role),
				BeadID: "spi-role",
				Role:   role,
				Tower:  "t",
			}

			if _, err := b.Spawn(cfg); err != nil {
				t.Fatalf("Spawn: %v", err)
			}

			pods, err := client.CoreV1().Pods(testNamespace).List(
				context.Background(), metav1.ListOptions{},
			)
			if err != nil {
				t.Fatalf("list pods: %v", err)
			}
			if len(pods.Items) != 1 {
				t.Fatalf("expected 1 pod, got %d", len(pods.Items))
			}

			var got string
			var found bool
			for _, e := range pods.Items[0].Spec.Containers[0].Env {
				if e.Name == "SPIRE_ROLE" {
					got = e.Value
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("missing SPIRE_ROLE env var")
			}
			if got != string(role) {
				t.Errorf("SPIRE_ROLE = %q, want %q", got, string(role))
			}
		})
	}
}

// TestK8sBackend_Spawn_OmitsEmptyIdentity verifies that identity env vars
// left unset on SpawnConfig are NOT injected.
func TestK8sBackend_Spawn_OmitsEmptyIdentity(t *testing.T) {
	b, client := newTestBackend()

	cfg := SpawnConfig{
		Name:  "reviewer",
		Role:  RoleSage,
		Tower: "t",
	}

	if _, err := b.Spawn(cfg); err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	pods, err := client.CoreV1().Pods(testNamespace).List(
		context.Background(), metav1.ListOptions{},
	)
	if err != nil {
		t.Fatalf("list pods: %v", err)
	}
	if len(pods.Items) != 1 {
		t.Fatalf("expected 1 pod, got %d", len(pods.Items))
	}

	for _, e := range pods.Items[0].Spec.Containers[0].Env {
		switch e.Name {
		case "SPIRE_BEAD_ID", "SPIRE_ATTEMPT_ID", "SPIRE_APPRENTICE_IDX":
			t.Errorf("unexpected env var set: %s=%s", e.Name, e.Value)
		}
	}
}

func TestK8sBackend_Spawn_ResourceTiers(t *testing.T) {
	tests := []struct {
		role      SpawnRole
		wantMemLt string // memory limit
	}{
		{RoleApprentice, "4Gi"},
		{RoleSage, "1Gi"},
		{RoleWizard, "512Mi"},
		{RoleExecutor, "512Mi"},
	}

	for _, tt := range tests {
		t.Run(string(tt.role), func(t *testing.T) {
			b, client := newTestBackend()

			cfg := SpawnConfig{
				Name:   "test-" + string(tt.role),
				BeadID: "spi-test",
				Role:   tt.role,
				Tower:  "test-tower",
			}

			_, err := b.Spawn(cfg)
			if err != nil {
				t.Fatalf("Spawn: %v", err)
			}

			pods, _ := client.CoreV1().Pods(testNamespace).List(
				context.Background(), metav1.ListOptions{},
			)
			if len(pods.Items) == 0 {
				t.Fatal("no pods created")
			}

			c := pods.Items[0].Spec.Containers[0]
			wantMem := resource.MustParse(tt.wantMemLt)
			gotMem := c.Resources.Limits[corev1.ResourceMemory]
			if !gotMem.Equal(wantMem) {
				t.Errorf("role %s: memory limit = %s, want %s", tt.role, gotMem.String(), wantMem.String())
			}
		})
	}
}

func TestK8sBackend_Wait_Success(t *testing.T) {
	client := fake.NewSimpleClientset()
	b := NewK8sBackendFromClient(client, testNamespace, testImage)

	cfg := SpawnConfig{
		Name:   "test-wait-ok",
		BeadID: "spi-wait",
		Role:   RoleWizard,
		Tower:  "test",
	}

	handle, err := b.Spawn(cfg)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	// Simulate pod completing successfully by updating its status.
	podName := handle.Identifier()
	pod, _ := client.CoreV1().Pods(testNamespace).Get(
		context.Background(), podName, metav1.GetOptions{},
	)
	pod.Status.Phase = corev1.PodSucceeded
	_, err = client.CoreV1().Pods(testNamespace).UpdateStatus(
		context.Background(), pod, metav1.UpdateOptions{},
	)
	if err != nil {
		t.Fatalf("update pod status: %v", err)
	}

	// Wait should return nil since the pod already succeeded.
	err = handle.Wait()
	if err != nil {
		t.Errorf("Wait() = %v, want nil", err)
	}

	if handle.Alive() {
		t.Error("handle.Alive() = true after Wait, want false")
	}
}

func TestK8sBackend_Wait_Failure(t *testing.T) {
	client := fake.NewSimpleClientset()
	b := NewK8sBackendFromClient(client, testNamespace, testImage)

	cfg := SpawnConfig{
		Name:   "test-wait-fail",
		BeadID: "spi-waitfail",
		Role:   RoleApprentice,
		Tower:  "test",
	}

	handle, err := b.Spawn(cfg)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	// Simulate pod failure.
	podName := handle.Identifier()
	pod, _ := client.CoreV1().Pods(testNamespace).Get(
		context.Background(), podName, metav1.GetOptions{},
	)
	pod.Status.Phase = corev1.PodFailed
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{
		{
			State: corev1.ContainerState{
				Terminated: &corev1.ContainerStateTerminated{
					ExitCode: 1,
					Reason:   "Error",
				},
			},
		},
	}
	_, err = client.CoreV1().Pods(testNamespace).UpdateStatus(
		context.Background(), pod, metav1.UpdateOptions{},
	)
	if err != nil {
		t.Fatalf("update pod status: %v", err)
	}

	err = handle.Wait()
	if err == nil {
		t.Error("Wait() = nil, want error for failed pod")
	}
	if !contains(err.Error(), "exit code 1") {
		t.Errorf("Wait() error = %q, want to contain 'exit code 1'", err.Error())
	}

	if handle.Alive() {
		t.Error("handle.Alive() = true after Wait, want false")
	}
}

func TestK8sBackend_Kill(t *testing.T) {
	b, client := newTestBackend()

	cfg := SpawnConfig{
		Name:   "test-kill",
		BeadID: "spi-kill",
		Role:   RoleSage,
		Tower:  "test",
	}

	_, err := b.Spawn(cfg)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	// Verify pod exists.
	pods, _ := client.CoreV1().Pods(testNamespace).List(
		context.Background(), metav1.ListOptions{},
	)
	if len(pods.Items) != 1 {
		t.Fatalf("expected 1 pod before kill, got %d", len(pods.Items))
	}

	// Kill.
	if err := b.Kill(cfg.Name); err != nil {
		t.Fatalf("Kill: %v", err)
	}

	// Verify pod was deleted.
	pods, _ = client.CoreV1().Pods(testNamespace).List(
		context.Background(), metav1.ListOptions{},
	)
	if len(pods.Items) != 0 {
		t.Errorf("expected 0 pods after kill, got %d", len(pods.Items))
	}
}

func TestK8sBackend_List(t *testing.T) {
	b, _ := newTestBackend()

	// Spawn multiple agents.
	roles := []SpawnRole{RoleApprentice, RoleSage, RoleWizard}
	names := []string{"agent-a", "agent-b", "agent-c"}
	beads := []string{"spi-a", "spi-b", "spi-c"}

	for i, role := range roles {
		_, err := b.Spawn(SpawnConfig{
			Name:   names[i],
			BeadID: beads[i],
			Role:   role,
			Tower:  "test",
		})
		if err != nil {
			t.Fatalf("Spawn %s: %v", names[i], err)
		}
	}

	infos, err := b.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	if len(infos) != 3 {
		t.Fatalf("List returned %d infos, want 3", len(infos))
	}

	// Build a map by name for easier assertion.
	infoMap := make(map[string]Info)
	for _, info := range infos {
		infoMap[info.Name] = info
	}

	for i, name := range names {
		info, ok := infoMap[name]
		if !ok {
			t.Errorf("missing info for agent %q", name)
			continue
		}
		if info.BeadID != beads[i] {
			t.Errorf("agent %s: BeadID = %q, want %q", name, info.BeadID, beads[i])
		}
		if info.Phase != string(roles[i]) {
			t.Errorf("agent %s: Phase = %q, want %q", name, info.Phase, roles[i])
		}
	}
}

func TestK8sBackend_Logs(t *testing.T) {
	b, _ := newTestBackend()

	// Logs for a non-existent agent should return os.ErrNotExist.
	_, err := b.Logs("nonexistent-agent")
	if err == nil {
		t.Error("Logs for nonexistent agent should return error")
	}
}

// contains checks if s contains substr.
func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchString(s, substr)
}

func searchString(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
