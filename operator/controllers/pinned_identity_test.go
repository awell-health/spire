package controllers

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/steveyegge/beads"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	spirev1 "github.com/awell-health/spire/operator/api/v1alpha1"
	"github.com/awell-health/spire/pkg/store"
)

// recordingPinnedStore is an in-memory pinnedIdentityStore that records
// every operation in call-order. Tests assert on the recorded sequence
// to catch ordering regressions (specifically the wisp-then-pinned
// close ordering required by design W1).
type recordingPinnedStore struct {
	beadsByID map[string]*recordedBead
	// dependents maps pinnedID → list of dependents the next
	// GetDependentsWithMeta call returns. Pre-seeded by tests.
	dependents map[string][]*beads.IssueWithDependencyMetadata

	// Operations recorded in order. Tests inspect this to verify that
	// CloseBead("wisp-*") runs before CloseBead(pinnedID).
	calls []recordedCall

	// nextIDSeq drives synthetic IDs for CreateBead.
	nextIDSeq int

	// closeErr, when non-nil, is returned from CloseBead for the named
	// bead ID. Lets tests inject failures into the wisp-close loop.
	closeErr      error
	closeErrForID string
}

type recordedBead struct {
	ID     string
	Title  string
	Type   beads.IssueType
	Status string
	Pinned bool
	Labels []string
	Body   string
}

type recordedCall struct {
	Op string // "GetBead" | "CreateBead" | "UpdateBead" | "CloseBead" | "GetDependentsWithMeta"
	ID string
}

func newRecordingStore() *recordingPinnedStore {
	return &recordingPinnedStore{
		beadsByID:  make(map[string]*recordedBead),
		dependents: make(map[string][]*beads.IssueWithDependencyMetadata),
	}
}

func (s *recordingPinnedStore) GetBead(_ context.Context, id string) (store.Bead, error) {
	s.calls = append(s.calls, recordedCall{Op: "GetBead", ID: id})
	b, ok := s.beadsByID[id]
	if !ok {
		return store.Bead{}, fmt.Errorf("bead not found: %s", id)
	}
	return store.Bead{
		ID:     b.ID,
		Title:  b.Title,
		Status: b.Status,
		Type:   string(b.Type),
		Labels: b.Labels,
	}, nil
}

func (s *recordingPinnedStore) CreateBead(_ context.Context, opts store.CreateOpts) (string, error) {
	s.nextIDSeq++
	id := fmt.Sprintf("test-pinned-%d", s.nextIDSeq)
	s.beadsByID[id] = &recordedBead{
		ID:     id,
		Title:  opts.Title,
		Type:   opts.Type,
		Status: "open", // matches pkg/store.CreateBead default
		Labels: append([]string(nil), opts.Labels...),
		Body:   opts.Description,
	}
	s.calls = append(s.calls, recordedCall{Op: "CreateBead", ID: id})
	return id, nil
}

func (s *recordingPinnedStore) UpdateBead(_ context.Context, id string, updates map[string]interface{}) error {
	s.calls = append(s.calls, recordedCall{Op: "UpdateBead", ID: id})
	b, ok := s.beadsByID[id]
	if !ok {
		return fmt.Errorf("update missing bead: %s", id)
	}
	if v, ok := updates["status"]; ok {
		if s, ok := v.(string); ok {
			b.Status = s
		}
	}
	if v, ok := updates["pinned"]; ok {
		if pb, ok := v.(bool); ok {
			b.Pinned = pb
		}
	}
	return nil
}

func (s *recordingPinnedStore) CloseBead(_ context.Context, id string) error {
	s.calls = append(s.calls, recordedCall{Op: "CloseBead", ID: id})
	if s.closeErr != nil && s.closeErrForID == id {
		return s.closeErr
	}
	if b, ok := s.beadsByID[id]; ok {
		b.Status = "closed"
	}
	return nil
}

func (s *recordingPinnedStore) GetDependentsWithMeta(_ context.Context, id string) ([]*beads.IssueWithDependencyMetadata, error) {
	s.calls = append(s.calls, recordedCall{Op: "GetDependentsWithMeta", ID: id})
	return s.dependents[id], nil
}

// closeOps returns the IDs of CloseBead calls in the order they were
// made. Used to assert the wisp-then-pinned ordering contract.
func (s *recordingPinnedStore) closeOps() []string {
	out := make([]string, 0, len(s.calls))
	for _, c := range s.calls {
		if c.Op == "CloseBead" {
			out = append(out, c.ID)
		}
	}
	return out
}

// makeWispDep returns an IssueWithDependencyMetadata shaped like a
// caused-by wisp dependent of a pinned bead. status="" defaults to
// "open"; ephemeral defaults to true (a real wisp).
func makeWispDep(id, status string, ephemeral bool, depType string) *beads.IssueWithDependencyMetadata {
	if status == "" {
		status = string(beads.StatusOpen)
	}
	d := &beads.IssueWithDependencyMetadata{
		DependencyType: beads.DependencyType(depType),
	}
	d.ID = id
	d.Status = beads.Status(status)
	d.Ephemeral = ephemeral
	return d
}

// makeBareGuild returns the smallest guild needed to drive the
// pinned-identity helpers — just an ObjectMeta with name and UID and
// (optionally) a stamped Status.PinnedIdentityBeadID.
func makeBareGuild(name, stampedID string) *spirev1.WizardGuild {
	g := &spirev1.WizardGuild{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			UID:  types.UID("uid-" + name),
		},
	}
	if stampedID != "" {
		g.Status.PinnedIdentityBeadID = stampedID
	}
	return g
}

// --- ensurePinnedIdentity tests -------------------------------------------

// TestEnsurePinnedIdentity_Fresh covers the create path: no stamped
// ID, no bead in the store. ensurePinnedIdentity must create exactly
// one bead, flip it to status=pinned + pinned=true, and return its ID.
// The single CreateBead is the load-bearing assertion — duplicating
// would orphan beads on requeue.
func TestEnsurePinnedIdentity_Fresh(t *testing.T) {
	st := newRecordingStore()
	g := makeBareGuild("guild-a", "")

	id, err := ensurePinnedIdentity(context.Background(), st, g)
	if err != nil {
		t.Fatalf("ensurePinnedIdentity: %v", err)
	}
	if id == "" {
		t.Fatal("ensurePinnedIdentity returned empty ID")
	}

	// Exactly one bead created.
	creates := 0
	for _, c := range st.calls {
		if c.Op == "CreateBead" {
			creates++
		}
	}
	if creates != 1 {
		t.Errorf("CreateBead called %d times, want 1", creates)
	}

	b, ok := st.beadsByID[id]
	if !ok {
		t.Fatalf("created bead %s missing from fake store", id)
	}
	if b.Status != "pinned" {
		t.Errorf("bead status = %q, want pinned", b.Status)
	}
	if !b.Pinned {
		t.Errorf("bead pinned flag = false, want true")
	}
	if b.Type != beads.TypeTask {
		t.Errorf("bead type = %q, want %q", b.Type, beads.TypeTask)
	}
	if want := "WizardGuild/guild-a/Cache"; b.Title != want {
		t.Errorf("bead title = %q, want %q", b.Title, want)
	}

	// Labels must include the canonical resource/guild/owner-uid set.
	labels := map[string]bool{}
	for _, l := range b.Labels {
		labels[l] = true
	}
	for _, want := range []string{
		"pinned-identity",
		"resource:wizardguild-cache",
		"guild:guild-a",
		"owner-uid:uid-guild-a",
	} {
		if !labels[want] {
			t.Errorf("labels missing %q; got %v", want, b.Labels)
		}
	}
}

// TestEnsurePinnedIdentity_StampedIDExists covers the idempotent
// no-op path: a stamped ID that resolves in the store returns
// immediately without creating a new bead. Without this, every
// reconcile cycle would create a new pinned bead and orphan the
// previous one.
func TestEnsurePinnedIdentity_StampedIDExists(t *testing.T) {
	st := newRecordingStore()
	// Pre-seed an existing pinned bead and stamp its ID on the guild.
	const stamped = "test-pinned-pre-existing"
	st.beadsByID[stamped] = &recordedBead{
		ID:     stamped,
		Title:  "WizardGuild/guild-b/Cache",
		Status: "pinned",
		Pinned: true,
	}
	g := makeBareGuild("guild-b", stamped)

	id, err := ensurePinnedIdentity(context.Background(), st, g)
	if err != nil {
		t.Fatalf("ensurePinnedIdentity: %v", err)
	}
	if id != stamped {
		t.Errorf("returned ID = %q, want stamped %q", id, stamped)
	}

	for _, c := range st.calls {
		if c.Op == "CreateBead" {
			t.Errorf("CreateBead called when stamped ID was resolvable; calls=%v", st.calls)
		}
	}
}

// TestEnsurePinnedIdentity_StampedIDMissing covers the recreation
// path: a stamped ID that no longer resolves (db reset, manual
// delete) triggers a fresh create with a new ID. The operator caller
// re-stamps the new ID; the recovered state is "drift accepted, move
// on" rather than blocking the reconcile.
func TestEnsurePinnedIdentity_StampedIDMissing(t *testing.T) {
	st := newRecordingStore()
	g := makeBareGuild("guild-c", "test-pinned-vanished") // not pre-seeded

	id, err := ensurePinnedIdentity(context.Background(), st, g)
	if err != nil {
		t.Fatalf("ensurePinnedIdentity: %v", err)
	}
	if id == "" || id == "test-pinned-vanished" {
		t.Errorf("returned ID = %q; want a freshly-created ID", id)
	}
	if _, ok := st.beadsByID[id]; !ok {
		t.Errorf("recreated bead %s not present in store", id)
	}
}

// --- finalizePinnedIdentity tests -----------------------------------------

// TestFinalizePinnedIdentity_NoStampedID covers the no-op path: a
// guild without a Status.PinnedIdentityBeadID skips all store work.
// This makes the finalizer safe to install before ensurePinnedIdentity
// has had a chance to run (crash recovery).
func TestFinalizePinnedIdentity_NoStampedID(t *testing.T) {
	st := newRecordingStore()
	g := makeBareGuild("guild-d", "")

	if err := finalizePinnedIdentity(context.Background(), st, g); err != nil {
		t.Fatalf("finalize on empty guild: %v", err)
	}
	if len(st.calls) != 0 {
		t.Errorf("expected zero store calls; got %v", st.calls)
	}
}

// TestFinalizePinnedIdentity_NoWisps covers the simple delete: the
// pinned bead has no caused-by dependents, so the finalizer just
// closes the pinned bead and returns. Asserts the empty-dependents
// path doesn't accidentally close other beads.
func TestFinalizePinnedIdentity_NoWisps(t *testing.T) {
	st := newRecordingStore()
	const pinnedID = "test-pinned-1"
	st.beadsByID[pinnedID] = &recordedBead{ID: pinnedID, Status: "pinned", Pinned: true}
	g := makeBareGuild("guild-e", pinnedID)

	if err := finalizePinnedIdentity(context.Background(), st, g); err != nil {
		t.Fatalf("finalize: %v", err)
	}

	closes := st.closeOps()
	if len(closes) != 1 || closes[0] != pinnedID {
		t.Errorf("CloseBead sequence = %v, want [%q]", closes, pinnedID)
	}
	if st.beadsByID[pinnedID].Status != "closed" {
		t.Errorf("pinned bead status = %q, want closed", st.beadsByID[pinnedID].Status)
	}
}

// TestFinalizePinnedIdentity_OrderedClose is the load-bearing test:
// open wisps must close BEFORE the pinned bead, with closed wisps
// skipped and non-caused-by dependents ignored. A reversal of the
// ordering would leave wisps with caused-by edges to a closed pinned
// bead — exactly what the design rejects.
func TestFinalizePinnedIdentity_OrderedClose(t *testing.T) {
	st := newRecordingStore()
	const pinnedID = "test-pinned-2"
	st.beadsByID[pinnedID] = &recordedBead{ID: pinnedID, Status: "pinned", Pinned: true}

	// Pre-seed dependents: 2 open wisps, 1 closed wisp (skipped),
	// 1 non-ephemeral caused-by (skipped — not a wisp), 1
	// non-caused-by ephemeral (skipped — wrong dep type).
	st.dependents[pinnedID] = []*beads.IssueWithDependencyMetadata{
		makeWispDep("wisp-open-1", "", true, store.DepCausedBy),
		makeWispDep("wisp-closed-1", "closed", true, store.DepCausedBy),
		makeWispDep("wisp-open-2", "", true, store.DepCausedBy),
		makeWispDep("non-ephemeral-causedby", "", false, store.DepCausedBy),
		makeWispDep("ephemeral-blocks", "", true, "blocks"),
	}
	g := makeBareGuild("guild-f", pinnedID)

	if err := finalizePinnedIdentity(context.Background(), st, g); err != nil {
		t.Fatalf("finalize: %v", err)
	}

	closes := st.closeOps()
	// Both open wisps closed (in slice order), then pinned. No
	// closed-wisp re-close, no non-wisp / non-causedby close.
	want := []string{"wisp-open-1", "wisp-open-2", pinnedID}
	if !equalStringSlice(closes, want) {
		t.Errorf("CloseBead sequence = %v, want %v", closes, want)
	}

	// Pinned must be LAST — defensive check on top of the equality
	// assertion, matching the design's W1 ordering language.
	last := closes[len(closes)-1]
	if last != pinnedID {
		t.Errorf("last CloseBead = %q, want pinned %q (wisps must close first)",
			last, pinnedID)
	}
}

// TestFinalizePinnedIdentity_WispCloseErrorAborts asserts the
// partial-failure safety net: when a wisp close fails, the pinned
// bead must NOT be closed. Closing pinned after a wisp failure would
// leave the failed wisp dangling on a closed pinned bead — exactly
// the broken state the ordering rule exists to prevent.
func TestFinalizePinnedIdentity_WispCloseErrorAborts(t *testing.T) {
	st := newRecordingStore()
	const pinnedID = "test-pinned-3"
	st.beadsByID[pinnedID] = &recordedBead{ID: pinnedID, Status: "pinned", Pinned: true}

	// Two open wisps; the first close errors out.
	st.dependents[pinnedID] = []*beads.IssueWithDependencyMetadata{
		makeWispDep("wisp-fail", "", true, store.DepCausedBy),
		makeWispDep("wisp-after", "", true, store.DepCausedBy),
	}
	st.closeErr = errors.New("synthetic close failure")
	st.closeErrForID = "wisp-fail"
	g := makeBareGuild("guild-g", pinnedID)

	err := finalizePinnedIdentity(context.Background(), st, g)
	if err == nil {
		t.Fatal("finalize succeeded; want wisp-close error to propagate")
	}
	if !strings.Contains(err.Error(), "wisp-fail") {
		t.Errorf("error %q does not mention wisp-fail", err.Error())
	}

	// Pinned must NOT have been closed — the recorded close ops should
	// be only the failing wisp (no second wisp, no pinned).
	closes := st.closeOps()
	if len(closes) != 1 || closes[0] != "wisp-fail" {
		t.Errorf("CloseBead sequence = %v, want only [wisp-fail] (abort before pinned)",
			closes)
	}
	if st.beadsByID[pinnedID].Status == "closed" {
		t.Errorf("pinned bead status = closed; must remain open after wisp-close failure")
	}
}

// --- helper-function tests ------------------------------------------------

// TestPinnedIdentityTitle_Stable locks the canonical title format. A
// silent change here would invalidate post-GC analytics that
// rediscover beads by title prefix.
func TestPinnedIdentityTitle_Stable(t *testing.T) {
	g := makeBareGuild("alpha-guild", "")
	if got, want := pinnedIdentityTitle(g), "WizardGuild/alpha-guild/Cache"; got != want {
		t.Errorf("pinnedIdentityTitle = %q, want %q", got, want)
	}
}

// TestPinnedIdentityLabels_Schema covers the queryable label set.
// Tests assert each label individually so a single label drift is
// caught with a precise message.
func TestPinnedIdentityLabels_Schema(t *testing.T) {
	g := makeBareGuild("beta-guild", "")
	got := pinnedIdentityLabels(g)
	want := map[string]bool{
		"pinned-identity":            true,
		"resource:wizardguild-cache": true,
		"guild:beta-guild":           true,
		"owner-uid:uid-beta-guild":   true,
	}
	for _, l := range got {
		if !want[l] {
			t.Errorf("unexpected label %q in %v", l, got)
		}
		delete(want, l)
	}
	for missing := range want {
		t.Errorf("missing label %q from %v", missing, got)
	}
}

// TestPinnedIdentityBody_ContainsResourceURI keeps the body format
// honest: tooling that scrapes the bead description for the resource
// URI must continue to find it. Timestamp content is intentionally
// not asserted (changes every run).
func TestPinnedIdentityBody_ContainsResourceURI(t *testing.T) {
	g := makeBareGuild("gamma-guild", "")
	body := pinnedIdentityBody(g)
	for _, want := range []string{
		"WizardGuild/gamma-guild/Cache",
		"wizardguild/gamma-guild/cache",
		"uid-gamma-guild",
		"do not edit or close it manually",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q; got:\n%s", want, body)
		}
	}
}

// equalStringSlice is a small ordered-equality helper used by the
// finalize-ordering assertions. Avoids pulling in reflect.DeepEqual
// for test failure messages that already need precise diffs.
func equalStringSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// --- reconciler-integration tests ----------------------------------------
//
// The unit tests above cover the lifecycle helpers in isolation. The
// integration tests below cover the wiring: how reconcileGuild
// composes finalizer add → ensurePinnedIdentity → Status stamp →
// existing PVC/Job logic on create, and the symmetric cleanup on
// delete.

// TestCacheReconciler_PinnedIdentity_CreateFlow covers the happy
// path: a fresh guild with Cache set ends up with the pinned-identity
// finalizer attached, a pinned bead created in the store, and
// Status.PinnedIdentityBeadID stamped — all on a single cycle. The
// PVC and refresh Job continuing to be created on the same cycle is
// asserted because the finalizer-add path returns mid-reconcile would
// stall cache provisioning by a polling interval.
func TestCacheReconciler_PinnedIdentity_CreateFlow(t *testing.T) {
	const (
		ns   = "spire"
		name = "core"
		repo = "git@example.com:awell-health/spire.git"
	)
	guild := makeCacheGuild(name, ns, repo)
	c := fake.NewClientBuilder().
		WithScheme(newCacheTestScheme(t)).
		WithObjects(guild).
		WithStatusSubresource(&spirev1.WizardGuild{}).
		Build()
	st := newRecordingStore()
	r := newCacheReconciler(t, c, ns)
	r.PinnedStore = st
	r.cycle(context.Background())

	// Finalizer attached.
	var got spirev1.WizardGuild
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, &got); err != nil {
		t.Fatalf("get guild: %v", err)
	}
	if !controllerutil.ContainsFinalizer(&got, pinnedIdentityFinalizer) {
		t.Errorf("finalizer %q not added; finalizers=%v", pinnedIdentityFinalizer, got.Finalizers)
	}

	// Status stamped with the bead ID returned by the fake store.
	if got.Status.PinnedIdentityBeadID == "" {
		t.Errorf("Status.PinnedIdentityBeadID not stamped after first cycle")
	}
	if _, ok := st.beadsByID[got.Status.PinnedIdentityBeadID]; !ok {
		t.Errorf("stamped bead ID %q not present in fake store; created=%v",
			got.Status.PinnedIdentityBeadID, st.beadsByID)
	}
	pinnedBead := st.beadsByID[got.Status.PinnedIdentityBeadID]
	if pinnedBead.Status != "pinned" || !pinnedBead.Pinned {
		t.Errorf("pinned bead state = (status=%q, pinned=%v); want (pinned, true)",
			pinnedBead.Status, pinnedBead.Pinned)
	}

	// PVC and Job still created on the same cycle — the finalizer-add
	// path must not short-circuit the rest of the reconcile.
	pvcName := name + "-repo-cache"
	if !objectExists(t, c, ns, pvcName, "PersistentVolumeClaim") {
		t.Errorf("PVC %q not created on first cycle (finalizer-add path stalled cache provisioning)", pvcName)
	}
	jobName := name + "-repo-cache-refresh"
	if !objectExists(t, c, ns, jobName, "Job") {
		t.Errorf("refresh Job %q not created on first cycle", jobName)
	}
}

// TestCacheReconciler_PinnedIdentity_Idempotent asserts that a second
// reconcile cycle does NOT create a duplicate pinned bead and does
// NOT re-add the finalizer. Without this, every polling tick would
// orphan the previous pinned bead.
func TestCacheReconciler_PinnedIdentity_Idempotent(t *testing.T) {
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
	st := newRecordingStore()
	r := newCacheReconciler(t, c, ns)
	r.PinnedStore = st

	r.cycle(context.Background())
	createsAfterFirst := countCreateBead(st)
	if createsAfterFirst != 1 {
		t.Fatalf("first cycle created %d beads, want 1", createsAfterFirst)
	}
	r.cycle(context.Background())
	if createsAfter := countCreateBead(st); createsAfter != 1 {
		t.Errorf("second cycle bumped CreateBead count to %d; want 1 (idempotent)", createsAfter)
	}
}

// TestCacheReconciler_PinnedIdentity_DeleteRunsFinalizer covers the
// symmetric delete path: when a guild with the finalizer is deleted,
// the next cycle closes the pinned bead and removes the finalizer so
// kube can complete CR removal.
func TestCacheReconciler_PinnedIdentity_DeleteRunsFinalizer(t *testing.T) {
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
	st := newRecordingStore()
	r := newCacheReconciler(t, c, ns)
	r.PinnedStore = st

	// First cycle attaches finalizer + creates bead.
	r.cycle(context.Background())
	var afterCreate spirev1.WizardGuild
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, &afterCreate); err != nil {
		t.Fatalf("get guild: %v", err)
	}
	pinnedID := afterCreate.Status.PinnedIdentityBeadID
	if pinnedID == "" {
		t.Fatalf("first cycle did not stamp PinnedIdentityBeadID")
	}

	// Delete the guild — finalizer keeps the object alive with
	// DeletionTimestamp set, matching real-kube behavior.
	if err := c.Delete(context.Background(), &afterCreate); err != nil {
		t.Fatalf("delete guild: %v", err)
	}

	// Second cycle: finalizer drains the bead and is removed.
	r.cycle(context.Background())

	if b, ok := st.beadsByID[pinnedID]; !ok {
		t.Errorf("pinned bead %q vanished from store after finalizer", pinnedID)
	} else if b.Status != "closed" {
		t.Errorf("pinned bead status = %q after finalizer; want closed", b.Status)
	}

	// The CR should be gone now (no finalizers left → kube reaps the
	// fake object when DeletionTimestamp is set).
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, &spirev1.WizardGuild{}); err == nil {
		t.Errorf("guild still present after finalizer cleanup; want NotFound")
	}
}

// TestCacheReconciler_PinnedIdentity_DeleteCleansWisps covers the
// load-bearing wisp-then-pinned ordering on the delete path. With
// open wisps pre-seeded as caused-by dependents of the pinned bead,
// the finalizer must close them all before closing the pinned bead.
func TestCacheReconciler_PinnedIdentity_DeleteCleansWisps(t *testing.T) {
	const (
		ns   = "spire"
		name = "core"
		repo = "git@example.com:spire-test/repo.git"
	)
	// Pre-stamp the guild with a known pinned ID and pre-seed two open
	// wisps as dependents — this lets the test inject the dependent
	// graph state without needing a real wisp-filing path (out of
	// scope for this task; spi-htay5).
	const pinnedID = "test-pinned-pre-stamped"
	guild := makeCacheGuild(name, ns, repo)
	guild.Finalizers = append(guild.Finalizers, pinnedIdentityFinalizer)
	guild.Status.PinnedIdentityBeadID = pinnedID

	c := fake.NewClientBuilder().
		WithScheme(newCacheTestScheme(t)).
		WithObjects(guild).
		WithStatusSubresource(&spirev1.WizardGuild{}).
		Build()
	st := newRecordingStore()
	st.beadsByID[pinnedID] = &recordedBead{ID: pinnedID, Status: "pinned", Pinned: true}
	st.dependents[pinnedID] = []*beads.IssueWithDependencyMetadata{
		makeWispDep("wisp-1", "", true, store.DepCausedBy),
		makeWispDep("wisp-2", "", true, store.DepCausedBy),
	}
	r := newCacheReconciler(t, c, ns)
	r.PinnedStore = st

	// Trigger delete + run cycle so the finalizer fires.
	var fetched spirev1.WizardGuild
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, &fetched); err != nil {
		t.Fatalf("get guild: %v", err)
	}
	if err := c.Delete(context.Background(), &fetched); err != nil {
		t.Fatalf("delete guild: %v", err)
	}
	r.cycle(context.Background())

	// All three closes happened in wisp→pinned order.
	closes := st.closeOps()
	want := []string{"wisp-1", "wisp-2", pinnedID}
	if !equalStringSlice(closes, want) {
		t.Errorf("CloseBead sequence = %v, want %v", closes, want)
	}
}

// TestCacheReconciler_PinnedIdentity_NoFinalizerOnMisconfig asserts a
// guild whose Cache is set but Repo is empty does NOT receive the
// finalizer. Attaching it would wedge the CR on delete (finalizer
// drainage runs but never advances past the misconfig) — better to
// surface the misconfig and leave the finalizer off.
func TestCacheReconciler_PinnedIdentity_NoFinalizerOnMisconfig(t *testing.T) {
	const (
		ns   = "spire"
		name = "misconfig"
	)
	guild := makeCacheGuild(name, ns, "") // empty Repo
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
	if controllerutil.ContainsFinalizer(&got, pinnedIdentityFinalizer) {
		t.Errorf("finalizer was attached to misconfigured guild (Repo=\"\"); finalizers=%v", got.Finalizers)
	}
}

// objectExists is a small Get-by-kind helper that returns true when
// the fake client has an object with the given name in the namespace.
// Lets the integration tests assert PVC/Job presence without
// repeating per-kind Get boilerplate.
func objectExists(t *testing.T, c client.Client, ns, name, kind string) bool {
	t.Helper()
	switch kind {
	case "PersistentVolumeClaim":
		err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, &corev1.PersistentVolumeClaim{})
		return err == nil
	case "Job":
		err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, &batchv1.Job{})
		return err == nil
	}
	t.Fatalf("objectExists: unsupported kind %q", kind)
	return false
}

// countCreateBead returns the number of CreateBead calls recorded by
// the fake store. Used by idempotency assertions.
func countCreateBead(st *recordingPinnedStore) int {
	n := 0
	for _, c := range st.calls {
		if c.Op == "CreateBead" {
			n++
		}
	}
	return n
}
