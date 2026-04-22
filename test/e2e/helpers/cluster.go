//go:build e2e

// Package helpers provides cluster-side setup and teardown primitives for
// the end-to-end cache-recovery test suite (spi-p18tr).
//
// The helpers are intentionally thin wrappers around the same building
// blocks used by test/smoke (helm, kubectl, direct k8s client). They are
// split across files by concern: cluster bring-up, WizardGuild lifecycle,
// failure injection, bead/store access, and cleric-side observation.
//
// All helpers are test-scoped — they take *testing.T and call t.Fatal /
// t.Helper as appropriate. They do not gracefully degrade; the test is
// expected to assert end-to-end shape against a live cluster and fail
// loudly if any stage is missing.
package helpers

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

// DefaultHelmTimeout is the per-operation ceiling for helm install/upgrade.
// 5 minutes accommodates cold image pulls on minikube first-run.
const DefaultHelmTimeout = 5 * time.Minute

// Fixture carries the per-test cluster+store handles. A single Fixture is
// built by seedFixture and passed through each t.Run block.
type Fixture struct {
	TowerName     string
	Namespace     string
	DoltLocalPort int
	KubeConfig    string

	Kube    kubernetes.Interface
	Dynamic dynamic.Interface

	// Cancel is the port-forward cancel func registered via t.Cleanup.
	// Tests do not call it directly; it is invoked automatically.
	Cancel func()
}

// EnsureMinikubeUp skips the test when no reachable k8s cluster is present.
// Minikube is the primary target per project_k8s_minikube_status.md, but any
// kubeconfig-reachable cluster (kind, docker-desktop) is accepted.
func EnsureMinikubeUp(t *testing.T) {
	t.Helper()
	if err := exec.Command("kubectl", "cluster-info").Run(); err != nil {
		t.Skipf("no k8s cluster reachable via kubectl: %v — start minikube or set KUBECONFIG", err)
	}
}

// GetKubeClient returns typed + dynamic clients from KUBECONFIG or the
// default path. Fatals on unreachable clusters rather than skipping —
// EnsureMinikubeUp is the skip gate; by the time this runs, the caller has
// committed to interacting with the cluster.
func GetKubeClient(t *testing.T) (kubernetes.Interface, dynamic.Interface) {
	t.Helper()
	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			t.Fatalf("cannot determine home dir for kubeconfig: %v", err)
		}
		kubeconfig = home + "/.kube/config"
	}
	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		t.Fatalf("build kubeconfig from %s: %v", kubeconfig, err)
	}
	kube, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("build typed k8s client: %v", err)
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("build dynamic k8s client: %v", err)
	}
	return kube, dyn
}

// HelmInstallOpts carries the values-overrides applied to the chart install.
// The defaults mirror test/smoke/cluster_test.go so the operator picks up
// the dev images minikube has loaded via `minikube image load`.
type HelmInstallOpts struct {
	Namespace  string
	TowerName  string
	Values     map[string]string
	ExtraFlags []string
}

// InstallSpireHelm runs `helm install spire chart/spire/ --namespace <ns>
// --create-namespace --wait` with the caller's values overlaid on top of
// the smoke-test defaults. Any --set key supplied in opts.Values replaces
// the default for that key.
func InstallSpireHelm(t *testing.T, opts HelmInstallOpts) {
	t.Helper()
	if opts.Namespace == "" {
		t.Fatalf("InstallSpireHelm: namespace is required")
	}

	values := map[string]string{
		"tower.name":               opts.TowerName,
		"image.steward.repository": "spire-steward",
		"image.steward.tag":        "dev",
		"image.agent.repository":   "spire-agent",
		"image.agent.tag":          "dev",
	}
	if key := os.Getenv("ANTHROPIC_API_KEY"); key != "" {
		values["secrets.anthropicKey"] = key
	}
	if tok := os.Getenv("GITHUB_TOKEN"); tok != "" {
		values["secrets.githubToken"] = tok
	}
	for k, v := range opts.Values {
		values[k] = v
	}

	args := []string{
		"install", "spire", "chart/spire/",
		"--namespace", opts.Namespace,
		"--create-namespace",
		"--wait",
		"--timeout", DefaultHelmTimeout.String(),
	}
	for k, v := range values {
		args = append(args, "--set", fmt.Sprintf("%s=%s", k, v))
	}
	args = append(args, opts.ExtraFlags...)

	cmd := exec.Command("helm", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("helm install spire into %s: %v", opts.Namespace, err)
	}
}

// UninstallSpireHelm runs `helm uninstall spire` and deletes the namespace.
// Registered via t.Cleanup in seedFixture. Best-effort — logs rather than
// fatals on error so a failed teardown doesn't mask the real test failure.
func UninstallSpireHelm(t *testing.T, namespace string) {
	t.Helper()
	cmd := exec.Command("helm", "uninstall", "spire", "--namespace", namespace)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Logf("helm uninstall warning (namespace=%s): %v", namespace, err)
	}
	del := exec.Command("kubectl", "delete", "namespace", namespace, "--ignore-not-found")
	del.Stdout = os.Stdout
	del.Stderr = os.Stderr
	if err := del.Run(); err != nil {
		t.Logf("namespace delete warning (namespace=%s): %v", namespace, err)
	}
}

// CreateTestTower invokes `spire tower create --name <name>` so the tower
// exists locally on the test runner's machine before the beads store is
// opened. This mirrors how spire-up bootstraps a tower in dev workflows.
//
// The function is idempotent: if the tower already exists, the error is
// logged and ignored.
func CreateTestTower(t *testing.T, name string) {
	t.Helper()
	cmd := exec.Command("spire", "tower", "create", "--name", name)
	out, err := cmd.CombinedOutput()
	if err != nil {
		if strings.Contains(string(out), "already exists") {
			t.Logf("tower %q already exists — reusing", name)
			return
		}
		t.Fatalf("spire tower create --name %s: %v\noutput: %s", name, err, string(out))
	}
}

// WaitForOperatorReady blocks until the spire-operator Deployment reports
// ReadyReplicas >= 1 or the timeout elapses.
func WaitForOperatorReady(t *testing.T, kube kubernetes.Interface, namespace string, timeout time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			t.Fatalf("spire-operator not ready in %s within %s", namespace, timeout)
		case <-tick.C:
			dep, err := kube.AppsV1().Deployments(namespace).
				Get(ctx, "spire-operator", metav1.GetOptions{})
			if err != nil {
				t.Logf("get spire-operator deployment: %v", err)
				continue
			}
			if dep.Status.ReadyReplicas >= 1 {
				return
			}
		}
	}
}

// WaitForAllPodsReady polls until every pod in the namespace is in a
// Running or Succeeded phase. Borrowed from test/smoke/cluster_helpers_test.go;
// duplicated here so the e2e suite does not share a build tag with smoke.
func WaitForAllPodsReady(t *testing.T, kube kubernetes.Interface, namespace string, timeout time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	tick := time.NewTicker(10 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			t.Fatalf("not all pods ready in %s within %s", namespace, timeout)
		case <-tick.C:
			pods, err := kube.CoreV1().Pods(namespace).
				List(ctx, metav1.ListOptions{})
			if err != nil {
				t.Logf("list pods in %s: %v", namespace, err)
				continue
			}
			if len(pods.Items) == 0 {
				continue
			}
			if allRunningOrSucceeded(pods.Items) {
				return
			}
		}
	}
}

func allRunningOrSucceeded(pods []corev1.Pod) bool {
	for _, p := range pods {
		if p.Status.Phase != corev1.PodRunning && p.Status.Phase != corev1.PodSucceeded {
			return false
		}
	}
	return true
}
