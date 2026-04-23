// Package controllers — cache reconciler (spi-myzn5).
//
// CacheReconciler turns a WizardGuild.Spec.CacheSpec into a guild-owned
// PersistentVolumeClaim plus a refresh Kubernetes Job, and surfaces
// lifecycle state on WizardGuild.Status.Cache. It composes with the
// phase-2 cluster repo-cache contract (spi-sn7o3) and the CRD types
// from spi-tpzcq.
//
// Serialization approach
// ----------------------
// The refresh script writes a generation marker file atomically at the
// end of each successful refresh: the revision is written to a
// sibling .tmp file and then `mv`d onto the final marker name. POSIX
// rename within the same filesystem is atomic, so wizard pods
// observing the marker file never see a half-written cache. We chose
// this over the snapshot-promote-by-rename variant because the marker
// adds only one tiny file to the PVC (vs. a full shadow tree) and the
// refresh Job can run `git fetch` in place on a pre-existing mirror
// without an intermediate copy.
//
// The exact marker filenames and the mirror subdirectory name are
// declared in pkg/agent as CacheRevisionMarkerName,
// CacheRevisionTmpMarkerName, and CacheMirrorSubdir. The worker-side
// bootstrap helper (pkg/agent/cache_bootstrap.go) consumes those same
// constants, and the refresh script receives them via SPIRE_CACHE_*
// env vars from cacheRefreshEnv — the intra-PVC layout is never
// hardcoded twice.
//
// What this reconciler does NOT do
// --------------------------------
// - It does NOT mutate shared repos-table state. No `spire repo add`
//   side effects, no writes to pkg/store. Repo identity stays
//   authoritative via tower/shared registration (spi-xplwy). The
//   reconciler reads Guild.Spec.Repo and uses it verbatim.
// - It does NOT build wizard pod specs. The pod-builder
//   (operator/controllers/agent_monitor.go + future
//   operator/internal/builders/) owns the mount wiring that consumes
//   the PVC this reconciler provisions.
// - It does NOT redefine the runtime contract. Identity/workspace
//   vocabulary stays with spi-xplwy.
//
// Garbage collection
// ------------------
// The PVC and refresh Job both carry an owner reference pointing at
// the WizardGuild. When the guild is deleted, the Kubernetes garbage
// collector cascades the delete through the PVC and any lingering
// refresh Jobs (and their Pods) without extra work in this
// reconciler.
package controllers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/go-logr/logr"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	spirev1 "github.com/awell-health/spire/operator/api/v1alpha1"
	"github.com/awell-health/spire/pkg/agent"
)

// cacheRevisionSHARegex enforces the shape of a git commit SHA on the
// reader side: a 7-to-64 char lowercase hex string. The validator is
// operator-local on purpose — pkg/git intentionally exposes no public
// SHA-validator helper and we don't want to force every caller to
// import pkg/git for a trivial string-shape check.
var cacheRevisionSHARegex = regexp.MustCompile(`^[0-9a-f]{7,64}$`)

// cacheRefreshMountPath is the refresh Job's container-side mount point
// for the cache PVC. Wizard pods mount the same PVC at
// agent.CacheMountPath (different mount point, same volume). The
// mount point is refresh-Job-internal and not part of the intra-PVC
// contract workers observe — workers see paths relative to their own
// mount.
const cacheRefreshMountPath = "/cache"

// cacheRefreshTerminationLog is the path the refresh Job writes the
// resolved revision to so the reconciler can read it back via the
// Pod's termination message. It is the kubelet-provided path; wiring
// it via env lets tests (and any non-Pod harness) override it with a
// writable path.
const cacheRefreshTerminationLog = "/dev/termination-log"

// cacheRefreshContainerName is the container the refresh script runs
// in. The Job builder stamps this on the Job's PodSpec; the reader
// path filters Pod ContainerStatuses by the same name to pick the
// right termination message.
const cacheRefreshContainerName = "refresh"

// jobNameLabel is the Kubernetes-stamped label on Pods the Job
// controller spawns; the reconciler uses it to list refresh Pods by
// owning Job name without traversing OwnerReferences manually.
const jobNameLabel = "job-name"

// cacheRefreshEnv returns the container-side env vars that the refresh
// script reads. They are sourced from the canonical
// agent.CacheMirrorSubdir / CacheRevisionMarkerName /
// CacheRevisionTmpMarkerName constants so the script and the worker
// consume the same values — a single Go source of truth crosses the
// Go ↔ shell boundary rather than two sets of string literals.
//
// SPIRE_CACHE_MOUNT keeps the mount path in env so the script doesn't
// hardcode "/cache". SPIRE_CACHE_TERMINATION_LOG does the same for the
// termination-message write, which is /dev/termination-log inside a
// Pod but has to be a writable temp file under a test harness.
func cacheRefreshEnv(mountPath string) []corev1.EnvVar {
	if mountPath == "" {
		mountPath = cacheRefreshMountPath
	}
	return []corev1.EnvVar{
		{Name: "SPIRE_CACHE_MOUNT", Value: mountPath},
		{Name: "SPIRE_CACHE_MIRROR_SUBDIR", Value: agent.CacheMirrorSubdir},
		{Name: "SPIRE_CACHE_REVISION_MARKER", Value: agent.CacheRevisionMarkerName},
		{Name: "SPIRE_CACHE_REVISION_TMP_MARKER", Value: agent.CacheRevisionTmpMarkerName},
		{Name: "SPIRE_CACHE_TERMINATION_LOG", Value: cacheRefreshTerminationLog},
	}
}

// cacheRefreshScript is the shell program the refresh Job runs.
//
// Uses `git clone --mirror` on a fresh PVC and `git fetch` on an
// existing mirror, then writes the resolved HEAD commit SHA to the
// revision marker file via an atomic rename. The same SHA is also
// echoed to /dev/termination-log so the reconciler can surface it on
// CacheStatus.Revision without mounting the PVC.
//
// Intra-PVC layout is env-driven: the script references only the
// SPIRE_CACHE_* env vars set by cacheRefreshEnv — no layout string
// literals live here. That is deliberate: the worker side in
// pkg/agent/cache_bootstrap.go consumes the same constants, so the
// producer and consumer cannot drift apart.
//
// BranchPin, when set, constrains the resolved revision to
// `refs/heads/<BranchPin>`. When unset, HEAD of the mirror's
// symbolic HEAD (the upstream's default branch) is used.
const cacheRefreshScript = `set -eu
MOUNT="$SPIRE_CACHE_MOUNT"
MIRROR="$MOUNT/$SPIRE_CACHE_MIRROR_SUBDIR"
if [ -d "$MIRROR/objects" ]; then
  echo "refreshing existing mirror at $MIRROR" >&2
  cd "$MIRROR"
  git fetch --prune --prune-tags --tags origin
else
  echo "cloning $SPIRE_REPO_URL into $MIRROR" >&2
  mkdir -p "$MOUNT"
  git clone --mirror "$SPIRE_REPO_URL" "$MIRROR"
  cd "$MIRROR"
fi
if [ -n "${SPIRE_BRANCH_PIN:-}" ]; then
  REV=$(git rev-parse "refs/heads/$SPIRE_BRANCH_PIN")
else
  DEFAULT=$(git symbolic-ref HEAD 2>/dev/null | sed 's|^refs/heads/||' || true)
  if [ -z "$DEFAULT" ]; then
    # symbolic HEAD can be missing on a bare mirror after a force-fetch;
    # fall back to resolving HEAD directly.
    REV=$(git rev-parse HEAD)
  else
    REV=$(git rev-parse "refs/heads/$DEFAULT")
  fi
fi
TMP="$MOUNT/$SPIRE_CACHE_REVISION_TMP_MARKER"
FINAL="$MOUNT/$SPIRE_CACHE_REVISION_MARKER"
printf '%s\n' "$REV" > "$TMP"
sync
mv "$TMP" "$FINAL"
printf '%s' "$REV" > "$SPIRE_CACHE_TERMINATION_LOG"
echo "cache refreshed to $REV" >&2
`

const (
	// cacheDefaultSize is the deployment-time fallback when a guild's
	// CacheSpec.Size is unset. Matches the helm chart default
	// (`cache.defaultSize` in helm/spire/values.yaml) — keep these in
	// lockstep if either is changed.
	cacheDefaultSize = "10Gi"

	// cacheDefaultRefreshInterval matches the CRD default
	// (`+kubebuilder:default="5m"` on CacheSpec.RefreshInterval).
	cacheDefaultRefreshInterval = 5 * time.Minute

	// cacheSpecHashAnnotation is stamped onto the PVC and the refresh
	// Job so the reconciler can detect CacheSpec changes by comparing
	// the desired hash with the observed annotation. When the hash
	// drifts, the existing refresh Job is deleted and a new one is
	// created with the updated spec.
	cacheSpecHashAnnotation = "spire.awell.io/cache-spec-hash"

	// cacheGuildLabel links a PVC or refresh Job back to the owning
	// WizardGuild for list/select convenience.
	cacheGuildLabel = "spire.awell.io/guild"

	// cacheRoleLabel identifies the object's role in the cache
	// lifecycle (pvc | refresh-job).
	cacheRoleLabel = "spire.awell.io/cache-role"
)

// RBAC for the cache reconciler. Derived from r.Client calls on
// WizardGuild (list for cycle fan-out, update for finalizer persistence,
// status update for CacheStatus), batch/Job (full lifecycle for refresh
// Jobs), core/PVC (create + get in ensurePVC), and core/Pod (list in
// revisionFromJob / collectJobFailureSnapshot). Watch is required on
// read paths because the reconciler uses the controller-manager cached
// client, which backs typed reads with an informer.
//+kubebuilder:rbac:groups=spire.awell.io,resources=wizardguilds,verbs=get;list;watch;update
//+kubebuilder:rbac:groups=spire.awell.io,resources=wizardguilds/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;patch;delete
//+kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;watch;create
//+kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch

// CacheReconciler reconciles a WizardGuild's CacheSpec into a guild-owned
// PVC plus a refresh Job, and surfaces state on WizardGuild.Status.Cache.
type CacheReconciler struct {
	Client    client.Client
	Log       logr.Logger
	Namespace string
	// Interval is the cycle period for reconciling caches. The per-guild
	// CacheSpec.RefreshInterval controls how often the Job itself runs;
	// this Interval just controls how often the controller wakes up to
	// check whether a refresh is due.
	Interval time.Duration

	// GitImage is the container image used by refresh Jobs. Must ship
	// git and a POSIX shell. Default: "alpine/git:latest".
	GitImage string

	// ChartCacheStorageClass / ChartCacheSize / ChartCacheAccessMode are
	// the deployment-time defaults that back CacheSpec fields when left
	// unset. The operator startup wires these from the chart's
	// `cache.storageClassName` / `cache.defaultSize` /
	// `cache.defaultAccessMode` values (see helm/spire/values.yaml and
	// the `spire.cachePVCSpec` / `spire.cacheDefaults*` helpers in
	// helm/spire/templates/_helpers.tpl).
	ChartCacheStorageClass string
	ChartCacheSize         string
	ChartCacheAccessMode   corev1.PersistentVolumeAccessMode

	// Identity fields reused for structured logging — same startup
	// inputs as AgentMonitor (docs/design/spi-xplwy-runtime-contract.md
	// §1.1). Never read from pod env at reconcile time.
	Database string
	Prefix   string

	// PinnedStore is the bead-graph backend the reconciler uses to
	// maintain the per-guild pinned identity bead (spi-2bgsm). nil
	// means "use the package-level pkg/store" — main.go leaves it
	// unset and the reconciler resolves it lazily on first use; tests
	// inject a fake.
	PinnedStore pinnedIdentityStore

	// WispStore is the bead-graph backend the reconciler uses to file
	// cache-refresh failure wisps (spi-htay5). Same lazy-resolution
	// contract as PinnedStore: nil falls back to pkg/store, tests
	// inject a fake.
	WispStore wispFilingStore
}

// pinnedStore returns r.PinnedStore, falling back to the
// package-level pkg/store wrapper when unset. Lazy resolution lets
// tests construct CacheReconciler without touching the global store
// singleton, and main.go does not need to know about the interface.
func (r *CacheReconciler) pinnedStore() pinnedIdentityStore {
	if r.PinnedStore != nil {
		return r.PinnedStore
	}
	return defaultPinnedIdentityStore{}
}

// wispStore returns r.WispStore, falling back to the package-level
// pkg/store wrapper when unset. Mirrors the pinnedStore lazy-resolution
// contract — tests inject a fake, prod leaves it nil.
func (r *CacheReconciler) wispStore() wispFilingStore {
	if r.WispStore != nil {
		return r.WispStore
	}
	return defaultWispFilingStore{}
}

// Start implements controller-runtime's Runnable interface.
func (r *CacheReconciler) Start(ctx context.Context) error {
	r.Run(ctx)
	return nil
}

// Run is the main loop — call from the operator's main.go via mgr.Add.
func (r *CacheReconciler) Run(ctx context.Context) {
	r.Log.Info("cache reconciler starting",
		"interval", r.Interval,
		"tower", r.Database,
		"prefix", r.Prefix,
		"backend", "operator-k8s")
	ticker := time.NewTicker(r.Interval)
	defer ticker.Stop()

	r.cycle(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.cycle(ctx)
		}
	}
}

func (r *CacheReconciler) cycle(ctx context.Context) {
	var guilds spirev1.WizardGuildList
	if err := r.Client.List(ctx, &guilds, client.InNamespace(r.Namespace)); err != nil {
		r.Log.Error(err, "failed to list guilds",
			"tower", r.Database, "prefix", r.Prefix, "backend", "operator-k8s")
		return
	}
	for i := range guilds.Items {
		g := &guilds.Items[i]
		// Guilds with no Cache spec are skipped, EXCEPT when they still
		// carry the pinned-identity finalizer — those need finalizer
		// drainage on deletion regardless of whether the cache spec
		// was later removed (otherwise the CR would be wedged).
		if g.Spec.Cache == nil && !controllerutil.ContainsFinalizer(g, pinnedIdentityFinalizer) {
			continue
		}
		r.reconcileGuild(ctx, g)
	}
}

// reconcileGuild walks the create → refresh → status-update path for a
// single guild. Owner references on the PVC and Job mean guild delete
// cascades through kube GC; the bead-graph cleanup (pinned identity +
// caused-by wisps) runs here under the pinned-identity finalizer.
func (r *CacheReconciler) reconcileGuild(ctx context.Context, guild *spirev1.WizardGuild) {
	log := r.Log.WithValues(
		"guild", guild.Name,
		"tower", r.Database, "prefix", r.Prefix,
		"backend", "operator-k8s")

	// Deletion path: if our finalizer is present, drain the bead graph
	// before letting the Kubernetes GC reap the CR. PVC + refresh Job
	// still cascade via owner references — this branch only owns the
	// pinned-identity cleanup, not the k8s objects.
	if guild.DeletionTimestamp != nil {
		if !controllerutil.ContainsFinalizer(guild, pinnedIdentityFinalizer) {
			return
		}
		if err := finalizePinnedIdentity(ctx, r.pinnedStore(), guild); err != nil {
			log.Error(err, "pinned identity finalizer failed; will retry on next cycle",
				"pinned_id", guild.Status.PinnedIdentityBeadID)
			return
		}
		controllerutil.RemoveFinalizer(guild, pinnedIdentityFinalizer)
		if err := r.Client.Update(ctx, guild); err != nil {
			log.Error(err, "failed to remove pinned-identity finalizer")
		}
		return
	}

	// Cache spec was removed without a CR delete — leave the finalizer
	// and pinned bead in place (re-enabling Cache picks the same bead
	// up via idempotent ensurePinnedIdentity), and skip the rest of
	// the cache PVC/Job reconciliation since there's nothing to own.
	if guild.Spec.Cache == nil {
		return
	}

	if guild.Spec.Repo == "" {
		r.patchCacheStatus(ctx, guild, &spirev1.CacheStatus{
			Phase:        cachePhaseFailed,
			RefreshError: "WizardGuild.Spec.Repo is empty; cannot seed cache",
		})
		log.Error(nil, "guild has CacheSpec but no Repo; skipping")
		return
	}

	// Add the pinned-identity finalizer before any bead-graph
	// side-effects so a crash between bead create and finalizer install
	// can't leave the bead orphaned. We continue (not return) after a
	// successful add: this is a polling reconciler, not a Reconcile()
	// loop with a requeue, so deferring the rest of the work would
	// stall PVC/Job creation by a full polling interval. The in-memory
	// guild is mutated by AddFinalizer + Update so subsequent
	// Status().Update calls below carry the right resourceVersion.
	if !controllerutil.ContainsFinalizer(guild, pinnedIdentityFinalizer) {
		controllerutil.AddFinalizer(guild, pinnedIdentityFinalizer)
		if err := r.Client.Update(ctx, guild); err != nil {
			log.Error(err, "failed to add pinned-identity finalizer")
			return
		}
	}

	// Ensure the pinned identity bead exists and Status carries its ID.
	beadID, err := ensurePinnedIdentity(ctx, r.pinnedStore(), guild)
	if err != nil {
		log.Error(err, "failed to ensure pinned identity bead")
		// Surface the error on CacheStatus so operators see it without
		// digging through controller logs. Status updates are
		// best-effort here; the next cycle will retry the bead create.
		r.patchCacheStatus(ctx, guild, &spirev1.CacheStatus{
			Phase:        cachePhaseFailed,
			RefreshError: fmt.Sprintf("ensure pinned identity bead: %v", err),
		})
		return
	}
	if guild.Status.PinnedIdentityBeadID != beadID {
		guild.Status.PinnedIdentityBeadID = beadID
		if err := r.Client.Status().Update(ctx, guild); err != nil {
			log.Error(err, "failed to stamp Status.PinnedIdentityBeadID",
				"pinned_id", beadID)
			// The next cycle will see the bead in the store and re-stamp
			// idempotently — keep going so the cache PVC/Job still
			// reconcile this cycle.
		}
	}

	pvc, err := r.ensurePVC(ctx, guild)
	if err != nil {
		r.patchCacheStatus(ctx, guild, &spirev1.CacheStatus{
			Phase:        cachePhaseFailed,
			RefreshError: fmt.Sprintf("PVC reconcile failed: %v", err),
		})
		log.Error(err, "failed to ensure guild cache PVC")
		return
	}

	desiredHash := specHash(guild.Spec.Cache, guild.Spec.Repo, guild.Spec.RepoBranch)
	job, err := r.ensureRefreshJob(ctx, guild, pvc, desiredHash)
	if err != nil {
		r.patchCacheStatus(ctx, guild, &spirev1.CacheStatus{
			Phase:        cachePhaseFailed,
			RefreshError: fmt.Sprintf("refresh Job reconcile failed: %v", err),
		})
		log.Error(err, "failed to ensure guild cache refresh Job")
		return
	}

	// File a wisp recovery bead when the refresh Job has reached a
	// permanent terminal failure (BackoffLimitExceeded / DeadlineExceeded).
	// Idempotent via (a) the spire.awell.io/wisp-id annotation on the Job
	// for per-generation dedupe and (b) a job_uid metadata query inside
	// fileWispForCacheFailure that covers the post-create-pre-annotate
	// race. See cache_recovery.go for the full contract.
	if isRefreshJobBackoffExhausted(job) {
		if err := r.dispatchCacheRecovery(ctx, guild, job); err != nil {
			log.Error(err, "failed to dispatch cache recovery wisp")
			// Fall through to status derivation so the CR still reflects
			// the Failed phase + the underlying refresh error. The next
			// reconcile cycle will retry wisp filing — the Job is still
			// permanent-failed, the annotation is still absent.
		}
	}

	newStatus := r.deriveStatus(ctx, guild, job)
	r.patchCacheStatus(ctx, guild, newStatus)
}

// dispatchCacheRecovery files a wisp recovery bead for a permanently
// failed refresh Job and stamps the wisp ID on the Job annotation for
// idempotent reconcile. The helper is a thin adapter: it resolves the
// pinned identity bead ID (owned by spi-2bgsm), short-circuits when the
// Job is already annotated, and delegates the wisp-graph work to
// fileWispForCacheFailure (owned by this task).
//
// Returns nil (with no error) when the pinned identity bead has not
// yet been stamped — the reconciler requeues naturally on its
// polling interval, so a missing pinned ID is a "not yet" rather than
// a fatal state.
func (r *CacheReconciler) dispatchCacheRecovery(
	ctx context.Context,
	guild *spirev1.WizardGuild,
	job *batchv1.Job,
) error {
	if existing := job.Annotations[cacheWispIDAnnotation]; existing != "" {
		// Already filed for this Job generation. Nothing to do until a
		// new Job (new UID, annotation-less) is created for the next
		// refresh attempt.
		return nil
	}

	pinnedID, err := resolvePinnedIdentityID(guild)
	if err != nil {
		// Pinned identity not yet provisioned by spi-2bgsm path — the
		// next cycle will run ensurePinnedIdentity and stamp Status, at
		// which point this branch is unblocked.
		r.Log.V(1).Info("cache recovery waiting for pinned identity",
			"guild", guild.Name, "job", job.Name,
			"tower", r.Database, "prefix", r.Prefix, "backend", "operator-k8s")
		return nil
	}

	wispID, err := fileWispForCacheFailure(ctx, r.wispStore(), r.Client, guild, job, pinnedID)
	if err != nil {
		return fmt.Errorf("file wisp for %s/%s: %w", guild.Namespace, guild.Name, err)
	}

	if err := r.annotateJobWithWispID(ctx, job, wispID); err != nil {
		// Don't propagate — the wisp is filed and fileWispForCacheFailure's
		// entry guard will return the existing ID on the next cycle.
		// Logging at Error level keeps the incident visible without
		// wedging the status-update path.
		r.Log.Error(err, "failed to annotate refresh Job with wisp ID",
			"guild", guild.Name, "job", job.Name, "wisp_id", wispID,
			"tower", r.Database, "prefix", r.Prefix, "backend", "operator-k8s")
	}

	r.Log.Info("filed cache-refresh recovery wisp",
		"guild", guild.Name, "job", job.Name, "wisp_id", wispID, "pinned_id", pinnedID,
		"tower", r.Database, "prefix", r.Prefix, "backend", "operator-k8s")
	return nil
}

// annotateJobWithWispID stamps the Job with the wisp ID so subsequent
// reconciles short-circuit the filing path. Patch semantics (rather
// than Update) avoid racing with the Job controller's own status
// writes.
func (r *CacheReconciler) annotateJobWithWispID(
	ctx context.Context,
	job *batchv1.Job,
	wispID string,
) error {
	patch := client.MergeFrom(job.DeepCopy())
	if job.Annotations == nil {
		job.Annotations = map[string]string{}
	}
	job.Annotations[cacheWispIDAnnotation] = wispID
	return r.Client.Patch(ctx, job, patch)
}

// ensurePVC creates the guild cache PVC when missing. When present, it
// leaves the existing PVC alone — PVCs are immutable for size/mode on
// most storage classes, so updating them is out of scope for the first
// cut. A future iteration can add a resize path once we know which
// storage classes are in play.
func (r *CacheReconciler) ensurePVC(ctx context.Context, guild *spirev1.WizardGuild) (*corev1.PersistentVolumeClaim, error) {
	name := pvcName(guild.Name)
	var pvc corev1.PersistentVolumeClaim
	err := r.Client.Get(ctx, client.ObjectKey{Namespace: r.Namespace, Name: name}, &pvc)
	if err == nil {
		return &pvc, nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, err
	}

	size := guild.Spec.Cache.Size
	if size.IsZero() {
		size = r.chartDefaultSize()
	}
	accessMode := guild.Spec.Cache.AccessMode
	if accessMode == "" {
		accessMode = r.chartDefaultAccessMode()
	}

	pvcSpec := corev1.PersistentVolumeClaimSpec{
		AccessModes: []corev1.PersistentVolumeAccessMode{accessMode},
		Resources: corev1.VolumeResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceStorage: size,
			},
		},
	}
	// StorageClassName: guild override wins; chart default next.
	// Empty string is deliberately NOT passed through — Kubernetes
	// treats explicit empty as "disable dynamic provisioning" rather
	// than "use cluster default" (matching the
	// `spire.cacheDefaultStorageClassName` helper semantics).
	sc := guild.Spec.Cache.StorageClassName
	if sc == "" {
		sc = r.ChartCacheStorageClass
	}
	if sc != "" {
		pvcSpec.StorageClassName = &sc
	}

	newPVC := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: r.Namespace,
			Labels: map[string]string{
				cacheGuildLabel:            guild.Name,
				cacheRoleLabel:             "pvc",
				"app.kubernetes.io/name":   "spire-guild-cache",
				"app.kubernetes.io/part-of": "spire",
			},
			Annotations: map[string]string{
				cacheSpecHashAnnotation: specHash(guild.Spec.Cache, guild.Spec.Repo, guild.Spec.RepoBranch),
			},
			OwnerReferences: []metav1.OwnerReference{ownerRefFor(guild)},
		},
		Spec: pvcSpec,
	}
	if err := r.Client.Create(ctx, newPVC); err != nil {
		return nil, err
	}
	r.Log.Info("created guild cache PVC",
		"guild", guild.Name, "pvc", name,
		"tower", r.Database, "prefix", r.Prefix, "backend", "operator-k8s")
	return newPVC, nil
}

// ensureRefreshJob materializes (or recreates) the per-guild refresh
// Job. It returns the Job the reconciler should inspect to derive
// status. A nil return without error means no Job exists yet and a new
// one was just created — callers treat that as "refreshing".
func (r *CacheReconciler) ensureRefreshJob(
	ctx context.Context,
	guild *spirev1.WizardGuild,
	pvc *corev1.PersistentVolumeClaim,
	desiredHash string,
) (*batchv1.Job, error) {
	name := refreshJobName(guild.Name)
	var existing batchv1.Job
	err := r.Client.Get(ctx, client.ObjectKey{Namespace: r.Namespace, Name: name}, &existing)
	switch {
	case apierrors.IsNotFound(err):
		return r.createRefreshJob(ctx, guild, pvc, desiredHash, name)

	case err != nil:
		return nil, err
	}

	observedHash := existing.Annotations[cacheSpecHashAnnotation]

	// Spec change → recreate the Job with the new hash so the refresh
	// reflects the updated CacheSpec / repo URL / branch.
	if observedHash != desiredHash {
		if err := r.deleteJob(ctx, &existing); err != nil {
			return nil, err
		}
		return r.createRefreshJob(ctx, guild, pvc, desiredHash, name)
	}

	// Still running or just completed — use as-is for status derivation.
	if !isJobFinished(&existing) {
		return &existing, nil
	}

	// Job has finished. If the guild's refresh interval has elapsed
	// since completion, replace the Job with a fresh run. Otherwise,
	// keep it around so CacheStatus reflects the last completion.
	due, next := r.isRefreshDue(guild, &existing)
	if !due {
		r.Log.V(1).Info("refresh not due yet",
			"guild", guild.Name,
			"next_refresh", next,
			"tower", r.Database, "prefix", r.Prefix, "backend", "operator-k8s")
		return &existing, nil
	}
	if err := r.deleteJob(ctx, &existing); err != nil {
		return nil, err
	}
	return r.createRefreshJob(ctx, guild, pvc, desiredHash, name)
}

func (r *CacheReconciler) createRefreshJob(
	ctx context.Context,
	guild *spirev1.WizardGuild,
	pvc *corev1.PersistentVolumeClaim,
	desiredHash string,
	name string,
) (*batchv1.Job, error) {
	branchPin := ""
	if guild.Spec.Cache.BranchPin != nil {
		branchPin = *guild.Spec.Cache.BranchPin
	}

	// BackoffLimit 2 so transient git/network errors auto-retry without
	// spamming the reconciler; TTLSecondsAfterFinished set so successful
	// Jobs are reaped by the Job controller once status is observed.
	backoffLimit := int32(2)
	ttl := int32(3600) // reap after 1h, giving the reconciler time to read status

	// cacheReadOnlyAccess tracks whether the PVC's access mode allows the
	// refresh Job to mount RW. Even ReadOnlyMany volumes accept a
	// ReadWrite mount from a single writer (the refresh Job), but some
	// CSI drivers enforce read-only at the volume level. We always mount
	// RW from the refresh Job — wizard pods are the ones that mount
	// read-only.
	image := r.GitImage
	if image == "" {
		image = "alpine/git:latest"
	}

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: r.Namespace,
			Labels: map[string]string{
				cacheGuildLabel:            guild.Name,
				cacheRoleLabel:             "refresh-job",
				"app.kubernetes.io/name":   "spire-guild-cache-refresh",
				"app.kubernetes.io/part-of": "spire",
			},
			Annotations: map[string]string{
				cacheSpecHashAnnotation: desiredHash,
			},
			OwnerReferences: []metav1.OwnerReference{ownerRefFor(guild)},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoffLimit,
			TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						cacheGuildLabel: guild.Name,
						cacheRoleLabel:  "refresh-job",
					},
				},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Volumes: []corev1.Volume{{
						Name: "cache",
						VolumeSource: corev1.VolumeSource{
							PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
								ClaimName: pvc.Name,
							},
						},
					}},
					Containers: []corev1.Container{{
						Name:    cacheRefreshContainerName,
						Image:   image,
						Command: []string{"/bin/sh", "-c", cacheRefreshScript},
						// Env mixes two concerns: (a) repo identity
						// inputs the script reads (SPIRE_REPO_URL,
						// SPIRE_BRANCH_PIN), (b) the canonical intra-PVC
						// layout env vars from cacheRefreshEnv.
						// Separating (b) into its own helper keeps the
						// producer/consumer contract in one place; any
						// layout drift would now require editing the
						// agent.Cache* constants, which the worker side
						// also reads.
						Env: append([]corev1.EnvVar{
							{Name: "SPIRE_REPO_URL", Value: guild.Spec.Repo},
							{Name: "SPIRE_BRANCH_PIN", Value: branchPin},
						}, cacheRefreshEnv(cacheRefreshMountPath)...),
						VolumeMounts: []corev1.VolumeMount{{
							Name:      "cache",
							MountPath: cacheRefreshMountPath,
						}},
						TerminationMessagePath:   "/dev/termination-log",
						TerminationMessagePolicy: corev1.TerminationMessageReadFile,
					}},
				},
			},
		},
	}
	if err := r.Client.Create(ctx, job); err != nil {
		return nil, err
	}
	r.Log.Info("created guild cache refresh Job",
		"guild", guild.Name, "job", name, "repo", guild.Spec.Repo, "branch_pin", branchPin,
		"tower", r.Database, "prefix", r.Prefix, "backend", "operator-k8s")
	return job, nil
}

// deleteJob deletes the Job and its dependent Pods (propagation
// policy=Background so the reconciler doesn't block on pod teardown).
func (r *CacheReconciler) deleteJob(ctx context.Context, job *batchv1.Job) error {
	prop := metav1.DeletePropagationBackground
	err := r.Client.Delete(ctx, job, &client.DeleteOptions{PropagationPolicy: &prop})
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// isRefreshDue reports whether CacheSpec.RefreshInterval has elapsed
// since the last Job's completion time. The second return is the
// absolute time of the next scheduled refresh (for logging).
func (r *CacheReconciler) isRefreshDue(guild *spirev1.WizardGuild, job *batchv1.Job) (bool, time.Time) {
	interval := guild.Spec.Cache.RefreshInterval.Duration
	if interval == 0 {
		interval = cacheDefaultRefreshInterval
	}
	completed := jobCompletionTime(job)
	if completed == nil {
		// Job is terminal but the completion timestamp isn't populated
		// (Failed Jobs can lack CompletionTime). Treat as "due now" so
		// the next cycle retries.
		return true, time.Now()
	}
	next := completed.Add(interval)
	return !time.Now().Before(next), next
}

// deriveStatus maps the refresh Job's state onto a CacheStatus.
func (r *CacheReconciler) deriveStatus(ctx context.Context, guild *spirev1.WizardGuild, job *batchv1.Job) *spirev1.CacheStatus {
	status := &spirev1.CacheStatus{}

	// Preserve Revision from the previous successful refresh so
	// transient Failed/Refreshing transitions don't drop it.
	prev := guild.Status.Cache
	if prev != nil {
		status.Revision = prev.Revision
		status.LastRefreshTime = prev.LastRefreshTime
	}

	switch {
	case job == nil:
		status.Phase = cachePhasePending
		return status

	case isJobSucceeded(job):
		status.Phase = cachePhaseReady
		if ct := jobCompletionTime(job); ct != nil {
			status.LastRefreshTime = ct
		}
		if rev := r.revisionFromJob(ctx, job); rev != "" {
			status.Revision = rev
		}
		status.RefreshError = ""
		return status

	case isJobFailed(job):
		status.Phase = cachePhaseFailed
		status.RefreshError = failureMessageFromJob(job)
		return status

	default:
		status.Phase = cachePhaseRefreshing
		return status
	}
}

// patchCacheStatus writes newStatus onto guild.Status.Cache and pushes
// the change via Status().Update. Errors are logged; we don't fail the
// reconcile just because a status write lost a race.
func (r *CacheReconciler) patchCacheStatus(ctx context.Context, guild *spirev1.WizardGuild, newStatus *spirev1.CacheStatus) {
	if cacheStatusEqual(guild.Status.Cache, newStatus) {
		return
	}
	guild.Status.Cache = newStatus
	if err := r.Client.Status().Update(ctx, guild); err != nil {
		r.Log.Error(err, "failed to update guild cache status",
			"guild", guild.Name,
			"phase", newStatus.Phase,
			"tower", r.Database, "prefix", r.Prefix, "backend", "operator-k8s")
		return
	}
	// Log a line that maps Phase → condition type so operators can grep
	// for the condition even while a WizardGuildStatus.Conditions field
	// is not yet wired on the CRD. The condition type constants live in
	// operator/api/v1alpha1/wizardguild_types.go.
	r.Log.Info("updated guild cache status",
		"guild", guild.Name,
		"phase", newStatus.Phase,
		"condition", conditionForPhase(newStatus.Phase),
		"revision", newStatus.Revision,
		"refresh_error", newStatus.RefreshError,
		"tower", r.Database, "prefix", r.Prefix, "backend", "operator-k8s")
}

func (r *CacheReconciler) chartDefaultSize() resource.Quantity {
	if r.ChartCacheSize != "" {
		if q, err := resource.ParseQuantity(r.ChartCacheSize); err == nil {
			return q
		}
	}
	return resource.MustParse(cacheDefaultSize)
}

func (r *CacheReconciler) chartDefaultAccessMode() corev1.PersistentVolumeAccessMode {
	if r.ChartCacheAccessMode != "" {
		return r.ChartCacheAccessMode
	}
	return corev1.ReadOnlyMany
}

// --- phase / condition vocabulary ----------------------------------------

// CacheStatus.Phase enum values (also enforced by
// +kubebuilder:validation:Enum in the CRD types).
const (
	cachePhasePending    = "Pending"
	cachePhaseReady      = "Ready"
	cachePhaseRefreshing = "Refreshing"
	cachePhaseFailed     = "Failed"
)

// conditionForPhase maps a CacheStatus.Phase enum value to the
// corresponding CRD-declared condition type (spirev1.CacheReady /
// CacheRefreshing / CacheFailed). Returns "" for Pending, which has
// no dedicated condition.
func conditionForPhase(phase string) string {
	switch phase {
	case cachePhaseReady:
		return spirev1.CacheReady
	case cachePhaseRefreshing:
		return spirev1.CacheRefreshing
	case cachePhaseFailed:
		return spirev1.CacheFailed
	default:
		return ""
	}
}

// --- helpers -------------------------------------------------------------

// pvcName is `<guild-name>-repo-cache` per the spi-sn7o3 naming
// convention. Kept as a function so other controllers (pod-builder) can
// share the same string in a later task without re-deriving it.
func pvcName(guildName string) string {
	return sanitizeK8sName(guildName) + "-repo-cache"
}

func refreshJobName(guildName string) string {
	return sanitizeK8sName(guildName) + "-repo-cache-refresh"
}

// ownerRefFor builds an OwnerReference pointing at the given
// WizardGuild. Controller=true so Kubernetes GC cascades the delete.
// BlockOwnerDeletion is deliberately NOT set — PVC-bound volumes can
// be slow to drain and we don't want to block guild delete on a
// misbehaving CSI driver.
func ownerRefFor(guild *spirev1.WizardGuild) metav1.OwnerReference {
	t := true
	return metav1.OwnerReference{
		APIVersion: spirev1.SchemeGroupVersion.String(),
		Kind:       "WizardGuild",
		Name:       guild.Name,
		UID:        guild.UID,
		Controller: &t,
	}
}

// specHash returns a deterministic digest of the cache-relevant parts
// of the guild spec. Changes to Repo, RepoBranch, or any CacheSpec
// field flip the hash and cause the reconciler to recreate the
// refresh Job.
func specHash(cs *spirev1.CacheSpec, repo, branch string) string {
	h := sha256.New()
	if cs != nil {
		fmt.Fprintf(h, "sc=%s\n", cs.StorageClassName)
		fmt.Fprintf(h, "size=%s\n", cs.Size.String())
		fmt.Fprintf(h, "am=%s\n", cs.AccessMode)
		fmt.Fprintf(h, "ri=%s\n", cs.RefreshInterval.Duration)
		bp := ""
		if cs.BranchPin != nil {
			bp = *cs.BranchPin
		}
		fmt.Fprintf(h, "bp=%s\n", bp)
	}
	fmt.Fprintf(h, "repo=%s\n", repo)
	fmt.Fprintf(h, "repobranch=%s\n", branch)
	return hex.EncodeToString(h.Sum(nil))
}

func isJobFinished(job *batchv1.Job) bool {
	return isJobSucceeded(job) || isJobFailed(job)
}

func isJobSucceeded(job *batchv1.Job) bool {
	if job == nil {
		return false
	}
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobComplete && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func isJobFailed(job *batchv1.Job) bool {
	if job == nil {
		return false
	}
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func jobCompletionTime(job *batchv1.Job) *metav1.Time {
	if job == nil {
		return nil
	}
	if job.Status.CompletionTime != nil {
		t := *job.Status.CompletionTime
		return &t
	}
	for _, c := range job.Status.Conditions {
		if (c.Type == batchv1.JobComplete || c.Type == batchv1.JobFailed) && c.Status == corev1.ConditionTrue {
			t := c.LastTransitionTime
			return &t
		}
	}
	return nil
}

// revisionFromJob reads the resolved commit SHA from the refresh
// pod's `refresh` container termination message. The refresh script
// writes the SHA to /dev/termination-log (see cacheRefreshScript),
// which the kubelet surfaces on ContainerStatus.State.Terminated.Message.
//
// Returns "" when:
//   - the Pod list call errors,
//   - no Pods are owned by the Job (e.g. kube GC reaped them after
//     completion — accepted cost; deriveStatus preserves the previous
//     Revision when it can, and the next refresh repopulates it),
//   - the container has not produced a terminated state yet,
//   - or the message does not look like a git SHA.
//
// The reconciler treats an empty return as "leave Revision unchanged"
// in deriveStatus, so a transient pod-list failure never wipes a
// known-good Revision.
func (r *CacheReconciler) revisionFromJob(ctx context.Context, job *batchv1.Job) string {
	if job == nil {
		return ""
	}
	var pods corev1.PodList
	if err := r.Client.List(ctx, &pods,
		client.InNamespace(job.Namespace),
		client.MatchingLabels{jobNameLabel: job.Name},
	); err != nil {
		r.Log.V(1).Info("revisionFromJob: pod list failed",
			"guild_job", job.Name, "err", err.Error(),
			"tower", r.Database, "prefix", r.Prefix, "backend", "operator-k8s")
		return ""
	}
	for _, pod := range pods.Items {
		for _, cs := range pod.Status.ContainerStatuses {
			if cs.Name != cacheRefreshContainerName || cs.State.Terminated == nil {
				continue
			}
			msg := strings.TrimSpace(cs.State.Terminated.Message)
			if cacheRevisionSHARegex.MatchString(msg) {
				return msg
			}
		}
	}
	return ""
}

// failureMessageFromJob returns a short human-readable reason from the
// Job's Failed condition, falling back to a generic label when the
// reason isn't populated.
func failureMessageFromJob(job *batchv1.Job) string {
	if job == nil {
		return "refresh Job missing"
	}
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			if c.Message != "" {
				return c.Message
			}
			if c.Reason != "" {
				return c.Reason
			}
			return "refresh Job failed"
		}
	}
	return "refresh Job failed"
}

func cacheStatusEqual(a, b *spirev1.CacheStatus) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.Phase != b.Phase {
		return false
	}
	if a.Revision != b.Revision {
		return false
	}
	if a.RefreshError != b.RefreshError {
		return false
	}
	if (a.LastRefreshTime == nil) != (b.LastRefreshTime == nil) {
		return false
	}
	if a.LastRefreshTime != nil && !a.LastRefreshTime.Equal(b.LastRefreshTime) {
		return false
	}
	return true
}
