package agent

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/awell-health/spire/pkg/runtime"
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
		"BEADS_DOLT_SERVER_PORT":            "3306",
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
			if role == RoleWizard {
				cfg.RepoURL = "https://github.com/example/repo.git"
				cfg.RepoBranch = "main"
				cfg.RepoPrefix = "spi"
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
		// RoleWizard has its own tier (spi-3ca64): wizards orchestrate
		// apprentices and need more headroom than the old shared default.
		// Defaults live in resources.go (wizardDefaultMemoryLimit) and
		// can be overridden via SPIRE_WIZARD_MEMORY_LIMIT.
		{RoleWizard, "2Gi"},
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
			if tt.role == RoleWizard {
				cfg.RepoURL = "https://github.com/example/repo.git"
				cfg.RepoBranch = "main"
				cfg.RepoPrefix = "spi"
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
		Name:       "test-wait-ok",
		BeadID:     "spi-wait",
		Role:       RoleWizard,
		Tower:      "test",
		RepoURL:    "https://github.com/example/repo.git",
		RepoBranch: "main",
		RepoPrefix: "spi",
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
		cfg := SpawnConfig{
			Name:   names[i],
			BeadID: beads[i],
			Role:   role,
			Tower:  "test",
		}
		if role == RoleWizard {
			cfg.RepoURL = "https://github.com/example/repo.git"
			cfg.RepoBranch = "main"
			cfg.RepoPrefix = "spi"
		}
		_, err := b.Spawn(cfg)
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

// --- Wizard pod spec tests ----------------------------------------------
//
// These tests pin the canonical wizard pod contract produced by the
// RoleWizard branch in (*K8sBackend).Spawn. Each test spawns a single
// wizard pod via the fake clientset and asserts one aspect of the pod
// spec (volumes, init container, env, resources, command, restart
// policy) so that regressions in any one dimension surface on their
// own rather than folded into a single mega-assertion.

// wizardSpawnConfig returns a SpawnConfig populated with the repo
// bootstrap fields now required by buildWizardPod (spi-fopwn). Used by
// every wizard-branch test so fixtures stay DRY and any future field
// additions surface in a single place.
func wizardSpawnConfig() SpawnConfig {
	return SpawnConfig{
		Name:       "wizard-spi-abcde-0",
		BeadID:     "spi-abcde",
		Role:       RoleWizard,
		Tower:      "test-tower",
		RepoURL:    "https://github.com/example/repo.git",
		RepoBranch: "main",
		RepoPrefix: "spi",
	}
}

func TestK8sBackend_SpawnWizard_Volumes(t *testing.T) {
	b, client := newTestBackend()

	if _, err := b.Spawn(wizardSpawnConfig()); err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	pod := spawnedPod(t, client)

	if len(pod.Spec.Volumes) != 2 {
		t.Fatalf("len(Volumes) = %d, want 2; got %+v", len(pod.Spec.Volumes), pod.Spec.Volumes)
	}
	vols := make(map[string]corev1.Volume, 2)
	for _, v := range pod.Spec.Volumes {
		vols[v.Name] = v
	}
	for _, name := range []string{"data", "workspace"} {
		v, ok := vols[name]
		if !ok {
			t.Errorf("missing volume %q", name)
			continue
		}
		if v.EmptyDir == nil {
			t.Errorf("volume %q: EmptyDir is nil, want EmptyDir source", name)
		}
	}

	if len(pod.Spec.Containers) != 1 {
		t.Fatalf("len(Containers) = %d, want 1", len(pod.Spec.Containers))
	}
	mainMounts := make(map[string]string, 2) // path -> volume name
	for _, m := range pod.Spec.Containers[0].VolumeMounts {
		mainMounts[m.MountPath] = m.Name
	}
	if mainMounts["/data"] != "data" {
		t.Errorf("main container /data mount volume = %q, want %q", mainMounts["/data"], "data")
	}
	if mainMounts["/workspace"] != "workspace" {
		t.Errorf("main container /workspace mount volume = %q, want %q", mainMounts["/workspace"], "workspace")
	}

	if len(pod.Spec.InitContainers) != 2 {
		t.Fatalf("len(InitContainers) = %d, want 2 (tower-attach, repo-bootstrap)", len(pod.Spec.InitContainers))
	}
	// tower-attach mounts /data only; repo-bootstrap mounts /data + /workspace.
	towerAttach := pod.Spec.InitContainers[0]
	var taDataMount bool
	for _, m := range towerAttach.VolumeMounts {
		if m.MountPath == "/data" && m.Name == "data" {
			taDataMount = true
			break
		}
	}
	if !taDataMount {
		t.Errorf("tower-attach missing /data mount backed by volume %q; mounts = %+v",
			"data", towerAttach.VolumeMounts)
	}

	repoBootstrap := pod.Spec.InitContainers[1]
	rbMounts := make(map[string]string, 2)
	for _, m := range repoBootstrap.VolumeMounts {
		rbMounts[m.MountPath] = m.Name
	}
	if rbMounts["/data"] != "data" {
		t.Errorf("repo-bootstrap /data mount volume = %q, want %q", rbMounts["/data"], "data")
	}
	if rbMounts["/workspace"] != "workspace" {
		t.Errorf("repo-bootstrap /workspace mount volume = %q, want %q", rbMounts["/workspace"], "workspace")
	}
}

func TestK8sBackend_SpawnWizard_InitContainer(t *testing.T) {
	b, client := newTestBackend()

	if _, err := b.Spawn(wizardSpawnConfig()); err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	pod := spawnedPod(t, client)

	if len(pod.Spec.InitContainers) != 2 {
		t.Fatalf("len(InitContainers) = %d, want 2 (tower-attach, repo-bootstrap)", len(pod.Spec.InitContainers))
	}
	ic := pod.Spec.InitContainers[0]
	if ic.Name != "tower-attach" {
		t.Errorf("init container[0].Name = %q, want tower-attach", ic.Name)
	}
	if len(ic.Command) < 3 {
		t.Fatalf("tower-attach Command too short: %v", ic.Command)
	}
	wantPrefix := []string{"spire", "tower", "attach-cluster"}
	for i, w := range wantPrefix {
		if ic.Command[i] != w {
			t.Errorf("tower-attach Command[%d] = %q, want %q", i, ic.Command[i], w)
		}
	}
	for _, flag := range []string{"--data-dir=/data/", "--database=", "--prefix=", "--dolthub-remote="} {
		if !containsFlag(ic.Command, flag) {
			t.Errorf("tower-attach Command missing flag starting with %q; got %v", flag, ic.Command)
		}
	}
}

// TestK8sBackend_SpawnWizard_RepoBootstrapInitContainer pins the
// repo-bootstrap init container (spi-fopwn): it must run after
// tower-attach, reference the three SPIRE_REPO_* env vars in its shell
// command, and mount both /data (for bind-local to write tower config)
// and /workspace (for the clone target).
func TestK8sBackend_SpawnWizard_RepoBootstrapInitContainer(t *testing.T) {
	b, client := newTestBackend()

	if _, err := b.Spawn(wizardSpawnConfig()); err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	pod := spawnedPod(t, client)

	if len(pod.Spec.InitContainers) != 2 {
		t.Fatalf("len(InitContainers) = %d, want 2 (tower-attach, repo-bootstrap)", len(pod.Spec.InitContainers))
	}
	// Order matters: tower-attach seeds /data/<db>/.beads, then
	// repo-bootstrap clones and binds; swapping the order breaks the
	// bind-local write target.
	if pod.Spec.InitContainers[0].Name != "tower-attach" {
		t.Errorf("init container[0].Name = %q, want tower-attach", pod.Spec.InitContainers[0].Name)
	}
	if pod.Spec.InitContainers[1].Name != "repo-bootstrap" {
		t.Errorf("init container[1].Name = %q, want repo-bootstrap", pod.Spec.InitContainers[1].Name)
	}

	rb := pod.Spec.InitContainers[1]
	if rb.Image != testImage {
		t.Errorf("repo-bootstrap Image = %q, want %q", rb.Image, testImage)
	}
	if len(rb.Command) < 2 || rb.Command[0] != "sh" || rb.Command[1] != "-c" {
		t.Errorf("repo-bootstrap Command[:2] = %v, want [sh -c ...]", rb.Command)
	}
	if len(rb.Command) < 3 {
		t.Fatalf("repo-bootstrap Command missing script: %v", rb.Command)
	}
	script := rb.Command[2]
	for _, substr := range []string{
		"SPIRE_REPO_URL",
		"SPIRE_REPO_BRANCH",
		"SPIRE_REPO_PREFIX",
		"git clone",
		"spire repo bind-local",
	} {
		if !strings.Contains(script, substr) {
			t.Errorf("repo-bootstrap script missing %q\nscript: %s", substr, script)
		}
	}

	// The env vars themselves must be on the container (not just
	// referenced in the script) so the shell can expand them.
	envMap := make(map[string]corev1.EnvVar, len(rb.Env))
	for _, e := range rb.Env {
		envMap[e.Name] = e
	}
	wantEnv := map[string]string{
		"SPIRE_REPO_URL":    "https://github.com/example/repo.git",
		"SPIRE_REPO_BRANCH": "main",
		"SPIRE_REPO_PREFIX": "spi",
	}
	for k, want := range wantEnv {
		got, ok := envMap[k]
		if !ok {
			t.Errorf("repo-bootstrap missing env %s", k)
			continue
		}
		if got.Value != want {
			t.Errorf("repo-bootstrap env %s = %q, want %q", k, got.Value, want)
		}
	}

	mounts := make(map[string]string, 2)
	for _, m := range rb.VolumeMounts {
		mounts[m.MountPath] = m.Name
	}
	if mounts["/data"] != "data" {
		t.Errorf("repo-bootstrap /data mount volume = %q, want data", mounts["/data"])
	}
	if mounts["/workspace"] != "workspace" {
		t.Errorf("repo-bootstrap /workspace mount volume = %q, want workspace", mounts["/workspace"])
	}
}

// TestK8sBackend_SpawnWizard_RejectsEmptyRepoFields pins the fail-fast
// guard on buildWizardPod (spi-fopwn): an empty RepoURL, RepoBranch, or
// RepoPrefix must surface as a clear error at Spawn time rather than
// producing a pod that dies later in ResolveRepo.
func TestK8sBackend_SpawnWizard_RejectsEmptyRepoFields(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*SpawnConfig)
		want string
	}{
		{"empty RepoURL", func(c *SpawnConfig) { c.RepoURL = "" }, "RepoURL is required"},
		{"empty RepoBranch", func(c *SpawnConfig) { c.RepoBranch = "" }, "RepoBranch is required"},
		{"empty RepoPrefix", func(c *SpawnConfig) { c.RepoPrefix = "" }, "RepoPrefix is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, _ := newTestBackend()

			cfg := wizardSpawnConfig()
			tc.mut(&cfg)

			_, err := b.Spawn(cfg)
			if err == nil {
				t.Fatalf("Spawn(%s): want error, got nil", tc.name)
			}
			if !contains(err.Error(), tc.want) {
				t.Errorf("Spawn(%s) error = %q, want containing %q", tc.name, err.Error(), tc.want)
			}
		})
	}
}

func TestK8sBackend_SpawnWizard_Env(t *testing.T) {
	b, client := newTestBackend()

	if _, err := b.Spawn(wizardSpawnConfig()); err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	pod := spawnedPod(t, client)

	envMap := make(map[string]corev1.EnvVar, len(pod.Spec.Containers[0].Env))
	for _, e := range pod.Spec.Containers[0].Env {
		envMap[e.Name] = e
	}

	// Wizard-specific literal values — DOLT_DATA_DIR, SPIRE_CONFIG_DIR,
	// and the three SPIRE_REPO_* vars must be set on the main container
	// so resolveBeadsDir() / ResolveRepo find the store the init
	// containers staged into /data and /workspace.
	wantLiteral := map[string]string{
		"DOLT_DATA_DIR":     "/data",
		"SPIRE_CONFIG_DIR":  "/data/spire-config",
		"SPIRE_REPO_URL":    "https://github.com/example/repo.git",
		"SPIRE_REPO_BRANCH": "main",
		"SPIRE_REPO_PREFIX": "spi",
	}
	for k, want := range wantLiteral {
		got, ok := envMap[k]
		if !ok {
			t.Errorf("missing env var %s", k)
			continue
		}
		if got.Value != want {
			t.Errorf("env %s = %q, want %q", k, got.Value, want)
		}
	}

	// Preserved keys — existence check only (values are pinned by the
	// existing TestK8sBackend_Spawn_* tests; here we verify the wizard
	// branch did not drop them relative to the executor branch).
	// SPIRE_AGENT_NAME is mentioned by the change spec but is not injected
	// by (*K8sBackend).buildEnvVars today — only the operator path sets
	// it — so it is deliberately absent from this existence check.
	for _, k := range []string{
		"SPIRE_BEAD_ID",
		"SPIRE_TOWER",
		"SPIRE_ROLE",
		"BEADS_DOLT_SERVER_HOST",
		"BEADS_DOLT_SERVER_PORT",
	} {
		if _, ok := envMap[k]; !ok {
			t.Errorf("missing env var %s", k)
		}
	}

	// ANTHROPIC_API_KEY must be wired through Secret, not a literal value.
	apiKey, ok := envMap["ANTHROPIC_API_KEY"]
	if !ok {
		t.Fatal("missing ANTHROPIC_API_KEY env var")
	}
	if apiKey.ValueFrom == nil || apiKey.ValueFrom.SecretKeyRef == nil {
		t.Error("ANTHROPIC_API_KEY should use ValueFrom.SecretKeyRef, not a literal Value")
	}
}

func TestK8sBackend_SpawnWizard_Resources(t *testing.T) {
	b, client := newTestBackend()

	if _, err := b.Spawn(wizardSpawnConfig()); err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	pod := spawnedPod(t, client)
	res := pod.Spec.Containers[0].Resources

	checkQty(t, "Requests[memory]", res.Requests[corev1.ResourceMemory], "1Gi")
	checkQty(t, "Requests[cpu]", res.Requests[corev1.ResourceCPU], "250m")
	checkQty(t, "Limits[memory]", res.Limits[corev1.ResourceMemory], "2Gi")
	checkQty(t, "Limits[cpu]", res.Limits[corev1.ResourceCPU], "1000m")
}

func TestK8sBackend_SpawnWizard_ResourceOverride(t *testing.T) {
	// t.Setenv auto-restores on cleanup; no manual defer needed.
	t.Setenv("SPIRE_WIZARD_MEMORY_LIMIT", "4Gi")
	t.Setenv("SPIRE_WIZARD_CPU_LIMIT", "2000m")

	b, client := newTestBackend()

	if _, err := b.Spawn(wizardSpawnConfig()); err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	pod := spawnedPod(t, client)
	res := pod.Spec.Containers[0].Resources

	checkQty(t, "Limits[memory]", res.Limits[corev1.ResourceMemory], "4Gi")
	checkQty(t, "Limits[cpu]", res.Limits[corev1.ResourceCPU], "2000m")
}

func TestK8sBackend_SpawnWizard_Command(t *testing.T) {
	b, client := newTestBackend()

	cfg := wizardSpawnConfig()
	if _, err := b.Spawn(cfg); err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	pod := spawnedPod(t, client)
	got := pod.Spec.Containers[0].Command
	want := []string{"spire", "execute", cfg.BeadID, "--name", cfg.Name}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("main container Command = %v, want %v", got, want)
	}
}

func TestK8sBackend_SpawnWizard_RestartPolicyNever(t *testing.T) {
	b, client := newTestBackend()

	if _, err := b.Spawn(wizardSpawnConfig()); err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	pod := spawnedPod(t, client)
	if pod.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("RestartPolicy = %q, want %q", pod.Spec.RestartPolicy, corev1.RestartPolicyNever)
	}
}

// spawnedPod fetches the single pod created by Spawn from the fake
// clientset. Fails the test if zero or multiple pods are present.
// Additive helper for the wizard spec tests — existing tests still use
// their inline list pattern.
func spawnedPod(t *testing.T, client *fake.Clientset) *corev1.Pod {
	t.Helper()
	pods, err := client.CoreV1().Pods(testNamespace).List(
		context.Background(), metav1.ListOptions{},
	)
	if err != nil {
		t.Fatalf("list pods: %v", err)
	}
	if len(pods.Items) != 1 {
		t.Fatalf("expected 1 pod, got %d", len(pods.Items))
	}
	return &pods.Items[0]
}

// containsFlag reports whether cmd contains any argument that starts
// with the given prefix. Used to assert the tower-attach init container
// wire up without pinning the (possibly empty) attach-cluster values.
func containsFlag(cmd []string, prefix string) bool {
	for _, arg := range cmd {
		if strings.HasPrefix(arg, prefix) {
			return true
		}
	}
	return false
}

// checkQty fails the test if q is not equal to the quantity parsed
// from want. Uses resource.Quantity.Equal so we compare canonical
// values rather than == on the struct.
func checkQty(t *testing.T, label string, q resource.Quantity, want string) {
	t.Helper()
	w := resource.MustParse(want)
	if !q.Equal(w) {
		t.Errorf("%s = %s, want %s", label, q.String(), w.String())
	}
}

// --- buildRolePod Role × Kind matrix ---------------------------------------
//
// The tests below pin the shared-pod-builder routing (spi-wqax9 §4): every
// Role × WorkspaceKind combination lands in exactly one of the three pod
// shapes (flat, wizard, substrate), and the emitted pod carries the
// canonical env / label / annotation set.

// canonicalIdentity returns a RepoIdentity populated with the fields
// buildRolePod requires for substrate shapes. Shared across the matrix
// tests so field additions surface in one place.
func canonicalIdentity() runtime.RepoIdentity {
	return runtime.RepoIdentity{
		TowerName:  "test-tower",
		Prefix:     "spi",
		RepoURL:    "https://github.com/example/repo.git",
		BaseBranch: "main",
	}
}

// borrowedWorkspace returns a workspace handle keyed at a borrowed
// worktree kind. Shared across matrix tests for consistency.
func borrowedWorkspace() *runtime.WorkspaceHandle {
	return &runtime.WorkspaceHandle{
		Name:       "spi-abc-impl",
		Kind:       runtime.WorkspaceKindBorrowedWorktree,
		Branch:     "spi-abc/implement",
		BaseBranch: "main",
		Path:       "/workspace/spi",
		Origin:     runtime.WorkspaceOriginOriginClone,
		Borrowed:   true,
	}
}

// TestK8sBackend_BuildRolePod_RoleKindMatrix pins the shape routing for
// every Role × Kind combination we support today.
func TestK8sBackend_BuildRolePod_RoleKindMatrix(t *testing.T) {
	cases := []struct {
		name      string
		role      SpawnRole
		workspace *runtime.WorkspaceHandle
		// Expected number of init containers (0 = flat, 2 = wizard/substrate).
		wantInitContainers int
		wantVolumes        int
	}{
		{
			name:               "wizard always gets substrate init containers",
			role:               RoleWizard,
			workspace:          nil,
			wantInitContainers: 2,
			wantVolumes:        2,
		},
		{
			name: "apprentice with borrowed_worktree → substrate",
			role: RoleApprentice,
			workspace: &runtime.WorkspaceHandle{
				Kind: runtime.WorkspaceKindBorrowedWorktree,
				Path: "/workspace/spi",
			},
			wantInitContainers: 2,
			wantVolumes:        2,
		},
		{
			name: "apprentice with owned_worktree → substrate",
			role: RoleApprentice,
			workspace: &runtime.WorkspaceHandle{
				Kind: runtime.WorkspaceKindOwnedWorktree,
				Path: "/workspace/spi",
			},
			wantInitContainers: 2,
			wantVolumes:        2,
		},
		{
			name: "sage with borrowed_worktree → substrate",
			role: RoleSage,
			workspace: &runtime.WorkspaceHandle{
				Kind: runtime.WorkspaceKindBorrowedWorktree,
				Path: "/workspace/spi",
			},
			wantInitContainers: 2,
			wantVolumes:        2,
		},
		{
			name: "apprentice with kind=repo → flat (no init containers)",
			role: RoleApprentice,
			workspace: &runtime.WorkspaceHandle{
				Kind: runtime.WorkspaceKindRepo,
				Path: "/workspace/spi",
			},
			wantInitContainers: 0,
			wantVolumes:        0,
		},
		{
			name:               "apprentice without workspace → flat (gate-OFF baseline)",
			role:               RoleApprentice,
			workspace:          nil,
			wantInitContainers: 0,
			wantVolumes:        0,
		},
		{
			name:               "sage without workspace → flat",
			role:               RoleSage,
			workspace:          nil,
			wantInitContainers: 0,
			wantVolumes:        0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, _ := newTestBackend()

			cfg := SpawnConfig{
				Name:      "test-" + string(tc.role),
				BeadID:    "spi-matrix",
				Role:      tc.role,
				Identity:  canonicalIdentity(),
				Workspace: tc.workspace,
			}

			pod, err := b.buildRolePod(cfg)
			if err != nil {
				t.Fatalf("buildRolePod: %v", err)
			}

			if got := len(pod.Spec.InitContainers); got != tc.wantInitContainers {
				t.Errorf("InitContainers count = %d, want %d", got, tc.wantInitContainers)
			}
			if got := len(pod.Spec.Volumes); got != tc.wantVolumes {
				t.Errorf("Volumes count = %d, want %d", got, tc.wantVolumes)
			}
		})
	}
}

// TestK8sBackend_BuildRolePod_CanonicalLabels pins the spire.io/* label
// vocabulary emitted on every pod (spi-wqax9 §4, table in contract doc).
// Low-cardinality fields go on labels; attempt_id / run_id go on
// annotations.
func TestK8sBackend_BuildRolePod_CanonicalLabels(t *testing.T) {
	b, _ := newTestBackend()

	cfg := SpawnConfig{
		Name:      "apprentice-spi-abc-0",
		BeadID:    "spi-abc",
		Role:      RoleApprentice,
		AttemptID: "spi-att-legacy",
		Identity:  canonicalIdentity(),
		Workspace: borrowedWorkspace(),
		Run: runtime.RunContext{
			TowerName:       "test-tower",
			Prefix:          "spi",
			BeadID:          "spi-abc",
			AttemptID:       "spi-att-new",
			RunID:           "run-xyz",
			Role:            RoleApprentice,
			FormulaStep:     "implement",
			Backend:         "k8s",
			WorkspaceKind:   runtime.WorkspaceKindBorrowedWorktree,
			WorkspaceName:   "spi-abc-impl",
			WorkspaceOrigin: runtime.WorkspaceOriginOriginClone,
			HandoffMode:     runtime.HandoffBorrowed,
		},
	}

	pod, err := b.buildRolePod(cfg)
	if err != nil {
		t.Fatalf("buildRolePod: %v", err)
	}

	wantLabels := map[string]string{
		LabelBackend:         "k8s",
		LabelTower:           "test-tower",
		LabelPrefix:          "spi",
		LabelBead:            "spi-abc",
		LabelRole:            string(RoleApprentice),
		LabelFormulaStep:     "implement",
		LabelWorkspaceKind:   string(runtime.WorkspaceKindBorrowedWorktree),
		LabelWorkspaceName:   "spi-abc-impl",
		LabelWorkspaceOrigin: string(runtime.WorkspaceOriginOriginClone),
		LabelHandoffMode:     string(runtime.HandoffBorrowed),
	}
	for k, want := range wantLabels {
		if got := pod.Labels[k]; got != want {
			t.Errorf("label %s = %q, want %q", k, got, want)
		}
	}

	// Legacy labels preserved byte-for-byte so discovery & network
	// policies keep working.
	if pod.Labels["spire.agent"] != "true" {
		t.Errorf("legacy spire.agent label = %q, want true", pod.Labels["spire.agent"])
	}
	if pod.Labels["spire.bead"] != "spi-abc" {
		t.Errorf("legacy spire.bead = %q, want spi-abc", pod.Labels["spire.bead"])
	}

	// Attempt and run IDs go on annotations, not labels, so metric
	// cardinality stays bounded.
	if got := pod.Annotations[AnnotationAttemptID]; got != "spi-att-new" {
		t.Errorf("annotation %s = %q, want spi-att-new (from cfg.Run)", AnnotationAttemptID, got)
	}
	if got := pod.Annotations[AnnotationRunID]; got != "run-xyz" {
		t.Errorf("annotation %s = %q, want run-xyz", AnnotationRunID, got)
	}
	// Attempt/run must NOT appear as labels.
	for _, k := range []string{AnnotationAttemptID, AnnotationRunID} {
		if _, ok := pod.Labels[k]; ok {
			t.Errorf("high-cardinality key %s leaked into Labels", k)
		}
	}
}

// TestK8sBackend_BuildRolePod_SubstrateRequiresWorkspace pins the
// fail-fast guard on substrate shape: a non-wizard role arriving with a
// non-repo workspace kind but cfg.Workspace == nil is a contract bug.
// buildRolePod cannot encounter this today (selectPodShape routes
// nil-workspace to flat), but a direct call to buildSubstratePod should
// still surface ErrWorkspaceRequired so future refactors cannot silently
// drop the check.
func TestK8sBackend_BuildRolePod_SubstrateRequiresWorkspace(t *testing.T) {
	b, _ := newTestBackend()

	cfg := SpawnConfig{
		Name:     "apprentice-no-ws",
		BeadID:   "spi-nows",
		Role:     RoleApprentice,
		Identity: canonicalIdentity(),
		// Workspace: nil
	}
	_, err := b.buildSubstratePod(cfg, cfg.Identity, "test-pod", []string{"apprentice", "run"}, nil)
	if err == nil {
		t.Fatal("buildSubstratePod: want error, got nil")
	}
	if !errors.Is(err, ErrWorkspaceRequired) {
		t.Errorf("error = %v, want errors.Is(ErrWorkspaceRequired)", err)
	}
}

// TestK8sBackend_BuildRolePod_SubstrateRequiresIdentity pins the identity
// fail-fast: a substrate pod without a populated Identity.RepoURL /
// BaseBranch / Prefix is unbootable (the repo-bootstrap init container
// clones from those values).
func TestK8sBackend_BuildRolePod_SubstrateRequiresIdentity(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*runtime.RepoIdentity)
	}{
		{"empty RepoURL", func(i *runtime.RepoIdentity) { i.RepoURL = "" }},
		{"empty BaseBranch", func(i *runtime.RepoIdentity) { i.BaseBranch = "" }},
		{"empty Prefix", func(i *runtime.RepoIdentity) { i.Prefix = "" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, _ := newTestBackend()
			ident := canonicalIdentity()
			tc.mut(&ident)

			cfg := SpawnConfig{
				Name:      "apprentice-missing-id",
				BeadID:    "spi-missid",
				Role:      RoleApprentice,
				Identity:  ident,
				Workspace: borrowedWorkspace(),
			}
			_, err := b.buildRolePod(cfg)
			if err == nil {
				t.Fatalf("buildRolePod(%s): want error, got nil", tc.name)
			}
			if !errors.Is(err, ErrIdentityRequired) {
				t.Errorf("error = %v, want errors.Is(ErrIdentityRequired)", err)
			}
		})
	}
}

// TestK8sBackend_BuildRolePod_ApprenticeSubstrateEnv verifies the
// apprentice substrate pod carries the canonical env vocabulary so the
// init containers and main container all see the same values.
func TestK8sBackend_BuildRolePod_ApprenticeSubstrateEnv(t *testing.T) {
	b, _ := newTestBackend()

	cfg := SpawnConfig{
		Name:      "apprentice-spi-abc-0",
		BeadID:    "spi-abc",
		Role:      RoleApprentice,
		Identity:  canonicalIdentity(),
		Workspace: borrowedWorkspace(),
	}
	pod, err := b.buildRolePod(cfg)
	if err != nil {
		t.Fatalf("buildRolePod: %v", err)
	}

	if len(pod.Spec.Containers) != 1 {
		t.Fatalf("Containers = %d, want 1", len(pod.Spec.Containers))
	}
	envMap := make(map[string]corev1.EnvVar, len(pod.Spec.Containers[0].Env))
	for _, e := range pod.Spec.Containers[0].Env {
		envMap[e.Name] = e
	}

	wantLiteral := map[string]string{
		"DOLT_DATA_DIR":     "/data",
		"SPIRE_CONFIG_DIR":  "/data/spire-config",
		"SPIRE_REPO_URL":    "https://github.com/example/repo.git",
		"SPIRE_REPO_BRANCH": "main",
		"SPIRE_REPO_PREFIX": "spi",
		"BEADS_DATABASE":    "test-tower",
		"BEADS_PREFIX":      "spi",
	}
	for k, want := range wantLiteral {
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

// TestK8sBackend_BuildRolePod_GateOffPreservesLegacy verifies that the
// gate-OFF baseline (no cfg.Workspace) still produces a flat pod —
// byte-for-byte identical to the pre-shared-builder output. This is the
// safety net: migrating dispatch sites to populate Workspace is a
// later task, and until then the legacy behavior MUST hold.
func TestK8sBackend_BuildRolePod_GateOffPreservesLegacy(t *testing.T) {
	b, _ := newTestBackend()

	cfg := SpawnConfig{
		Name:   "apprentice-legacy",
		BeadID: "spi-legacy",
		Role:   RoleApprentice,
		Tower:  "legacy-tower", // legacy field, not Identity
		// No Identity, no Workspace — pre-migration dispatch site shape.
	}
	pod, err := b.buildRolePod(cfg)
	if err != nil {
		t.Fatalf("buildRolePod: %v", err)
	}
	if len(pod.Spec.InitContainers) != 0 {
		t.Errorf("gate-OFF apprentice pod got %d init containers, want 0", len(pod.Spec.InitContainers))
	}
	if len(pod.Spec.Volumes) != 0 {
		t.Errorf("gate-OFF apprentice pod got %d volumes, want 0", len(pod.Spec.Volumes))
	}
	if pod.Labels["spire.tower"] != "legacy-tower" {
		t.Errorf("legacy spire.tower label = %q, want legacy-tower", pod.Labels["spire.tower"])
	}
}

// TestK8sBackend_BuildRolePod_SharedPVC_BorrowedWorkspace verifies the
// shared-workspace gate: when SPIRE_K8S_SHARED_WORKSPACE=1 and cfg.Workspace.Kind
// is borrowed_worktree, the child apprentice pod mounts the parent
// wizard's PVC rather than an emptyDir.
func TestK8sBackend_BuildRolePod_SharedPVC_BorrowedWorkspace(t *testing.T) {
	t.Setenv("SPIRE_K8S_SHARED_WORKSPACE", "1")

	b, client := newTestBackend()

	// Seed a PVC owned by the parent wizard pod "wizard-spi-abc".
	_, err := client.CoreV1().PersistentVolumeClaims(testNamespace).Create(
		context.Background(),
		&corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "wizard-spi-abc-workspace",
				Namespace: testNamespace,
				Labels:    map[string]string{LabelOwningWizardPod: "wizard-spi-abc"},
			},
		},
		metav1.CreateOptions{},
	)
	if err != nil {
		t.Fatalf("seed PVC: %v", err)
	}

	cfg := SpawnConfig{
		Name:      "apprentice-spi-abc-0",
		BeadID:    "spi-abc",
		Role:      RoleApprentice,
		Identity:  canonicalIdentity(),
		Workspace: borrowedWorkspace(),
	}

	pod, err := b.buildRolePod(cfg)
	if err != nil {
		t.Fatalf("buildRolePod: %v", err)
	}

	var workspaceVol *corev1.Volume
	for i := range pod.Spec.Volumes {
		if pod.Spec.Volumes[i].Name == "workspace" {
			workspaceVol = &pod.Spec.Volumes[i]
			break
		}
	}
	if workspaceVol == nil {
		t.Fatalf("pod missing 'workspace' volume; volumes = %+v", pod.Spec.Volumes)
	}
	if workspaceVol.PersistentVolumeClaim == nil {
		t.Fatalf("workspace volume is not a PVC (emptyDir=%v); gate should have routed to PVC",
			workspaceVol.EmptyDir != nil)
	}
	if got := workspaceVol.PersistentVolumeClaim.ClaimName; got != "wizard-spi-abc-workspace" {
		t.Errorf("ClaimName = %q, want wizard-spi-abc-workspace", got)
	}
}

// TestK8sBackend_BuildRolePod_SharedPVCMissing verifies a missing PVC
// surfaces as ErrSharedWorkspacePVCNotFound rather than silently falling
// back to an emptyDir (which would mask a misconfigured gate).
func TestK8sBackend_BuildRolePod_SharedPVCMissing(t *testing.T) {
	t.Setenv("SPIRE_K8S_SHARED_WORKSPACE", "1")

	b, _ := newTestBackend()

	cfg := SpawnConfig{
		Name:      "apprentice-spi-xyz-0",
		BeadID:    "spi-xyz",
		Role:      RoleApprentice,
		Identity:  canonicalIdentity(),
		Workspace: borrowedWorkspace(),
	}

	_, err := b.buildRolePod(cfg)
	if err == nil {
		t.Fatal("want error for missing PVC, got nil")
	}
	if !errors.Is(err, ErrSharedWorkspacePVCNotFound) {
		t.Errorf("error = %v, want errors.Is(ErrSharedWorkspacePVCNotFound)", err)
	}
}

// TestK8sBackend_BuildRolePod_SharedPVCGateOff verifies the default path:
// gate off → emptyDir regardless of Workspace.Kind.
func TestK8sBackend_BuildRolePod_SharedPVCGateOff(t *testing.T) {
	// Explicitly unset so a dev machine's env doesn't leak in.
	t.Setenv("SPIRE_K8S_SHARED_WORKSPACE", "")

	b, _ := newTestBackend()

	cfg := SpawnConfig{
		Name:      "apprentice-spi-abc-0",
		BeadID:    "spi-abc",
		Role:      RoleApprentice,
		Identity:  canonicalIdentity(),
		Workspace: borrowedWorkspace(),
	}
	pod, err := b.buildRolePod(cfg)
	if err != nil {
		t.Fatalf("buildRolePod: %v", err)
	}
	var workspaceVol *corev1.Volume
	for i := range pod.Spec.Volumes {
		if pod.Spec.Volumes[i].Name == "workspace" {
			workspaceVol = &pod.Spec.Volumes[i]
			break
		}
	}
	if workspaceVol == nil {
		t.Fatalf("pod missing 'workspace' volume")
	}
	if workspaceVol.EmptyDir == nil {
		t.Errorf("gate-OFF workspace should be emptyDir, got %+v", workspaceVol)
	}
	if workspaceVol.PersistentVolumeClaim != nil {
		t.Error("gate-OFF workspace unexpectedly routed to PVC")
	}
}

// TestK8sBackend_BuildRolePod_LegacyFieldFallback verifies dispatch sites
// that have not yet been migrated to cfg.Identity (set Tower/RepoURL/
// RepoBranch/RepoPrefix on SpawnConfig directly) still produce a valid
// wizard pod. resolveIdentity fills in the identity from the legacy
// fields.
func TestK8sBackend_BuildRolePod_LegacyFieldFallback(t *testing.T) {
	b, _ := newTestBackend()

	cfg := SpawnConfig{
		Name:       "wizard-legacy",
		BeadID:     "spi-legacy",
		Role:       RoleWizard,
		Tower:      "legacy-tower",
		RepoURL:    "https://github.com/example/repo.git",
		RepoBranch: "main",
		RepoPrefix: "spi",
	}
	pod, err := b.buildRolePod(cfg)
	if err != nil {
		t.Fatalf("buildRolePod(legacy fields): %v", err)
	}
	if pod.Labels[LabelTower] != "legacy-tower" {
		t.Errorf("label %s = %q, want legacy-tower (from resolveIdentity fallback)", LabelTower, pod.Labels[LabelTower])
	}
	if pod.Labels[LabelPrefix] != "spi" {
		t.Errorf("label %s = %q, want spi", LabelPrefix, pod.Labels[LabelPrefix])
	}
}
