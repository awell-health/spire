package gateway

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
)

// newWorkshopMux wires the same /api/v1/workshop/* routes Run() registers
// in production. We rebuild the mux here so tests can drive the handlers
// through the bearerAuth + corsMiddleware wrappers (used for the auth
// gating cases) without booting an HTTP listener.
func newWorkshopMux(s *Server) *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("/api/v1/workshop/formulas", s.corsMiddleware(s.bearerAuth(s.handleWorkshopFormulas)))
	mux.Handle("/api/v1/workshop/formulas/", s.corsMiddleware(s.bearerAuth(s.handleWorkshopFormulaByName)))
	return mux
}

// newWorkshopTestServer builds a Server and an httptest.Server pairing for
// the workshop endpoint suite. apiToken="" exercises dev mode (no auth),
// matching gateway_test.go conventions; tests that need auth gating set the
// token explicitly via newWorkshopTestServerWithToken.
func newWorkshopTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	s := &Server{addr: ":0", log: log.New(io.Discard, "", 0)}
	return httptest.NewServer(newWorkshopMux(s))
}

func newWorkshopTestServerWithToken(t *testing.T, token string) *httptest.Server {
	t.Helper()
	s := &Server{addr: ":0", log: log.New(io.Discard, "", 0), apiToken: token}
	return httptest.NewServer(newWorkshopMux(s))
}

// --- /api/v1/workshop/formulas (list) ---

// TestWorkshopFormulas_ReturnsAllEmbedded asserts that every embedded
// formula appears in the catalog with a derived category and default_for
// matching the expectation table below. Specifically guards the rule from
// docs/design/workshop-desktop.md §3.1 — task-default must surface
// category="task" with default_for=["task","feature"], and subgraph-review
// must surface category="subgraph" with an empty default_for.
func TestWorkshopFormulas_ReturnsAllEmbedded(t *testing.T) {
	srv := newWorkshopTestServer(t)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/v1/workshop/formulas")
	if err != nil {
		t.Fatalf("GET formulas: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200 (body=%q)", resp.StatusCode, body)
	}

	var got []FormulaInfoWire
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}

	wantNames := []string{
		"bug-default",
		"chore-default",
		"cleric-default",
		"epic-default",
		"subgraph-implement",
		"subgraph-review",
		"task-default",
	}
	if len(got) != len(wantNames) {
		t.Fatalf("len(formulas) = %d, want %d (names=%v)", len(got), len(wantNames), formulaNames(got))
	}
	gotNames := formulaNames(got)
	sort.Strings(gotNames)
	for i, w := range wantNames {
		if gotNames[i] != w {
			t.Errorf("formulas[%d] name = %q, want %q (full list=%v)", i, gotNames[i], w, gotNames)
		}
	}

	// Spot-check derived fields per the expectation table.
	expectations := map[string]struct {
		category   string
		defaultFor []string
		source     string
	}{
		"task-default":       {category: "task", defaultFor: []string{"task", "feature"}, source: "embedded"},
		"bug-default":        {category: "bug", defaultFor: []string{"bug"}, source: "embedded"},
		"epic-default":       {category: "epic", defaultFor: []string{"epic"}, source: "embedded"},
		"chore-default":      {category: "chore", defaultFor: []string{"chore"}, source: "embedded"},
		"cleric-default":     {category: "recovery", defaultFor: []string{"recovery"}, source: "embedded"},
		"subgraph-review":    {category: "subgraph", defaultFor: []string{}, source: "embedded"},
		"subgraph-implement": {category: "subgraph", defaultFor: []string{}, source: "embedded"},
	}

	for _, info := range got {
		exp, ok := expectations[info.Name]
		if !ok {
			t.Errorf("unexpected formula %q in catalog", info.Name)
			continue
		}
		if info.Category != exp.category {
			t.Errorf("%s.category = %q, want %q", info.Name, info.Category, exp.category)
		}
		if !stringSlicesEqual(info.DefaultFor, exp.defaultFor) {
			t.Errorf("%s.default_for = %v, want %v", info.Name, info.DefaultFor, exp.defaultFor)
		}
		if info.Source != exp.source {
			t.Errorf("%s.source = %q, want %q", info.Name, info.Source, exp.source)
		}
		if info.StepCount <= 0 {
			t.Errorf("%s.step_count = %d, want > 0", info.Name, info.StepCount)
		}
		if info.Version != 3 {
			t.Errorf("%s.version = %d, want 3", info.Name, info.Version)
		}
	}
}

// TestWorkshopFormulas_FilterSourceEmbedded covers the ?source=embedded
// query param. With no on-disk custom formulas in the test environment,
// the filtered result must equal the full embedded set — the test fails
// fast if the filter accidentally drops embedded rows or lets non-embedded
// rows through.
func TestWorkshopFormulas_FilterSourceEmbedded(t *testing.T) {
	srv := newWorkshopTestServer(t)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/v1/workshop/formulas?source=embedded")
	if err != nil {
		t.Fatalf("GET ?source=embedded: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200 (body=%q)", resp.StatusCode, body)
	}

	var got []FormulaInfoWire
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("source=embedded returned 0 formulas; want >= 7")
	}
	for _, f := range got {
		if f.Source != "embedded" {
			t.Errorf("formula %q source = %q, want \"embedded\"", f.Name, f.Source)
		}
	}

	// ?source=custom should return [] in the test environment (no on-disk
	// custom formulas). Sanity-check the filter cuts both ways.
	resp2, err := http.Get(srv.URL + "/api/v1/workshop/formulas?source=custom")
	if err != nil {
		t.Fatalf("GET ?source=custom: %v", err)
	}
	defer resp2.Body.Close()
	var got2 []FormulaInfoWire
	if err := json.NewDecoder(resp2.Body).Decode(&got2); err != nil {
		t.Fatalf("decode custom: %v", err)
	}
	if len(got2) != 0 {
		t.Errorf("source=custom returned %d formulas, want 0 (in test env)", len(got2))
	}
}

// --- /api/v1/workshop/formulas/{name} (detail) ---

// TestWorkshopFormulaDetail_TaskDefault asserts the canonical sample shape
// from docs/design/workshop-desktop.md §2.2: entry=plan, 6 steps, both
// needs and guard edges materialized, two paths reachable from entry.
func TestWorkshopFormulaDetail_TaskDefault(t *testing.T) {
	srv := newWorkshopTestServer(t)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/v1/workshop/formulas/task-default")
	if err != nil {
		t.Fatalf("GET task-default: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200 (body=%q)", resp.StatusCode, body)
	}

	var got FormulaDetailWire
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if got.Name != "task-default" {
		t.Errorf("name = %q, want \"task-default\"", got.Name)
	}
	if got.Entry != "plan" {
		t.Errorf("entry = %q, want \"plan\"", got.Entry)
	}
	if got.StepCount != 6 {
		t.Errorf("step_count = %d, want 6", got.StepCount)
	}
	if got.Category != "task" {
		t.Errorf("category = %q, want \"task\"", got.Category)
	}
	if !stringSlicesEqual(got.DefaultFor, []string{"task", "feature"}) {
		t.Errorf("default_for = %v, want [task feature]", got.DefaultFor)
	}
	if got.Source != "embedded" {
		t.Errorf("source = %q, want \"embedded\"", got.Source)
	}
	if len(got.Steps) != 6 {
		t.Errorf("len(steps) = %d, want 6", len(got.Steps))
	}
	if got.Stats != nil {
		t.Errorf("stats = %+v, want nil (omitted in v1)", got.Stats)
	}

	// Edges: must include at least one needs edge and one guard edge.
	// Specifically, plan→implement is a "needs" edge, and review→merge
	// is a "guard" edge whose `when` mentions outcome and merge.
	var hasPlanImplement, hasReviewMergeGuard, hasReviewDiscardGuard bool
	for _, e := range got.Edges {
		if e.From == "plan" && e.To == "implement" && e.Kind == "needs" {
			hasPlanImplement = true
		}
		if e.From == "review" && e.To == "merge" && e.Kind == "guard" {
			hasReviewMergeGuard = true
			if !strings.Contains(e.When, "merge") {
				t.Errorf("review→merge.when = %q, want contains \"merge\"", e.When)
			}
		}
		if e.From == "review" && e.To == "discard" && e.Kind == "guard" {
			hasReviewDiscardGuard = true
			if !strings.Contains(e.When, "discard") {
				t.Errorf("review→discard.when = %q, want contains \"discard\"", e.When)
			}
		}
	}
	if !hasPlanImplement {
		t.Error("missing plan→implement needs edge")
	}
	if !hasReviewMergeGuard {
		t.Error("missing review→merge guard edge")
	}
	if !hasReviewDiscardGuard {
		t.Error("missing review→discard guard edge")
	}

	// Paths: must include both [plan,implement,review,merge,close]
	// and [plan,implement,review,discard].
	wantMergePath := []string{"plan", "implement", "review", "merge", "close"}
	wantDiscardPath := []string{"plan", "implement", "review", "discard"}
	var hasMergePath, hasDiscardPath bool
	for _, p := range got.Paths {
		if stringSlicesEqual(p, wantMergePath) {
			hasMergePath = true
		}
		if stringSlicesEqual(p, wantDiscardPath) {
			hasDiscardPath = true
		}
	}
	if !hasMergePath {
		t.Errorf("missing merge path %v in paths=%v", wantMergePath, got.Paths)
	}
	if !hasDiscardPath {
		t.Errorf("missing discard path %v in paths=%v", wantDiscardPath, got.Paths)
	}

	// Vars/Workspaces sanity: task-default declares 3 vars and 1 workspace.
	if len(got.Vars) != 3 {
		t.Errorf("len(vars) = %d, want 3", len(got.Vars))
	}
	if len(got.Workspaces) != 1 || got.Workspaces[0].Name != "feature" {
		t.Errorf("workspaces = %+v, want one named \"feature\"", got.Workspaces)
	}

	// Issues should serialize as [] not null.
	if got.Issues == nil {
		t.Error("issues = nil, want [] (non-nil)")
	}
}

// TestWorkshopFormulaDetail_SubgraphReview covers the cyclic case where
// subgraph-review's "fix" step declares resets=["sage-review", "fix"].
// The materialized edge list must include at least one edge with
// kind="reset" pointing back to sage-review, and DryRunStepGraph's path
// enumeration must record the loop point so the path explorer can render it.
func TestWorkshopFormulaDetail_SubgraphReview(t *testing.T) {
	srv := newWorkshopTestServer(t)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/v1/workshop/formulas/subgraph-review")
	if err != nil {
		t.Fatalf("GET subgraph-review: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200 (body=%q)", resp.StatusCode, body)
	}

	var got FormulaDetailWire
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if got.Category != "subgraph" {
		t.Errorf("category = %q, want \"subgraph\"", got.Category)
	}
	if !stringSlicesEqual(got.DefaultFor, []string{}) {
		t.Errorf("default_for = %v, want []", got.DefaultFor)
	}

	// At least one reset edge from fix to sage-review.
	var hasFixSageReset, hasFixSelfReset bool
	for _, e := range got.Edges {
		if e.From == "fix" && e.To == "sage-review" && e.Kind == "reset" {
			hasFixSageReset = true
		}
		if e.From == "fix" && e.To == "fix" && e.Kind == "reset" {
			hasFixSelfReset = true
		}
	}
	if !hasFixSageReset {
		t.Errorf("missing fix→sage-review reset edge in edges=%+v", got.Edges)
	}
	if !hasFixSelfReset {
		t.Errorf("missing fix→fix reset edge (self-reset) in edges=%+v", got.Edges)
	}

	// Outputs from subgraph-review TOML.
	if len(got.Outputs) != 4 {
		t.Errorf("len(outputs) = %d, want 4 (got=%+v)", len(got.Outputs), got.Outputs)
	}

	// Paths must include at least one path that records the loop point —
	// i.e. "sage-review" appears more than once. The DFS in DryRunStepGraph
	// emits paths up to the cycle point.
	var foundLoopPath bool
	for _, p := range got.Paths {
		if countOccurrences(p, "sage-review") >= 2 {
			foundLoopPath = true
			break
		}
	}
	if !foundLoopPath {
		t.Errorf("no path records the sage-review loop point; paths=%v", got.Paths)
	}
}

// TestWorkshopFormulaDetail_NotFound asserts the 404 path for an unknown
// formula name. The contract is { "error": "formula not found" }.
func TestWorkshopFormulaDetail_NotFound(t *testing.T) {
	srv := newWorkshopTestServer(t)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/v1/workshop/formulas/does-not-exist")
	if err != nil {
		t.Fatalf("GET does-not-exist: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 404 (body=%q)", resp.StatusCode, body)
	}

	var errResp map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&errResp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.Contains(errResp["error"], "not found") {
		t.Errorf("error = %q, want contains \"not found\"", errResp["error"])
	}
}

// --- /api/v1/workshop/formulas/{name}/source ---

func TestWorkshopFormulaSource_Embedded(t *testing.T) {
	srv := newWorkshopTestServer(t)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/v1/workshop/formulas/task-default/source")
	if err != nil {
		t.Fatalf("GET source: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200 (body=%q)", resp.StatusCode, body)
	}

	var got map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["name"] != "task-default" {
		t.Errorf("name = %q, want \"task-default\"", got["name"])
	}
	if got["source"] != "embedded" {
		t.Errorf("source = %q, want \"embedded\"", got["source"])
	}
	if got["toml"] == "" {
		t.Error("toml is empty; want raw TOML bytes")
	}
	// The raw TOML must start with the canonical comment header so the
	// desktop renders the actual file the operator can copy-paste.
	if !strings.HasPrefix(got["toml"], "# task-default") {
		t.Errorf("toml prefix = %q..., want starts with \"# task-default\"", firstN(got["toml"], 40))
	}
}

func TestWorkshopFormulaSource_NotFound(t *testing.T) {
	srv := newWorkshopTestServer(t)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/v1/workshop/formulas/does-not-exist/source")
	if err != nil {
		t.Fatalf("GET nonexistent source: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 404 (body=%q)", resp.StatusCode, body)
	}
}

// --- /api/v1/workshop/formulas/{name}/validate ---

// TestWorkshopFormulaValidate_TaskDefault asserts the validate endpoint
// returns a well-formed { issues: [...] } envelope for an embedded formula.
// task-default emits one structural warning (required bead_id var with no
// default) — we don't pin the count, only the envelope shape and that any
// surfaced issues carry valid level/phase/message keys. The structural
// assertion guards regressions in the wire shape; the workshop package
// itself has its own validator unit tests.
func TestWorkshopFormulaValidate_TaskDefault(t *testing.T) {
	srv := newWorkshopTestServer(t)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/v1/workshop/formulas/task-default/validate")
	if err != nil {
		t.Fatalf("GET validate: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200 (body=%q)", resp.StatusCode, body)
	}

	var got map[string][]map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	issues, ok := got["issues"]
	if !ok {
		t.Fatalf("response missing \"issues\" key: %v", got)
	}
	for i, issue := range issues {
		if issue["level"] != "error" && issue["level"] != "warning" {
			t.Errorf("issues[%d].level = %q, want \"error\" or \"warning\"", i, issue["level"])
		}
		if issue["message"] == "" {
			t.Errorf("issues[%d].message is empty", i)
		}
	}
}

func TestWorkshopFormulaValidate_NotFound(t *testing.T) {
	srv := newWorkshopTestServer(t)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/v1/workshop/formulas/does-not-exist/validate")
	if err != nil {
		t.Fatalf("GET nonexistent validate: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 404 (body=%q)", resp.StatusCode, body)
	}
}

// --- bearer auth gating ---

// TestWorkshopFormulas_BearerAuthGating checks that requests without a
// valid bearer token are rejected with 401. Mirrors the existing
// auth-gating tests in the rest of the gateway suite.
func TestWorkshopFormulas_BearerAuthGating(t *testing.T) {
	srv := newWorkshopTestServerWithToken(t, "secret-token")
	defer srv.Close()

	// Missing Authorization header → 401.
	resp, err := http.Get(srv.URL + "/api/v1/workshop/formulas")
	if err != nil {
		t.Fatalf("GET no auth: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no-auth status = %d, want 401", resp.StatusCode)
	}

	// Wrong token → 401.
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/workshop/formulas", nil)
	req.Header.Set("Authorization", "Bearer wrong-token")
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET wrong token: %v", err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Errorf("wrong-token status = %d, want 401", resp2.StatusCode)
	}

	// Correct token → 200.
	req3, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/workshop/formulas", nil)
	req3.Header.Set("Authorization", "Bearer secret-token")
	resp3, err := http.DefaultClient.Do(req3)
	if err != nil {
		t.Fatalf("GET correct token: %v", err)
	}
	resp3.Body.Close()
	if resp3.StatusCode != http.StatusOK {
		t.Errorf("correct-token status = %d, want 200", resp3.StatusCode)
	}

	// Sub-resource paths gate too — covers the /{name} branch reaching
	// the bearerAuth wrapper as well.
	req4, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/workshop/formulas/task-default", nil)
	resp4, err := http.DefaultClient.Do(req4)
	if err != nil {
		t.Fatalf("GET sub-resource no auth: %v", err)
	}
	resp4.Body.Close()
	if resp4.StatusCode != http.StatusUnauthorized {
		t.Errorf("sub-resource no-auth status = %d, want 401", resp4.StatusCode)
	}
}

// --- method gating ---

// TestWorkshopFormulas_RejectsNonGET exercises the method-not-allowed path
// on every workshop endpoint. The handlers reject anything but GET.
func TestWorkshopFormulas_RejectsNonGET(t *testing.T) {
	srv := newWorkshopTestServer(t)
	defer srv.Close()

	endpoints := []string{
		"/api/v1/workshop/formulas",
		"/api/v1/workshop/formulas/task-default",
		"/api/v1/workshop/formulas/task-default/source",
		"/api/v1/workshop/formulas/task-default/validate",
	}
	for _, ep := range endpoints {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+ep, strings.NewReader("{}"))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST %s: %v", ep, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("POST %s: status = %d, want 405", ep, resp.StatusCode)
		}
	}
}

// --- helpers ---

func formulaNames(infos []FormulaInfoWire) []string {
	out := make([]string, len(infos))
	for i, f := range infos {
		out[i] = f.Name
	}
	return out
}

func stringSlicesEqual(a, b []string) bool {
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

func countOccurrences(slice []string, target string) int {
	n := 0
	for _, s := range slice {
		if s == target {
			n++
		}
	}
	return n
}

func firstN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
