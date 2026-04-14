//go:build cluster

package smoke

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	testNamespace = "spire-test"
	doltPort      = 3307
	clickhouseHTTPPort = 8123
)

// TestClusterBootstrap is the main end-to-end integration test that validates the
// full cluster lifecycle: Helm install -> file a bead -> agent pod spawns ->
// work completes -> bead closes.
func TestClusterBootstrap(t *testing.T) {
	if !clusterAvailable() {
		t.Skip("no k8s cluster available")
	}

	provider := detectClusterProvider()
	t.Logf("cluster provider: %s", provider)

	client := getK8sClient(t)

	// ── 1. Helm install ──────────────────────────────────────────────────
	values := map[string]string{
		"tower.name":                "smoke-test",
		"image.steward.repository":  "spire-steward",
		"image.steward.tag":         "dev",
		"image.agent.repository":    "spire-agent",
		"image.agent.tag":           "dev",
	}
	if key := os.Getenv("ANTHROPIC_API_KEY"); key != "" {
		values["secrets.anthropicKey"] = key
	}
	if tok := os.Getenv("GITHUB_TOKEN"); tok != "" {
		values["secrets.githubToken"] = tok
	}

	t.Cleanup(func() {
		t.Log("tearing down: helm uninstall + namespace delete")
		if err := helmUninstall(t, testNamespace); err != nil {
			t.Logf("teardown warning: %v", err)
		}
	})

	if err := helmInstall(t, testNamespace, values); err != nil {
		t.Fatalf("helm install failed: %v", err)
	}

	// ── 2. Verify infrastructure pods ────────────────────────────────────
	t.Run("InfraPodsReady", func(t *testing.T) {
		if err := waitForAllPodsReady(t, client, testNamespace, 5*time.Minute); err != nil {
			t.Fatalf("infrastructure pods not ready: %v", err)
		}

		// Verify specific pods exist and are running.
		for _, tc := range []struct {
			name     string
			selector string
		}{
			{"dolt", "app=spire-dolt"},
			{"clickhouse", "app=spire-clickhouse"},
			{"steward", "app=spire-steward"},
		} {
			pod, err := podByLabel(t, client, testNamespace, tc.selector)
			if err != nil {
				t.Errorf("%s pod not found: %v", tc.name, err)
				continue
			}
			if pod.Status.Phase != corev1.PodRunning {
				t.Errorf("%s pod phase = %s, want Running", tc.name, pod.Status.Phase)
			}
		}
	})

	// ── 3. Verify service endpoints ──────────────────────────────────────
	t.Run("ServiceEndpoints", func(t *testing.T) {
		for _, svc := range []string{"spire-dolt", "spire-clickhouse", "spire-steward"} {
			if !serviceHasEndpoints(t, client, testNamespace, svc) {
				t.Errorf("service %s has no endpoints", svc)
			}
		}
	})

	// ── 4. Verify Dolt connectivity ──────────────────────────────────────
	localDoltPort := freePort(t)
	cancelDolt := portForward(t, testNamespace, "spire-dolt", localDoltPort, doltPort)
	t.Cleanup(cancelDolt)

	t.Run("DoltConnectivity", func(t *testing.T) {
		dsn := fmt.Sprintf("root:@tcp(127.0.0.1:%d)/", localDoltPort)
		rows, err := queryDolt(t, dsn, "SELECT 1")
		if err != nil {
			t.Fatalf("dolt SELECT 1 failed: %v", err)
		}
		rows.Close()
		t.Log("dolt connectivity OK")
	})

	// ── 5. Verify ClickHouse connectivity ────────────────────────────────
	localCHPort := freePort(t)
	cancelCH := portForward(t, testNamespace, "spire-clickhouse", localCHPort, clickhouseHTTPPort)
	t.Cleanup(cancelCH)

	t.Run("ClickHouseConnectivity", func(t *testing.T) {
		dsn := fmt.Sprintf("clickhouse://127.0.0.1:%d/default", localCHPort)
		rows, err := queryClickHouse(t, dsn, "SELECT 1")
		if err != nil {
			t.Fatalf("clickhouse SELECT 1 failed: %v", err)
		}
		rows.Close()
		t.Log("clickhouse connectivity OK")
	})

	// ── 6. File a bead ───────────────────────────────────────────────────
	var beadID string
	t.Run("FileBead", func(t *testing.T) {
		beadID = fileBead(t, "127.0.0.1", localDoltPort, "Smoke test task")
		t.Logf("filed bead: %s", beadID)
	})

	if beadID == "" {
		t.Fatal("no bead ID captured, cannot continue")
	}

	// ── 7. Wait for agent pod ────────────────────────────────────────────
	var agentPod *corev1.Pod
	t.Run("AgentPodSpawns", func(t *testing.T) {
		selector := fmt.Sprintf("spire.bead=%s", beadID)
		var err error
		agentPod, err = waitForPod(t, client, testNamespace, selector, 3*time.Minute)
		if err != nil {
			t.Fatalf("agent pod not spawned: %v", err)
		}

		// Verify labels.
		labels := agentPod.Labels
		if labels["spire.bead"] != beadID {
			t.Errorf("spire.bead label = %q, want %q", labels["spire.bead"], beadID)
		}
		if labels["spire.agent"] == "" {
			t.Error("spire.agent label is missing")
		}
		if labels["spire.role"] == "" {
			t.Error("spire.role label is missing")
		}

		// Verify environment variables exist on the first container.
		if len(agentPod.Spec.Containers) == 0 {
			t.Fatal("agent pod has no containers")
		}
		envMap := envToMap(agentPod.Spec.Containers[0].Env)
		for _, key := range []string{"SPIRE_TOWER", "OTEL_EXPORTER_OTLP_ENDPOINT", "BEADS_DOLT_SERVER_HOST"} {
			if _, ok := envMap[key]; !ok {
				t.Errorf("expected env var %s not found on agent pod", key)
			}
		}

		// Verify resource limits are set.
		limits := agentPod.Spec.Containers[0].Resources.Limits
		if limits.Memory().IsZero() {
			t.Error("agent pod has no memory limit")
		}

		// Verify secret refs.
		hasSecretRef := false
		for _, envFrom := range agentPod.Spec.Containers[0].EnvFrom {
			if envFrom.SecretRef != nil && envFrom.SecretRef.Name == "spire-credentials" {
				hasSecretRef = true
				break
			}
		}
		for _, env := range agentPod.Spec.Containers[0].Env {
			if env.ValueFrom != nil && env.ValueFrom.SecretKeyRef != nil &&
				env.ValueFrom.SecretKeyRef.Name == "spire-credentials" {
				hasSecretRef = true
				break
			}
		}
		if !hasSecretRef {
			t.Error("agent pod has no reference to spire-credentials secret")
		}

		t.Logf("agent pod %s spawned with role=%s", agentPod.Name, labels["spire.role"])
	})

	// ── 8. Wait for completion ───────────────────────────────────────────
	t.Run("AgentCompletes", func(t *testing.T) {
		if agentPod == nil {
			t.Skip("no agent pod to wait for")
		}
		err := waitForPodPhase(t, client, testNamespace, agentPod.Name, corev1.PodSucceeded, 10*time.Minute)
		if err != nil {
			t.Fatalf("agent pod did not succeed: %v", err)
		}
		t.Log("agent pod completed successfully")
	})

	// ── 9. Verify bead closed ────────────────────────────────────────────
	t.Run("BeadClosed", func(t *testing.T) {
		dsn := fmt.Sprintf("root:@tcp(127.0.0.1:%d)/spire", localDoltPort)
		rows, err := queryDolt(t, dsn, fmt.Sprintf(
			"SELECT status FROM beads WHERE id = '%s'", beadID))
		if err != nil {
			t.Fatalf("query bead status: %v", err)
		}
		defer rows.Close()

		if !rows.Next() {
			t.Fatal("bead not found in dolt")
		}
		var status string
		if err := rows.Scan(&status); err != nil {
			t.Fatalf("scan bead status: %v", err)
		}
		if status != "closed" && status != "done" {
			t.Errorf("bead status = %q, want closed or done", status)
		}
		t.Logf("bead %s status: %s", beadID, status)
	})

	// ── 10. Verify OTel events in ClickHouse ─────────────────────────────
	t.Run("OTelEvents", func(t *testing.T) {
		dsn := fmt.Sprintf("clickhouse://127.0.0.1:%d/spire", localCHPort)
		rows, err := queryClickHouse(t, dsn, fmt.Sprintf(
			"SELECT count() FROM tool_events WHERE bead_id = '%s'", beadID))
		if err != nil {
			t.Logf("query tool_events: %v (ClickHouse pipeline may not be wired yet)", err)
			t.Skip("ClickHouse OTel pipeline not yet active")
		}
		defer rows.Close()

		if !rows.Next() {
			t.Skip("no rows returned from tool_events count")
		}
		var count int64
		if err := rows.Scan(&count); err != nil {
			t.Fatalf("scan event count: %v", err)
		}
		if count == 0 {
			t.Log("warning: no OTel events recorded for bead (pipeline may not be active)")
		} else {
			t.Logf("OTel events for bead %s: %d", beadID, count)
		}
	})

	// ── 11. Verify graph state cleanup in Dolt ───────────────────────────
	t.Run("GraphStateCleanup", func(t *testing.T) {
		dsn := fmt.Sprintf("root:@tcp(127.0.0.1:%d)/spire", localDoltPort)
		rows, err := queryDolt(t, dsn, fmt.Sprintf(
			"SELECT count(*) FROM graph_state WHERE bead_id = '%s'", beadID))
		if err != nil {
			t.Logf("query graph_state: %v (table may not exist yet)", err)
			t.Skip("graph_state table not available")
		}
		defer rows.Close()

		if !rows.Next() {
			t.Skip("no rows returned")
		}
		var count int64
		if err := rows.Scan(&count); err != nil {
			t.Fatalf("scan graph_state count: %v", err)
		}
		if count != 0 {
			t.Errorf("graph_state has %d rows for bead %s after completion, want 0 (cleanup failed)", count, beadID)
		} else {
			t.Log("graph state cleaned up after completion")
		}
	})

	// ── 12. Verify network policies (best-effort) ────────────────────────
	t.Run("NetworkPolicyEnforced", func(t *testing.T) {
		if agentPod == nil {
			t.Skip("no agent pod to test network policy")
		}

		// Try to reach ClickHouse from the agent pod (should be blocked).
		// This is best-effort — network policy enforcement depends on the CNI.
		out, err := execInPod(t, testNamespace, agentPod.Name,
			[]string{"sh", "-c", "timeout 3 nc -z spire-clickhouse 8123 2>&1 || echo BLOCKED"})
		if err != nil {
			t.Logf("exec in agent pod failed (pod may have exited): %v", err)
			t.Skip("cannot exec in agent pod")
		}
		if out == "" || out == "BLOCKED\n" {
			t.Log("agent pod correctly cannot reach ClickHouse directly")
		} else {
			t.Logf("warning: agent pod may be able to reach ClickHouse: %s", out)
		}
	})
}

// TestClusterNetworkPolicies validates network policy enforcement by creating
// test pods with and without the spire.agent label.
func TestClusterNetworkPolicies(t *testing.T) {
	if !clusterAvailable() {
		t.Skip("no k8s cluster available")
	}

	client := getK8sClient(t)

	// Ensure the namespace exists (from a prior TestClusterBootstrap run or manual setup).
	_, err := client.CoreV1().Namespaces().Get(context.Background(), testNamespace, metav1.GetOptions{})
	if err != nil {
		t.Skipf("namespace %s not found — run TestClusterBootstrap first: %v", testNamespace, err)
	}

	// ── Labeled pod (agent) ──────────────────────────────────────────────
	t.Run("LabeledPodAccess", func(t *testing.T) {
		agentPod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "netpol-test-agent",
				Namespace: testNamespace,
				Labels: map[string]string{
					"spire.agent": "netpol-test",
				},
			},
			Spec: corev1.PodSpec{
				RestartPolicy: corev1.RestartPolicyNever,
				Containers: []corev1.Container{{
					Name:    "probe",
					Image:   "busybox:1.36",
					Command: []string{"sleep", "300"},
				}},
			},
		}

		created, err := client.CoreV1().Pods(testNamespace).Create(
			context.Background(), agentPod, metav1.CreateOptions{})
		if err != nil {
			t.Fatalf("create agent test pod: %v", err)
		}
		t.Cleanup(func() {
			_ = client.CoreV1().Pods(testNamespace).Delete(
				context.Background(), created.Name, metav1.DeleteOptions{})
		})

		// Wait for pod to be running.
		if err := waitForPodPhase(t, client, testNamespace, created.Name, corev1.PodRunning, 2*time.Minute); err != nil {
			t.Fatalf("agent test pod not running: %v", err)
		}

		// Agent pod should reach dolt.
		out, err := execInPod(t, testNamespace, created.Name,
			[]string{"sh", "-c", "nc -z spire-dolt 3307 && echo OK || echo FAIL"})
		if err != nil {
			t.Logf("exec failed: %v", err)
		} else if out != "OK\n" {
			t.Errorf("agent pod cannot reach dolt: %s", out)
		}

		// Agent pod should NOT reach ClickHouse (if network policies enforced).
		out, err = execInPod(t, testNamespace, created.Name,
			[]string{"sh", "-c", "timeout 3 nc -z spire-clickhouse 8123 && echo OK || echo BLOCKED"})
		if err != nil {
			t.Logf("exec failed: %v", err)
		} else if out == "OK\n" {
			t.Log("warning: agent pod can reach ClickHouse — network policies may not be enforced by CNI")
		} else {
			t.Log("agent pod correctly blocked from ClickHouse")
		}
	})

	// ── Unlabeled pod ────────────────────────────────────────────────────
	t.Run("UnlabeledPodAccess", func(t *testing.T) {
		unlabeledPod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "netpol-test-unlabeled",
				Namespace: testNamespace,
			},
			Spec: corev1.PodSpec{
				RestartPolicy: corev1.RestartPolicyNever,
				Containers: []corev1.Container{{
					Name:    "probe",
					Image:   "busybox:1.36",
					Command: []string{"sleep", "300"},
				}},
			},
		}

		created, err := client.CoreV1().Pods(testNamespace).Create(
			context.Background(), unlabeledPod, metav1.CreateOptions{})
		if err != nil {
			t.Fatalf("create unlabeled test pod: %v", err)
		}
		t.Cleanup(func() {
			_ = client.CoreV1().Pods(testNamespace).Delete(
				context.Background(), created.Name, metav1.DeleteOptions{})
		})

		if err := waitForPodPhase(t, client, testNamespace, created.Name, corev1.PodRunning, 2*time.Minute); err != nil {
			t.Fatalf("unlabeled test pod not running: %v", err)
		}

		// Unlabeled pod access to dolt is allowed by the spire-dolt-ingress policy
		// (allows all pods in namespace). The default-deny only blocks ingress from
		// outside the namespace. Egress is unrestricted for non-agent pods since
		// only agent and steward pods have egress policies.

		// Unlabeled pod should NOT reach steward OTLP.
		out, err := execInPod(t, testNamespace, created.Name,
			[]string{"sh", "-c", "timeout 3 nc -z spire-steward 4317 && echo OK || echo BLOCKED"})
		if err != nil {
			t.Logf("exec failed: %v", err)
		} else if out == "OK\n" {
			t.Log("warning: unlabeled pod can reach steward OTLP — network policy may not be enforced")
		} else {
			t.Log("unlabeled pod correctly blocked from steward OTLP")
		}
	})
}

// TestClusterResourceTiers verifies that agent pods spawned for different roles
// get the correct resource limits.
func TestClusterResourceTiers(t *testing.T) {
	if !clusterAvailable() {
		t.Skip("no k8s cluster available")
	}

	client := getK8sClient(t)

	_, err := client.CoreV1().Namespaces().Get(context.Background(), testNamespace, metav1.GetOptions{})
	if err != nil {
		t.Skipf("namespace %s not found — run TestClusterBootstrap first: %v", testNamespace, err)
	}

	tests := []struct {
		role          string
		expectMemory  string
	}{
		{"apprentice", "4Gi"},
		{"sage", "1Gi"},
		{"wizard", "512Mi"},
	}

	for _, tc := range tests {
		tc := tc
		t.Run("Role_"+tc.role, func(t *testing.T) {
			// Look for existing agent pods with this role.
			selector := fmt.Sprintf("spire.role=%s", tc.role)
			pods, err := client.CoreV1().Pods(testNamespace).List(
				context.Background(), metav1.ListOptions{LabelSelector: selector})
			if err != nil {
				t.Fatalf("list pods: %v", err)
			}

			if len(pods.Items) == 0 {
				t.Skipf("no pod with role=%s found — spawn one first", tc.role)
			}

			pod := pods.Items[0]
			if len(pod.Spec.Containers) == 0 {
				t.Fatal("pod has no containers")
			}

			memLimit := pod.Spec.Containers[0].Resources.Limits.Memory()
			expected := resource.MustParse(tc.expectMemory)
			if memLimit.Cmp(expected) != 0 {
				t.Errorf("role %s: memory limit = %s, want %s", tc.role, memLimit.String(), expected.String())
			} else {
				t.Logf("role %s: memory limit = %s (correct)", tc.role, memLimit.String())
			}
		})
	}
}

// envToMap converts a slice of k8s EnvVar to a map for easy lookup.
func envToMap(envs []corev1.EnvVar) map[string]string {
	m := make(map[string]string, len(envs))
	for _, e := range envs {
		m[e.Name] = e.Value
	}
	return m
}
