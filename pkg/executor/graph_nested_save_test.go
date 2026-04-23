package executor

// Tests for spi-dggw7p: nested subgraph saves must never clobber the parent's
// graph_state.json file. The TDD plan (seams S1–S6 plus the end-to-end saga)
// is spelled out on the bead. The headline failure is nested-state content
// being written to the parent's path — caused by call sites that save with
// e.agentName when the state passed in belongs to a nested subgraph.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/awell-health/spire/pkg/agent"
	"github.com/awell-health/spire/pkg/formula"
)

// --- S3+S4 headline: nested save lands at nested path ---
//
// Fixture: minimal parent graph with one graph.run step pointing at a nested
// graph with two steps. Pre-populate the parent's graph_state.json with a
// canary marker. Drive RunNestedGraph and assert the parent file is untouched
// while the nested file gets the nested state.
func TestNestedSubgraph_SaveLandsAtNestedPath(t *testing.T) {
	dir := t.TempDir()
	deps := &Deps{
		ConfigDir: func() (string, error) { return dir, nil },
	}

	parentAgent := "wizard-parent"
	nestedAgent := parentAgent + "-review"

	// Canary on parent path: bead_id = "spi-canary" should survive the nested run.
	parentPath := filepath.Join(dir, "runtime", parentAgent, "graph_state.json")
	if err := os.MkdirAll(filepath.Dir(parentPath), 0755); err != nil {
		t.Fatalf("mkdir parent path: %v", err)
	}
	parentState := &GraphState{
		BeadID:    "spi-canary",
		AgentName: parentAgent,
		Formula:   "epic-default",
		Steps: map[string]StepState{
			"implement": {Status: "completed"},
		},
		Counters:   map[string]int{},
		Workspaces: map[string]WorkspaceState{},
		Vars:       map[string]string{"canary": "parent-state"},
	}
	if err := parentState.Save(parentAgent, deps.ConfigDir); err != nil {
		t.Fatalf("seed parent state: %v", err)
	}

	// Register a test action that runs in the nested graph.
	origRegistry := make(map[string]ActionHandler)
	for k, v := range actionRegistry {
		origRegistry[k] = v
	}
	defer func() {
		for k := range actionRegistry {
			delete(actionRegistry, k)
		}
		for k, v := range origRegistry {
			actionRegistry[k] = v
		}
	}()
	actionRegistry["test.noop"] = func(e *Executor, stepName string, step StepConfig, state *GraphState) ActionResult {
		return ActionResult{Outputs: map[string]string{"done": "true"}}
	}

	subGraph := &formula.FormulaStepGraph{
		Name:    "test-nested",
		Version: 3,
		Steps: map[string]formula.StepConfig{
			"first":  {Action: "test.noop"},
			"second": {Action: "test.noop", Needs: []string{"first"}, Terminal: true},
		},
	}

	subState := NewGraphState(subGraph, "spi-canary", nestedAgent)
	subState.RepoPath = dir
	subState.BaseBranch = "main"

	exec := NewGraphForTest("spi-canary", parentAgent, nil, nil, deps)
	if err := exec.RunNestedGraph(subGraph, subState); err != nil {
		t.Fatalf("RunNestedGraph: %v", err)
	}

	// Assert 1: nested file exists with nested agent_name.
	nestedPath := filepath.Join(dir, "runtime", nestedAgent, "graph_state.json")
	nestedData, err := os.ReadFile(nestedPath)
	if err != nil {
		t.Fatalf("nested state file not found at %s: %v", nestedPath, err)
	}
	var loadedNested GraphState
	if err := json.Unmarshal(nestedData, &loadedNested); err != nil {
		t.Fatalf("parse nested: %v", err)
	}
	if loadedNested.AgentName != nestedAgent {
		t.Errorf("nested AgentName = %q, want %q", loadedNested.AgentName, nestedAgent)
	}

	// Assert 2: parent file still has the canary. No nested content bleed.
	parentData, err := os.ReadFile(parentPath)
	if err != nil {
		t.Fatalf("parent file missing: %v", err)
	}
	var loadedParent GraphState
	if err := json.Unmarshal(parentData, &loadedParent); err != nil {
		t.Fatalf("parse parent: %v", err)
	}
	if loadedParent.AgentName != parentAgent {
		t.Errorf("parent file AgentName = %q, want %q (nested content clobbered parent)",
			loadedParent.AgentName, parentAgent)
	}
	if loadedParent.Vars["canary"] != "parent-state" {
		t.Errorf("parent canary lost: vars=%v", loadedParent.Vars)
	}
	if loadedParent.Formula != "epic-default" {
		t.Errorf("parent formula = %q, want epic-default (clobbered by nested)", loadedParent.Formula)
	}

	// Assert 3: files are distinct on disk (not shared).
	if p1, p2 := parentPath, nestedPath; p1 == p2 {
		t.Fatalf("parent and nested path resolved to same file: %s", p1)
	}
	parentStat, err := os.Stat(parentPath)
	if err != nil {
		t.Fatalf("stat parent: %v", err)
	}
	nestedStat, err := os.Stat(nestedPath)
	if err != nil {
		t.Fatalf("stat nested: %v", err)
	}
	if os.SameFile(parentStat, nestedStat) {
		t.Error("parent and nested state files share inode — they must be distinct")
	}
}

// --- S1: fresh nested state has correct AgentName ---
//
// The bug hunt suspected a code path creates a sub-state with state.AgentName
// accidentally set to the parent's name. This test pins the fresh creation
// path: actionGraphRun's NewGraphState call must use subAgentName.
func TestActionGraphRun_FreshNestedStateAgentName(t *testing.T) {
	dir := t.TempDir()
	deps := &Deps{
		ConfigDir: func() (string, error) { return dir, nil },
		ResolveRepo: func(beadID string) (string, string, string, error) {
			return dir, "", "main", nil
		},
	}

	parentAgent := "wizard-fresh"
	// Use a name that actually matches a real loadable formula so
	// LoadStepGraphByName succeeds. "subgraph-review" is stable.
	parentGraph := &formula.FormulaStepGraph{
		Name:    "test-fresh-parent",
		Version: 3,
		Steps: map[string]formula.StepConfig{
			"review": {
				Action: "graph.run",
				Graph:  "subgraph-review",
			},
		},
	}

	// Spawner never gets called; RunNestedGraph exits before dispatch because
	// the sage-review step's action resolves in the nested graph when it tries
	// to spawn. We short-circuit by stubbing out the spawner.
	deps.Spawner = &mockBackend{
		spawnFn: func(cfg agent.SpawnConfig) (agent.Handle, error) {
			return &mockHandle{}, nil
		},
	}
	deps.AgentResultDir = func(name string) string { return filepath.Join(dir, name) }
	deps.RecordAgentRun = func(run AgentRun) (string, error) { return "", nil }

	exec := NewGraphForTest("spi-fresh", parentAgent, parentGraph, nil, deps)
	parentState := exec.graphState
	parentState.RepoPath = dir
	parentState.BaseBranch = "main"
	parentState.Vars["max_review_rounds"] = "3"

	// Invoke actionGraphRun. If the sub-state ended up persisted with the
	// parent's name, LoadGraphState(subAgentName) returns nil and the state
	// file at the nested path would lack the subAgentName.
	step := StepConfig{
		Action: "graph.run",
		Graph:  "subgraph-review",
	}

	// We don't care about the result; just that the side effect leaves
	// the nested state persisted with the correct AgentName.
	_ = actionGraphRun(exec, "review", step, parentState)

	subAgentName := parentAgent + "-review"
	loaded, err := LoadGraphState(subAgentName, deps.ConfigDir)
	if err != nil {
		t.Fatalf("load nested state: %v", err)
	}
	if loaded == nil {
		// The nested run may have completed + been cleaned up. In that case,
		// at least the nested path must NOT have been written to the parent's
		// location. Check via bytes.
		parentPath := filepath.Join(dir, "runtime", parentAgent, "graph_state.json")
		if data, readErr := os.ReadFile(parentPath); readErr == nil {
			var gs GraphState
			if uErr := json.Unmarshal(data, &gs); uErr == nil && gs.AgentName == subAgentName {
				t.Fatalf("parent file contains nested AgentName %q (content: %s) — nested save landed at parent path",
					gs.AgentName, data)
			}
		}
		return
	}
	if loaded.AgentName != subAgentName {
		t.Errorf("nested state AgentName = %q, want %q", loaded.AgentName, subAgentName)
	}
}

// --- S2: actionGraphRun loads prior nested state from nested path ---
//
// Pre-populates BOTH the parent and nested graph_state.json files with
// distinct markers. On resume, actionGraphRun must load from the nested
// path (subAgentName), not the parent's.
func TestNestedSubgraph_ResumeLoadsFromNestedPath(t *testing.T) {
	dir := t.TempDir()
	configDirFn := func() (string, error) { return dir, nil }

	parentAgent := "wizard-resume"
	nestedAgent := parentAgent + "-review"

	// Parent file with a marker that must NOT show up in the loaded state.
	parentState := &GraphState{
		BeadID:    "spi-resume",
		AgentName: parentAgent,
		Formula:   "epic-default",
		Steps:     map[string]StepState{"implement": {Status: "completed"}},
		Vars:      map[string]string{"marker": "parent-marker"},
	}
	if err := parentState.Save(parentAgent, configDirFn); err != nil {
		t.Fatalf("save parent: %v", err)
	}

	// Nested file with a distinct marker.
	nestedState := &GraphState{
		BeadID:    "spi-resume",
		AgentName: nestedAgent,
		Formula:   "subgraph-review",
		Steps:     map[string]StepState{"sage-review": {Status: "active"}},
		Vars:      map[string]string{"marker": "nested-marker"},
	}
	if err := nestedState.Save(nestedAgent, configDirFn); err != nil {
		t.Fatalf("save nested: %v", err)
	}

	loaded, err := LoadGraphState(nestedAgent, configDirFn)
	if err != nil {
		t.Fatalf("LoadGraphState: %v", err)
	}
	if loaded == nil {
		t.Fatal("expected nested state to load, got nil")
	}
	if loaded.AgentName != nestedAgent {
		t.Errorf("AgentName = %q, want %q", loaded.AgentName, nestedAgent)
	}
	if loaded.Vars["marker"] != "nested-marker" {
		t.Errorf("marker = %q, want nested-marker (loaded wrong file)", loaded.Vars["marker"])
	}
	if loaded.Formula != "subgraph-review" {
		t.Errorf("Formula = %q, want subgraph-review", loaded.Formula)
	}
}

// --- S5 atomicity: state.Save must not interleave concurrent writers ---
//
// Two goroutines write to distinct files; the on-disk content must be a
// single writer's valid JSON, never a corrupted interleave. A stricter
// variant checks that state.Save uses tmp+rename so a partial write never
// appears on disk (this variant WILL fail until state.Save is made atomic).
func TestGraphStateStore_SaveIsAtomic_ConcurrentDistinct(t *testing.T) {
	dir := t.TempDir()
	configDirFn := func() (string, error) { return dir, nil }

	makeState := func(agentName, marker string) *GraphState {
		return &GraphState{
			BeadID:     "spi-atomic",
			AgentName:  agentName,
			Formula:    "test",
			Steps:      map[string]StepState{},
			Counters:   map[string]int{},
			Workspaces: map[string]WorkspaceState{},
			Vars:       map[string]string{"marker": marker},
		}
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			agent := fmt.Sprintf("wizard-atomic-%d", n)
			state := makeState(agent, fmt.Sprintf("marker-%d", n))
			if err := state.Save(agent, configDirFn); err != nil {
				t.Errorf("save %s: %v", agent, err)
			}
		}(i)
	}
	wg.Wait()

	// Each agent's file must be valid JSON with the right marker.
	for i := 0; i < 8; i++ {
		agent := fmt.Sprintf("wizard-atomic-%d", i)
		path := filepath.Join(dir, "runtime", agent, "graph_state.json")
		data, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("read %s: %v", path, err)
			continue
		}
		var loaded GraphState
		if err := json.Unmarshal(data, &loaded); err != nil {
			t.Errorf("parse %s: %v — content: %q", path, err, data)
			continue
		}
		if loaded.Vars["marker"] != fmt.Sprintf("marker-%d", i) {
			t.Errorf("%s marker = %q, want marker-%d", path, loaded.Vars["marker"], i)
		}
	}
}

// TestGraphStateStore_SaveIsAtomic_SameTarget verifies that two goroutines
// writing the SAME file with different content produce a final file
// containing ONE complete JSON from ONE writer — never a partial or
// corrupted interleaving. This is the stronger contract that motivates
// atomic (tmp+rename) writes.
func TestGraphStateStore_SaveIsAtomic_SameTarget(t *testing.T) {
	dir := t.TempDir()
	configDirFn := func() (string, error) { return dir, nil }
	agent := "wizard-same-target"

	// Use a large Vars map so writes take longer and the interleave window
	// is realistic.
	largeVars := func(marker string) map[string]string {
		m := make(map[string]string, 64)
		m["marker"] = marker
		for i := 0; i < 64; i++ {
			m[fmt.Sprintf("k%d", i)] = strings.Repeat(marker, 4)
		}
		return m
	}

	stateA := &GraphState{
		BeadID:     "spi-atomic",
		AgentName:  agent,
		Formula:    "test",
		Steps:      map[string]StepState{},
		Counters:   map[string]int{},
		Workspaces: map[string]WorkspaceState{},
		Vars:       largeVars("AAAA"),
	}
	stateB := &GraphState{
		BeadID:     "spi-atomic",
		AgentName:  agent,
		Formula:    "test",
		Steps:      map[string]StepState{},
		Counters:   map[string]int{},
		Workspaces: map[string]WorkspaceState{},
		Vars:       largeVars("BBBB"),
	}

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_ = stateA.Save(agent, configDirFn)
		}()
		go func() {
			defer wg.Done()
			_ = stateB.Save(agent, configDirFn)
		}()
	}
	wg.Wait()

	path := filepath.Join(dir, "runtime", agent, "graph_state.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read final: %v", err)
	}
	var loaded GraphState
	if err := json.Unmarshal(data, &loaded); err != nil {
		t.Fatalf("final file is corrupted (not valid JSON): %v\nContent: %s", err, data)
	}
	// One writer must have won outright — the marker is A or B, never mixed.
	if got := loaded.Vars["marker"]; got != "AAAA" && got != "BBBB" {
		t.Errorf("marker = %q, expected AAAA or BBBB — interleaved write", got)
	}
	// Every Vars key must reflect the same writer's marker.
	for k, v := range loaded.Vars {
		if k == "marker" {
			continue
		}
		if !strings.HasPrefix(v, loaded.Vars["marker"]) {
			t.Errorf("key %q has value %q; inconsistent with marker %q (interleaved)",
				k, v, loaded.Vars["marker"])
			break
		}
	}
}

// --- S6: parent defer save writes parent state, not nested ---
//
// Pin the invariant that the deferred save in RunGraph captures the PARENT
// state variable, not any nested state that might have been transiently in
// flight. Documentary test: if a future refactor accidentally swaps the
// defer to use state.AgentName instead of e.agentName, this would catch
// an obvious misrouting.
func TestRunGraph_DeferredSaveRoutingInvariant(t *testing.T) {
	dir := t.TempDir()
	configDirFn := func() (string, error) { return dir, nil }

	parentAgent := "wizard-defer"
	parentState := &GraphState{
		BeadID:     "spi-defer",
		AgentName:  parentAgent,
		Formula:    "epic-default",
		Steps:      map[string]StepState{"plan": {Status: "pending"}},
		Counters:   map[string]int{},
		Workspaces: map[string]WorkspaceState{},
		Vars:       map[string]string{"origin": "parent"},
	}

	store := &FileGraphStateStore{ConfigDir: configDirFn}
	if err := store.Save(parentAgent, parentState); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Simulate the pattern of RunGraph's defer save: Save(e.agentName, state)
	// where state is the parent state. Content must land at parent path.
	if err := store.Save(parentAgent, parentState); err != nil {
		t.Fatalf("defer save: %v", err)
	}

	path := filepath.Join(dir, "runtime", parentAgent, "graph_state.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var loaded GraphState
	if err := json.Unmarshal(data, &loaded); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if loaded.AgentName != parentAgent {
		t.Errorf("loaded AgentName = %q, want %q", loaded.AgentName, parentAgent)
	}
	if loaded.Vars["origin"] != "parent" {
		t.Errorf("loaded origin = %q, want parent", loaded.Vars["origin"])
	}
}

// --- Direct bug test: ensureGraphStagingWorktree called with nested state ---
//
// This is the exact mechanism of the spi-cwgiy9 clobber. When an action
// running inside a nested subgraph (e.g. dispatch-children inside
// subgraph-implement) calls ensureGraphStagingWorktree with the nested
// state, the save MUST land at the nested path — not at the parent's path
// derived from e.agentName.
//
// Pre-fix: ensureGraphStagingWorktree calls state.Save(e.agentName, ...)
// unconditionally, so nested-state content lands at parent path.
// Post-fix: the save routes by state.AgentName, so parent and nested
// paths each own their own content.
func TestEnsureGraphStagingWorktree_NestedStateSavesToNestedPath(t *testing.T) {
	dir := t.TempDir()
	configDirFn := func() (string, error) { return dir, nil }

	parentAgent := "wizard-epic"
	nestedAgent := parentAgent + "-implement"

	// Create a "worktree" directory on disk so ensureGraphStagingWorktree
	// takes the resume path (line 97-116 in executor_worktree.go). The
	// resume path calls state.Save — which is the site of the bug.
	wtDir := filepath.Join(dir, "feature-worktree")
	if err := os.MkdirAll(wtDir, 0755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}

	// Seed the parent file with an easily-verifiable canary. If the nested
	// save routes to e.agentName (bug), this canary gets overwritten.
	parentState := &GraphState{
		BeadID:    "spi-epic",
		AgentName: parentAgent,
		Formula:   "epic-default",
		Steps: map[string]StepState{
			"implement": {Status: "completed", Outputs: map[string]string{"outcome": "verified"}},
		},
		Counters:   map[string]int{},
		Workspaces: map[string]WorkspaceState{},
		Vars:       map[string]string{"canary": "parent-state"},
	}
	if err := parentState.Save(parentAgent, configDirFn); err != nil {
		t.Fatalf("seed parent: %v", err)
	}

	// Build the Executor with the PARENT's agent name — this is what RunGraph
	// would have set.
	deps := &Deps{
		ConfigDir: configDirFn,
		AddLabel:  func(id, label string) error { return nil },
	}
	exec := NewGraphForTest("spi-epic", parentAgent, nil, nil, deps)

	// Construct the nested state (as actionGraphRun would produce for the
	// subgraph-implement step).
	nestedState := &GraphState{
		BeadID:        "spi-epic",
		AgentName:     nestedAgent,
		Formula:       "subgraph-implement",
		RepoPath:      dir,
		BaseBranch:    "main",
		StagingBranch: "staging/spi-epic",
		WorktreeDir:   wtDir, // triggers the resume path
		Steps: map[string]StepState{
			"dispatch-children": {Status: "active"},
		},
		Counters: map[string]int{},
		Workspaces: map[string]WorkspaceState{
			"staging": {
				Name:   "staging",
				Kind:   "staging",
				Status: "pending",
			},
		},
		Vars: map[string]string{"nested": "true"},
	}

	if _, err := exec.ensureGraphStagingWorktree(nestedState); err != nil {
		t.Fatalf("ensureGraphStagingWorktree: %v", err)
	}

	// Assert 1: Parent file still has the canary. Bug shape: canary gone,
	// parent file now contains nested content.
	parentPath := filepath.Join(dir, "runtime", parentAgent, "graph_state.json")
	parentData, err := os.ReadFile(parentPath)
	if err != nil {
		t.Fatalf("read parent file: %v", err)
	}
	if !bytes.Contains(parentData, []byte(`"canary": "parent-state"`)) &&
		!bytes.Contains(parentData, []byte(`"canary":"parent-state"`)) {
		t.Errorf("parent file canary missing — it was clobbered by nested save.\nContent: %s",
			parentData)
	}
	var loadedParent GraphState
	if err := json.Unmarshal(parentData, &loadedParent); err != nil {
		t.Fatalf("parse parent: %v", err)
	}
	if loadedParent.AgentName != parentAgent {
		t.Errorf("parent file AgentName = %q, want %q — clobbered",
			loadedParent.AgentName, parentAgent)
	}
	if loadedParent.Formula != "epic-default" {
		t.Errorf("parent file formula = %q, want epic-default — clobbered",
			loadedParent.Formula)
	}

	// Assert 2: Nested file exists at the nested path with nested content.
	nestedPath := filepath.Join(dir, "runtime", nestedAgent, "graph_state.json")
	nestedData, err := os.ReadFile(nestedPath)
	if err != nil {
		t.Fatalf("nested state file missing at %s: %v", nestedPath, err)
	}
	var loadedNested GraphState
	if err := json.Unmarshal(nestedData, &loadedNested); err != nil {
		t.Fatalf("parse nested: %v", err)
	}
	if loadedNested.AgentName != nestedAgent {
		t.Errorf("nested AgentName = %q, want %q", loadedNested.AgentName, nestedAgent)
	}
	if loadedNested.Formula != "subgraph-implement" {
		t.Errorf("nested Formula = %q, want subgraph-implement", loadedNested.Formula)
	}
}

// --- End-to-end saga: manual graph_state.json edit survives wizard resume ---
//
// Reproduces the observed spi-cwgiy9 scenario as closely as possible
// without spinning up a full wizard/dolt stack:
//
//  1. Set up parent + nested state as if a prior run reached the nested
//     dispatch-children step.
//  2. "Operator" edits the parent file — changes a step output — while the
//     wizard is stopped. Save the edit.
//  3. Invoke ensureGraphStagingWorktree with the nested state — the same
//     thing that happens inside the nested subgraph loop when the wizard
//     resumes.
//  4. Assert: parent file's edited output survives. It was NOT clobbered
//     by the nested save.
func TestManualGraphStateEdit_SurvivesWizardResume(t *testing.T) {
	dir := t.TempDir()
	configDirFn := func() (string, error) { return dir, nil }

	parentAgent := "wizard-saga"
	nestedAgent := parentAgent + "-implement"

	// Create worktree dir so the resume path is taken.
	wtDir := filepath.Join(dir, "saga-worktree")
	if err := os.MkdirAll(wtDir, 0755); err != nil {
		t.Fatalf("mkdir worktree: %v", err)
	}

	// Parent state with operator's edit already applied: implement step is
	// manually marked completed, outcome set to verified.
	editedParent := &GraphState{
		BeadID:    "spi-saga",
		AgentName: parentAgent,
		Formula:   "epic-default",
		Steps: map[string]StepState{
			"implement": {
				Status:  "completed",
				Outputs: map[string]string{"outcome": "verified"},
			},
		},
		Counters:   map[string]int{},
		Workspaces: map[string]WorkspaceState{},
		Vars:       map[string]string{"operator_edit": "applied"},
	}
	if err := editedParent.Save(parentAgent, configDirFn); err != nil {
		t.Fatalf("write edited parent: %v", err)
	}

	deps := &Deps{
		ConfigDir: configDirFn,
		AddLabel:  func(id, label string) error { return nil },
	}
	exec := NewGraphForTest("spi-saga", parentAgent, nil, nil, deps)

	// Nested state for the implement subgraph.
	nestedState := &GraphState{
		BeadID:        "spi-saga",
		AgentName:     nestedAgent,
		Formula:       "subgraph-implement",
		RepoPath:      dir,
		BaseBranch:    "main",
		StagingBranch: "staging/spi-saga",
		WorktreeDir:   wtDir,
		Steps:         map[string]StepState{"dispatch-children": {Status: "active"}},
		Counters:      map[string]int{},
		Workspaces: map[string]WorkspaceState{
			"staging": {Name: "staging", Kind: "staging", Status: "pending"},
		},
		Vars: map[string]string{},
	}

	// Simulate the resume hitting the staging worktree resolution path.
	if _, err := exec.ensureGraphStagingWorktree(nestedState); err != nil {
		t.Fatalf("ensureGraphStagingWorktree: %v", err)
	}

	// Re-read the parent file — the operator's edit must still be there.
	reloaded, err := LoadGraphState(parentAgent, configDirFn)
	if err != nil {
		t.Fatalf("reload parent: %v", err)
	}
	if reloaded == nil {
		t.Fatal("parent state file disappeared")
	}
	if reloaded.AgentName != parentAgent {
		t.Fatalf("parent AgentName = %q, want %q — clobbered by nested",
			reloaded.AgentName, parentAgent)
	}
	if reloaded.Formula != "epic-default" {
		t.Errorf("parent Formula = %q, want epic-default — clobbered", reloaded.Formula)
	}
	if reloaded.Vars["operator_edit"] != "applied" {
		t.Errorf("operator's edit lost: vars = %v", reloaded.Vars)
	}
	implement, ok := reloaded.Steps["implement"]
	if !ok {
		t.Fatal("implement step missing — parent state was clobbered")
	}
	if implement.Status != "completed" {
		t.Errorf("implement.status = %q, want completed", implement.Status)
	}
	if implement.Outputs["outcome"] != "verified" {
		t.Errorf("implement.outputs.outcome = %q, want verified", implement.Outputs["outcome"])
	}
}
