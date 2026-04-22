//go:build e2e

package helpers

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
)

// wizardGuildGVR is the dynamic-client handle for the WizardGuild CRD.
// The group/version mirrors operator/api/v1alpha1; bumping the API
// version means this constant must move in lockstep.
var wizardGuildGVR = schema.GroupVersionResource{
	Group:    "spire.awell.io",
	Version:  "v1alpha1",
	Resource: "wizardguilds",
}

// DefaultCacheSize is the PVC capacity requested by ApplyWizardGuildWithCache
// when the caller does not override it. 1Gi is enough to clone small test
// repos without triggering storage-class retention policies on minikube.
const DefaultCacheSize = "1Gi"

// CacheStatusSnapshot mirrors operator/api/v1alpha1.CacheStatus. We
// define it locally rather than importing the operator module because
// operator is a separate Go module (operator/go.mod declares
// github.com/awell-health/spire as a replace-local dependency of itself,
// which makes the reverse import impossible without a circular module
// reference).
//
// Field names must track CacheStatus in
// operator/api/v1alpha1/wizardguild_types.go:143. The shape is stable
// per Q6 resolution (RecoveryOutcome + cache status both locked).
type CacheStatusSnapshot struct {
	Phase           string    `json:"phase,omitempty"`
	Revision        string    `json:"revision,omitempty"`
	LastRefreshTime *string   `json:"lastRefreshTime,omitempty"`
	RefreshError    string    `json:"refreshError,omitempty"`
}

// WizardGuildStatusSnapshot mirrors the subset of
// operator/api/v1alpha1.WizardGuildStatus the test reads. Same rationale
// as CacheStatusSnapshot (separate Go module).
type WizardGuildStatusSnapshot struct {
	Phase                string               `json:"phase,omitempty"`
	Registered           bool                 `json:"registered,omitempty"`
	PodName              string               `json:"podName,omitempty"`
	Message              string               `json:"message,omitempty"`
	Cache                *CacheStatusSnapshot `json:"cache,omitempty"`
	PinnedIdentityBeadID string               `json:"pinnedIdentityBeadID,omitempty"`
}

// ApplyWizardGuildWithCache creates a WizardGuild named `name` in
// `namespace` with Cache enabled and a refresh interval of 30s. The
// short interval is deliberate: it keeps backoff exhaustion within the
// 2-minute polling budget the tests use for wisp detection.
//
// Returns the resulting generation so subsequent Patch calls can detect
// that the operator has observed a spec change.
func ApplyWizardGuildWithCache(t *testing.T, dyn dynamic.Interface, namespace, name string) int64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	obj := &unstructured.Unstructured{}
	obj.SetAPIVersion("spire.awell.io/v1alpha1")
	obj.SetKind("WizardGuild")
	obj.SetNamespace(namespace)
	obj.SetName(name)

	if err := unstructured.SetNestedMap(obj.Object, map[string]interface{}{
		"mode":          "managed",
		"maxConcurrent": int64(1),
		"image":         "spire-agent:dev",
		"cache": map[string]interface{}{
			"size":            DefaultCacheSize,
			"accessMode":      "ReadWriteOnce",
			"refreshInterval": "30s",
		},
	}, "spec"); err != nil {
		t.Fatalf("assemble WizardGuild spec: %v", err)
	}

	created, err := dyn.Resource(wizardGuildGVR).Namespace(namespace).
		Create(ctx, obj, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create WizardGuild %s/%s: %v", namespace, name, err)
	}
	return created.GetGeneration()
}

// DeleteWizardGuild removes a WizardGuild CR, which triggers the
// pinned-identity finalizer described in operator/controllers/pinned_identity.go.
// Returns when the API server accepts the delete request, not when the
// object is fully purged — callers that need full teardown should poll
// for NotFound.
func DeleteWizardGuild(t *testing.T, dyn dynamic.Interface, namespace, name string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	policy := metav1.DeletePropagationForeground
	err := dyn.Resource(wizardGuildGVR).Namespace(namespace).
		Delete(ctx, name, metav1.DeleteOptions{PropagationPolicy: &policy})
	if err != nil {
		t.Fatalf("delete WizardGuild %s/%s: %v", namespace, name, err)
	}
}

// GetWizardGuildStatus fetches the current CR and decodes Status into
// the local snapshot shape. Fatals on missing objects — callers that
// want to detect absence should use the dynamic client directly.
func GetWizardGuildStatus(t *testing.T, dyn dynamic.Interface, namespace, name string) WizardGuildStatusSnapshot {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	u, err := dyn.Resource(wizardGuildGVR).Namespace(namespace).
		Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get WizardGuild %s/%s: %v", namespace, name, err)
	}

	raw, found, err := unstructured.NestedMap(u.Object, "status")
	if err != nil {
		t.Fatalf("read status from WizardGuild %s/%s: %v", namespace, name, err)
	}
	if !found {
		return WizardGuildStatusSnapshot{}
	}
	blob, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("marshal WizardGuild status: %v", err)
	}
	var status WizardGuildStatusSnapshot
	if err := json.Unmarshal(blob, &status); err != nil {
		t.Fatalf("unmarshal WizardGuild status: %v", err)
	}
	return status
}

// WaitForCacheReady polls the WizardGuild status until CacheStatus.Phase
// reports "Ready" or the timeout elapses. Returns the observed revision
// at the moment the Ready transition is seen — verify steps compare
// against this value to confirm a post-recovery refresh advanced the SHA.
func WaitForCacheReady(t *testing.T, dyn dynamic.Interface, namespace, name string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		status := GetWizardGuildStatus(t, dyn, namespace, name)
		if status.Cache != nil && status.Cache.Phase == "Ready" && status.Cache.Revision != "" {
			return status.Cache.Revision
		}
		time.Sleep(3 * time.Second)
	}
	t.Fatalf("WizardGuild %s/%s: cache did not reach Ready within %s", namespace, name, timeout)
	return ""
}

// WaitForPinnedIdentityStamped polls until Status.PinnedIdentityBeadID
// is non-empty. The operator stamps this field during the first reconcile
// after Cache.Enabled (see operator/controllers/pinned_identity.go).
func WaitForPinnedIdentityStamped(t *testing.T, dyn dynamic.Interface, namespace, name string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		status := GetWizardGuildStatus(t, dyn, namespace, name)
		if status.PinnedIdentityBeadID != "" {
			return status.PinnedIdentityBeadID
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("WizardGuild %s/%s: Status.PinnedIdentityBeadID never stamped within %s",
		namespace, name, timeout)
	return ""
}

// ResourceURIFor returns the canonical resource URI the operator stamps
// onto a cache wisp's `source_resource_uri` metadata. The shape is
// `spire.io/wizardguild/<namespace>/<name>/cache` and mirrors the
// operator's construction in cache_recovery.go.
func ResourceURIFor(namespace, name string) string {
	return fmt.Sprintf("spire.io/wizardguild/%s/%s/cache", namespace, name)
}

// PatchWizardGuildCacheBranchPin sets Spec.Cache.BranchPin on the live
// CR. Used by failure injection to redirect the next refresh Job at a
// non-existent branch (`git fetch` will fail → Job backoff-exhausts).
// The patch is a typed JSON merge so it does not disturb other Spec fields.
//
// Passing "" clears the pin (emits an explicit null in the merge patch
// so the operator resets BranchPin to nil — an empty-string pin might
// be interpreted as "pin to empty ref" rather than "unset").
func PatchWizardGuildCacheBranchPin(t *testing.T, dyn dynamic.Interface, namespace, name, branch string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var branchValue interface{} = branch
	if branch == "" {
		branchValue = nil
	}
	patch := map[string]interface{}{
		"spec": map[string]interface{}{
			"cache": map[string]interface{}{
				"branchPin": branchValue,
			},
		},
	}
	blob, err := json.Marshal(patch)
	if err != nil {
		t.Fatalf("marshal cache branchPin patch: %v", err)
	}
	_, err = dyn.Resource(wizardGuildGVR).Namespace(namespace).
		Patch(ctx, name, types.MergePatchType, blob, metav1.PatchOptions{})
	if err != nil {
		t.Fatalf("patch WizardGuild %s/%s cache.branchPin: %v", namespace, name, err)
	}
}
