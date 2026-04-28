package cleric

import (
	"database/sql"
	"errors"
	"testing"

	"github.com/awell-health/spire/pkg/store"
)

// fakeLearningStore is a deterministic in-memory LearningStore used by the
// promotion/demotion tests. addRow is chronological — first call is the
// oldest outcome, last call is the newest. LastNFinalizedOutcomes
// reverses on read to match the production newest-first ORDER BY DESC
// contract.
type fakeLearningStore struct {
	byPair  map[string][]store.ClericOutcome // chronological, oldest first
	demoted []store.DemotedClericPair
	err     error
}

func newFakeLearningStore() *fakeLearningStore {
	return &fakeLearningStore{byPair: map[string][]store.ClericOutcome{}}
}

func pairKey(fc, action string) string { return fc + "|" + action }

func (f *fakeLearningStore) LastNFinalizedOutcomes(fc, action string, n int) ([]store.ClericOutcome, error) {
	if f.err != nil {
		return nil, f.err
	}
	chrono := f.byPair[pairKey(fc, action)]
	rev := make([]store.ClericOutcome, len(chrono))
	for i, r := range chrono {
		rev[len(chrono)-1-i] = r
	}
	if len(rev) > n {
		rev = rev[:n]
	}
	return rev, nil
}

func (f *fakeLearningStore) ListDemotedPairs(threshold int) ([]store.DemotedClericPair, error) {
	if f.err != nil {
		return nil, f.err
	}
	return append([]store.DemotedClericPair{}, f.demoted...), nil
}

// addRow appends an outcome chronologically — first call is the oldest,
// most recent call is the newest. Tests that want a particular row to be
// "the latest reject" must call addRow for it last.
func (f *fakeLearningStore) addRow(fc, action, gate string, success *bool) {
	o := store.ClericOutcome{
		FailureClass: fc,
		Action:       action,
		Gate:         gate,
		Finalized:    true,
	}
	if success != nil {
		o.WizardPostActionSuccess = sql.NullBool{Bool: *success, Valid: true}
	}
	key := pairKey(fc, action)
	f.byPair[key] = append(f.byPair[key], o)
}

func boolPtr(b bool) *bool { return &b }

// --- IsPromoted ---

func TestIsPromoted_ThreeApproveSuccess(t *testing.T) {
	s := newFakeLearningStore()
	for i := 0; i < 3; i++ {
		s.addRow("step-failure:implement", "resummon", "approve", boolPtr(true))
	}
	if !IsPromoted(s, "step-failure:implement", "resummon") {
		t.Fatal("expected promoted after 3 approve+success outcomes")
	}
}

func TestIsPromoted_OneRejectResets(t *testing.T) {
	s := newFakeLearningStore()
	// 3 approve+success, then 1 reject (most recent).
	for i := 0; i < 3; i++ {
		s.addRow("step-failure:implement", "resummon", "approve", boolPtr(true))
	}
	s.addRow("step-failure:implement", "resummon", "reject", nil)
	if IsPromoted(s, "step-failure:implement", "resummon") {
		t.Fatal("a single reject should reset the promotion streak")
	}
}

func TestIsPromoted_ApproveButFailedSuccessResets(t *testing.T) {
	s := newFakeLearningStore()
	// Chronological: 2 approve+success, then approve+success=false (newest).
	s.addRow("fc", "act", "approve", boolPtr(true))
	s.addRow("fc", "act", "approve", boolPtr(true))
	s.addRow("fc", "act", "approve", boolPtr(false))
	if IsPromoted(s, "fc", "act") {
		t.Fatal("approve+success=false should not count toward promotion")
	}
}

func TestIsPromoted_FewerThanThreshold(t *testing.T) {
	s := newFakeLearningStore()
	s.addRow("fc", "act", "approve", boolPtr(true))
	s.addRow("fc", "act", "approve", boolPtr(true))
	if IsPromoted(s, "fc", "act") {
		t.Fatal("two approves should not promote (threshold=3)")
	}
}

func TestIsPromoted_TakeoverBetweenApproves(t *testing.T) {
	s := newFakeLearningStore()
	// Chronological: approve, takeover, approve (newest). The window holds
	// all three, so the middle takeover breaks the streak.
	s.addRow("fc", "act", "approve", boolPtr(true))
	s.addRow("fc", "act", "takeover", nil)
	s.addRow("fc", "act", "approve", boolPtr(true))
	if IsPromoted(s, "fc", "act") {
		t.Fatal("takeover in window should break promotion streak")
	}
}

func TestIsPromoted_StoreError(t *testing.T) {
	s := newFakeLearningStore()
	s.err = errors.New("boom")
	if IsPromoted(s, "fc", "act") {
		t.Fatal("store error should fail safe (return false)")
	}
}

func TestIsPromoted_NilStore(t *testing.T) {
	if IsPromoted(nil, "fc", "act") {
		t.Fatal("nil store should fail safe (return false)")
	}
}

// --- IsDemoted ---

func TestIsDemoted_ThreeRejects(t *testing.T) {
	s := newFakeLearningStore()
	for i := 0; i < 3; i++ {
		s.addRow("fc", "act", "reject", nil)
	}
	if !IsDemoted(s, "fc", "act") {
		t.Fatal("expected demoted after 3 rejects")
	}
}

func TestIsDemoted_OneApproveResets(t *testing.T) {
	s := newFakeLearningStore()
	// Chronological: 3 rejects then 1 approve. Approve is the newest row.
	s.addRow("fc", "act", "reject", nil)
	s.addRow("fc", "act", "reject", nil)
	s.addRow("fc", "act", "reject", nil)
	s.addRow("fc", "act", "approve", boolPtr(true))
	if IsDemoted(s, "fc", "act") {
		t.Fatal("a single approve at the head of the streak should reset demotion")
	}
}

func TestIsDemoted_TakeoverDoesNotCount(t *testing.T) {
	s := newFakeLearningStore()
	s.addRow("fc", "act", "takeover", nil)
	s.addRow("fc", "act", "reject", nil)
	s.addRow("fc", "act", "reject", nil)
	if IsDemoted(s, "fc", "act") {
		t.Fatal("a takeover in window should break demotion streak (not all reject)")
	}
}

// --- Pair independence ---

func TestPromotion_PairsIndependent(t *testing.T) {
	s := newFakeLearningStore()
	for i := 0; i < 3; i++ {
		s.addRow("fc-a", "resummon", "approve", boolPtr(true))
	}
	// Pair B has no outcomes — must not promote.
	if !IsPromoted(s, "fc-a", "resummon") {
		t.Fatal("pair A should be promoted")
	}
	if IsPromoted(s, "fc-b", "resummon") {
		t.Fatal("pair B (no outcomes) must not be promoted")
	}
	if IsPromoted(s, "fc-a", "dismiss") {
		t.Fatal("different action for fc-a must not be promoted")
	}
}

// --- ListDemoted ---

func TestListDemoted_ReturnsStorePairs(t *testing.T) {
	s := newFakeLearningStore()
	s.demoted = []store.DemotedClericPair{
		{FailureClass: "fc-a", Action: "resummon"},
		{FailureClass: "fc-b", Action: "dismiss"},
	}
	got := ListDemoted(s)
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
}

func TestListDemoted_EmptyOnError(t *testing.T) {
	s := newFakeLearningStore()
	s.err = errors.New("boom")
	got := ListDemoted(s)
	if len(got) != 0 {
		t.Fatalf("len = %d on error, want 0", len(got))
	}
}
