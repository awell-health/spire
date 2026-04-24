package store

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/awell-health/spire/pkg/config"
	"github.com/awell-health/spire/pkg/gatewayclient"
	"github.com/steveyegge/beads"
)

// setTestTower installs a tower resolver + gateway client factory for the
// duration of a test and restores the originals on cleanup. Passing a nil
// tower makes activeTower() succeed with a nil *TowerConfig (i.e. direct
// mode via the "no tower configured" fallback).
func setTestTower(t *testing.T, tower *config.TowerConfig, client *gatewayclient.Client) {
	t.Helper()
	prevTower := activeTowerFn
	prevClient := newGatewayClientFn
	activeTowerFn = func() (*config.TowerConfig, error) { return tower, nil }
	newGatewayClientFn = func(_ *config.TowerConfig) (*gatewayclient.Client, error) { return client, nil }
	t.Cleanup(func() {
		activeTowerFn = prevTower
		newGatewayClientFn = prevClient
	})
}

// gatewayTower returns a TowerConfig pointing at `url` in gateway mode.
// Token is ignored by tests (the fake server does not check auth) but we
// set it so gatewayClient() would succeed if its real body ever ran.
func gatewayTower(url string) *config.TowerConfig {
	return &config.TowerConfig{
		Name: "test-gateway",
		Mode: config.TowerModeGateway,
		URL:  url,
	}
}

// directTower returns a TowerConfig in explicit direct mode — the dispatch
// layer should treat it the same as "no tower" and fall through to the
// Dolt path.
func directTower() *config.TowerConfig {
	return &config.TowerConfig{
		Name: "test-direct",
		Mode: config.TowerModeDirect,
	}
}

// --- Shared mocks for the direct-mode branch ---

// dispatchMockStorage is the minimum beads.Storage surface the direct-mode
// branches of GetBead / CreateBead / SendMessage need. Each mutation or
// query is recorded so assertions can check behavior end-to-end.
type dispatchMockStorage struct {
	beads.Storage

	issues map[string]*beads.Issue

	createCalls  []*beads.Issue
	updateCalls  []map[string]interface{}
	closeCalls   []string
	depCalls     []*beads.Dependency
	searchFilter beads.IssueFilter

	nextID string
}

func (m *dispatchMockStorage) GetIssue(_ context.Context, id string) (*beads.Issue, error) {
	if issue, ok := m.issues[id]; ok {
		return issue, nil
	}
	return nil, errors.New("not found")
}

func (m *dispatchMockStorage) GetDependenciesWithMetadata(_ context.Context, _ string) ([]*beads.IssueWithDependencyMetadata, error) {
	return nil, nil
}

func (m *dispatchMockStorage) SearchIssues(_ context.Context, _ string, filter beads.IssueFilter) ([]*beads.Issue, error) {
	m.searchFilter = filter
	var out []*beads.Issue
	for _, issue := range m.issues {
		out = append(out, issue)
	}
	return out, nil
}

func (m *dispatchMockStorage) CreateIssue(_ context.Context, issue *beads.Issue, _ string) error {
	if issue.ID == "" {
		issue.ID = m.nextID
		if issue.ID == "" {
			issue.ID = "mock-" + string(issue.IssueType)
		}
	}
	m.createCalls = append(m.createCalls, issue)
	if m.issues == nil {
		m.issues = map[string]*beads.Issue{}
	}
	m.issues[issue.ID] = issue
	return nil
}

func (m *dispatchMockStorage) UpdateIssue(_ context.Context, _ string, updates map[string]interface{}, _ string) error {
	cp := make(map[string]interface{}, len(updates))
	for k, v := range updates {
		cp[k] = v
	}
	m.updateCalls = append(m.updateCalls, cp)
	return nil
}

func (m *dispatchMockStorage) CloseIssue(_ context.Context, id, _, _, _ string) error {
	m.closeCalls = append(m.closeCalls, id)
	return nil
}

func (m *dispatchMockStorage) AddDependency(_ context.Context, dep *beads.Dependency, _ string) error {
	m.depCalls = append(m.depCalls, dep)
	return nil
}

func (m *dispatchMockStorage) Close() error { return nil }

// --- GetBead ---

func TestGetBead_GatewayMode(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(gatewayclient.BeadRecord{
			ID:       "spi-a3f8",
			Title:    "hello from gateway",
			Status:   "open",
			Priority: 1,
			Type:     "task",
			Labels:   []string{"msg"},
		})
	}))
	defer srv.Close()

	setTestTower(t, gatewayTower(srv.URL), gatewayclient.NewClient(srv.URL, "tok"))

	got, err := GetBead("spi-a3f8")
	if err != nil {
		t.Fatalf("GetBead: %v", err)
	}
	if gotPath != "/api/v1/beads/spi-a3f8" {
		t.Errorf("path = %q, want /api/v1/beads/spi-a3f8", gotPath)
	}
	if got.ID != "spi-a3f8" || got.Title != "hello from gateway" {
		t.Errorf("bead = %+v", got)
	}
}

func TestGetBead_GatewayNotFoundMapsToErrNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	setTestTower(t, gatewayTower(srv.URL), gatewayclient.NewClient(srv.URL, "tok"))

	_, err := GetBead("spi-missing")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestGetBead_DirectMode(t *testing.T) {
	mock := &dispatchMockStorage{
		issues: map[string]*beads.Issue{
			"spi-a3f8": {
				ID:        "spi-a3f8",
				Title:     "hello from dolt",
				Status:    beads.StatusOpen,
				Priority:  1,
				IssueType: beads.TypeTask,
			},
		},
	}
	setTestStore(t, mock)
	setTestTower(t, directTower(), nil)

	got, err := GetBead("spi-a3f8")
	if err != nil {
		t.Fatalf("GetBead: %v", err)
	}
	if got.ID != "spi-a3f8" || got.Title != "hello from dolt" {
		t.Errorf("bead = %+v", got)
	}
}

// --- CreateBead ---

func TestCreateBead_GatewayMode(t *testing.T) {
	var gotMethod, gotPath string
	var gotBody gatewayclient.CreateBeadInput
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"id": "spi-new1"})
	}))
	defer srv.Close()

	setTestTower(t, gatewayTower(srv.URL), gatewayclient.NewClient(srv.URL, "tok"))

	id, err := CreateBead(CreateOpts{
		Title:       "new bead",
		Description: "body",
		Priority:    2,
		Type:        beads.TypeTask,
		Labels:      []string{"x"},
		Parent:      "spi-epic1",
		Prefix:      "spi",
	})
	if err != nil {
		t.Fatalf("CreateBead: %v", err)
	}
	if id != "spi-new1" {
		t.Errorf("id = %q, want spi-new1", id)
	}
	if gotMethod != http.MethodPost || gotPath != "/api/v1/beads" {
		t.Errorf("method/path = %s %s", gotMethod, gotPath)
	}
	if gotBody.Title != "new bead" || gotBody.Type != "task" || gotBody.Parent != "spi-epic1" {
		t.Errorf("body = %+v", gotBody)
	}
}

func TestCreateBead_DirectMode(t *testing.T) {
	mock := &dispatchMockStorage{nextID: "spi-new-direct"}
	setTestStore(t, mock)
	setTestTower(t, directTower(), nil)

	id, err := CreateBead(CreateOpts{
		Title:    "new bead",
		Priority: 1,
		Type:     beads.TypeTask,
		Parent:   "spi-parent",
	})
	if err != nil {
		t.Fatalf("CreateBead: %v", err)
	}
	if id != "spi-new-direct" {
		t.Errorf("id = %q, want spi-new-direct", id)
	}
	if len(mock.createCalls) != 1 {
		t.Fatalf("expected 1 CreateIssue call, got %d", len(mock.createCalls))
	}
	if len(mock.depCalls) != 1 || mock.depCalls[0].DependsOnID != "spi-parent" {
		t.Errorf("expected parent dep, got %+v", mock.depCalls)
	}
}

// --- SendMessage ---

func TestSendMessage_GatewayMode(t *testing.T) {
	var gotMethod, gotPath string
	var gotBody gatewayclient.SendMessageInput
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"id": "spi-msg1"})
	}))
	defer srv.Close()

	setTestTower(t, gatewayTower(srv.URL), gatewayclient.NewClient(srv.URL, "tok"))

	id, err := SendMessage(SendMessageOpts{
		To:       "wizard",
		From:     "steward",
		Message:  "ping",
		Ref:      "spi-a3f8",
		Priority: 2,
	})
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if id != "spi-msg1" {
		t.Errorf("id = %q, want spi-msg1", id)
	}
	if gotMethod != http.MethodPost || gotPath != "/api/v1/messages" {
		t.Errorf("method/path = %s %s", gotMethod, gotPath)
	}
	if gotBody.To != "wizard" || gotBody.From != "steward" || gotBody.Message != "ping" || gotBody.Ref != "spi-a3f8" {
		t.Errorf("body = %+v", gotBody)
	}
}

func TestSendMessage_DirectModeStampsLabels(t *testing.T) {
	mock := &dispatchMockStorage{nextID: "spi-msg-direct"}
	setTestStore(t, mock)
	setTestTower(t, directTower(), nil)

	id, err := SendMessage(SendMessageOpts{
		To:       "wizard",
		From:     "steward",
		Message:  "ping",
		Ref:      "spi-a3f8",
		Priority: 2,
	})
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if id != "spi-msg-direct" {
		t.Errorf("id = %q, want spi-msg-direct", id)
	}
	if len(mock.createCalls) != 1 {
		t.Fatalf("expected 1 CreateIssue call, got %d", len(mock.createCalls))
	}
	got := mock.createCalls[0]
	if got.IssueType != beads.IssueType("message") {
		t.Errorf("issue type = %q, want message", got.IssueType)
	}
	wantLabels := map[string]bool{"msg": true, "to:wizard": true, "from:steward": true, "ref:spi-a3f8": true}
	for _, l := range got.Labels {
		if !wantLabels[l] {
			t.Errorf("unexpected label %q", l)
		}
		delete(wantLabels, l)
	}
	if len(wantLabels) != 0 {
		t.Errorf("missing labels: %v", wantLabels)
	}
}

// --- Read / Update / ListBeads — smaller coverage to round out the table ---

func TestListBeads_GatewayMode(t *testing.T) {
	var gotRawQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRawQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]gatewayclient.BeadRecord{
			{ID: "spi-1", Title: "one", Status: "open"},
			{ID: "spi-2", Title: "two", Status: "open"},
		})
	}))
	defer srv.Close()

	setTestTower(t, gatewayTower(srv.URL), gatewayclient.NewClient(srv.URL, "tok"))

	open := beads.StatusOpen
	got, err := ListBeads(beads.IssueFilter{
		Status:   &open,
		IDPrefix: "spi-",
		Labels:   []string{"msg", "to:wizard"},
	})
	if err != nil {
		t.Fatalf("ListBeads: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 beads, got %d", len(got))
	}
	// Query params are unordered; check each substring instead.
	for _, want := range []string{"status=open", "prefix=spi", "label=msg%2Cto%3Awizard"} {
		if !contains(gotRawQuery, want) {
			t.Errorf("rawQuery %q missing %q", gotRawQuery, want)
		}
	}
}

func TestUpdateBead_GatewayMode(t *testing.T) {
	var gotMethod, gotPath string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
	}))
	defer srv.Close()

	setTestTower(t, gatewayTower(srv.URL), gatewayclient.NewClient(srv.URL, "tok"))

	if err := UpdateBead("spi-a3f8", map[string]interface{}{"status": "closed"}); err != nil {
		t.Fatalf("UpdateBead: %v", err)
	}
	if gotMethod != http.MethodPatch || gotPath != "/api/v1/beads/spi-a3f8" {
		t.Errorf("method/path = %s %s", gotMethod, gotPath)
	}
	if gotBody["status"] != "closed" {
		t.Errorf("body.status = %v, want closed", gotBody["status"])
	}
}

func TestMarkMessageRead_GatewayMode(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
	}))
	defer srv.Close()

	setTestTower(t, gatewayTower(srv.URL), gatewayclient.NewClient(srv.URL, "tok"))

	if err := MarkMessageRead("spi-msg1"); err != nil {
		t.Fatalf("MarkMessageRead: %v", err)
	}
	if gotMethod != http.MethodPost || gotPath != "/api/v1/messages/spi-msg1/read" {
		t.Errorf("method/path = %s %s", gotMethod, gotPath)
	}
}

func TestMarkMessageRead_DirectMode(t *testing.T) {
	mock := &dispatchMockStorage{}
	setTestStore(t, mock)
	setTestTower(t, directTower(), nil)

	if err := MarkMessageRead("spi-msg1"); err != nil {
		t.Fatalf("MarkMessageRead: %v", err)
	}
	if len(mock.closeCalls) != 1 || mock.closeCalls[0] != "spi-msg1" {
		t.Errorf("expected CloseIssue(spi-msg1), got %v", mock.closeCalls)
	}
}

// --- ListDeps / GetBlockedIssues gateway paths ---

func TestListDeps_GatewayMode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]gatewayclient.DepRecord{
			{IssueID: "spi-a3f8", DependsOnID: "spi-b", Type: "blocks"},
		})
	}))
	defer srv.Close()

	setTestTower(t, gatewayTower(srv.URL), gatewayclient.NewClient(srv.URL, "tok"))

	deps, err := ListDeps("spi-a3f8")
	if err != nil {
		t.Fatalf("ListDeps: %v", err)
	}
	if len(deps) != 1 || deps[0].DependsOnID != "spi-b" || deps[0].Type != "blocks" {
		t.Errorf("deps = %+v", deps)
	}
}

func TestGetBlockedIssues_GatewayMode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(gatewayclient.BlockedIssues{
			Count: 2,
			IDs:   []string{"spi-1", "spi-2"},
		})
	}))
	defer srv.Close()

	setTestTower(t, gatewayTower(srv.URL), gatewayclient.NewClient(srv.URL, "tok"))

	got, err := GetBlockedIssues(beads.WorkFilter{})
	if err != nil {
		t.Fatalf("GetBlockedIssues: %v", err)
	}
	if len(got) != 2 || got[0].ID != "spi-1" || got[1].ID != "spi-2" {
		t.Errorf("got = %+v", got)
	}
}

// --- Utility helpers for the tests ---

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// --- Mode resolution ---

// TestDispatch_DirectFallbackOnTowerError verifies that a tower-resolution
// failure does NOT break direct-mode callers — it silently falls through
// to the Dolt path. This keeps local dev (no tower configured) working.
func TestDispatch_DirectFallbackOnTowerError(t *testing.T) {
	prev := activeTowerFn
	activeTowerFn = func() (*config.TowerConfig, error) { return nil, errors.New("no tower") }
	t.Cleanup(func() { activeTowerFn = prev })

	mock := &dispatchMockStorage{
		issues: map[string]*beads.Issue{
			"spi-a3f8": {ID: "spi-a3f8", Title: "local", IssueType: beads.TypeTask},
		},
	}
	setTestStore(t, mock)

	got, err := GetBead("spi-a3f8")
	if err != nil {
		t.Fatalf("GetBead: %v", err)
	}
	if got.ID != "spi-a3f8" {
		t.Errorf("bead = %+v", got)
	}
}
