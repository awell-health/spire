package store

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/awell-health/spire/pkg/config"
	"github.com/awell-health/spire/pkg/gatewayclient"
	storetest "github.com/awell-health/spire/pkg/store/testing"
	"github.com/steveyegge/beads"
)

// installPanicStore swaps the active store for a PanicStore for the
// duration of a test. Any direct-mode call that escapes dispatch lands
// on a PanicStore method and panics with PanicMessage — which Recover
// surfaces as a test failure with the leaked operation name.
func installPanicStore(t *testing.T) {
	t.Helper()
	cleanup := SetTestStorage(storetest.PanicStore{})
	t.Cleanup(cleanup)
}

// TestDispatch_PanicStoreBackstop installs a gateway tower + a PanicStore
// and walks every dispatched API. Each call must either succeed via the
// fake gateway server or fail with ErrGatewayUnsupported. If any call
// reaches the local store, the PanicStore panics and the test fails.
//
// This is the structural regression the epic calls for: gateway-mode
// safety must hold whether the call routes (Gateway has the endpoint)
// or fails closed (Gateway has no endpoint yet). Either result is
// acceptable; what isn't acceptable is silent local mutation.
func TestDispatch_PanicStoreBackstop(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Default response: empty JSON body. Endpoint-specific shapes
		// matter only to the few tests that assert on response data;
		// the backstop test only cares that the request reached HTTP.
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/beads":
			_, _ = w.Write([]byte(`{"id":"spi-x"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/messages":
			_, _ = w.Write([]byte(`{"id":"spi-msg"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/beads/blocked":
			_, _ = w.Write([]byte(`{"count":0,"ids":[]}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/beads":
			_, _ = w.Write([]byte(`[]`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/messages":
			_, _ = w.Write([]byte(`[]`))
		case strings.HasPrefix(r.URL.Path, "/api/v1/beads/") && strings.HasSuffix(r.URL.Path, "/deps"):
			_, _ = w.Write([]byte(`[]`))
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v1/beads/"):
			_, _ = w.Write([]byte(`{"id":"spi-x"}`))
		default:
			// PATCH /api/v1/beads/{id}, POST /api/v1/messages/{id}/read, etc.
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()

	setTestTower(t, gatewayTower(srv.URL), gatewayclient.NewClient(srv.URL, "tok"))
	installPanicStore(t)

	// Each closure invokes one dispatched API and asserts the result was
	// either a successful gateway round-trip OR a wrapped
	// ErrGatewayUnsupported. Any panic from PanicStore fails the test
	// (panics propagate up through the caller's t.Run and Go's test
	// runner reports them by default).
	cases := []struct {
		name string
		call func() error
	}{
		// queries.go
		{"GetBead", func() error { _, err := GetBead("spi-x"); return err }},
		{"GetIssue", func() error { _, err := GetIssue("spi-x"); return err }},
		{"ListBeads", func() error { _, err := ListBeads(beads.IssueFilter{}); return err }},
		{"ListBoardBeads", func() error { _, err := ListBoardBeads(beads.IssueFilter{}); return err }},
		{"GetDepsWithMeta", func() error { _, err := GetDepsWithMeta("spi-x"); return err }},
		{"GetConfig", func() error { _, err := GetConfig("k"); return err }},
		{"GetReadyWork", func() error { _, err := GetReadyWork(beads.WorkFilter{}); return err }},
		{"GetBlockedIssues", func() error { _, err := GetBlockedIssues(beads.WorkFilter{}); return err }},
		{"GetDependentsWithMeta", func() error { _, err := GetDependentsWithMeta("spi-x"); return err }},
		{"GetComments", func() error { _, err := GetComments("spi-x"); return err }},
		{"GetChildren", func() error { _, err := GetChildren("spi-x"); return err }},
		{"GetChildrenBatch", func() error { _, err := GetChildrenBatch([]string{"spi-x"}); return err }},
		{"GetChildrenBoardBatch", func() error { _, err := GetChildrenBoardBatch([]string{"spi-x"}); return err }},

		// mutations.go
		{"CreateBead", func() error { _, err := CreateBead(CreateOpts{Title: "x"}); return err }},
		{"AddDep", func() error { return AddDep("spi-a", "spi-b") }},
		{"AddDepTyped", func() error { return AddDepTyped("spi-a", "spi-b", "blocks") }},
		{"RemoveDep", func() error { return RemoveDep("spi-a", "spi-b") }},
		{"CloseBead", func() error { return CloseBead("spi-x") }},
		{"DeleteBead", func() error { return DeleteBead("spi-x") }},
		{"UpdateBead", func() error { return UpdateBead("spi-x", map[string]interface{}{"status": "closed"}) }},
		{"AddLabel", func() error { return AddLabel("spi-x", "y") }},
		{"RemoveLabel", func() error { return RemoveLabel("spi-x", "y") }},
		{"SetConfig", func() error { return SetConfig("k", "v") }},
		{"DeleteConfig", func() error { return DeleteConfig("k") }},
		{"AddComment", func() error { return AddComment("spi-x", "hi") }},
		{"AddCommentReturning", func() error { _, err := AddCommentReturning("spi-x", "hi"); return err }},
		{"AddCommentAs", func() error { return AddCommentAs("spi-x", "alice", "hi") }},
		{"AddCommentAsReturning", func() error { _, err := AddCommentAsReturning("spi-x", "alice", "hi"); return err }},
		{"CommitPending", func() error { return CommitPending("msg") }},

		// dispatch.go
		{"ListMessages", func() error { _, err := ListMessages(""); return err }},
		{"SendMessage", func() error {
			_, err := SendMessage(SendMessageOpts{To: "x", Message: "y"})
			return err
		}},
		{"MarkMessageRead", func() error { return MarkMessageRead("spi-x") }},
		{"ListDeps", func() error { _, err := ListDeps("spi-x"); return err }},

		// recovery_learnings.go (Auto wrappers go through getDB → getStore).
		{"WriteRecoveryLearningAuto", func() error { return WriteRecoveryLearningAuto(RecoveryLearningRow{}) }},
		{"GetBeadLearningsAuto", func() error { _, err := GetBeadLearningsAuto("a", "b"); return err }},
		{"GetCrossBeadLearningsAuto", func() error { _, err := GetCrossBeadLearningsAuto("c", 5); return err }},
		{"GetLearningStatsAuto", func() error { _, err := GetLearningStatsAuto("c"); return err }},
		{"UpdateLearningOutcomeAuto", func() error { return UpdateLearningOutcomeAuto("rb", "clean") }},
		{"GetPromotionSnapshotAuto", func() error { _, err := GetPromotionSnapshotAuto("sig"); return err }},
		{"DemotePromotedRowsAuto", func() error { return DemotePromotedRowsAuto("sig") }},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("%s leaked to local Dolt: panic=%v", c.name, r)
				}
			}()
			err := c.call()
			// nil err means the gateway took the call. A wrapped
			// ErrGatewayUnsupported means dispatch fail-closed at the
			// public-API boundary. Anything else is acceptable too as
			// long as the panic store didn't fire (gateway HTTP errors
			// are not part of this assertion).
			_ = err
		})
	}
}

// TestDispatch_ListMessagesGateway covers the existing gateway path for
// ListMessages — the dispatch_test.go suite has SendMessage / MarkMessageRead
// gateway coverage but no positive ListMessages case yet.
func TestDispatch_ListMessagesGateway(t *testing.T) {
	var gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id":"spi-msg-1","title":"hi"}]`))
	}))
	defer srv.Close()

	setTestTower(t, gatewayTower(srv.URL), gatewayclient.NewClient(srv.URL, "tok"))

	got, err := ListMessages("wizard")
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if gotPath != "/api/v1/messages" {
		t.Errorf("path = %q, want /api/v1/messages", gotPath)
	}
	if gotQuery != "to=wizard" {
		t.Errorf("query = %q, want to=wizard", gotQuery)
	}
	if len(got) != 1 || got[0].ID != "spi-msg-1" {
		t.Errorf("got = %+v", got)
	}
}

// TestDispatch_GatewayErrorWrapping_ErrNotFound and friends already exist in
// dispatch_test.go (TestGetBead_GatewayNotFoundMapsToErrNotFound). We add a
// matching error-sentinel test for the "no client method yet" branch so
// callers can match with errors.Is.
func TestDispatch_FailsClosedWithSentinel(t *testing.T) {
	// Use a gateway tower without spinning up a server: the test only
	// reaches the dispatch entry point, never the actual HTTP layer,
	// because the fail-closed branch returns before gatewayClient() runs.
	setTestTower(t, gatewayTower("http://127.0.0.1:0"), gatewayclient.NewClient("http://127.0.0.1:0", "tok"))
	installPanicStore(t)

	cases := []struct {
		name string
		call func() error
		op   string
	}{
		{"AddDepTyped", func() error { return AddDepTyped("a", "b", "blocks") }, "AddDepTyped"},
		{"RemoveDep", func() error { return RemoveDep("a", "b") }, "RemoveDep"},
		{"DeleteBead", func() error { return DeleteBead("a") }, "DeleteBead"},
		{"AddLabel", func() error { return AddLabel("a", "b") }, "AddLabel"},
		{"RemoveLabel", func() error { return RemoveLabel("a", "b") }, "RemoveLabel"},
		{"SetConfig", func() error { return SetConfig("k", "v") }, "SetConfig"},
		{"DeleteConfig", func() error { return DeleteConfig("k") }, "DeleteConfig"},
		{"GetConfig", func() error { _, err := GetConfig("k"); return err }, "GetConfig"},
		{"AddComment", func() error { return AddComment("a", "b") }, "AddComment"},
		{"AddCommentReturning", func() error {
			_, err := AddCommentReturning("a", "b")
			return err
		}, "AddCommentReturning"},
		{"AddCommentAs", func() error { return AddCommentAs("a", "b", "c") }, "AddCommentAs"},
		{"AddCommentAsReturning", func() error {
			_, err := AddCommentAsReturning("a", "b", "c")
			return err
		}, "AddCommentAsReturning"},
		{"GetComments", func() error { _, err := GetComments("a"); return err }, "GetComments"},
		{"CommitPending", func() error { return CommitPending("m") }, "CommitPending"},
		{"GetIssue", func() error { _, err := GetIssue("a"); return err }, "GetIssue"},
		{"ListBoardBeads", func() error { _, err := ListBoardBeads(beads.IssueFilter{}); return err }, "ListBoardBeads"},
		{"GetDepsWithMeta", func() error {
			_, err := GetDepsWithMeta("a")
			return err
		}, "GetDepsWithMeta"},
		{"GetDependentsWithMeta", func() error {
			_, err := GetDependentsWithMeta("a")
			return err
		}, "GetDependentsWithMeta"},
		{"GetReadyWork", func() error {
			_, err := GetReadyWork(beads.WorkFilter{})
			return err
		}, "GetReadyWork"},
		{"GetChildren", func() error { _, err := GetChildren("a"); return err }, "GetChildren"},
		{"GetChildrenBatch", func() error {
			_, err := GetChildrenBatch([]string{"a"})
			return err
		}, "GetChildrenBatch"},
		{"GetChildrenBoardBatch", func() error {
			_, err := GetChildrenBoardBatch([]string{"a"})
			return err
		}, "GetChildrenBoardBatch"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("%s reached local Dolt instead of failing closed: panic=%v", c.name, r)
				}
			}()
			err := c.call()
			if !errors.Is(err, ErrGatewayUnsupported) {
				t.Fatalf("%s: err = %v, want ErrGatewayUnsupported", c.name, err)
			}
			if !strings.Contains(err.Error(), c.op) {
				t.Errorf("%s: err = %q does not include op name %q", c.name, err.Error(), c.op)
			}
		})
	}
}

// TestDispatch_GetStoreFailsClosedUnderGatewayMode covers the
// belt-and-suspenders backstop. Even if a future contributor adds a
// public API and forgets the dispatch branch, getStore() refuses to
// return a Storage when the active tower is gateway-mode.
func TestDispatch_GetStoreFailsClosedUnderGatewayMode(t *testing.T) {
	setTestTower(t, gatewayTower("http://127.0.0.1:0"), gatewayclient.NewClient("http://127.0.0.1:0", "tok"))
	installPanicStore(t)

	_, _, err := getStore()
	if !errors.Is(err, ErrGatewayUnsupported) {
		t.Fatalf("getStore err = %v, want ErrGatewayUnsupported", err)
	}
	if !strings.Contains(err.Error(), "gateway-mode") {
		t.Errorf("getStore err = %q, want substring %q", err.Error(), "gateway-mode")
	}
}

// TestDispatch_DirectModeUnchanged confirms the gateway-mode guard does
// not regress local-native callers: a direct tower passes through
// getStore() to the active store as before.
func TestDispatch_DirectModeUnchanged(t *testing.T) {
	mock := &dispatchMockStorage{
		issues: map[string]*beads.Issue{
			"spi-a": {ID: "spi-a", Title: "local", IssueType: beads.TypeTask},
		},
	}
	setTestStore(t, mock)
	setTestTower(t, directTower(), nil)

	got, err := GetBead("spi-a")
	if err != nil {
		t.Fatalf("GetBead: %v", err)
	}
	if got.ID != "spi-a" || got.Title != "local" {
		t.Errorf("got = %+v", got)
	}
}

// --- Three explicit scenarios required by the acceptance criteria ---

// TestDispatch_ActiveGatewayOutsideRepo simulates the operator running a
// store call from a directory that is NOT registered with any local
// instance (e.g. /tmp). Resolution must still pick up the active gateway
// tower and route through the gateway. Reproduces the scenario from
// spi-43q7hp ("same command from /tmp creates bead in cluster Dolt").
func TestDispatch_ActiveGatewayOutsideRepo(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"spi-x","title":"from gateway"}`))
	}))
	defer srv.Close()

	// Stand in for "no CWD instance, active gateway tower wins" by
	// forcing activeTowerFn to return the gateway tower regardless of
	// CWD. This matches the contract resolveActiveTower follows under
	// real config: cfg.ActiveTower outranks CWD lookup, and a non-repo
	// CWD never registers an instance to begin with.
	setTestTower(t, gatewayTower(srv.URL), gatewayclient.NewClient(srv.URL, "tok"))
	installPanicStore(t)

	got, err := GetBead("spi-x")
	if err != nil {
		t.Fatalf("GetBead: %v", err)
	}
	if gotPath != "/api/v1/beads/spi-x" {
		t.Errorf("path = %q, want /api/v1/beads/spi-x", gotPath)
	}
	if got.ID != "spi-x" {
		t.Errorf("bead = %+v", got)
	}
}

// TestDispatch_SpireTowerEnvOverride confirms that SPIRE_TOWER pointing
// at a gateway tower routes through the gateway even when the CWD would
// otherwise resolve a direct local-native tower. The dispatch layer
// only sees the result of activeTowerFn — config.ResolveTowerConfig
// honors SPIRE_TOWER first per pkg/config/config.go's documented
// precedence order. We model the post-resolution state here.
func TestDispatch_SpireTowerEnvOverride(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"spi-x","title":"via env override"}`))
	}))
	defer srv.Close()

	// Real precedence chain: SPIRE_TOWER → ActiveTower → CWD instance →
	// sole tower. A SPIRE_TOWER pointing at a gateway tower yields a
	// gateway TowerConfig from activeTower(); that's exactly what the
	// fake activeTowerFn returns below.
	t.Setenv("SPIRE_TOWER", "test-gateway")
	setTestTower(t, gatewayTower(srv.URL), gatewayclient.NewClient(srv.URL, "tok"))
	installPanicStore(t)

	got, err := GetBead("spi-x")
	if err != nil {
		t.Fatalf("GetBead: %v", err)
	}
	if gotPath != "/api/v1/beads/spi-x" {
		t.Errorf("path = %q, want /api/v1/beads/spi-x", gotPath)
	}
	if got.Title != "via env override" {
		t.Errorf("bead = %+v", got)
	}
}

// TestDispatch_SamePrefixCWDCollision covers the case where a desktop
// laptop has both a local-native tower and a gateway tower registered
// under the same prefix (e.g. "spi"), the operator selected the gateway
// tower with `spire tower use`, but the shell is inside a directory
// registered to the local-native tower.
//
// Expected behavior post-spi-43q7hp: ActiveTower wins over CWD. The
// dispatch sees the gateway tower and routes through the gateway —
// PanicStore proves no local Dolt access happens. spi-6f6ky8 (already
// merged) prevents new collisions; this test guards against regression.
func TestDispatch_SamePrefixCWDCollision(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"spi-x","title":"active gateway wins"}`))
	}))
	defer srv.Close()

	// We assert dispatch behavior given the post-resolution result.
	// resolveActiveTower's correctness under same-prefix collision is
	// covered by pkg/config/resolve_tower_test.go (added in spi-43q7hp);
	// pkg/store/dispatch.go just trusts whatever activeTowerFn returns.
	gw := gatewayTower(srv.URL)
	gw.HubPrefix = "spi" // collides with the imagined local-native tower's prefix
	setTestTower(t, gw, gatewayclient.NewClient(srv.URL, "tok"))
	installPanicStore(t)

	if _, err := GetBead("spi-x"); err != nil {
		t.Fatalf("GetBead: %v", err)
	}
	if gotPath != "/api/v1/beads/spi-x" {
		t.Errorf("path = %q, want /api/v1/beads/spi-x", gotPath)
	}
}

// TestDispatch_NoTowerConfigured falls through to direct mode (the legacy
// behavior expected on first run before any tower is set up).
func TestDispatch_NoTowerConfigured(t *testing.T) {
	mock := &dispatchMockStorage{
		issues: map[string]*beads.Issue{"spi-a": {ID: "spi-a", IssueType: beads.TypeTask}},
	}
	setTestStore(t, mock)

	prev := activeTowerFn
	activeTowerFn = func() (*config.TowerConfig, error) { return nil, nil }
	t.Cleanup(func() { activeTowerFn = prev })

	if _, err := GetBead("spi-a"); err != nil {
		t.Fatalf("GetBead: %v", err)
	}
}
