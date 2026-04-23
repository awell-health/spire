package steward

// Tests for the cluster-native dispatch scheduling seams added in
// spi-0fek6l: the countInFlight predicate (dispatched + in_progress)
// and the ClusterDispatchConfig.MaxConcurrent cap applied before
// ClaimThenEmit.

import (
	"context"
	"errors"
	"testing"

	"github.com/awell-health/spire/pkg/bd"
	"github.com/awell-health/spire/pkg/steward/dispatch"
	"github.com/awell-health/spire/pkg/steward/identity"
	"github.com/awell-health/spire/pkg/steward/intent"
	"github.com/awell-health/spire/pkg/store"
	"github.com/steveyegge/beads"
)

// --- countInFlight ---

func TestCountInFlight_CountsDispatchedAndInProgress(t *testing.T) {
	origList := ListBeadsFunc
	defer func() { ListBeadsFunc = origList }()

	ListBeadsFunc = func(filter beads.IssueFilter) ([]store.Bead, error) {
		if filter.Status == nil {
			return nil, nil
		}
		switch *filter.Status {
		case beads.StatusInProgress:
			return []store.Bead{
				{ID: "spi-a", Type: "task"},
				{ID: "spi-b", Type: "bug"},
			}, nil
		case beads.Status(bd.StatusDispatched):
			return []store.Bead{
				{ID: "spi-c", Type: "task"},
			}, nil
		}
		return nil, nil
	}

	got := countInFlight()
	if got != 3 {
		t.Fatalf("countInFlight = %d, want 3 (2 in_progress + 1 dispatched)", got)
	}
}

func TestCountInFlight_SkipsInternalAndChildBeads(t *testing.T) {
	origList := ListBeadsFunc
	defer func() { ListBeadsFunc = origList }()

	// Internal types (attempt, review, step, message) and children
	// (non-empty Parent) must not count toward the in-flight cap —
	// IsWorkBead is the gate.
	ListBeadsFunc = func(filter beads.IssueFilter) ([]store.Bead, error) {
		switch *filter.Status {
		case beads.StatusInProgress:
			return []store.Bead{
				{ID: "spi-attempt", Type: "attempt"},          // internal
				{ID: "spi-child", Type: "task", Parent: "spi-a"}, // child
				{ID: "spi-top", Type: "task"},                   // counts
			}, nil
		case beads.Status(bd.StatusDispatched):
			return []store.Bead{
				{ID: "spi-review", Type: "review"}, // internal
				{ID: "spi-top2", Type: "bug"},      // counts
			}, nil
		}
		return nil, nil
	}

	got := countInFlight()
	if got != 2 {
		t.Fatalf("countInFlight = %d, want 2 (only top-level work beads)", got)
	}
}

func TestCountInFlight_ListErrorIsSoftFailure(t *testing.T) {
	origList := ListBeadsFunc
	defer func() { ListBeadsFunc = origList }()

	// Both list calls error — count should return 0 without panicking.
	ListBeadsFunc = func(filter beads.IssueFilter) ([]store.Bead, error) {
		return nil, errors.New("list failure")
	}

	got := countInFlight()
	if got != 0 {
		t.Fatalf("countInFlight on errors = %d, want 0", got)
	}
}

// --- dispatchClusterNative concurrency cap ---

// fakeClaimer is a dispatch.AttemptClaimer that always succeeds with a
// pre-canned ClaimHandle. It records how many ClaimNext calls happened
// so tests can assert the cap truncated the cycle.
type fakeClaimer struct {
	claims int
}

func (f *fakeClaimer) ClaimNext(_ context.Context, sel dispatch.ReadyWorkSelector) (*dispatch.ClaimHandle, error) {
	ids, err := sel.SelectReady(context.Background())
	if err != nil || len(ids) == 0 {
		return nil, nil
	}
	f.claims++
	return &dispatch.ClaimHandle{
		TaskID:      ids[0],
		DispatchSeq: 1,
		Reason:      "test",
	}, nil
}

// fakePublisher records Publish calls so tests can assert how many
// intents were emitted.
type fakePublisher struct {
	published int
}

func (f *fakePublisher) Publish(_ context.Context, _ intent.WorkloadIntent) error {
	f.published++
	return nil
}

// fakeResolver returns a canned ClusterRepoIdentity for any prefix.
type fakeResolver struct{}

func (fakeResolver) Resolve(_ context.Context, prefix string) (identity.ClusterRepoIdentity, error) {
	return identity.ClusterRepoIdentity{
		Prefix:     prefix,
		URL:        "git@example.test:repo.git",
		BaseBranch: "main",
	}, nil
}

func TestDispatchClusterNative_CapAlreadyReached(t *testing.T) {
	origList := ListBeadsFunc
	defer func() { ListBeadsFunc = origList }()

	// Simulate: cap=2, already 2 in-flight → zero emits, zero claims.
	ListBeadsFunc = func(filter beads.IssueFilter) ([]store.Bead, error) {
		switch *filter.Status {
		case beads.StatusInProgress:
			return []store.Bead{{ID: "spi-a", Type: "task"}, {ID: "spi-b", Type: "task"}}, nil
		}
		return nil, nil
	}

	cl := &fakeClaimer{}
	pub := &fakePublisher{}
	cfg := StewardConfig{
		ClusterDispatch: &ClusterDispatchConfig{
			Resolver:      fakeResolver{},
			Claimer:       cl,
			Publisher:     pub,
			MaxConcurrent: 2,
		},
	}

	emitted := dispatchClusterNative(context.Background(), "", []store.Bead{
		{ID: "spi-x", Type: "task"},
		{ID: "spi-y", Type: "task"},
	}, cfg)
	if emitted != 0 {
		t.Errorf("emitted = %d, want 0 (cap already reached)", emitted)
	}
	if cl.claims != 0 {
		t.Errorf("claimer.claims = %d, want 0 (no work attempted when capped)", cl.claims)
	}
	if pub.published != 0 {
		t.Errorf("publisher.published = %d, want 0", pub.published)
	}
}

func TestDispatchClusterNative_CapPartialRemaining(t *testing.T) {
	origList := ListBeadsFunc
	defer func() { ListBeadsFunc = origList }()

	// cap=3, 1 already in-flight → remaining=2; three candidates offered,
	// only two may emit before we break on remaining==0.
	ListBeadsFunc = func(filter beads.IssueFilter) ([]store.Bead, error) {
		switch *filter.Status {
		case beads.StatusInProgress:
			return []store.Bead{{ID: "spi-a", Type: "task"}}, nil
		}
		return nil, nil
	}

	cl := &fakeClaimer{}
	pub := &fakePublisher{}
	cfg := StewardConfig{
		ClusterDispatch: &ClusterDispatchConfig{
			Resolver:      fakeResolver{},
			Claimer:       cl,
			Publisher:     pub,
			MaxConcurrent: 3,
		},
	}

	emitted := dispatchClusterNative(context.Background(), "", []store.Bead{
		{ID: "spi-x", Type: "task"},
		{ID: "spi-y", Type: "task"},
		{ID: "spi-z", Type: "task"},
	}, cfg)
	if emitted != 2 {
		t.Errorf("emitted = %d, want 2 (remaining slots = 3-1)", emitted)
	}
	if pub.published != 2 {
		t.Errorf("publisher.published = %d, want 2", pub.published)
	}
}

func TestDispatchClusterNative_UnlimitedWhenMaxZero(t *testing.T) {
	origList := ListBeadsFunc
	defer func() { ListBeadsFunc = origList }()

	// MaxConcurrent=0 must disable the cap entirely — no ListBeadsFunc
	// calls for the count, and all candidates are offered to the claimer.
	listCalled := 0
	ListBeadsFunc = func(filter beads.IssueFilter) ([]store.Bead, error) {
		listCalled++
		return nil, nil
	}

	cl := &fakeClaimer{}
	pub := &fakePublisher{}
	cfg := StewardConfig{
		ClusterDispatch: &ClusterDispatchConfig{
			Resolver:      fakeResolver{},
			Claimer:       cl,
			Publisher:     pub,
			MaxConcurrent: 0,
		},
	}

	emitted := dispatchClusterNative(context.Background(), "", []store.Bead{
		{ID: "spi-x", Type: "task"},
		{ID: "spi-y", Type: "task"},
		{ID: "spi-z", Type: "task"},
	}, cfg)
	if emitted != 3 {
		t.Errorf("emitted = %d, want 3 (unlimited)", emitted)
	}
	if listCalled != 0 {
		t.Errorf("ListBeadsFunc called %d times when cap disabled, want 0", listCalled)
	}
}
