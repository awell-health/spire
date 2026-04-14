//go:build cluster

package smoke

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	_ "github.com/ClickHouse/clickhouse-go/v2"
	_ "github.com/go-sql-driver/mysql"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

// getK8sClient returns a kubernetes client from KUBECONFIG or the default path.
func getK8sClient(t *testing.T) kubernetes.Interface {
	t.Helper()

	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			t.Fatalf("cannot determine home dir: %v", err)
		}
		kubeconfig = home + "/.kube/config"
	}

	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		t.Fatalf("cannot build k8s config: %v", err)
	}

	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		t.Fatalf("cannot create k8s client: %v", err)
	}
	return client
}

// helmInstall runs helm install with the given values in the specified namespace.
func helmInstall(t *testing.T, namespace string, values map[string]string) error {
	t.Helper()

	args := []string{
		"install", "spire", "chart/spire/",
		"--namespace", namespace,
		"--create-namespace",
		"--wait",
		"--timeout", "5m",
	}
	for k, v := range values {
		args = append(args, "--set", k+"="+v)
	}

	cmd := exec.Command("helm", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// helmUninstall runs helm uninstall and deletes the namespace.
func helmUninstall(t *testing.T, namespace string) error {
	t.Helper()

	cmd := exec.Command("helm", "uninstall", "spire", "--namespace", namespace)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Logf("helm uninstall warning: %v", err)
	}

	del := exec.Command("kubectl", "delete", "namespace", namespace, "--ignore-not-found")
	del.Stdout = os.Stdout
	del.Stderr = os.Stderr
	return del.Run()
}

// waitForPod polls for a pod matching the label selector until it appears or the timeout expires.
func waitForPod(t *testing.T, client kubernetes.Interface, namespace, labelSelector string, timeout time.Duration) (*corev1.Pod, error) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("timeout waiting for pod with selector %q in %s", labelSelector, namespace)
		case <-ticker.C:
			pods, err := client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
				LabelSelector: labelSelector,
			})
			if err != nil {
				t.Logf("list pods: %v", err)
				continue
			}
			if len(pods.Items) > 0 {
				return &pods.Items[0], nil
			}
		}
	}
}

// waitForPodPhase waits for a named pod to reach the given phase.
func waitForPodPhase(t *testing.T, client kubernetes.Interface, namespace, podName string, phase corev1.PodPhase, timeout time.Duration) error {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	watcher, err := client.CoreV1().Pods(namespace).Watch(ctx, metav1.ListOptions{
		FieldSelector: "metadata.name=" + podName,
	})
	if err != nil {
		return fmt.Errorf("watch pod %s: %w", podName, err)
	}
	defer watcher.Stop()

	for event := range watcher.ResultChan() {
		if event.Type == watch.Error {
			return fmt.Errorf("watch error for pod %s", podName)
		}
		pod, ok := event.Object.(*corev1.Pod)
		if !ok {
			continue
		}
		if pod.Status.Phase == phase {
			return nil
		}
		// Also accept terminal phases when waiting for completion.
		if phase == corev1.PodSucceeded && pod.Status.Phase == corev1.PodFailed {
			return fmt.Errorf("pod %s failed instead of succeeding", podName)
		}
	}

	return fmt.Errorf("watch closed before pod %s reached phase %s", podName, phase)
}

// portForward starts a kubectl port-forward to the given service and returns a cancel function.
// It finds a free local port and returns it along with the cancel func.
func portForward(t *testing.T, namespace, svc string, localPort, remotePort int) (cancel func()) {
	t.Helper()

	ctx, cancelFn := context.WithCancel(context.Background())

	cmd := exec.CommandContext(ctx, "kubectl", "port-forward",
		"-n", namespace,
		"svc/"+svc,
		fmt.Sprintf("%d:%d", localPort, remotePort),
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		cancelFn()
		t.Fatalf("port-forward to %s/%s failed to start: %v", namespace, svc, err)
	}

	// Wait briefly for port-forward to establish.
	time.Sleep(2 * time.Second)

	return func() {
		cancelFn()
		_ = cmd.Wait()
	}
}

// freePort finds an available TCP port on localhost.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("freePort: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return port
}

// queryDolt executes a SQL query against a Dolt (MySQL-compatible) server.
func queryDolt(t *testing.T, dsn, query string) (*sql.Rows, error) {
	t.Helper()
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("open dolt connection: %w", err)
	}
	defer db.Close()

	db.SetConnMaxLifetime(30 * time.Second)
	return db.Query(query)
}

// queryClickHouse executes a SQL query against ClickHouse.
func queryClickHouse(t *testing.T, dsn, query string) (*sql.Rows, error) {
	t.Helper()
	db, err := sql.Open("clickhouse", dsn)
	if err != nil {
		return nil, fmt.Errorf("open clickhouse connection: %w", err)
	}
	defer db.Close()

	db.SetConnMaxLifetime(30 * time.Second)
	return db.Query(query)
}

// fileBead creates a bead via the bd CLI with BEADS_DOLT_SERVER_HOST pointing at the given DSN host:port.
// Returns the bead ID.
func fileBead(t *testing.T, doltHost string, doltPort int, title string) string {
	t.Helper()

	cmd := exec.Command("bd", "create", title, "-p", "2", "-t", "task")
	cmd.Env = append(os.Environ(),
		fmt.Sprintf("BEADS_DOLT_SERVER_HOST=%s", doltHost),
		fmt.Sprintf("BEADS_DOLT_SERVER_PORT=%d", doltPort),
	)

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("bd create failed: %v\noutput: %s", err, out)
	}

	// bd create outputs something like "Created spi-xxxx" — extract the ID.
	line := strings.TrimSpace(string(out))
	parts := strings.Fields(line)
	for _, p := range parts {
		if strings.Contains(p, "-") && len(p) >= 5 {
			return p
		}
	}
	t.Fatalf("could not parse bead ID from bd create output: %s", line)
	return ""
}

// clusterAvailable checks if a k8s cluster is reachable.
func clusterAvailable() bool {
	cmd := exec.Command("kubectl", "cluster-info")
	cmd.Stdout = nil
	cmd.Stderr = nil
	return cmd.Run() == nil
}

// detectClusterProvider returns the cluster provider (kind, minikube, or generic).
func detectClusterProvider() string {
	if p := os.Getenv("CLUSTER_PROVIDER"); p != "" {
		return p
	}

	// Check for kind.
	if out, err := exec.Command("kind", "get", "clusters").CombinedOutput(); err == nil {
		if strings.TrimSpace(string(out)) != "" {
			return "kind"
		}
	}

	// Check for minikube.
	if out, err := exec.Command("minikube", "status", "--format", "{{.Host}}").CombinedOutput(); err == nil {
		if strings.TrimSpace(string(out)) == "Running" {
			return "minikube"
		}
	}

	return "generic"
}

// allPodsReady returns true if all pods in the namespace are Ready.
func allPodsReady(t *testing.T, client kubernetes.Interface, namespace string) bool {
	t.Helper()

	pods, err := client.CoreV1().Pods(namespace).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Logf("list pods: %v", err)
		return false
	}

	for _, pod := range pods.Items {
		if pod.Status.Phase != corev1.PodRunning && pod.Status.Phase != corev1.PodSucceeded {
			return false
		}
		for _, cond := range pod.Status.Conditions {
			if cond.Type == corev1.PodReady && cond.Status != corev1.ConditionTrue {
				// PodSucceeded won't have Ready=True, that's fine.
				if pod.Status.Phase != corev1.PodSucceeded {
					return false
				}
			}
		}
	}
	return len(pods.Items) > 0
}

// waitForAllPodsReady polls until all pods in the namespace are ready or the timeout expires.
func waitForAllPodsReady(t *testing.T, client kubernetes.Interface, namespace string, timeout time.Duration) error {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("timeout waiting for all pods ready in %s", namespace)
		case <-ticker.C:
			if allPodsReady(t, client, namespace) {
				return nil
			}
		}
	}
}

// podByLabel finds the first pod matching the given label selector.
func podByLabel(t *testing.T, client kubernetes.Interface, namespace, labelSelector string) (*corev1.Pod, error) {
	t.Helper()

	pods, err := client.CoreV1().Pods(namespace).List(context.Background(), metav1.ListOptions{
		LabelSelector: labelSelector,
	})
	if err != nil {
		return nil, err
	}
	if len(pods.Items) == 0 {
		return nil, fmt.Errorf("no pod found with selector %q", labelSelector)
	}
	return &pods.Items[0], nil
}

// execInPod runs a command inside a pod and returns its combined output.
func execInPod(t *testing.T, namespace, podName string, command []string) (string, error) {
	t.Helper()

	args := append([]string{"exec", "-n", namespace, podName, "--"}, command...)
	cmd := exec.Command("kubectl", args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// serviceHasEndpoints checks whether a service has at least one endpoint.
func serviceHasEndpoints(t *testing.T, client kubernetes.Interface, namespace, svcName string) bool {
	t.Helper()

	eps, err := client.CoreV1().Endpoints(namespace).Get(context.Background(), svcName, metav1.GetOptions{})
	if err != nil {
		t.Logf("get endpoints for %s: %v", svcName, err)
		return false
	}
	for _, subset := range eps.Subsets {
		if len(subset.Addresses) > 0 {
			return true
		}
	}
	return false
}
