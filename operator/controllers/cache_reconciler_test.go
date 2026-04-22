package controllers

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr/testr"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spirev1 "github.com/awell-health/spire/operator/api/v1alpha1"
	"github.com/awell-health/spire/pkg/agent"
)

// newCacheTestScheme registers corev1, batchv1, and spirev1 on a scheme
// for the cache reconciler tests. batchv1 is pulled in directly here
// (rather than reusing newTestScheme in agent_monitor_test.go) because
// the reconciler creates Jobs.
func newCacheTestScheme(t *testing.T) *k8sruntime.Scheme {
	t.Helper()
	sch := k8sruntime.NewScheme()
	if err := corev1.AddToScheme(sch); err != nil {
		t.Fatalf("add corev1: %v", err)
	}
	if err := batchv1.AddToScheme(sch); err != nil {
		t.Fatalf("add batchv1: %v", err)
	}
	if err := spirev1.AddToScheme(sch); err != nil {
		t.Fatalf("add spirev1: %v", err)
	}
	return sch
}

// makeCacheGuild builds a WizardGuild with a populated CacheSpec ready
// for the reconciler. Tests mutate the returned object before passing it
// in via WithObjects.
func makeCacheGuild(name, namespace, repo string) *spirev1.WizardGuild {
	branchPin := "main"
	return &spirev1.WizardGuild{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			UID:       types.UID("uid-" + name),
		},
		Spec: spirev1.WizardGuildSpec{
			Mode:       "managed",
			Repo:       repo,
			RepoBranch: "main",
			Prefixes:   []string{"spi"},
			Cache: &spirev1.CacheSpec{
				Size:            resource.MustParse("5Gi"),
				AccessMode:      corev1.ReadOnlyMany,
				RefreshInterval: metav1.Duration{Duration: 10 * time.Minute},
				BranchPin:       &branchPin,
			},
		},
	}
}

func newCacheReconciler(t *testing.T, c client.Client, ns string) *CacheReconciler {
	t.Helper()
	return &CacheReconciler{
		Client:    c,
		Log:       testr.New(t),
		Namespace: ns,
		Interval:  time.Minute,
		GitImage:  "alpine/git:test",
		Database:  "spire",
		Prefix:    "spi",
	}
}

// TestCacheReconciler_GuildCreate_CreatesPVCAndJob covers the first
// responsibility from spi-myzn5: on guild create (+ a populated CacheSpec),
// the reconciler creates a PVC named `<guild>-repo-cache` and a refresh Job
// named `<guild>-repo-cache-refresh`, both carrying an owner reference
// back to the WizardGuild.
func TestCacheReconciler_GuildCreate_CreatesPVCAndJob(t *testing.T) {
	const (
		ns    = "spire"
		name  = "core"
		repo  = "git@example.com:awell-health/spire.git"
	)
	guild := makeCacheGuild(name, ns, repo)

	c := fake.NewClientBuilder().
		WithScheme(newCacheTestScheme(t)).
		WithObjects(guild).
		WithStatusSubresource(&spirev1.WizardGuild{}).
		Build()
	r := newCacheReconciler(t, c, ns)
	r.cycle(context.Background())

	// PVC
	var pvc corev1.PersistentVolumeClaim
	if err := c.Get(context.Background(), client.ObjectKey{
		Namespace: ns, Name: name + "-repo-cache",
	}, &pvc); err != nil {
		t.Fatalf("expected PVC %q to be created: %v", name+"-repo-cache", err)
	}
	if got := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; got.String() != "5Gi" {
		t.Errorf("PVC size = %s, want 5Gi", got.String())
	}
	if len(pvc.Spec.AccessModes) != 1 || pvc.Spec.AccessModes[0] != corev1.ReadOnlyMany {
		t.Errorf("PVC AccessModes = %v, want [ReadOnlyMany]", pvc.Spec.AccessModes)
	}
	assertOwnedBy(t, pvc.ObjectMeta, guild)
	if pvc.Labels[cacheGuildLabel] != name {
		t.Errorf("PVC label %q = %q, want %q", cacheGuildLabel, pvc.Labels[cacheGuildLabel], name)
	}
	if pvc.Labels[cacheRoleLabel] != "pvc" {
		t.Errorf("PVC label %q = %q, want pvc", cacheRoleLabel, pvc.Labels[cacheRoleLabel])
	}

	// Refresh Job
	var job batchv1.Job
	if err := c.Get(context.Background(), client.ObjectKey{
		Namespace: ns, Name: name + "-repo-cache-refresh",
	}, &job); err != nil {
		t.Fatalf("expected refresh Job %q to be created: %v", name+"-repo-cache-refresh", err)
	}
	assertOwnedBy(t, job.ObjectMeta, guild)
	if job.Labels[cacheGuildLabel] != name {
		t.Errorf("Job label %q = %q, want %q", cacheGuildLabel, job.Labels[cacheGuildLabel], name)
	}
	if job.Labels[cacheRoleLabel] != "refresh-job" {
		t.Errorf("Job label %q = %q, want refresh-job", cacheRoleLabel, job.Labels[cacheRoleLabel])
	}

	// Job Pod template must mount the PVC and carry repo URL + branch pin as env.
	spec := job.Spec.Template.Spec
	if len(spec.Volumes) != 1 || spec.Volumes[0].PersistentVolumeClaim == nil ||
		spec.Volumes[0].PersistentVolumeClaim.ClaimName != name+"-repo-cache" {
		t.Errorf("Job PodSpec volumes = %+v, want single PVC volume referencing %s-repo-cache",
			spec.Volumes, name)
	}
	if len(spec.Containers) != 1 {
		t.Fatalf("Job PodSpec.Containers = %d, want 1", len(spec.Containers))
	}
	container := spec.Containers[0]
	envMap := envVarMap(container.Env)
	if got := envMap["SPIRE_REPO_URL"]; got.Value != repo {
		t.Errorf("SPIRE_REPO_URL env = %q, want %q", got.Value, repo)
	}
	if got := envMap["SPIRE_BRANCH_PIN"]; got.Value != "main" {
		t.Errorf("SPIRE_BRANCH_PIN env = %q, want main", got.Value)
	}

	// Intra-PVC layout env must equal the canonical agent.* symbols —
	// not equal to hardcoded test strings. Comparing against the Go
	// constant means any silent rename of the constant will be caught
	// by this test before it ships; comparing against a literal here
	// would just repeat the same drift that shipped the original bug.
	assertEnvEquals(t, envMap, "SPIRE_CACHE_MOUNT", cacheRefreshMountPath)
	assertEnvEquals(t, envMap, "SPIRE_CACHE_MIRROR_SUBDIR", agent.CacheMirrorSubdir)
	assertEnvEquals(t, envMap, "SPIRE_CACHE_REVISION_MARKER", agent.CacheRevisionMarkerName)
	assertEnvEquals(t, envMap, "SPIRE_CACHE_REVISION_TMP_MARKER", agent.CacheRevisionTmpMarkerName)
}

// TestCacheReconciler_CacheSpecChange_RecreatesJob asserts that mutating
// the WizardGuild's CacheSpec drives the reconciler to delete the
// existing refresh Job and create a replacement with a new spec-hash
// annotation. This is the only way a spec change (e.g. new BranchPin)
// takes effect.
func TestCacheReconciler_CacheSpecChange_RecreatesJob(t *testing.T) {
	const (
		ns   = "spire"
		name = "core"
		repo = "git@example.com:spire-test/repo.git"
	)
	guild := makeCacheGuild(name, ns, repo)
	c := fake.NewClientBuilder().
		WithScheme(newCacheTestScheme(t)).
		WithObjects(guild).
		WithStatusSubresource(&spirev1.WizardGuild{}).
		Build()
	r := newCacheReconciler(t, c, ns)

	r.cycle(context.Background())

	var job1 batchv1.Job
	if err := c.Get(context.Background(), client.ObjectKey{
		Namespace: ns, Name: name + "-repo-cache-refresh",
	}, &job1); err != nil {
		t.Fatalf("initial Job missing: %v", err)
	}
	hash1 := job1.Annotations[cacheSpecHashAnnotation]
	if hash1 == "" {
		t.Fatalf("initial Job annotation %q empty", cacheSpecHashAnnotation)
	}

	// Mutate CacheSpec via the client.
	var latest spirev1.WizardGuild
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, &latest); err != nil {
		t.Fatalf("re-get guild: %v", err)
	}
	newPin := "release"
	latest.Spec.Cache.BranchPin = &newPin
	if err := c.Update(context.Background(), &latest); err != nil {
		t.Fatalf("update guild: %v", err)
	}

	r.cycle(context.Background())

	var job2 batchv1.Job
	if err := c.Get(context.Background(), client.ObjectKey{
		Namespace: ns, Name: name + "-repo-cache-refresh",
	}, &job2); err != nil {
		t.Fatalf("recreated Job missing: %v", err)
	}
	hash2 := job2.Annotations[cacheSpecHashAnnotation]
	if hash2 == hash1 {
		t.Fatalf("spec hash unchanged after CacheSpec mutation: %q", hash2)
	}
	// The env on the recreated Job carries the new branch pin.
	if got := envVarMap(job2.Spec.Template.Spec.Containers[0].Env)["SPIRE_BRANCH_PIN"]; got.Value != "release" {
		t.Errorf("recreated Job SPIRE_BRANCH_PIN = %q, want release", got.Value)
	}
}

// TestCacheReconciler_GuildDelete_OwnerRefsForGC verifies the contract
// that guild delete triggers kube GC of the PVC and refresh Job. The
// fake client does not run the GC loop itself, so we assert the
// pre-condition that makes GC work: both child objects carry an
// OwnerReference with Controller=true pointing back at the guild, which
// is the reconciler's only responsibility on the delete path.
func TestCacheReconciler_GuildDelete_OwnerRefsForGC(t *testing.T) {
	const (
		ns   = "spire"
		name = "core"
		repo = "git@example.com:spire-test/repo.git"
	)
	guild := makeCacheGuild(name, ns, repo)
	// Pre-seed a finalizer so c.Delete sets DeletionTimestamp instead of
	// physically removing the object — the fake client honors the same
	// finalizer contract the real API server does.
	guild.Finalizers = []string{"spire.awell.io/test-keepalive"}
	c := fake.NewClientBuilder().
		WithScheme(newCacheTestScheme(t)).
		WithObjects(guild).
		WithStatusSubresource(&spirev1.WizardGuild{}).
		Build()
	r := newCacheReconciler(t, c, ns)
	r.cycle(context.Background())

	var pvc corev1.PersistentVolumeClaim
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name + "-repo-cache"}, &pvc); err != nil {
		t.Fatalf("PVC not created: %v", err)
	}
	var job batchv1.Job
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name + "-repo-cache-refresh"}, &job); err != nil {
		t.Fatalf("Job not created: %v", err)
	}

	// Both must carry a controller owner-ref pointing at the guild. The
	// real kube GC uses this to cascade the delete. Controller=true also
	// ensures no two controllers fight for ownership.
	if !hasControllerOwnerRef(pvc.OwnerReferences, guild) {
		t.Errorf("PVC missing controller owner ref for guild; got %+v", pvc.OwnerReferences)
	}
	if !hasControllerOwnerRef(job.OwnerReferences, guild) {
		t.Errorf("Job missing controller owner ref for guild; got %+v", job.OwnerReferences)
	}

	// Issue the delete — finalizer keeps the object alive with a set
	// DeletionTimestamp, matching real-kube behavior.
	var latest spirev1.WizardGuild
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, &latest); err != nil {
		t.Fatalf("get guild: %v", err)
	}
	if err := c.Delete(context.Background(), &latest); err != nil {
		t.Fatalf("delete guild: %v", err)
	}
	// Confirm the guild is now DeletionTimestamp-marked (i.e. the
	// reconciler will see it as "being deleted").
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, &latest); err != nil {
		t.Fatalf("re-get guild after delete: %v", err)
	}
	if latest.DeletionTimestamp == nil {
		t.Fatalf("DeletionTimestamp not set after c.Delete (finalizer missing?); guild=%+v", latest)
	}

	// Physically delete the child Job to prove the reconciler does NOT
	// recreate it on the delete path — owner-ref GC is the only
	// mechanism; the reconciler's only job here is to stop touching the
	// guild.
	if err := c.Delete(context.Background(), &job); err != nil {
		t.Fatalf("delete Job: %v", err)
	}
	r.cycle(context.Background())

	if err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name + "-repo-cache-refresh"}, &batchv1.Job{}); !apierrors.IsNotFound(err) {
		t.Errorf("reconciler recreated Job on delete path; want NotFound, got err=%v", err)
	}
}

// TestCacheReconciler_RefreshFailure_PopulatesStatus constructs a
// pre-existing refresh Job with a Failed condition and asserts the
// reconciler sets CacheStatus.Phase=Failed, populates RefreshError, and
// the Phase maps to the CacheFailed condition type. The Phase→condition
// mapping is how spi-tpzcq's CacheFailed condition gets flipped when
// conditions are wired onto the CRD.
func TestCacheReconciler_RefreshFailure_PopulatesStatus(t *testing.T) {
	const (
		ns   = "spire"
		name = "core"
		repo = "git@example.com:spire-test/repo.git"
	)
	guild := makeCacheGuild(name, ns, repo)
	guild.Spec.Cache.BranchPin = nil // simpler spec-hash

	// Precompute the spec hash so the pre-seeded Job carries the matching
	// annotation — otherwise the reconciler would treat it as stale and
	// recreate it before deriving status. LastTransitionTime on the
	// Failed condition must be recent so isRefreshDue returns false
	// (RefreshInterval is 10m on this guild); otherwise the reconciler
	// would replace the Job before reading its status.
	desiredHash := specHash(guild.Spec.Cache, guild.Spec.Repo, guild.Spec.RepoBranch)
	now := metav1.Now()
	failedJob := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name + "-repo-cache-refresh",
			Namespace: ns,
			Labels: map[string]string{
				cacheGuildLabel: name,
				cacheRoleLabel:  "refresh-job",
			},
			Annotations: map[string]string{
				cacheSpecHashAnnotation: desiredHash,
			},
			OwnerReferences: []metav1.OwnerReference{ownerRefFor(guild)},
		},
		Status: batchv1.JobStatus{
			CompletionTime: &now,
			Conditions: []batchv1.JobCondition{{
				Type:               batchv1.JobFailed,
				Status:             corev1.ConditionTrue,
				LastTransitionTime: now,
				Reason:             "BackoffLimitExceeded",
				Message:            "clone failed: auth denied",
			}},
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(newCacheTestScheme(t)).
		WithObjects(guild, failedJob).
		WithStatusSubresource(&spirev1.WizardGuild{}).
		Build()
	r := newCacheReconciler(t, c, ns)
	r.cycle(context.Background())

	var got spirev1.WizardGuild
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, &got); err != nil {
		t.Fatalf("get guild: %v", err)
	}
	if got.Status.Cache == nil {
		t.Fatalf("Status.Cache not set")
	}
	if got.Status.Cache.Phase != cachePhaseFailed {
		t.Errorf("Status.Cache.Phase = %q, want %q", got.Status.Cache.Phase, cachePhaseFailed)
	}
	if !strings.Contains(got.Status.Cache.RefreshError, "auth denied") {
		t.Errorf("Status.Cache.RefreshError = %q, want to include 'auth denied'", got.Status.Cache.RefreshError)
	}
	// conditionForPhase is the bridge between Phase and CRD condition
	// types; assert the Failed phase maps to CacheFailed.
	if got := conditionForPhase(cachePhaseFailed); got != spirev1.CacheFailed {
		t.Errorf("conditionForPhase(Failed) = %q, want %q", got, spirev1.CacheFailed)
	}
}

// TestCacheReconciler_RefreshScript_AtomicMarker asserts the refresh
// script the Job runs writes the generation marker via an atomic
// write-then-rename pattern. The reconciler's file-header comment
// commits to this approach precisely so workers never observe a
// half-written marker; any change that moves to "write in place"
// must break this test.
func TestCacheReconciler_RefreshScript_AtomicMarker(t *testing.T) {
	script := cacheRefreshScript

	// Atomic rename pattern: write .tmp, then mv .tmp → final. A
	// reader that opens the marker at any moment either finds the old
	// file, the new file, or NotFound — never a truncated/partial
	// read. The script references the marker names via SPIRE_CACHE_*
	// env vars (see cacheRefreshEnv) rather than string literals — the
	// layout contract is the agent.* constants, not a string in this
	// file.
	if !strings.Contains(script, `TMP="$MOUNT/$SPIRE_CACHE_REVISION_TMP_MARKER"`) {
		t.Errorf("refresh script missing tmp-marker env wiring; got:\n%s", script)
	}
	if !strings.Contains(script, `FINAL="$MOUNT/$SPIRE_CACHE_REVISION_MARKER"`) {
		t.Errorf("refresh script missing final-marker env wiring; got:\n%s", script)
	}
	if !strings.Contains(script, `mv "$TMP" "$FINAL"`) {
		t.Errorf("refresh script missing atomic rename (mv $TMP $FINAL); got:\n%s", script)
	}
	// A `sync` between write and rename flushes the tmp contents so a
	// crash-restart cannot observe an empty marker after rename.
	if !strings.Contains(script, "sync") {
		t.Errorf("refresh script missing sync before rename; got:\n%s", script)
	}
	// The printf > $TMP pattern must precede the mv; a reversed order
	// (mv before write) would defeat the atomic contract. The simplest
	// check: the printf line occurs before the mv line.
	iPrintf := strings.Index(script, `printf '%s\n' "$REV" > "$TMP"`)
	iMv := strings.Index(script, `mv "$TMP" "$FINAL"`)
	if iPrintf < 0 || iMv < 0 || iPrintf > iMv {
		t.Errorf("refresh script must write $TMP BEFORE mv $TMP $FINAL; got printf@%d mv@%d", iPrintf, iMv)
	}

	// Guardrail against drift: the layout contract lives in
	// pkg/agent constants. The script must NOT hardcode the marker
	// filenames or mirror subdirectory — those must come from env
	// vars. A hardcoded copy would re-introduce the producer/consumer
	// drift bug (spi-yzmq0).
	forbidden := []string{".spire-cache-revision", `"mirror"`, "/mirror"}
	for _, lit := range forbidden {
		if strings.Contains(script, lit) {
			t.Errorf("refresh script must not hardcode %q; the layout is env-driven from pkg/agent constants", lit)
		}
	}
}

// TestCacheReconciler_RefreshEnv_LayoutFromAgentConstants locks the
// contract that the Job's env carries the canonical intra-PVC layout
// values, sourced from pkg/agent constants. If any of these assertions
// fail, the producer and consumer have drifted: the refresh script
// would write to paths the worker does not read (or vice versa).
// Compared against the symbol, not a hardcoded string, so a silent
// rename of the constant is still caught.
func TestCacheReconciler_RefreshEnv_LayoutFromAgentConstants(t *testing.T) {
	envs := cacheRefreshEnv(cacheRefreshMountPath)
	got := make(map[string]string, len(envs))
	for _, e := range envs {
		got[e.Name] = e.Value
	}

	want := map[string]string{
		"SPIRE_CACHE_MOUNT":               cacheRefreshMountPath,
		"SPIRE_CACHE_MIRROR_SUBDIR":       agent.CacheMirrorSubdir,
		"SPIRE_CACHE_REVISION_MARKER":     agent.CacheRevisionMarkerName,
		"SPIRE_CACHE_REVISION_TMP_MARKER": agent.CacheRevisionTmpMarkerName,
		"SPIRE_CACHE_TERMINATION_LOG":     cacheRefreshTerminationLog,
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("cacheRefreshEnv[%s] = %q, want %q", k, got[k], v)
		}
	}
	if len(got) != len(want) {
		t.Errorf("cacheRefreshEnv entries = %d, want %d; extra=%v", len(got), len(want), got)
	}
}

// TestCacheReconciler_SkipsGuildWithoutCacheSpec asserts the reconciler
// leaves guilds that have not opted in alone. Nothing gets created, no
// status is patched — this is the gate that keeps pre-cache installs
// unaffected by the reconciler.
func TestCacheReconciler_SkipsGuildWithoutCacheSpec(t *testing.T) {
	const ns = "spire"
	guild := &spirev1.WizardGuild{
		ObjectMeta: metav1.ObjectMeta{Name: "legacy", Namespace: ns, UID: "uid-legacy"},
		Spec: spirev1.WizardGuildSpec{
			Mode: "managed",
			Repo: "git@example.com:spire/repo.git",
		},
	}
	c := fake.NewClientBuilder().
		WithScheme(newCacheTestScheme(t)).
		WithObjects(guild).
		WithStatusSubresource(&spirev1.WizardGuild{}).
		Build()
	r := newCacheReconciler(t, c, ns)
	r.cycle(context.Background())

	if err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "legacy-repo-cache"}, &corev1.PersistentVolumeClaim{}); !apierrors.IsNotFound(err) {
		t.Errorf("PVC must not be created for guild without CacheSpec; got err=%v", err)
	}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "legacy-repo-cache-refresh"}, &batchv1.Job{}); !apierrors.IsNotFound(err) {
		t.Errorf("Job must not be created for guild without CacheSpec; got err=%v", err)
	}
}

// TestCacheReconciler_EmptyRepo_FailsStatus asserts the reconciler
// surfaces the "CacheSpec set but Spec.Repo empty" misconfig as a
// CacheStatus.Phase=Failed with a helpful message rather than crashing
// the reconcile loop.
func TestCacheReconciler_EmptyRepo_FailsStatus(t *testing.T) {
	const (
		ns   = "spire"
		name = "misconfig"
	)
	guild := makeCacheGuild(name, ns, "")
	c := fake.NewClientBuilder().
		WithScheme(newCacheTestScheme(t)).
		WithObjects(guild).
		WithStatusSubresource(&spirev1.WizardGuild{}).
		Build()
	r := newCacheReconciler(t, c, ns)
	r.cycle(context.Background())

	var got spirev1.WizardGuild
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, &got); err != nil {
		t.Fatalf("get guild: %v", err)
	}
	if got.Status.Cache == nil || got.Status.Cache.Phase != cachePhaseFailed {
		t.Fatalf("Status.Cache.Phase = %+v, want Failed", got.Status.Cache)
	}
	if !strings.Contains(got.Status.Cache.RefreshError, "Repo is empty") {
		t.Errorf("Status.Cache.RefreshError = %q, want to mention 'Repo is empty'", got.Status.Cache.RefreshError)
	}
	// PVC must not be created when the guild is misconfigured.
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name + "-repo-cache"}, &corev1.PersistentVolumeClaim{}); !apierrors.IsNotFound(err) {
		t.Errorf("PVC must not be created for empty-repo guild; got err=%v", err)
	}
}

// TestCacheReconciler_ChartDefaults_ApplyWhenGuildUnset asserts that
// deployment-time chart defaults (StorageClassName / Size / AccessMode)
// back the PVC when the guild leaves those CacheSpec fields unset, per
// spi-bsngj's storage contract.
func TestCacheReconciler_ChartDefaults_ApplyWhenGuildUnset(t *testing.T) {
	const (
		ns   = "spire"
		name = "core"
		repo = "git@example.com:spire/repo.git"
	)
	// Explicitly zero the fields the reconciler should fill from chart
	// defaults: Size unset (zero quantity), AccessMode empty,
	// StorageClassName empty.
	guild := &spirev1.WizardGuild{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, UID: types.UID("uid-" + name)},
		Spec: spirev1.WizardGuildSpec{
			Mode:       "managed",
			Repo:       repo,
			RepoBranch: "main",
			Cache:      &spirev1.CacheSpec{}, // size=zero, access mode unset, storage class unset
		},
	}
	c := fake.NewClientBuilder().
		WithScheme(newCacheTestScheme(t)).
		WithObjects(guild).
		WithStatusSubresource(&spirev1.WizardGuild{}).
		Build()

	r := newCacheReconciler(t, c, ns)
	r.ChartCacheStorageClass = "fast-ssd"
	r.ChartCacheSize = "42Gi"
	r.ChartCacheAccessMode = corev1.ReadWriteMany
	r.cycle(context.Background())

	var pvc corev1.PersistentVolumeClaim
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name + "-repo-cache"}, &pvc); err != nil {
		t.Fatalf("PVC missing: %v", err)
	}
	if pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName != "fast-ssd" {
		t.Errorf("PVC StorageClassName = %+v, want %q (chart default)", pvc.Spec.StorageClassName, "fast-ssd")
	}
	if got := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; got.String() != "42Gi" {
		t.Errorf("PVC size = %s, want 42Gi (chart default)", got.String())
	}
	if len(pvc.Spec.AccessModes) != 1 || pvc.Spec.AccessModes[0] != corev1.ReadWriteMany {
		t.Errorf("PVC AccessModes = %v, want [ReadWriteMany] (chart default)", pvc.Spec.AccessModes)
	}
}

// TestCacheReconciler_StatusPreservesRevisionOnTransientFailure verifies
// that a transient Failed state does not drop the cache Revision
// recorded by the previous successful refresh. This keeps wizard pods
// binding a stale-but-usable cache instead of failing to start.
func TestCacheReconciler_StatusPreservesRevisionOnTransientFailure(t *testing.T) {
	const (
		ns   = "spire"
		name = "core"
	)
	guild := makeCacheGuild(name, ns, "git@example.com:spire/repo.git")
	guild.Spec.Cache.BranchPin = nil
	// Seed an existing successful Revision.
	oldRefresh := metav1.NewTime(time.Now().Add(-2 * time.Minute))
	guild.Status = spirev1.WizardGuildStatus{
		Cache: &spirev1.CacheStatus{
			Phase:           cachePhaseReady,
			Revision:        "abcdef1234567890",
			LastRefreshTime: &oldRefresh,
		},
	}

	// Pre-seed a failed Job with a matching spec-hash so the reconciler
	// derives Failed rather than recreating it. CompletionTime=now keeps
	// isRefreshDue=false (RefreshInterval 10m), so the job survives the
	// cycle and its status is what deriveStatus reads.
	desiredHash := specHash(guild.Spec.Cache, guild.Spec.Repo, guild.Spec.RepoBranch)
	now := metav1.Now()
	failedJob := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name + "-repo-cache-refresh",
			Namespace:       ns,
			Annotations:     map[string]string{cacheSpecHashAnnotation: desiredHash},
			OwnerReferences: []metav1.OwnerReference{ownerRefFor(guild)},
		},
		Status: batchv1.JobStatus{
			CompletionTime: &now,
			Conditions: []batchv1.JobCondition{{
				Type:               batchv1.JobFailed,
				Status:             corev1.ConditionTrue,
				LastTransitionTime: now,
				Reason:             "Backoff",
			}},
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(newCacheTestScheme(t)).
		WithObjects(guild, failedJob).
		WithStatusSubresource(&spirev1.WizardGuild{}).
		Build()
	r := newCacheReconciler(t, c, ns)
	r.cycle(context.Background())

	var got spirev1.WizardGuild
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, &got); err != nil {
		t.Fatalf("get guild: %v", err)
	}
	if got.Status.Cache == nil {
		t.Fatalf("Status.Cache nil")
	}
	if got.Status.Cache.Phase != cachePhaseFailed {
		t.Errorf("Phase = %q, want Failed", got.Status.Cache.Phase)
	}
	if got.Status.Cache.Revision != "abcdef1234567890" {
		t.Errorf("Revision = %q, want to preserve previous abcdef1234567890", got.Status.Cache.Revision)
	}
}

// TestCacheReconciler_Revision_FirstSuccess_Populated covers the
// first-ever successful refresh: prior status is nil and a Pod owned
// by the Job carries a terminated-state message containing the
// resolved SHA. The reconciler must surface that SHA on
// CacheStatus.Revision and flip Phase to Ready.
func TestCacheReconciler_Revision_FirstSuccess_Populated(t *testing.T) {
	const (
		ns   = "spire"
		name = "core"
		sha  = "abc1234deadbeef"
	)
	guild := makeCacheGuild(name, ns, "git@example.com:spire/repo.git")
	guild.Spec.Cache.BranchPin = nil

	desiredHash := specHash(guild.Spec.Cache, guild.Spec.Repo, guild.Spec.RepoBranch)
	now := metav1.Now()
	jobName := name + "-repo-cache-refresh"
	succeededJob := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:            jobName,
			Namespace:       ns,
			Annotations:     map[string]string{cacheSpecHashAnnotation: desiredHash},
			OwnerReferences: []metav1.OwnerReference{ownerRefFor(guild)},
		},
		Status: batchv1.JobStatus{
			CompletionTime: &now,
			Conditions: []batchv1.JobCondition{{
				Type:               batchv1.JobComplete,
				Status:             corev1.ConditionTrue,
				LastTransitionTime: now,
			}},
		},
	}
	refreshPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName + "-pod",
			Namespace: ns,
			Labels:    map[string]string{jobNameLabel: jobName},
		},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: cacheRefreshContainerName,
				State: corev1.ContainerState{
					Terminated: &corev1.ContainerStateTerminated{
						Message: sha,
					},
				},
			}},
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(newCacheTestScheme(t)).
		WithObjects(guild, succeededJob, refreshPod).
		WithStatusSubresource(&spirev1.WizardGuild{}).
		Build()
	r := newCacheReconciler(t, c, ns)
	r.cycle(context.Background())

	var got spirev1.WizardGuild
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, &got); err != nil {
		t.Fatalf("get guild: %v", err)
	}
	if got.Status.Cache == nil {
		t.Fatalf("Status.Cache nil")
	}
	if got.Status.Cache.Phase != cachePhaseReady {
		t.Errorf("Phase = %q, want Ready", got.Status.Cache.Phase)
	}
	if got.Status.Cache.Revision != sha {
		t.Errorf("Revision = %q, want %q", got.Status.Cache.Revision, sha)
	}
}

// TestCacheReconciler_Revision_PreserveOnRefreshing covers the
// in-flight refresh case: a prior successful Revision must survive a
// transition where the new Job is still running (no terminal
// condition yet).
func TestCacheReconciler_Revision_PreserveOnRefreshing(t *testing.T) {
	const (
		ns   = "spire"
		name = "core"
		oldRev = "deadbeef"
	)
	guild := makeCacheGuild(name, ns, "git@example.com:spire/repo.git")
	guild.Spec.Cache.BranchPin = nil
	prevRefresh := metav1.NewTime(time.Now().Add(-2 * time.Minute))
	guild.Status = spirev1.WizardGuildStatus{
		Cache: &spirev1.CacheStatus{
			Phase:           cachePhaseReady,
			Revision:        oldRev,
			LastRefreshTime: &prevRefresh,
		},
	}

	desiredHash := specHash(guild.Spec.Cache, guild.Spec.Repo, guild.Spec.RepoBranch)
	// Running Job: no Complete/Failed condition, no CompletionTime.
	runningJob := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name + "-repo-cache-refresh",
			Namespace:       ns,
			Annotations:     map[string]string{cacheSpecHashAnnotation: desiredHash},
			OwnerReferences: []metav1.OwnerReference{ownerRefFor(guild)},
		},
		Status: batchv1.JobStatus{Active: 1},
	}

	c := fake.NewClientBuilder().
		WithScheme(newCacheTestScheme(t)).
		WithObjects(guild, runningJob).
		WithStatusSubresource(&spirev1.WizardGuild{}).
		Build()
	r := newCacheReconciler(t, c, ns)
	r.cycle(context.Background())

	var got spirev1.WizardGuild
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, &got); err != nil {
		t.Fatalf("get guild: %v", err)
	}
	if got.Status.Cache == nil {
		t.Fatalf("Status.Cache nil")
	}
	if got.Status.Cache.Phase != cachePhaseRefreshing {
		t.Errorf("Phase = %q, want Refreshing", got.Status.Cache.Phase)
	}
	if got.Status.Cache.Revision != oldRev {
		t.Errorf("Revision = %q, want preserved %q", got.Status.Cache.Revision, oldRev)
	}
}

// TestCacheReconciler_Revision_PodGCRace_FirstSuccess covers the case
// where a Job has succeeded but its Pod has already been
// garbage-collected (no Pod matches the job-name label). The
// reconciler must NOT crash and Revision must remain empty rather
// than fabricate a value — Phase still flips to Ready, since the Job
// itself reports success.
func TestCacheReconciler_Revision_PodGCRace_FirstSuccess(t *testing.T) {
	const (
		ns   = "spire"
		name = "core"
	)
	guild := makeCacheGuild(name, ns, "git@example.com:spire/repo.git")
	guild.Spec.Cache.BranchPin = nil

	desiredHash := specHash(guild.Spec.Cache, guild.Spec.Repo, guild.Spec.RepoBranch)
	now := metav1.Now()
	succeededJob := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name + "-repo-cache-refresh",
			Namespace:       ns,
			Annotations:     map[string]string{cacheSpecHashAnnotation: desiredHash},
			OwnerReferences: []metav1.OwnerReference{ownerRefFor(guild)},
		},
		Status: batchv1.JobStatus{
			CompletionTime: &now,
			Conditions: []batchv1.JobCondition{{
				Type:               batchv1.JobComplete,
				Status:             corev1.ConditionTrue,
				LastTransitionTime: now,
			}},
		},
	}

	// No Pod object — simulates kube GC having reaped it.
	c := fake.NewClientBuilder().
		WithScheme(newCacheTestScheme(t)).
		WithObjects(guild, succeededJob).
		WithStatusSubresource(&spirev1.WizardGuild{}).
		Build()
	r := newCacheReconciler(t, c, ns)
	r.cycle(context.Background())

	var got spirev1.WizardGuild
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, &got); err != nil {
		t.Fatalf("get guild: %v", err)
	}
	if got.Status.Cache == nil {
		t.Fatalf("Status.Cache nil")
	}
	if got.Status.Cache.Phase != cachePhaseReady {
		t.Errorf("Phase = %q, want Ready (Job-level success is independent of Pod presence)", got.Status.Cache.Phase)
	}
	if got.Status.Cache.Revision != "" {
		t.Errorf("Revision = %q, want empty (no Pod to read termination message from)", got.Status.Cache.Revision)
	}
}

// TestCacheReconciler_Revision_RejectsNonSHATerminationMessage
// ensures the reader's shape-check rejects garbage in the termination
// message rather than surfacing it as the cache Revision. A worker
// would later fail to check out a non-SHA value, so the reader is the
// right place to gate it.
func TestCacheReconciler_Revision_RejectsNonSHATerminationMessage(t *testing.T) {
	const (
		ns   = "spire"
		name = "core"
	)
	guild := makeCacheGuild(name, ns, "git@example.com:spire/repo.git")
	guild.Spec.Cache.BranchPin = nil

	desiredHash := specHash(guild.Spec.Cache, guild.Spec.Repo, guild.Spec.RepoBranch)
	jobName := name + "-repo-cache-refresh"
	now := metav1.Now()
	succeededJob := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:            jobName,
			Namespace:       ns,
			Annotations:     map[string]string{cacheSpecHashAnnotation: desiredHash},
			OwnerReferences: []metav1.OwnerReference{ownerRefFor(guild)},
		},
		Status: batchv1.JobStatus{
			CompletionTime: &now,
			Conditions: []batchv1.JobCondition{{
				Type:               batchv1.JobComplete,
				Status:             corev1.ConditionTrue,
				LastTransitionTime: now,
			}},
		},
	}
	noisyPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName + "-pod",
			Namespace: ns,
			Labels:    map[string]string{jobNameLabel: jobName},
		},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: cacheRefreshContainerName,
				State: corev1.ContainerState{
					Terminated: &corev1.ContainerStateTerminated{
						Message: "fatal: clone failed: auth denied\n",
					},
				},
			}},
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(newCacheTestScheme(t)).
		WithObjects(guild, succeededJob, noisyPod).
		WithStatusSubresource(&spirev1.WizardGuild{}).
		Build()
	r := newCacheReconciler(t, c, ns)
	r.cycle(context.Background())

	var got spirev1.WizardGuild
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, &got); err != nil {
		t.Fatalf("get guild: %v", err)
	}
	if got.Status.Cache == nil {
		t.Fatalf("Status.Cache nil")
	}
	if got.Status.Cache.Revision != "" {
		t.Errorf("Revision = %q, want empty (non-SHA termination message must be rejected)", got.Status.Cache.Revision)
	}
}

// --- helpers ---

// envVarMap returns a name-keyed map of env vars. Name collisions retain
// the last value seen, matching the k8s runtime behavior.
func envVarMap(env []corev1.EnvVar) map[string]corev1.EnvVar {
	m := make(map[string]corev1.EnvVar, len(env))
	for _, e := range env {
		m[e.Name] = e
	}
	return m
}

// assertEnvEquals fails the test when the env var `name` is absent or
// has a Value other than `want`. Used instead of direct map reads so
// the failure message consistently names the key.
func assertEnvEquals(t *testing.T, envs map[string]corev1.EnvVar, name, want string) {
	t.Helper()
	got, ok := envs[name]
	if !ok {
		t.Errorf("env %s missing from container; have: %v", name, keysOfEnvMap(envs))
		return
	}
	if got.Value != want {
		t.Errorf("env %s = %q, want %q", name, got.Value, want)
	}
}

func keysOfEnvMap(envs map[string]corev1.EnvVar) []string {
	out := make([]string, 0, len(envs))
	for k := range envs {
		out = append(out, k)
	}
	return out
}

// assertOwnedBy fails the test unless meta carries a controller owner
// reference pointing at the given guild.
func assertOwnedBy(t *testing.T, meta metav1.ObjectMeta, guild *spirev1.WizardGuild) {
	t.Helper()
	if !hasControllerOwnerRef(meta.OwnerReferences, guild) {
		t.Errorf("object %q missing controller owner ref for guild %q; got %+v",
			meta.Name, guild.Name, meta.OwnerReferences)
	}
}

func hasControllerOwnerRef(refs []metav1.OwnerReference, guild *spirev1.WizardGuild) bool {
	for _, r := range refs {
		if r.Kind == "WizardGuild" && r.Name == guild.Name && r.UID == guild.UID &&
			r.Controller != nil && *r.Controller {
			return true
		}
	}
	return false
}
