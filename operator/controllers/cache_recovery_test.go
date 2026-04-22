package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spirev1 "github.com/awell-health/spire/operator/api/v1alpha1"
	"github.com/awell-health/spire/pkg/recovery"
	"github.com/awell-health/spire/pkg/store"
)

// recordingWispStore is an in-memory wispFilingStore that captures
// every create/dep/list call. Tests assert on the recorded shape to
// catch drift in wisp metadata, labels, or the caused-by edge. Missing
// coverage here is load-bearing — the reconciler writes with no
// further validation and the cleric consumes this data directly.
type recordingWispStore struct {
	beadsByID map[string]*recordedWisp
	deps      []recordedDep
	calls     []recordedWispCall
	nextIDSeq int

	// createErr, when non-nil, is returned from the first CreateBead call
	// made while this value is set (used to drive the error-propagation
	// tests).
	createErr error
	// addDepErr, when non-nil, is returned from AddDep (same pattern).
	addDepErr error
}

type recordedWisp struct {
	ID          string
	Title       string
	Type        string
	Labels      []string
	Priority    int
	Description string
	Ephemeral   bool
	Metadata    map[string]string
	Status      string
}

type recordedDep struct {
	IssueID     string
	DependsOnID string
	Type        string
}

type recordedWispCall struct {
	Op string // "CreateBead" | "AddDep" | "ListBeadsByMetadata"
	ID string
}

func newRecordingWispStore() *recordingWispStore {
	return &recordingWispStore{
		beadsByID: make(map[string]*recordedWisp),
	}
}

func (s *recordingWispStore) CreateBead(_ context.Context, opts store.CreateOpts) (string, error) {
	if s.createErr != nil {
		err := s.createErr
		s.createErr = nil
		return "", err
	}
	s.nextIDSeq++
	id := fmt.Sprintf("test-wisp-%d", s.nextIDSeq)
	metaCopy := make(map[string]string, len(opts.Metadata))
	for k, v := range opts.Metadata {
		metaCopy[k] = v
	}
	s.beadsByID[id] = &recordedWisp{
		ID:          id,
		Title:       opts.Title,
		Type:        string(opts.Type),
		Labels:      append([]string(nil), opts.Labels...),
		Priority:    opts.Priority,
		Description: opts.Description,
		Ephemeral:   opts.Ephemeral,
		Metadata:    metaCopy,
		Status:      "open",
	}
	s.calls = append(s.calls, recordedWispCall{Op: "CreateBead", ID: id})
	return id, nil
}

func (s *recordingWispStore) AddDep(_ context.Context, issueID, dependsOnID, depType string) error {
	s.calls = append(s.calls, recordedWispCall{Op: "AddDep", ID: issueID})
	if s.addDepErr != nil {
		err := s.addDepErr
		s.addDepErr = nil
		return err
	}
	s.deps = append(s.deps, recordedDep{
		IssueID:     issueID,
		DependsOnID: dependsOnID,
		Type:        depType,
	})
	return nil
}

func (s *recordingWispStore) ListBeadsByMetadata(_ context.Context, meta map[string]string) ([]store.Bead, error) {
	s.calls = append(s.calls, recordedWispCall{Op: "ListBeadsByMetadata"})
	var matches []store.Bead
	for _, b := range s.beadsByID {
		ok := true
		for k, v := range meta {
			if b.Metadata[k] != v {
				ok = false
				break
			}
		}
		if ok {
			matches = append(matches, store.Bead{
				ID:       b.ID,
				Title:    b.Title,
				Status:   b.Status,
				Type:     b.Type,
				Labels:   append([]string(nil), b.Labels...),
				Metadata: b.Metadata,
			})
		}
	}
	return matches, nil
}

func (s *recordingWispStore) creates() int {
	n := 0
	for _, c := range s.calls {
		if c.Op == "CreateBead" {
			n++
		}
	}
	return n
}

// --- isRefreshJobBackoffExhausted -----------------------------------------

func TestIsRefreshJobBackoffExhausted(t *testing.T) {
	cases := []struct {
		name string
		job  *batchv1.Job
		want bool
	}{
		{
			name: "nil job",
			job:  nil,
			want: false,
		},
		{
			name: "in-progress job",
			job: &batchv1.Job{
				Status: batchv1.JobStatus{
					Active: 1,
				},
			},
			want: false,
		},
		{
			name: "backoff limit exceeded",
			job: &batchv1.Job{
				Status: batchv1.JobStatus{
					Conditions: []batchv1.JobCondition{{
						Type:   batchv1.JobFailed,
						Status: corev1.ConditionTrue,
						Reason: "BackoffLimitExceeded",
					}},
				},
			},
			want: true,
		},
		{
			name: "deadline exceeded",
			job: &batchv1.Job{
				Status: batchv1.JobStatus{
					Conditions: []batchv1.JobCondition{{
						Type:   batchv1.JobFailed,
						Status: corev1.ConditionTrue,
						Reason: "DeadlineExceeded",
					}},
				},
			},
			want: true,
		},
		{
			name: "failed with other reason is not exhausted",
			job: &batchv1.Job{
				Status: batchv1.JobStatus{
					Conditions: []batchv1.JobCondition{{
						Type:   batchv1.JobFailed,
						Status: corev1.ConditionTrue,
						Reason: "ImagePullBackOff",
					}},
				},
			},
			want: false,
		},
		{
			name: "failed condition with status false",
			job: &batchv1.Job{
				Status: batchv1.JobStatus{
					Conditions: []batchv1.JobCondition{{
						Type:   batchv1.JobFailed,
						Status: corev1.ConditionFalse,
						Reason: "BackoffLimitExceeded",
					}},
				},
			},
			want: false,
		},
		{
			name: "complete, not failed",
			job: &batchv1.Job{
				Status: batchv1.JobStatus{
					Conditions: []batchv1.JobCondition{{
						Type:   batchv1.JobComplete,
						Status: corev1.ConditionTrue,
					}},
				},
			},
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isRefreshJobBackoffExhausted(tc.job); got != tc.want {
				t.Errorf("isRefreshJobBackoffExhausted = %v, want %v", got, tc.want)
			}
		})
	}
}

// --- resolvePinnedIdentityID ---------------------------------------------

func TestResolvePinnedIdentityID(t *testing.T) {
	t.Run("nil guild", func(t *testing.T) {
		if _, err := resolvePinnedIdentityID(nil); err == nil {
			t.Errorf("expected error for nil guild")
		}
	})
	t.Run("empty stamped ID", func(t *testing.T) {
		g := &spirev1.WizardGuild{ObjectMeta: metav1.ObjectMeta{Name: "g1"}}
		if _, err := resolvePinnedIdentityID(g); err == nil {
			t.Errorf("expected error when PinnedIdentityBeadID is empty")
		}
	})
	t.Run("stamped ID returned", func(t *testing.T) {
		g := &spirev1.WizardGuild{
			ObjectMeta: metav1.ObjectMeta{Name: "g1"},
			Status: spirev1.WizardGuildStatus{
				PinnedIdentityBeadID: "pinned-abc",
			},
		}
		got, err := resolvePinnedIdentityID(g)
		if err != nil {
			t.Fatalf("resolvePinnedIdentityID: %v", err)
		}
		if got != "pinned-abc" {
			t.Errorf("got %q, want pinned-abc", got)
		}
	})
}

// --- capTerminationLog ----------------------------------------------------

func TestCapTerminationLog(t *testing.T) {
	t.Run("small passes through", func(t *testing.T) {
		in := "exit 1: connection refused"
		if got := capTerminationLog(in); got != in {
			t.Errorf("capTerminationLog modified a small input: %q", got)
		}
	})
	t.Run("exactly at cap passes through", func(t *testing.T) {
		in := strings.Repeat("a", cacheWispTerminationLogCap)
		got := capTerminationLog(in)
		if got != in {
			t.Errorf("capTerminationLog modified input exactly at cap (len=%d)", len(got))
		}
	})
	t.Run("oversize gets truncated with marker", func(t *testing.T) {
		in := strings.Repeat("b", cacheWispTerminationLogCap+1024)
		got := capTerminationLog(in)
		if !strings.HasSuffix(got, cacheWispTruncationMarker) {
			t.Errorf("truncation marker missing; got suffix %q", got[len(got)-40:])
		}
		if len(got) != cacheWispTerminationLogCap+len(cacheWispTruncationMarker) {
			t.Errorf("truncated length = %d, want %d",
				len(got), cacheWispTerminationLogCap+len(cacheWispTruncationMarker))
		}
	})
}

// --- fileWispForCacheFailure: success path -------------------------------

// makeFailedRefreshJob builds a Job in the BackoffLimitExceeded state
// with a recognizable UID + generation so tests can assert job_uid /
// job_generation metadata are stamped faithfully.
func makeFailedRefreshJob(ns, guildName string, uid string, generation int64) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:       refreshJobName(guildName),
			Namespace:  ns,
			UID:        types.UID(uid),
			Generation: generation,
		},
		Status: batchv1.JobStatus{
			Conditions: []batchv1.JobCondition{{
				Type:               batchv1.JobFailed,
				Status:             corev1.ConditionTrue,
				Reason:             "BackoffLimitExceeded",
				Message:            "Job has reached the specified backoff limit",
				LastTransitionTime: metav1.Time{Time: time.Now().Add(-time.Minute)},
			}},
		},
	}
}

// makeReconcilerSeededFailedJob builds a failed refresh Job carrying
// the spec-hash annotation the reconciler stamps on create. Without
// the annotation, ensureRefreshJob sees the Job as drift and recreates
// it — which would wipe the Failed conditions before the dispatch
// branch fires. Tests that drive dispatchCacheRecovery via the full
// cycle must use this builder.
func makeReconcilerSeededFailedJob(guild *spirev1.WizardGuild, uid string, generation int64) *batchv1.Job {
	j := makeFailedRefreshJob(guild.Namespace, guild.Name, uid, generation)
	j.Annotations = map[string]string{
		cacheSpecHashAnnotation: specHash(guild.Spec.Cache, guild.Spec.Repo, guild.Spec.RepoBranch),
	}
	return j
}

// makeRefreshPod returns a Pod owned by the given Job with a terminated
// refresh container. The termination message is stamped on the
// refresh container's Terminated state so collectJobFailureSnapshot
// surfaces it as the wisp's termination_log metadata.
func makeRefreshPod(ns, jobName, termMsg string) *corev1.Pod {
	exit := int32(1)
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName + "-abc",
			Namespace: ns,
			Labels:    map[string]string{jobNameLabel: jobName},
		},
		Status: corev1.PodStatus{
			Phase:     corev1.PodFailed,
			StartTime: &metav1.Time{Time: time.Now().Add(-2 * time.Minute)},
			Conditions: []corev1.PodCondition{{
				Type:    corev1.PodReady,
				Status:  corev1.ConditionFalse,
				Reason:  "ContainersNotReady",
				Message: "containers with unready status: [refresh]",
			}},
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: cacheRefreshContainerName,
				State: corev1.ContainerState{
					Terminated: &corev1.ContainerStateTerminated{
						ExitCode: exit,
						Message:  termMsg,
						Reason:   "Error",
					},
				},
			}},
		},
	}
}

// TestFileWispForCacheFailure_CreatesWispWithMetadataAndEdge covers
// the happy path: a fresh failed Job + a matching Pod produces exactly
// one bead with Ephemeral=true, the full metadata map, and a caused-by
// edge to the pinned ID. Every field asserted here is consumed
// downstream by cleric — silent drift would corrupt the recovery graph.
func TestFileWispForCacheFailure_CreatesWispWithMetadataAndEdge(t *testing.T) {
	const (
		ns       = "spire"
		guildN   = "core"
		jobUID   = "uid-job-abc"
		pinnedID = "pinned-parent-1"
		termMsg  = "fatal: connection to git server refused"
	)
	job := makeFailedRefreshJob(ns, guildN, jobUID, 3)
	pod := makeRefreshPod(ns, job.Name, termMsg)

	c := fake.NewClientBuilder().
		WithScheme(newCacheTestScheme(t)).
		WithObjects(job, pod).
		Build()
	st := newRecordingWispStore()
	g := &spirev1.WizardGuild{
		ObjectMeta: metav1.ObjectMeta{Name: guildN, Namespace: ns},
		Status:     spirev1.WizardGuildStatus{PinnedIdentityBeadID: pinnedID},
	}

	wispID, err := fileWispForCacheFailure(context.Background(), st, c, g, job, pinnedID)
	if err != nil {
		t.Fatalf("fileWispForCacheFailure: %v", err)
	}
	if wispID == "" {
		t.Fatal("empty wisp ID returned")
	}
	if st.creates() != 1 {
		t.Errorf("CreateBead called %d times, want 1", st.creates())
	}

	w := st.beadsByID[wispID]
	if w == nil {
		t.Fatalf("wisp %q missing from fake store", wispID)
	}
	if !w.Ephemeral {
		t.Errorf("wisp.Ephemeral = false, want true")
	}
	if w.Priority != 1 {
		t.Errorf("wisp.Priority = %d, want 1", w.Priority)
	}
	if w.Type != "recovery" {
		t.Errorf("wisp.Type = %q, want recovery", w.Type)
	}

	// Metadata shape.
	wantMeta := map[string]string{
		"failure_class":           string(recovery.FailureClassCacheRefresh),
		"failed_step":             "refresh",
		"source_resource_uri":     fmt.Sprintf("spire.io/wizardguild/%s/%s/cache", ns, guildN),
		"job_uid":                 jobUID,
		"job_generation":          "3",
		"pinned_identity_bead_id": pinnedID,
		"guild_namespace":         ns,
		"guild_name":              guildN,
	}
	for k, want := range wantMeta {
		if got := w.Metadata[k]; got != want {
			t.Errorf("metadata[%s] = %q, want %q", k, got, want)
		}
	}
	if w.Metadata["termination_log"] != termMsg {
		t.Errorf("metadata[termination_log] = %q, want %q", w.Metadata["termination_log"], termMsg)
	}
	// condition_snapshot should be non-empty, valid JSON, and carry at
	// least the Job's BackoffLimitExceeded condition.
	var snap jobPodConditionSnapshot
	if err := json.Unmarshal([]byte(w.Metadata["condition_snapshot"]), &snap); err != nil {
		t.Fatalf("condition_snapshot is not valid JSON: %v\npayload: %s", err, w.Metadata["condition_snapshot"])
	}
	if len(snap.JobConditions) == 0 || snap.JobConditions[0].Reason != "BackoffLimitExceeded" {
		t.Errorf("condition_snapshot missing BackoffLimitExceeded: %+v", snap.JobConditions)
	}
	if snap.ExitCode == nil || *snap.ExitCode != 1 {
		t.Errorf("condition_snapshot exit_code = %v, want 1", snap.ExitCode)
	}

	// Labels.
	labels := map[string]bool{}
	for _, l := range w.Labels {
		labels[l] = true
	}
	for _, want := range []string{
		"recovery-bead",
		cacheRefreshFailureLabel,
		"failure_class:" + string(recovery.FailureClassCacheRefresh),
		"guild:" + guildN,
	} {
		if !labels[want] {
			t.Errorf("labels missing %q; got %v", want, w.Labels)
		}
	}

	// Caused-by edge.
	if len(st.deps) != 1 {
		t.Fatalf("deps = %+v, want exactly one caused-by edge", st.deps)
	}
	d := st.deps[0]
	if d.IssueID != wispID || d.DependsOnID != pinnedID || d.Type != store.DepCausedBy {
		t.Errorf("dep = %+v, want {Issue=%s DependsOn=%s Type=%s}",
			d, wispID, pinnedID, store.DepCausedBy)
	}
}

// --- fileWispForCacheFailure: idempotency --------------------------------

// TestFileWispForCacheFailure_IdempotentByJobUID covers the entry
// guard: when a wisp with the same job_uid already exists in the
// store, a second call returns the existing ID without creating a
// duplicate. This is the safety net for a crash between CreateBead
// and annotateJob where the Job loses its annotation but the wisp
// persists.
func TestFileWispForCacheFailure_IdempotentByJobUID(t *testing.T) {
	const (
		ns       = "spire"
		guildN   = "core"
		jobUID   = "uid-job-xyz"
		pinnedID = "pinned-parent-2"
	)
	job := makeFailedRefreshJob(ns, guildN, jobUID, 1)
	pod := makeRefreshPod(ns, job.Name, "first termination")

	c := fake.NewClientBuilder().
		WithScheme(newCacheTestScheme(t)).
		WithObjects(job, pod).
		Build()
	st := newRecordingWispStore()
	g := &spirev1.WizardGuild{
		ObjectMeta: metav1.ObjectMeta{Name: guildN, Namespace: ns},
		Status:     spirev1.WizardGuildStatus{PinnedIdentityBeadID: pinnedID},
	}

	first, err := fileWispForCacheFailure(context.Background(), st, c, g, job, pinnedID)
	if err != nil {
		t.Fatalf("first fileWispForCacheFailure: %v", err)
	}
	second, err := fileWispForCacheFailure(context.Background(), st, c, g, job, pinnedID)
	if err != nil {
		t.Fatalf("second fileWispForCacheFailure: %v", err)
	}
	if first != second {
		t.Errorf("idempotency broken: first=%s second=%s", first, second)
	}
	if st.creates() != 1 {
		t.Errorf("CreateBead called %d times across two invocations; want 1", st.creates())
	}
	if len(st.deps) != 1 {
		t.Errorf("caused-by edges = %d; want 1 (no duplicate edge)", len(st.deps))
	}
}

// TestFileWispForCacheFailure_DistinctJobUIDsFileDistinctWisps covers
// the "one wisp per failure incident" contract: two refresh Jobs with
// different UIDs (different generations, or post-recreate Jobs)
// produce two wisps, matching design Decision 2.
func TestFileWispForCacheFailure_DistinctJobUIDsFileDistinctWisps(t *testing.T) {
	const (
		ns       = "spire"
		guildN   = "core"
		pinnedID = "pinned-parent-3"
	)
	c := fake.NewClientBuilder().WithScheme(newCacheTestScheme(t)).Build()
	st := newRecordingWispStore()
	g := &spirev1.WizardGuild{
		ObjectMeta: metav1.ObjectMeta{Name: guildN, Namespace: ns},
		Status:     spirev1.WizardGuildStatus{PinnedIdentityBeadID: pinnedID},
	}

	job1 := makeFailedRefreshJob(ns, guildN, "uid-one", 1)
	job2 := makeFailedRefreshJob(ns, guildN, "uid-two", 2)

	id1, err := fileWispForCacheFailure(context.Background(), st, c, g, job1, pinnedID)
	if err != nil {
		t.Fatalf("file wisp 1: %v", err)
	}
	id2, err := fileWispForCacheFailure(context.Background(), st, c, g, job2, pinnedID)
	if err != nil {
		t.Fatalf("file wisp 2: %v", err)
	}
	if id1 == id2 {
		t.Errorf("distinct jobs produced identical wisp IDs: %s", id1)
	}
	if st.creates() != 2 {
		t.Errorf("CreateBead called %d times; want 2 for distinct Job UIDs", st.creates())
	}
}

// --- fileWispForCacheFailure: error paths --------------------------------

func TestFileWispForCacheFailure_ErrorsOnMissingPinnedID(t *testing.T) {
	const (
		ns     = "spire"
		guildN = "core"
		jobUID = "uid-p1"
	)
	job := makeFailedRefreshJob(ns, guildN, jobUID, 1)
	c := fake.NewClientBuilder().WithScheme(newCacheTestScheme(t)).WithObjects(job).Build()
	st := newRecordingWispStore()
	g := &spirev1.WizardGuild{
		ObjectMeta: metav1.ObjectMeta{Name: guildN, Namespace: ns},
	}
	if _, err := fileWispForCacheFailure(context.Background(), st, c, g, job, ""); err == nil {
		t.Fatal("expected error when pinnedID is empty")
	}
	if st.creates() != 0 {
		t.Errorf("CreateBead called %d times despite missing pinnedID; want 0", st.creates())
	}
}

func TestFileWispForCacheFailure_ErrorsOnNilJob(t *testing.T) {
	g := &spirev1.WizardGuild{
		ObjectMeta: metav1.ObjectMeta{Name: "core"},
		Status:     spirev1.WizardGuildStatus{PinnedIdentityBeadID: "pin"},
	}
	c := fake.NewClientBuilder().WithScheme(newCacheTestScheme(t)).Build()
	st := newRecordingWispStore()
	if _, err := fileWispForCacheFailure(context.Background(), st, c, g, nil, "pin"); err == nil {
		t.Fatal("expected error for nil job")
	}
}

// TestFileWispForCacheFailure_TruncatesOversizeTerminationLog asserts
// the metadata-size guard: a gigantic termination message is capped
// with a trailing marker so the bead metadata column doesn't overflow.
// Without this, a single spammy refresh log could push the whole wisp
// over the storage engine's TEXT limit and abort the filing path.
func TestFileWispForCacheFailure_TruncatesOversizeTerminationLog(t *testing.T) {
	const (
		ns       = "spire"
		guildN   = "core"
		jobUID   = "uid-bigger"
		pinnedID = "pinned-parent-big"
	)
	huge := strings.Repeat("X", cacheWispTerminationLogCap+2048)
	job := makeFailedRefreshJob(ns, guildN, jobUID, 1)
	pod := makeRefreshPod(ns, job.Name, huge)

	c := fake.NewClientBuilder().
		WithScheme(newCacheTestScheme(t)).
		WithObjects(job, pod).
		Build()
	st := newRecordingWispStore()
	g := &spirev1.WizardGuild{
		ObjectMeta: metav1.ObjectMeta{Name: guildN, Namespace: ns},
		Status:     spirev1.WizardGuildStatus{PinnedIdentityBeadID: pinnedID},
	}

	wispID, err := fileWispForCacheFailure(context.Background(), st, c, g, job, pinnedID)
	if err != nil {
		t.Fatalf("fileWispForCacheFailure: %v", err)
	}
	got := st.beadsByID[wispID].Metadata["termination_log"]
	if !strings.HasSuffix(got, cacheWispTruncationMarker) {
		t.Errorf("expected truncation marker suffix; got last 40 chars: %q", got[len(got)-40:])
	}
	if len(got) != cacheWispTerminationLogCap+len(cacheWispTruncationMarker) {
		t.Errorf("truncated length = %d, want %d",
			len(got), cacheWispTerminationLogCap+len(cacheWispTruncationMarker))
	}
}

// --- reconciler integration: dispatchCacheRecovery -----------------------

// TestCacheReconciler_DispatchCacheRecovery_FilesAndAnnotates covers
// the wiring from reconcileGuild → dispatchCacheRecovery →
// fileWispForCacheFailure → annotateJob. A freshly-failed Job with no
// annotation should end up with exactly one wisp filed + the wisp ID
// annotated on the Job for future dedupe.
func TestCacheReconciler_DispatchCacheRecovery_FilesAndAnnotates(t *testing.T) {
	const (
		ns       = "spire"
		name     = "core"
		repo     = "git@example.com:awell-health/spire.git"
		pinnedID = "test-pinned-known"
	)
	guild := makeCacheGuild(name, ns, repo)
	job := makeReconcilerSeededFailedJob(guild, "uid-recon-1", 1)
	pod := makeRefreshPod(ns, job.Name, "clone failed: exit 128")
	c := fake.NewClientBuilder().
		WithScheme(newCacheTestScheme(t)).
		WithObjects(guild, job, pod).
		WithStatusSubresource(&spirev1.WizardGuild{}).
		Build()

	// Pre-stamp the guild with a pinned identity so the wisp-filing
	// branch can proceed (otherwise dispatchCacheRecovery returns early
	// without erroring — a valid behavior but not what we assert here).
	var stamped spirev1.WizardGuild
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, &stamped); err != nil {
		t.Fatalf("get guild: %v", err)
	}
	stamped.Status.PinnedIdentityBeadID = pinnedID
	if err := c.Status().Update(context.Background(), &stamped); err != nil {
		t.Fatalf("stamp Status.PinnedIdentityBeadID: %v", err)
	}

	wispSt := newRecordingWispStore()
	pinSt := newRecordingStore()
	pinSt.beadsByID[pinnedID] = &recordedBead{ID: pinnedID, Status: "pinned", Pinned: true}
	r := newCacheReconciler(t, c, ns)
	r.PinnedStore = pinSt
	r.WispStore = wispSt

	r.cycle(context.Background())

	if wispSt.creates() != 1 {
		t.Fatalf("CreateBead called %d times; want 1 after dispatch", wispSt.creates())
	}
	// Find the wisp ID the reconciler produced.
	var wispID string
	for id := range wispSt.beadsByID {
		wispID = id
		break
	}

	// Job should now carry the wisp annotation.
	var updated batchv1.Job
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: job.Name}, &updated); err != nil {
		t.Fatalf("get job after dispatch: %v", err)
	}
	if got := updated.Annotations[cacheWispIDAnnotation]; got != wispID {
		t.Errorf("job annotation[%s] = %q, want %q", cacheWispIDAnnotation, got, wispID)
	}

	// A second cycle must not file a second wisp — the annotation is
	// the per-generation idempotency guard, and the job_uid entry
	// guard is the cross-request safety net. Exercise both by running
	// the cycle twice.
	r.cycle(context.Background())
	if wispSt.creates() != 1 {
		t.Errorf("CreateBead called %d times after two cycles; want 1 (idempotent)", wispSt.creates())
	}
}

// TestCacheReconciler_DispatchCacheRecovery_NoPinnedIDIsQuietNoop
// covers the "not yet provisioned" path: a failed Job reaching
// dispatchCacheRecovery with an empty pinned identity bead ID must
// NOT file a wisp or error out — the next cycle will stamp the ID
// via ensurePinnedIdentity, at which point the dispatch branch
// succeeds. The reconciler's full cycle re-fires at each tick, so
// the no-ID state is a transient "not yet" rather than a fault.
func TestCacheReconciler_DispatchCacheRecovery_NoPinnedIDIsQuietNoop(t *testing.T) {
	const (
		ns   = "spire"
		name = "core"
		repo = "git@example.com:awell-health/spire.git"
	)
	// Call dispatchCacheRecovery directly with a guild lacking the
	// Status.PinnedIdentityBeadID stamp. No wisp should be filed and
	// no error should surface.
	guild := makeCacheGuild(name, ns, repo)
	// Deliberately leave Status.PinnedIdentityBeadID empty.
	job := makeFailedRefreshJob(ns, name, "uid-no-pin-1", 1)

	c := fake.NewClientBuilder().
		WithScheme(newCacheTestScheme(t)).
		WithObjects(guild, job).
		WithStatusSubresource(&spirev1.WizardGuild{}).
		Build()

	wispSt := newRecordingWispStore()
	r := newCacheReconciler(t, c, ns)
	r.WispStore = wispSt

	if err := r.dispatchCacheRecovery(context.Background(), guild, job); err != nil {
		t.Fatalf("dispatchCacheRecovery with empty pinned ID returned error: %v", err)
	}
	if wispSt.creates() != 0 {
		t.Errorf("CreateBead called %d times despite empty pinned ID; want 0", wispSt.creates())
	}
}
