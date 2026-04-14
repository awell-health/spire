package agent

import (
	"context"
	"fmt"
	"os"
	"sync/atomic"
	"syscall"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
)

// K8sHandle tracks a running agent pod in Kubernetes.
type K8sHandle struct {
	client    kubernetes.Interface
	namespace string
	podName   string
	name      string
	exited    atomic.Bool
}

// Wait blocks until the pod reaches Succeeded or Failed phase.
// Returns nil on success, an error with the exit code on failure.
func (h *K8sHandle) Wait() error {
	defer h.exited.Store(true)

	// Check current state first — the pod may have already completed.
	pod, err := h.client.CoreV1().Pods(h.namespace).Get(
		context.Background(), h.podName, metav1.GetOptions{},
	)
	if err != nil {
		return fmt.Errorf("get pod %s: %w", h.podName, err)
	}
	if phase := pod.Status.Phase; phase == corev1.PodSucceeded || phase == corev1.PodFailed {
		return phaseError(pod)
	}

	// Watch for phase changes. Use the ResourceVersion from the Get response
	// so the watch starts from that point, not "now". This avoids a race where
	// the pod completes between the Get and Watch calls — without this the
	// Watch would never see the completion event and hang forever.
	watcher, err := h.client.CoreV1().Pods(h.namespace).Watch(
		context.Background(),
		metav1.ListOptions{
			FieldSelector:   "metadata.name=" + h.podName,
			ResourceVersion: pod.ResourceVersion,
		},
	)
	if err != nil {
		return fmt.Errorf("watch pod %s: %w", h.podName, err)
	}
	defer watcher.Stop()

	for event := range watcher.ResultChan() {
		if event.Type == watch.Deleted {
			return fmt.Errorf("pod %s was deleted", h.podName)
		}
		pod, ok := event.Object.(*corev1.Pod)
		if !ok {
			continue
		}
		switch pod.Status.Phase {
		case corev1.PodSucceeded, corev1.PodFailed:
			return phaseError(pod)
		}
	}

	return fmt.Errorf("watch channel closed for pod %s", h.podName)
}

// Signal sends a signal to the pod. SIGTERM deletes with a grace period;
// SIGKILL deletes immediately.
func (h *K8sHandle) Signal(sig os.Signal) error {
	if h.exited.Load() {
		return fmt.Errorf("pod already exited")
	}

	var grace int64
	switch sig {
	case syscall.SIGTERM:
		grace = 30
	case syscall.SIGKILL:
		grace = 0
	default:
		grace = 30
	}

	return h.client.CoreV1().Pods(h.namespace).Delete(
		context.Background(), h.podName,
		metav1.DeleteOptions{GracePeriodSeconds: &grace},
	)
}

// Alive returns true if the pod has not yet exited.
func (h *K8sHandle) Alive() bool {
	return !h.exited.Load()
}

// Name returns the agent's configured name.
func (h *K8sHandle) Name() string {
	return h.name
}

// Identifier returns the pod name.
func (h *K8sHandle) Identifier() string {
	return h.podName
}

// phaseError returns nil for Succeeded pods and an error with exit code
// information for Failed pods.
func phaseError(pod *corev1.Pod) error {
	if pod.Status.Phase == corev1.PodSucceeded {
		return nil
	}
	// Extract exit code from the first container's terminated state.
	if len(pod.Status.ContainerStatuses) > 0 {
		cs := pod.Status.ContainerStatuses[0]
		if cs.State.Terminated != nil {
			return fmt.Errorf("pod %s failed with exit code %d: %s",
				pod.Name, cs.State.Terminated.ExitCode, cs.State.Terminated.Reason)
		}
	}
	return fmt.Errorf("pod %s failed", pod.Name)
}
