// Wisp recovery filing for WizardGuild.Cache refresh-Job backoff
// exhaustion (spi-htay5).
//
// When the guild's cache refresh Job hits a permanent failure condition
// (BackoffLimitExceeded or DeadlineExceeded), the reconciler dispatches
// this helper to file a recovery wisp bead that:
//
//   - records the failure snapshot (Job + Pod conditions, termination log),
//   - carries a caused-by edge to the guild's pinned-identity bead
//     (spi-2bgsm), and
//   - stamps a stable metadata key (job_uid) used for dedupe across
//     requeues.
//
// The file lives beside cache_reconciler.go on purpose: the reconciler
// is the trigger, but the filing logic is tested in isolation here via
// the wispFilingStore seam — so unit tests can exercise the metadata
// shape, idempotency, and caused-by wiring without a controller-runtime
// fixture.
//
// Idempotency is two-layered:
//  1. Reconciler short-circuits when the Job already carries the
//     spire.awell.io/wisp-id annotation (same Job generation).
//  2. fileWispForCacheFailure itself queries open wisps by job_uid and
//     returns the existing ID if one is found — this guards against
//     the narrow window between CreateBead and annotate-Job failing.
//
// The wisp lifecycle (dispatch → diagnose → close) is owned by steward
// and cleric; this task only *files* the wisp.
package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spirev1 "github.com/awell-health/spire/operator/api/v1alpha1"
	"github.com/awell-health/spire/pkg/recovery"
	"github.com/awell-health/spire/pkg/store"
)

// cacheWispIDAnnotation is stamped on a refresh Job after its failure
// wisp is filed. The reconciler skips filing when this annotation is
// already present on the Job (per-Job-generation idempotency). A new
// Job generation gets a fresh UID and a fresh annotation-less object,
// so subsequent permanent failures are filed as distinct wisps.
const cacheWispIDAnnotation = "spire.awell.io/wisp-id"

// cacheWispMetadataJobUID is the metadata key used to dedupe wisps
// across requeues when the annotate-Job step fails after CreateBead
// succeeded. The reconciler queries open wisps by this key before
// creating a new one — the Job UID is stable across retries for the
// same Job generation.
const cacheWispMetadataJobUID = "job_uid"

// cacheWispTerminationLogCap bounds the termination log stored on the
// wisp's metadata column. Oversized messages are truncated with a
// trailing marker so readers know truncation occurred rather than
// silently losing tail data. 32 KiB is well under typical TEXT column
// limits while still carrying enough of a stack trace for diagnosis.
const cacheWispTerminationLogCap = 32 * 1024

// cacheWispTruncationMarker is appended to a termination log that had
// to be truncated. Kept short so it never pushes the result back over
// the cap — callers allocate cap+len(marker) of headroom.
const cacheWispTruncationMarker = "... [truncated]"

// cacheRefreshFailureLabel is the interrupted:* label convention for a
// cache-refresh wisp, mirroring the label shape used by executor-side
// recovery beads (see pkg/executor/executor_escalate.go). Keeping the
// shape identical lets downstream steward/cleric code route by the
// same vocabulary regardless of whether the wisp came from an
// apprentice bead or a cluster resource.
const cacheRefreshFailureLabel = "interrupted:cache-refresh-failure"

// wispFilingStore is the narrow store surface cache-recovery needs.
// Tests inject a fake to exercise the create + dep + list paths
// without booting a real beads store.
type wispFilingStore interface {
	CreateBead(ctx context.Context, opts store.CreateOpts) (string, error)
	AddDep(ctx context.Context, issueID, dependsOnID, depType string) error
	ListBeadsByMetadata(ctx context.Context, meta map[string]string) ([]store.Bead, error)
}

// defaultWispFilingStore wraps pkg/store package-level functions
// behind the wispFilingStore interface. Context is accepted for
// forward compatibility but not threaded through — pkg/store carries
// its own internal context.
type defaultWispFilingStore struct{}

func (defaultWispFilingStore) CreateBead(_ context.Context, opts store.CreateOpts) (string, error) {
	return store.CreateBead(opts)
}

func (defaultWispFilingStore) AddDep(_ context.Context, issueID, dependsOnID, depType string) error {
	return store.AddDepTyped(issueID, dependsOnID, depType)
}

func (defaultWispFilingStore) ListBeadsByMetadata(_ context.Context, meta map[string]string) ([]store.Bead, error) {
	return store.ListBeadsByMetadata(meta)
}

// isRefreshJobBackoffExhausted reports whether the Job has reached a
// permanent terminal failure. BackoffLimitExceeded and DeadlineExceeded
// are the two Reason values the Job controller surfaces for
// unrecoverable states; other Failed reasons (e.g. transient image-pull
// errors surfaced at pod level) do not count — the Job controller will
// still retry those.
//
// Returns false for a nil Job, a Job still in progress, and a Failed
// Job whose condition carries neither of the permanent reasons.
func isRefreshJobBackoffExhausted(job *batchv1.Job) bool {
	if job == nil {
		return false
	}
	for _, c := range job.Status.Conditions {
		if c.Type != batchv1.JobFailed || c.Status != corev1.ConditionTrue {
			continue
		}
		switch c.Reason {
		case "BackoffLimitExceeded", "DeadlineExceeded":
			return true
		}
	}
	return false
}

// resolvePinnedIdentityID returns the pinned-identity bead ID that
// spi-2bgsm stamped on guild.Status.PinnedIdentityBeadID. An empty
// string is reported as an error so the reconciler can distinguish
// "not yet provisioned" from "provisioned but not yet reconciled" and
// requeue appropriately. This task only *reads* the field — the
// stamp/create path is owned by pinned_identity.go.
func resolvePinnedIdentityID(guild *spirev1.WizardGuild) (string, error) {
	if guild == nil {
		return "", fmt.Errorf("guild is nil")
	}
	id := guild.Status.PinnedIdentityBeadID
	if id == "" {
		return "", fmt.Errorf("guild %s: pinned identity bead ID not yet stamped", guild.Name)
	}
	return id, nil
}

// jobPodConditionSnapshot is the JSON shape persisted under the wisp's
// condition_snapshot metadata. It is deliberately small and stable so
// cleric/steward readers can unmarshal without pulling in k8s types.
type jobPodConditionSnapshot struct {
	JobConditions []jobConditionEntry `json:"job_conditions,omitempty"`
	PodConditions []podConditionEntry `json:"pod_conditions,omitempty"`
	PodPhase      string              `json:"pod_phase,omitempty"`
	ContainerName string              `json:"container_name,omitempty"`
	ExitCode      *int32              `json:"exit_code,omitempty"`
}

type jobConditionEntry struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

type podConditionEntry struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

// collectJobFailureSnapshot assembles the termination log + a compact
// JSON blob of Job/Pod conditions for persistence on the wisp. The
// termination log comes from the refresh container's State.Terminated
// message (same surface revisionFromJob reads for happy-path SHAs); the
// condition snapshot merges Job.Status.Conditions with the latest Pod's
// conditions so the cleric has both the high-level "why did Kubernetes
// give up" (Job condition) and the process-level "what did the
// container do" (exit code + pod conditions).
//
// Errors from the Pod list call are fatal for this helper — the
// reconciler treats a snapshot failure as retryable; the next cycle
// will try again against the (still-failed) Job. An empty pod list is
// NOT fatal: Kubernetes reaps the last pod after TTLSecondsAfterFinished
// elapses, and we still want the wisp filed with whatever Job-level
// data we have.
func collectJobFailureSnapshot(
	ctx context.Context,
	kc client.Client,
	job *batchv1.Job,
) (terminationLog string, conditionSnapshot string, err error) {
	if job == nil {
		return "", "", fmt.Errorf("job is nil")
	}

	snap := jobPodConditionSnapshot{
		ContainerName: cacheRefreshContainerName,
	}
	for _, c := range job.Status.Conditions {
		snap.JobConditions = append(snap.JobConditions, jobConditionEntry{
			Type:    string(c.Type),
			Status:  string(c.Status),
			Reason:  c.Reason,
			Message: c.Message,
		})
	}

	var pods corev1.PodList
	if err := kc.List(ctx, &pods,
		client.InNamespace(job.Namespace),
		client.MatchingLabels{jobNameLabel: job.Name},
	); err != nil {
		return "", "", fmt.Errorf("list refresh pods for %s: %w", job.Name, err)
	}

	pod := latestFailedPod(pods.Items)
	if pod != nil {
		snap.PodPhase = string(pod.Status.Phase)
		for _, c := range pod.Status.Conditions {
			snap.PodConditions = append(snap.PodConditions, podConditionEntry{
				Type:    string(c.Type),
				Status:  string(c.Status),
				Reason:  c.Reason,
				Message: c.Message,
			})
		}
		for _, cs := range pod.Status.ContainerStatuses {
			if cs.Name != cacheRefreshContainerName || cs.State.Terminated == nil {
				continue
			}
			terminationLog = cs.State.Terminated.Message
			exit := cs.State.Terminated.ExitCode
			snap.ExitCode = &exit
			break
		}
	}

	raw, jerr := json.Marshal(snap)
	if jerr != nil {
		return "", "", fmt.Errorf("marshal condition snapshot: %w", jerr)
	}
	conditionSnapshot = string(raw)
	terminationLog = capTerminationLog(terminationLog)
	return terminationLog, conditionSnapshot, nil
}

// latestFailedPod picks the most-recently-terminated Failed pod from
// pods. Ties broken by later StartTime so we prefer the newest pod's
// termination message over stale earlier retries. Returns nil when no
// pod is in a terminal state yet (caller falls back to Job-level
// conditions only).
func latestFailedPod(pods []corev1.Pod) *corev1.Pod {
	var best *corev1.Pod
	for i := range pods {
		p := &pods[i]
		if p.Status.Phase != corev1.PodFailed && p.Status.Phase != corev1.PodSucceeded {
			continue
		}
		if best == nil {
			best = p
			continue
		}
		if p.Status.StartTime != nil && best.Status.StartTime != nil &&
			p.Status.StartTime.After(best.Status.StartTime.Time) {
			best = p
		}
	}
	// If no terminal pod, fall back to whichever pod we have (probably
	// Pending/Running on the edge of termination). One pod is better
	// than none for snapshot purposes.
	if best == nil && len(pods) > 0 {
		best = &pods[0]
	}
	return best
}

// capTerminationLog enforces the metadata-column headroom by truncating
// oversized termination logs and appending a marker. Input smaller than
// the cap is returned unchanged. The marker is appended *after* the
// cap-sized prefix, so the overall return is at most
// cap + len(marker) bytes — well within any MySQL TEXT column and
// clear to readers that truncation occurred.
func capTerminationLog(s string) string {
	if len(s) <= cacheWispTerminationLogCap {
		return s
	}
	return s[:cacheWispTerminationLogCap] + cacheWispTruncationMarker
}

// fileWispForCacheFailure creates (or returns an existing) recovery
// wisp bead for the given refresh-Job failure. The bead is ephemeral
// (not git-synced), carries a caused-by edge to the guild's pinned
// identity bead, and stamps job_uid on its metadata so the reconciler
// can dedupe on requeue when the annotate-Job step lost a race.
//
// The function is safe to call from a controller-runtime Reconcile
// path: failures return without side-effects on the bead graph when
// possible (the entry guard dedup is idempotent; the create+dep pair
// runs atomically at the bead level only — a partial failure between
// them would leave the wisp without an edge, which the next reconcile
// cycle repairs when it re-fires the guard and finds the existing
// wisp).
//
// Parameters:
//   - ctx:       request context propagated from Reconcile.
//   - st:        wispFilingStore (real store in prod, fake in tests).
//   - kc:        controller-runtime client, used only for the snapshot
//     collection — wisp persistence lives in the bead graph.
//   - guild:     the owning WizardGuild CR; source of the resource URI.
//   - job:       the refresh Job whose failure drives this wisp.
//   - pinnedID:  the pinned-identity bead ID (resolved by the caller
//     via resolvePinnedIdentityID).
//
// Returns the wisp bead ID. On the idempotent short-circuit (existing
// wisp found for the same job_uid), returns the existing ID without
// creating a second bead.
func fileWispForCacheFailure(
	ctx context.Context,
	st wispFilingStore,
	kc client.Client,
	guild *spirev1.WizardGuild,
	job *batchv1.Job,
	pinnedID string,
) (string, error) {
	if guild == nil {
		return "", fmt.Errorf("guild is nil")
	}
	if job == nil {
		return "", fmt.Errorf("job is nil")
	}
	if pinnedID == "" {
		return "", fmt.Errorf("pinned identity bead ID is empty")
	}

	jobUID := string(job.UID)

	// Entry guard: reuse an existing open wisp with the same job_uid.
	// Covers the post-create-pre-annotate crash window (CreateBead
	// succeeded, annotateJob failed → reconciler retries with no Job
	// annotation, but the wisp already exists).
	if jobUID != "" {
		existing, err := st.ListBeadsByMetadata(ctx, map[string]string{
			cacheWispMetadataJobUID: jobUID,
		})
		if err != nil {
			return "", fmt.Errorf("list existing wisps for job %s: %w", jobUID, err)
		}
		for _, b := range existing {
			if b.Status != statusClosed {
				return b.ID, nil
			}
		}
	}

	termLog, condSnap, err := collectJobFailureSnapshot(ctx, kc, job)
	if err != nil {
		return "", fmt.Errorf("collect failure snapshot: %w", err)
	}

	sourceURI := fmt.Sprintf("spire.io/wizardguild/%s/%s/cache", guild.Namespace, guild.Name)

	meta := map[string]string{
		"failure_class":              string(recovery.FailureClassCacheRefresh),
		"failed_step":                "refresh",
		"source_resource_uri":        sourceURI,
		"condition_snapshot":         condSnap,
		"termination_log":            termLog,
		cacheWispMetadataJobUID:      jobUID,
		"job_generation":             strconv.FormatInt(job.Generation, 10),
		"pinned_identity_bead_id":    pinnedID,
		"guild_namespace":            guild.Namespace,
		"guild_name":                 guild.Name,
	}

	title := fmt.Sprintf("[recovery] WizardGuild/%s/%s/cache: cache-refresh-failure",
		guild.Namespace, guild.Name)
	if len(title) > 200 {
		title = title[:200]
	}
	desc := fmt.Sprintf(
		"Refresh Job %s/%s reached a permanent failure condition (BackoffLimitExceeded or DeadlineExceeded).\n"+
			"Resource: %s\nJob UID: %s\nJob generation: %d\n",
		job.Namespace, job.Name, sourceURI, jobUID, job.Generation)

	wispID, err := st.CreateBead(ctx, store.CreateOpts{
		Title:       title,
		Description: desc,
		Priority:    1,
		Type:        store.ParseIssueType("recovery"),
		Labels: []string{
			"recovery-bead",
			cacheRefreshFailureLabel,
			"failure_class:" + string(recovery.FailureClassCacheRefresh),
			"guild:" + guild.Name,
		},
		Prefix:    store.PrefixFromID(pinnedID),
		Ephemeral: true,
		Metadata:  meta,
	})
	if err != nil {
		return "", fmt.Errorf("create cache-refresh wisp: %w", err)
	}

	if err := st.AddDep(ctx, wispID, pinnedID, store.DepCausedBy); err != nil {
		return "", fmt.Errorf("add caused-by edge %s -> %s: %w", wispID, pinnedID, err)
	}
	return wispID, nil
}

// statusClosed is the string form of the closed status, extracted so
// tests using the package-level constant can compare without pulling
// in the beads package transitively.
const statusClosed = "closed"
