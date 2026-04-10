package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/awell-health/spire/pkg/store"
	"github.com/steveyegge/beads"
)

// saveStoreVars saves the current function vars and restores them on test cleanup.
func saveStoreVars(t *testing.T) {
	t.Helper()
	origListBeads := storeListBeads
	origGetBead := storeGetBead
	origGetChildren := storeGetChildren
	origGetComments := storeGetComments
	origGetDepsWithMeta := storeGetDepsWithMeta
	origAddLabel := storeAddLabel
	origRemoveLabel := storeRemoveLabel
	origUpdateBead := storeUpdateBead
	origAddDepTyped := storeAddDepTyped
	origCreateBead := storeCreateBead
	origCloseBead := storeCloseBead
	origAddComment := storeAddComment
	origAddDep := storeAddDep
	origDoltSQL := doltSQL
	t.Cleanup(func() {
		storeListBeads = origListBeads
		storeGetBead = origGetBead
		storeGetChildren = origGetChildren
		storeGetComments = origGetComments
		storeGetDepsWithMeta = origGetDepsWithMeta
		storeAddLabel = origAddLabel
		storeRemoveLabel = origRemoveLabel
		storeUpdateBead = origUpdateBead
		storeAddDepTyped = origAddDepTyped
		storeCreateBead = origCreateBead
		storeCloseBead = origCloseBead
		storeAddComment = origAddComment
		storeAddDep = origAddDep
		doltSQL = origDoltSQL
	})
}

// --- listBeads tests ---

func TestListBeads_ParentUsesGetChildren(t *testing.T) {
	saveStoreVars(t)
	getChildrenCalled := false
	listBeadsCalled := false

	storeGetChildren = func(parentID string) ([]store.Bead, error) {
		getChildrenCalled = true
		if parentID != "spi-abc" {
			t.Errorf("expected parent spi-abc, got %s", parentID)
		}
		return []store.Bead{
			{ID: "spi-abc.1", Title: "Child 1"},
			{ID: "spi-abc.2", Title: "Child 2"},
		}, nil
	}
	storeListBeads = func(_ beads.IssueFilter) ([]store.Bead, error) {
		listBeadsCalled = true
		return nil, nil
	}

	tools := NewStewardTools("/tmp/test")
	result, err := tools.listBeads(json.RawMessage(`{"parent": "spi-abc"}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !getChildrenCalled {
		t.Error("expected storeGetChildren to be called")
	}
	if listBeadsCalled {
		t.Error("storeListBeads should NOT be called when parent is set")
	}
	if !strings.Contains(result, "spi-abc.1") || !strings.Contains(result, "spi-abc.2") {
		t.Errorf("expected both children in result, got %s", result)
	}
}

func TestListBeads_StatusClosedClearsExclude(t *testing.T) {
	saveStoreVars(t)
	var capturedFilter beads.IssueFilter

	storeListBeads = func(filter beads.IssueFilter) ([]store.Bead, error) {
		capturedFilter = filter
		return []store.Bead{{ID: "spi-closed1", Status: "closed"}}, nil
	}

	tools := NewStewardTools("/tmp/test")
	_, err := tools.listBeads(json.RawMessage(`{"status": "closed"}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if capturedFilter.Status == nil {
		t.Fatal("expected Status to be set")
	}
	if *capturedFilter.Status != beads.StatusClosed {
		t.Errorf("expected StatusClosed, got %v", *capturedFilter.Status)
	}
	if capturedFilter.ExcludeStatus != nil {
		t.Errorf("expected ExcludeStatus to be nil for closed filter, got %v", capturedFilter.ExcludeStatus)
	}
}

func TestListBeads_StatusOpenDoesNotClearExclude(t *testing.T) {
	saveStoreVars(t)
	var capturedFilter beads.IssueFilter

	storeListBeads = func(filter beads.IssueFilter) ([]store.Bead, error) {
		capturedFilter = filter
		return nil, nil
	}

	tools := NewStewardTools("/tmp/test")
	_, err := tools.listBeads(json.RawMessage(`{"status": "open"}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if capturedFilter.Status == nil || *capturedFilter.Status != beads.StatusOpen {
		t.Errorf("expected StatusOpen, got %v", capturedFilter.Status)
	}
	// ExcludeStatus should NOT be explicitly nil-ed for non-closed statuses.
	// The zero value (nil) is fine — ListBeads in pkg/store applies its own default.
}

func TestListBeads_LabelCommaSplitting(t *testing.T) {
	saveStoreVars(t)
	var capturedFilter beads.IssueFilter

	storeListBeads = func(filter beads.IssueFilter) ([]store.Bead, error) {
		capturedFilter = filter
		return nil, nil
	}

	tools := NewStewardTools("/tmp/test")
	_, err := tools.listBeads(json.RawMessage(`{"labels": "review-ready, owner:wizard-1 , pkg:store"}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expected := []string{"review-ready", "owner:wizard-1", "pkg:store"}
	if len(capturedFilter.Labels) != len(expected) {
		t.Fatalf("expected %d labels, got %d: %v", len(expected), len(capturedFilter.Labels), capturedFilter.Labels)
	}
	for i, want := range expected {
		if capturedFilter.Labels[i] != want {
			t.Errorf("label[%d]: expected %q, got %q", i, want, capturedFilter.Labels[i])
		}
	}
}

func TestListBeads_NoFilters(t *testing.T) {
	saveStoreVars(t)
	var capturedFilter beads.IssueFilter

	storeListBeads = func(filter beads.IssueFilter) ([]store.Bead, error) {
		capturedFilter = filter
		return []store.Bead{{ID: "spi-1"}}, nil
	}

	tools := NewStewardTools("/tmp/test")
	result, err := tools.listBeads(json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if capturedFilter.Status != nil {
		t.Error("expected nil Status for empty filter")
	}
	if capturedFilter.Labels != nil {
		t.Error("expected nil Labels for empty filter")
	}
	if !strings.Contains(result, "spi-1") {
		t.Errorf("expected result to contain spi-1, got %s", result)
	}
}

// --- showBead tests ---

func TestShowBead_CompositeAssembly(t *testing.T) {
	saveStoreVars(t)

	storeGetBead = func(id string) (store.Bead, error) {
		return store.Bead{ID: "spi-abc", Title: "Test bead", Status: "open", Priority: 1}, nil
	}
	storeGetComments = func(id string) ([]*beads.Comment, error) {
		return []*beads.Comment{{Text: "comment-1"}}, nil
	}
	storeGetDepsWithMeta = func(id string) ([]*beads.IssueWithDependencyMetadata, error) {
		return []*beads.IssueWithDependencyMetadata{
			{Issue: beads.Issue{ID: "spi-dep1"}, DependencyType: beads.DepBlocks},
		}, nil
	}
	storeGetChildren = func(id string) ([]store.Bead, error) {
		return []store.Bead{{ID: "spi-abc.1", Title: "Child"}}, nil
	}

	tools := NewStewardTools("/tmp/test")
	result, err := tools.showBead(json.RawMessage(`{"id": "spi-abc"}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify composite: must contain bead fields, comments, deps, children.
	var parsed showBeadResult
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatalf("result is not valid JSON: %v\n%s", err, result)
	}
	if parsed.ID != "spi-abc" {
		t.Errorf("expected ID spi-abc, got %s", parsed.ID)
	}
	if len(parsed.Comments) != 1 || parsed.Comments[0].Text != "comment-1" {
		t.Errorf("expected 1 comment, got %v", parsed.Comments)
	}
	if len(parsed.Dependencies) != 1 || parsed.Dependencies[0].ID != "spi-dep1" {
		t.Errorf("expected 1 dependency, got %v", parsed.Dependencies)
	}
	if len(parsed.Children) != 1 || parsed.Children[0].ID != "spi-abc.1" {
		t.Errorf("expected 1 child, got %v", parsed.Children)
	}
}

func TestShowBead_SupplementaryErrorsSwallowed(t *testing.T) {
	saveStoreVars(t)

	storeGetBead = func(id string) (store.Bead, error) {
		return store.Bead{ID: "spi-abc", Title: "Test"}, nil
	}
	storeGetComments = func(id string) ([]*beads.Comment, error) {
		return nil, errors.New("comments DB error")
	}
	storeGetDepsWithMeta = func(id string) ([]*beads.IssueWithDependencyMetadata, error) {
		return nil, errors.New("deps DB error")
	}
	storeGetChildren = func(id string) ([]store.Bead, error) {
		return nil, errors.New("children DB error")
	}

	tools := NewStewardTools("/tmp/test")
	result, err := tools.showBead(json.RawMessage(`{"id": "spi-abc"}`))
	if err != nil {
		t.Fatalf("should not fail when supplementary fetches error: %v", err)
	}

	var parsed showBeadResult
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		t.Fatalf("result is not valid JSON: %v", err)
	}
	if parsed.ID != "spi-abc" {
		t.Errorf("expected ID spi-abc, got %s", parsed.ID)
	}
	if parsed.Comments != nil {
		t.Errorf("expected nil comments when fetch fails, got %v", parsed.Comments)
	}
	if parsed.Dependencies != nil {
		t.Errorf("expected nil deps when fetch fails, got %v", parsed.Dependencies)
	}
	if parsed.Children != nil {
		t.Errorf("expected nil children when fetch fails, got %v", parsed.Children)
	}
}

func TestShowBead_PrimaryError(t *testing.T) {
	saveStoreVars(t)

	storeGetBead = func(id string) (store.Bead, error) {
		return store.Bead{}, errors.New("bead not found")
	}

	tools := NewStewardTools("/tmp/test")
	_, err := tools.showBead(json.RawMessage(`{"id": "spi-missing"}`))
	if err == nil {
		t.Fatal("expected error when GetBead fails")
	}
	if !strings.Contains(err.Error(), "bead not found") {
		t.Errorf("expected 'bead not found' in error, got %v", err)
	}
}

// --- updateBead tests ---

func TestUpdateBead_AllOperations(t *testing.T) {
	saveStoreVars(t)
	var ops []string

	storeAddLabel = func(id, label string) error {
		ops = append(ops, "add-label:"+label)
		return nil
	}
	storeRemoveLabel = func(id, label string) error {
		ops = append(ops, "remove-label:"+label)
		return nil
	}
	storeUpdateBead = func(id string, updates map[string]interface{}) error {
		ops = append(ops, "update-priority")
		return nil
	}
	storeAddDepTyped = func(issueID, dependsOnID, depType string) error {
		ops = append(ops, "set-parent:"+dependsOnID)
		return nil
	}

	tools := NewStewardTools("/tmp/test")
	input := `{"id": "spi-abc", "add_labels": ["foo", "bar"], "remove_labels": ["old"], "priority": 1, "parent": "spi-epic"}`
	result, err := tools.updateBead(json.RawMessage(input))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result, "spi-abc") {
		t.Errorf("expected result to mention bead ID, got %s", result)
	}

	expected := []string{"add-label:foo", "add-label:bar", "remove-label:old", "update-priority", "set-parent:spi-epic"}
	if len(ops) != len(expected) {
		t.Fatalf("expected %d operations, got %d: %v", len(expected), len(ops), ops)
	}
	for i, want := range expected {
		if ops[i] != want {
			t.Errorf("op[%d]: expected %q, got %q", i, want, ops[i])
		}
	}
}

func TestUpdateBead_PartialFailure_LabelAdd(t *testing.T) {
	saveStoreVars(t)
	callCount := 0

	storeAddLabel = func(id, label string) error {
		callCount++
		if callCount == 2 {
			return errors.New("store unavailable")
		}
		return nil
	}
	// These should NOT be called due to early return.
	storeRemoveLabel = func(id, label string) error {
		t.Error("removeLabel should not be called after addLabel failure")
		return nil
	}
	storeUpdateBead = func(id string, updates map[string]interface{}) error {
		t.Error("updateBead should not be called after addLabel failure")
		return nil
	}

	tools := NewStewardTools("/tmp/test")
	input := `{"id": "spi-abc", "add_labels": ["ok-label", "fail-label"], "remove_labels": ["x"], "priority": 2}`
	_, err := tools.updateBead(json.RawMessage(input))
	if err == nil {
		t.Fatal("expected error on partial failure")
	}
	if !strings.Contains(err.Error(), "fail-label") {
		t.Errorf("expected error to mention failed label, got %v", err)
	}
	// First label was added successfully, but second failed — partial state.
	if callCount != 2 {
		t.Errorf("expected 2 addLabel calls, got %d", callCount)
	}
}

func TestUpdateBead_PartialFailure_RemoveLabel(t *testing.T) {
	saveStoreVars(t)

	storeAddLabel = func(id, label string) error { return nil }
	storeRemoveLabel = func(id, label string) error {
		return errors.New("remove failed")
	}

	tools := NewStewardTools("/tmp/test")
	input := `{"id": "spi-abc", "add_labels": ["added"], "remove_labels": ["fail-remove"]}`
	_, err := tools.updateBead(json.RawMessage(input))
	if err == nil {
		t.Fatal("expected error on remove failure")
	}
	if !strings.Contains(err.Error(), "fail-remove") {
		t.Errorf("expected error to mention failed label, got %v", err)
	}
}

func TestUpdateBead_PartialFailure_Priority(t *testing.T) {
	saveStoreVars(t)

	storeAddLabel = func(id, label string) error { return nil }
	storeRemoveLabel = func(id, label string) error { return nil }
	storeUpdateBead = func(id string, updates map[string]interface{}) error {
		return errors.New("priority update failed")
	}

	tools := NewStewardTools("/tmp/test")
	input := `{"id": "spi-abc", "add_labels": ["ok"], "remove_labels": ["rm"], "priority": 0}`
	_, err := tools.updateBead(json.RawMessage(input))
	if err == nil {
		t.Fatal("expected error on priority failure")
	}
	if !strings.Contains(err.Error(), "priority") {
		t.Errorf("expected error to mention priority, got %v", err)
	}
}

// --- ensureProjectID tests ---

func TestEnsureProjectID_TableFormat(t *testing.T) {
	saveStoreVars(t)

	// Create a temp directory with metadata.json.
	tmpDir := t.TempDir()
	beadsDir := filepath.Join(tmpDir, ".beads")
	os.MkdirAll(beadsDir, 0755)
	metaPath := filepath.Join(beadsDir, "metadata.json")

	meta := map[string]any{"project_id": "old-id"}
	data, _ := json.MarshalIndent(meta, "", "  ")
	os.WriteFile(metaPath, data, 0644)

	// Override ensureProjectID to work from our temp dir.
	// The function reads from ".beads/metadata.json" relative to cwd.
	origDir, _ := os.Getwd()
	os.Chdir(tmpDir)
	t.Cleanup(func() { os.Chdir(origDir) })

	// Mock doltSQL to return table-format output with borders.
	doltSQL = func(query string, jsonOutput bool, dbName string) (string, error) {
		return "+-------+\n| value |\n+-------+\n| new-project-id |\n+-------+", nil
	}

	ensureProjectID()

	// Read back the metadata.json to verify it was updated.
	updated, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatalf("failed to read metadata: %v", err)
	}
	var result map[string]any
	json.Unmarshal(updated, &result)
	if result["project_id"] != "new-project-id" {
		t.Errorf("expected project_id 'new-project-id', got %v", result["project_id"])
	}
}

func TestEnsureProjectID_SimpleOutput(t *testing.T) {
	saveStoreVars(t)

	tmpDir := t.TempDir()
	beadsDir := filepath.Join(tmpDir, ".beads")
	os.MkdirAll(beadsDir, 0755)
	metaPath := filepath.Join(beadsDir, "metadata.json")

	meta := map[string]any{"project_id": "old-id"}
	data, _ := json.MarshalIndent(meta, "", "  ")
	os.WriteFile(metaPath, data, 0644)

	origDir, _ := os.Getwd()
	os.Chdir(tmpDir)
	t.Cleanup(func() { os.Chdir(origDir) })

	// Mock doltSQL to return simple two-line output (header + value).
	doltSQL = func(query string, jsonOutput bool, dbName string) (string, error) {
		return "value\nmy-project-id", nil
	}

	ensureProjectID()

	updated, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatalf("failed to read metadata: %v", err)
	}
	var result map[string]any
	json.Unmarshal(updated, &result)
	if result["project_id"] != "my-project-id" {
		t.Errorf("expected project_id 'my-project-id', got %v", result["project_id"])
	}
}

func TestEnsureProjectID_AlreadyAligned(t *testing.T) {
	saveStoreVars(t)

	tmpDir := t.TempDir()
	beadsDir := filepath.Join(tmpDir, ".beads")
	os.MkdirAll(beadsDir, 0755)
	metaPath := filepath.Join(beadsDir, "metadata.json")

	meta := map[string]any{"project_id": "same-id"}
	data, _ := json.MarshalIndent(meta, "", "  ")
	os.WriteFile(metaPath, data, 0644)

	origDir, _ := os.Getwd()
	os.Chdir(tmpDir)
	t.Cleanup(func() { os.Chdir(origDir) })

	doltSQL = func(query string, jsonOutput bool, dbName string) (string, error) {
		return "value\nsame-id", nil
	}

	ensureProjectID()

	// Metadata should be unchanged.
	updated, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatalf("failed to read metadata: %v", err)
	}
	var result map[string]any
	json.Unmarshal(updated, &result)
	if result["project_id"] != "same-id" {
		t.Errorf("expected project_id to remain 'same-id', got %v", result["project_id"])
	}
}

func TestEnsureProjectID_EmptyResponse(t *testing.T) {
	saveStoreVars(t)

	tmpDir := t.TempDir()
	beadsDir := filepath.Join(tmpDir, ".beads")
	os.MkdirAll(beadsDir, 0755)
	metaPath := filepath.Join(beadsDir, "metadata.json")

	meta := map[string]any{"project_id": "original"}
	data, _ := json.MarshalIndent(meta, "", "  ")
	os.WriteFile(metaPath, data, 0644)

	origDir, _ := os.Getwd()
	os.Chdir(tmpDir)
	t.Cleanup(func() { os.Chdir(origDir) })

	// Empty response — should not update.
	doltSQL = func(query string, jsonOutput bool, dbName string) (string, error) {
		return "", nil
	}

	ensureProjectID()

	updated, _ := os.ReadFile(metaPath)
	var result map[string]any
	json.Unmarshal(updated, &result)
	if result["project_id"] != "original" {
		t.Errorf("expected project_id to remain 'original' on empty response, got %v", result["project_id"])
	}
}

func TestEnsureProjectID_SingleLineResponse(t *testing.T) {
	saveStoreVars(t)

	tmpDir := t.TempDir()
	beadsDir := filepath.Join(tmpDir, ".beads")
	os.MkdirAll(beadsDir, 0755)
	metaPath := filepath.Join(beadsDir, "metadata.json")

	meta := map[string]any{"project_id": "original"}
	data, _ := json.MarshalIndent(meta, "", "  ")
	os.WriteFile(metaPath, data, 0644)

	origDir, _ := os.Getwd()
	os.Chdir(tmpDir)
	t.Cleanup(func() { os.Chdir(origDir) })

	// Single line — fewer than 2 lines, should bail.
	doltSQL = func(query string, jsonOutput bool, dbName string) (string, error) {
		return "value", nil
	}

	ensureProjectID()

	updated, _ := os.ReadFile(metaPath)
	var result map[string]any
	json.Unmarshal(updated, &result)
	if result["project_id"] != "original" {
		t.Errorf("expected project_id to remain 'original' on single-line response, got %v", result["project_id"])
	}
}

func TestEnsureProjectID_DoltError(t *testing.T) {
	saveStoreVars(t)

	tmpDir := t.TempDir()
	beadsDir := filepath.Join(tmpDir, ".beads")
	os.MkdirAll(beadsDir, 0755)
	metaPath := filepath.Join(beadsDir, "metadata.json")

	meta := map[string]any{"project_id": "original"}
	data, _ := json.MarshalIndent(meta, "", "  ")
	os.WriteFile(metaPath, data, 0644)

	origDir, _ := os.Getwd()
	os.Chdir(tmpDir)
	t.Cleanup(func() { os.Chdir(origDir) })

	doltSQL = func(query string, jsonOutput bool, dbName string) (string, error) {
		return "", errors.New("connection refused")
	}

	ensureProjectID()

	updated, _ := os.ReadFile(metaPath)
	var result map[string]any
	json.Unmarshal(updated, &result)
	if result["project_id"] != "original" {
		t.Errorf("expected project_id to remain 'original' on dolt error, got %v", result["project_id"])
	}
}

func TestEnsureProjectID_TableHeaderOnly(t *testing.T) {
	saveStoreVars(t)

	tmpDir := t.TempDir()
	beadsDir := filepath.Join(tmpDir, ".beads")
	os.MkdirAll(beadsDir, 0755)
	metaPath := filepath.Join(beadsDir, "metadata.json")

	meta := map[string]any{"project_id": "original"}
	data, _ := json.MarshalIndent(meta, "", "  ")
	os.WriteFile(metaPath, data, 0644)

	origDir, _ := os.Getwd()
	os.Chdir(tmpDir)
	t.Cleanup(func() { os.Chdir(origDir) })

	// Table with header but no data rows.
	doltSQL = func(query string, jsonOutput bool, dbName string) (string, error) {
		return "+-------+\n| value |\n+-------+\n+-------+", nil
	}

	ensureProjectID()

	updated, _ := os.ReadFile(metaPath)
	var result map[string]any
	json.Unmarshal(updated, &result)
	if result["project_id"] != "original" {
		t.Errorf("expected project_id to remain 'original' on header-only table, got %v", result["project_id"])
	}
}

func TestEnsureProjectID_DbNameFromEnv(t *testing.T) {
	saveStoreVars(t)

	tmpDir := t.TempDir()
	beadsDir := filepath.Join(tmpDir, ".beads")
	os.MkdirAll(beadsDir, 0755)
	metaPath := filepath.Join(beadsDir, "metadata.json")

	meta := map[string]any{"project_id": "old"}
	data, _ := json.MarshalIndent(meta, "", "  ")
	os.WriteFile(metaPath, data, 0644)

	origDir, _ := os.Getwd()
	os.Chdir(tmpDir)
	t.Cleanup(func() { os.Chdir(origDir) })

	t.Setenv("BEADS_RIG", "custom-rig")

	var capturedDB string
	doltSQL = func(query string, jsonOutput bool, dbName string) (string, error) {
		capturedDB = dbName
		return "value\nnew", nil
	}

	ensureProjectID()

	if capturedDB != "custom-rig" {
		t.Errorf("expected dbName 'custom-rig', got %q", capturedDB)
	}
}

// --- getRoster tests ---

func TestGetRoster_KubectlFallbackToStore(t *testing.T) {
	saveStoreVars(t)

	// We can't easily mock runKubectl without a var, but the function will fail
	// in test environment (no kubectl). So it will always fall back to store.
	// This tests the fallback path.
	storeListBeads = func(filter beads.IssueFilter) ([]store.Bead, error) {
		if filter.Labels != nil && len(filter.Labels) == 1 && filter.Labels[0] == "agent" {
			return []store.Bead{
				{ID: "spi-agent1", Title: "wizard-1", Labels: []string{"agent"}},
			}, nil
		}
		if filter.Status != nil && *filter.Status == beads.StatusInProgress {
			return []store.Bead{
				{ID: "spi-work1", Title: "Active work", Status: "in_progress"},
			}, nil
		}
		return nil, nil
	}

	tools := NewStewardTools("/tmp/test")
	result, err := tools.getRoster(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result, "Roster:") {
		t.Error("expected 'Roster:' in output")
	}
	if !strings.Contains(result, "In-progress work:") {
		t.Error("expected 'In-progress work:' in output")
	}
	if !strings.Contains(result, "spi-agent1") {
		t.Errorf("expected agent bead in roster, got %s", result)
	}
	if !strings.Contains(result, "spi-work1") {
		t.Errorf("expected in-progress bead in busy section, got %s", result)
	}
}

func TestGetRoster_FallbackStoreError(t *testing.T) {
	saveStoreVars(t)

	callCount := 0
	storeListBeads = func(filter beads.IssueFilter) ([]store.Bead, error) {
		callCount++
		if callCount == 1 {
			// First call is the agent roster fallback.
			return nil, errors.New("store unavailable")
		}
		return nil, nil
	}

	tools := NewStewardTools("/tmp/test")
	_, err := tools.getRoster(nil)
	if err == nil {
		t.Fatal("expected error when store fallback fails")
	}
	if !strings.Contains(err.Error(), "list agent beads") {
		t.Errorf("expected 'list agent beads' in error, got %v", err)
	}
}

func TestGetRoster_BusyListError(t *testing.T) {
	saveStoreVars(t)

	callCount := 0
	storeListBeads = func(filter beads.IssueFilter) ([]store.Bead, error) {
		callCount++
		if filter.Labels != nil && len(filter.Labels) == 1 && filter.Labels[0] == "agent" {
			return []store.Bead{{ID: "spi-agent1"}}, nil
		}
		// Busy list fails.
		return nil, errors.New("busy list error")
	}

	tools := NewStewardTools("/tmp/test")
	_, err := tools.getRoster(nil)
	if err == nil {
		t.Fatal("expected error when busy list fails")
	}
	if !strings.Contains(err.Error(), "list busy beads") {
		t.Errorf("expected 'list busy beads' in error, got %v", err)
	}
}

// --- marshalJSON tests ---

func TestMarshalJSON_ValidStruct(t *testing.T) {
	result, err := marshalJSON(store.Bead{ID: "spi-abc", Title: "Test"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result, `"id":"spi-abc"`) {
		t.Errorf("expected JSON to contain id field, got %s", result)
	}
}

func TestMarshalJSON_Nil(t *testing.T) {
	result, err := marshalJSON(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "null" {
		t.Errorf("expected 'null', got %s", result)
	}
}

func TestMarshalJSON_EmptySlice(t *testing.T) {
	result, err := marshalJSON([]store.Bead{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != "[]" {
		t.Errorf("expected '[]', got %s", result)
	}
}

// --- Execute dispatch tests ---

func TestExecute_UnknownTool(t *testing.T) {
	tools := NewStewardTools("/tmp/test")
	_, err := tools.Execute("nonexistent_tool", nil)
	if err == nil {
		t.Fatal("expected error for unknown tool")
	}
	if !strings.Contains(err.Error(), "unknown tool") {
		t.Errorf("expected 'unknown tool' in error, got %v", err)
	}
}
