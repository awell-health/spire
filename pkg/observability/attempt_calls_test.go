package observability

import (
	"errors"
	"strings"
	"testing"

	"github.com/awell-health/spire/pkg/olap"
	"github.com/awell-health/spire/pkg/store"
)

// withFakeAttemptDeps swaps both seams used by ListAttemptToolCalls and
// restores them on cleanup. Returns recorders so tests can assert what
// the function forwarded.
type fakeAttemptDeps struct {
	gotAttemptID string
	gotSession   string
	gotLimit     int
	gotOffset    int
	calledQuery  bool
}

func withFakeAttemptDeps(
	t *testing.T,
	meta *store.InstanceMeta, metaErr error,
	rows []olap.ToolCallRecord, queryErr error,
) *fakeAttemptDeps {
	t.Helper()

	rec := &fakeAttemptDeps{}

	origMeta := getAttemptInstanceFunc
	getAttemptInstanceFunc = func(id string) (*store.InstanceMeta, error) {
		rec.gotAttemptID = id
		return meta, metaErr
	}
	t.Cleanup(func() { getAttemptInstanceFunc = origMeta })

	origQuery := queryToolCallsBySession
	queryToolCallsBySession = func(sessionID string, limit, offset int) ([]olap.ToolCallRecord, error) {
		rec.calledQuery = true
		rec.gotSession = sessionID
		rec.gotLimit = limit
		rec.gotOffset = offset
		return rows, queryErr
	}
	t.Cleanup(func() { queryToolCallsBySession = origQuery })

	return rec
}

// --- pageSize clamping ---

func TestListAttemptToolCalls_DefaultPageSize(t *testing.T) {
	rec := withFakeAttemptDeps(t,
		&store.InstanceMeta{SessionID: "sess-1"}, nil,
		nil, nil,
	)

	if _, err := ListAttemptToolCalls("att-x", 1, 0); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.gotLimit != DefaultToolCallPageSize {
		t.Errorf("limit = %d, want %d", rec.gotLimit, DefaultToolCallPageSize)
	}
}

func TestListAttemptToolCalls_NegativePageSizeFallsToDefault(t *testing.T) {
	rec := withFakeAttemptDeps(t,
		&store.InstanceMeta{SessionID: "sess-1"}, nil,
		nil, nil,
	)
	if _, err := ListAttemptToolCalls("att-x", 1, -50); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.gotLimit != DefaultToolCallPageSize {
		t.Errorf("limit = %d, want %d (fallback to default on negative)", rec.gotLimit, DefaultToolCallPageSize)
	}
}

func TestListAttemptToolCalls_MaxPageSizeClamp(t *testing.T) {
	rec := withFakeAttemptDeps(t,
		&store.InstanceMeta{SessionID: "sess-1"}, nil,
		nil, nil,
	)
	if _, err := ListAttemptToolCalls("att-x", 1, 99999); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.gotLimit != MaxToolCallPageSize {
		t.Errorf("limit = %d, want clamped to %d", rec.gotLimit, MaxToolCallPageSize)
	}
}

// --- page clamping / offset arithmetic ---

func TestListAttemptToolCalls_PageZeroBecomesOne(t *testing.T) {
	rec := withFakeAttemptDeps(t,
		&store.InstanceMeta{SessionID: "sess-1"}, nil,
		nil, nil,
	)
	if _, err := ListAttemptToolCalls("att-x", 0, 100); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.gotOffset != 0 {
		t.Errorf("offset = %d, want 0 (page=0 → page=1)", rec.gotOffset)
	}
}

func TestListAttemptToolCalls_NegativePageBecomesOne(t *testing.T) {
	rec := withFakeAttemptDeps(t,
		&store.InstanceMeta{SessionID: "sess-1"}, nil,
		nil, nil,
	)
	if _, err := ListAttemptToolCalls("att-x", -3, 100); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.gotOffset != 0 {
		t.Errorf("offset = %d, want 0 (negative page → page=1)", rec.gotOffset)
	}
}

func TestListAttemptToolCalls_OffsetArithmetic(t *testing.T) {
	rec := withFakeAttemptDeps(t,
		&store.InstanceMeta{SessionID: "sess-1"}, nil,
		nil, nil,
	)
	if _, err := ListAttemptToolCalls("att-x", 4, 50); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.gotOffset != 150 {
		t.Errorf("offset = %d, want (4-1)*50 = 150", rec.gotOffset)
	}
	if rec.gotLimit != 50 {
		t.Errorf("limit = %d, want 50", rec.gotLimit)
	}
}

// --- nil-session / pre-migration handling ---

func TestListAttemptToolCalls_NilMetaReturnsEmptyNotError(t *testing.T) {
	rec := withFakeAttemptDeps(t,
		nil, nil,
		nil, nil,
	)
	rows, err := ListAttemptToolCalls("att-legacy", 1, 200)
	if err != nil {
		t.Fatalf("expected no error for nil meta, got %v", err)
	}
	if rows != nil {
		t.Errorf("expected nil rows, got %v", rows)
	}
	if rec.calledQuery {
		t.Error("queryToolCallsBySession should not be called when meta is nil")
	}
}

func TestListAttemptToolCalls_EmptySessionReturnsEmptyNotError(t *testing.T) {
	rec := withFakeAttemptDeps(t,
		&store.InstanceMeta{InstanceID: "inst-1", SessionID: ""}, nil,
		nil, nil,
	)
	rows, err := ListAttemptToolCalls("att-blank", 1, 200)
	if err != nil {
		t.Fatalf("expected no error for blank session_id, got %v", err)
	}
	if rows != nil {
		t.Errorf("expected nil rows, got %v", rows)
	}
	if rec.calledQuery {
		t.Error("queryToolCallsBySession should not be called when session_id is empty")
	}
}

// --- error wrapping ---

func TestListAttemptToolCalls_MetaReadError(t *testing.T) {
	withFakeAttemptDeps(t,
		nil, errors.New("metadata read failed"),
		nil, nil,
	)
	_, err := ListAttemptToolCalls("att-broken", 1, 200)
	if err == nil {
		t.Fatal("expected error from metadata read failure")
	}
	if !strings.Contains(err.Error(), "metadata read failed") {
		t.Errorf("error should wrap underlying cause, got %v", err)
	}
	if !strings.Contains(err.Error(), "att-broken") {
		t.Errorf("error should include attempt id, got %v", err)
	}
}

func TestListAttemptToolCalls_QueryError(t *testing.T) {
	withFakeAttemptDeps(t,
		&store.InstanceMeta{SessionID: "sess-bad"}, nil,
		nil, errors.New("olap query exploded"),
	)
	_, err := ListAttemptToolCalls("att-bad", 1, 200)
	if err == nil {
		t.Fatal("expected error from query failure")
	}
	if !strings.Contains(err.Error(), "olap query exploded") {
		t.Errorf("error should wrap underlying cause, got %v", err)
	}
	if !strings.Contains(err.Error(), "sess-bad") {
		t.Errorf("error should include session id, got %v", err)
	}
}

// --- happy path ---

func TestListAttemptToolCalls_ForwardsAttemptIDAndReturnsRows(t *testing.T) {
	want := []olap.ToolCallRecord{
		{ToolName: "Bash", Source: "span", Success: true},
		{ToolName: "Read", Source: "span", Success: true},
	}
	rec := withFakeAttemptDeps(t,
		&store.InstanceMeta{SessionID: "sess-good"}, nil,
		want, nil,
	)
	got, err := ListAttemptToolCalls("att-good", 1, 200)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.gotAttemptID != "att-good" {
		t.Errorf("attempt id forwarded as %q, want att-good", rec.gotAttemptID)
	}
	if rec.gotSession != "sess-good" {
		t.Errorf("session id forwarded as %q, want sess-good", rec.gotSession)
	}
	if len(got) != 2 || got[0].ToolName != "Bash" {
		t.Errorf("rows mismatch: %+v", got)
	}
}
