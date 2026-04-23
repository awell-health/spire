package bd

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestBeadUnmarshal(t *testing.T) {
	raw := `{
		"id": "spi-a3f8",
		"title": "Add OAuth2 support",
		"description": "Implement OAuth2 flow",
		"status": "open",
		"priority": 1,
		"issue_type": "task",
		"owner": "wizard-1",
		"parent": "spi-a3f0",
		"created_at": "2026-03-20T10:00:00Z",
		"updated_at": "2026-03-20T12:00:00Z",
		"labels": ["feat-branch:feat/spi-a3f8", "review-ready"],
		"children": ["spi-a3f8.1"],
		"blocked_by": ["spi-b000"],
		"blocking": ["spi-c000"]
	}`

	var b Bead
	if err := json.Unmarshal([]byte(raw), &b); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if b.ID != "spi-a3f8" {
		t.Errorf("ID = %q, want %q", b.ID, "spi-a3f8")
	}
	if b.Title != "Add OAuth2 support" {
		t.Errorf("Title = %q, want %q", b.Title, "Add OAuth2 support")
	}
	if b.Status != "open" {
		t.Errorf("Status = %q, want %q", b.Status, "open")
	}
	if b.Priority != 1 {
		t.Errorf("Priority = %d, want %d", b.Priority, 1)
	}
	if b.Type != "task" {
		t.Errorf("Type = %q, want %q", b.Type, "task")
	}
	if b.Owner != "wizard-1" {
		t.Errorf("Owner = %q, want %q", b.Owner, "wizard-1")
	}
	if b.Parent != "spi-a3f0" {
		t.Errorf("Parent = %q, want %q", b.Parent, "spi-a3f0")
	}
	if len(b.Labels) != 2 {
		t.Errorf("Labels len = %d, want 2", len(b.Labels))
	}
	if len(b.Children) != 1 || b.Children[0] != "spi-a3f8.1" {
		t.Errorf("Children = %v, want [spi-a3f8.1]", b.Children)
	}
	if len(b.BlockedBy) != 1 || b.BlockedBy[0] != "spi-b000" {
		t.Errorf("BlockedBy = %v, want [spi-b000]", b.BlockedBy)
	}
	if len(b.Blocking) != 1 || b.Blocking[0] != "spi-c000" {
		t.Errorf("Blocking = %v, want [spi-c000]", b.Blocking)
	}
}

func TestBeadUnmarshalMinimal(t *testing.T) {
	raw := `{"id": "x-1", "title": "Minimal", "status": "open", "priority": 0, "issue_type": "task"}`

	var b Bead
	if err := json.Unmarshal([]byte(raw), &b); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if b.ID != "x-1" {
		t.Errorf("ID = %q, want %q", b.ID, "x-1")
	}
	if b.Labels != nil {
		t.Errorf("Labels = %v, want nil", b.Labels)
	}
}

func TestBeadHasLabel(t *testing.T) {
	b := Bead{Labels: []string{"agent", "review-ready"}}

	if !b.HasLabel("agent") {
		t.Error("HasLabel(agent) = false, want true")
	}
	if b.HasLabel("missing") {
		t.Error("HasLabel(missing) = true, want false")
	}
}

func TestBeadTimeParsing(t *testing.T) {
	b := Bead{
		CreatedAt: "2026-03-20T10:00:00Z",
		UpdatedAt: "2026-03-20T12:00:00Z",
	}

	ct, err := b.CreatedTime()
	if err != nil {
		t.Fatalf("CreatedTime: %v", err)
	}
	if ct.Hour() != 10 {
		t.Errorf("CreatedTime hour = %d, want 10", ct.Hour())
	}

	ut, err := b.UpdatedTime()
	if err != nil {
		t.Fatalf("UpdatedTime: %v", err)
	}
	if ut.Hour() != 12 {
		t.Errorf("UpdatedTime hour = %d, want 12", ut.Hour())
	}
}

func TestParseBeadJSONObject(t *testing.T) {
	raw := `{"id": "spi-001", "title": "Test", "status": "open", "priority": 1, "issue_type": "task"}`
	var b Bead
	if err := parseBeadJSON(raw, &b); err != nil {
		t.Fatalf("parseBeadJSON: %v", err)
	}
	if b.ID != "spi-001" {
		t.Errorf("ID = %q, want %q", b.ID, "spi-001")
	}
}

func TestParseBeadJSONArray(t *testing.T) {
	raw := `[{"id": "spi-002", "title": "Array", "status": "closed", "priority": 2, "issue_type": "bug"}]`
	var b Bead
	if err := parseBeadJSON(raw, &b); err != nil {
		t.Fatalf("parseBeadJSON: %v", err)
	}
	if b.ID != "spi-002" {
		t.Errorf("ID = %q, want %q", b.ID, "spi-002")
	}
	if b.Status != "closed" {
		t.Errorf("Status = %q, want %q", b.Status, "closed")
	}
}

func TestParseBeadJSONEmpty(t *testing.T) {
	var b Bead
	if err := parseBeadJSON("", &b); err == nil {
		t.Error("parseBeadJSON empty: expected error, got nil")
	}
	if err := parseBeadJSON("[]", &b); err == nil {
		t.Error("parseBeadJSON []: expected error, got nil")
	}
}

func TestCommentUnmarshal(t *testing.T) {
	raw := `{"issue_id": "spi-001", "author": "wizard-1", "body": "Done.", "created_at": "2026-03-20T10:00:00Z"}`
	var c Comment
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if c.Author != "wizard-1" {
		t.Errorf("Author = %q, want %q", c.Author, "wizard-1")
	}
}

func TestDefaultClient(t *testing.T) {
	c := DefaultClient()
	if c == nil {
		t.Fatal("DefaultClient() returned nil")
	}
	if c.BinPath != "bd" {
		t.Errorf("BinPath = %q, want %q", c.BinPath, "bd")
	}
	if c.Logger == nil {
		t.Error("Logger is nil")
	}
}

func TestNewClient(t *testing.T) {
	c := NewClient()
	if c.BinPath != "bd" {
		t.Errorf("BinPath = %q, want %q", c.BinPath, "bd")
	}
}

// TestCreateArgs verifies the args slice built by Create.
func TestCreateArgs(t *testing.T) {
	tests := []struct {
		name     string
		title    string
		opts     CreateOpts
		wantArgs []string
	}{
		{
			name:     "minimal",
			title:    "Fix bug",
			opts:     CreateOpts{},
			wantArgs: []string{"create", "Fix bug", "--silent"},
		},
		{
			name:  "full",
			title: "Add feature",
			opts: CreateOpts{
				Type:     "feature",
				Priority: intPtr(1),
				Parent:   "spi-001",
			},
			wantArgs: []string{"create", "Add feature", "-t", "feature", "-p", "1", "--parent", "spi-001", "--silent"},
		},
		{
			name:  "with description",
			title: "New task",
			opts: CreateOpts{
				Type:        "task",
				Description: "Details here",
			},
			wantArgs: []string{"create", "New task", "-t", "task", "--description", "Details here", "--silent"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildCreateArgs(tt.title, tt.opts)
			got = append(got, "--silent") // execSilent appends this
			if !reflect.DeepEqual(got, tt.wantArgs) {
				t.Errorf("args = %v, want %v", got, tt.wantArgs)
			}
		})
	}
}

// TestListArgs verifies the args slice built by List.
func TestListArgs(t *testing.T) {
	tests := []struct {
		name     string
		opts     ListOpts
		wantArgs []string
	}{
		{
			name:     "no filters",
			opts:     ListOpts{},
			wantArgs: []string{"list", "--json"},
		},
		{
			name:     "status only",
			opts:     ListOpts{Status: "open"},
			wantArgs: []string{"list", "--status=open", "--json"},
		},
		{
			name: "all filters",
			opts: ListOpts{
				Status: "in_progress",
				Type:   "epic",
				Label:  "agent",
				Rig:    "spi",
			},
			wantArgs: []string{"list", "--status=in_progress", "--type", "epic", "--label", "agent", "--prefix=spi", "--json"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildListArgs(tt.opts)
			got = append(got, "--json") // execJSON appends this
			if !reflect.DeepEqual(got, tt.wantArgs) {
				t.Errorf("args = %v, want %v", got, tt.wantArgs)
			}
		})
	}
}

// TestUpdateArgs verifies the args slice built by Update.
func TestUpdateArgs(t *testing.T) {
	tests := []struct {
		name     string
		id       string
		opts     UpdateOpts
		wantArgs []string
	}{
		{
			name:     "claim only",
			id:       "spi-001",
			opts:     UpdateOpts{Claim: boolPtr(true)},
			wantArgs: []string{"update", "spi-001", "--claim"},
		},
		{
			name: "status and label",
			id:   "spi-002",
			opts: UpdateOpts{
				Status:   "closed",
				AddLabel: "review-done",
			},
			wantArgs: []string{"update", "spi-002", "--status", "closed", "--add-label", "review-done"},
		},
		{
			name: "remove label",
			id:   "spi-003",
			opts: UpdateOpts{
				RemoveLabel: "review-feedback",
			},
			wantArgs: []string{"update", "spi-003", "--remove-label", "review-feedback"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildUpdateArgs(tt.id, tt.opts)
			if !reflect.DeepEqual(got, tt.wantArgs) {
				t.Errorf("args = %v, want %v", got, tt.wantArgs)
			}
		})
	}
}

// TestMolPourArgs verifies molecule args building.
func TestMolPourArgs(t *testing.T) {
	got := buildMolArgs("pour", "spire-agent-work", map[string]string{"task": "spi-001"})
	// vars order from map is nondeterministic, but with one var it's fine
	want := []string{"mol", "pour", "spire-agent-work", "--var", "task=spi-001"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("args = %v, want %v", got, want)
	}
}

// TestInitArgs verifies init args building.
func TestInitArgs(t *testing.T) {
	tests := []struct {
		name     string
		opts     InitOpts
		wantArgs []string
	}{
		{
			name:     "minimal",
			opts:     InitOpts{},
			wantArgs: []string{"init"},
		},
		{
			name: "full",
			opts: InitOpts{
				Database: "spi",
				Prefix:   "spi",
				Force:    true,
			},
			wantArgs: []string{"init", "--database", "spi", "--prefix", "spi", "--force"},
		},
		{
			name: "server mode",
			opts: InitOpts{
				Database:   "smoke",
				Prefix:     "smk",
				Server:     true,
				ServerHost: "spire-dolt.spire-smoke.svc",
				ServerPort: 3306,
				ServerUser: "root",
			},
			wantArgs: []string{
				"init",
				"--database", "smoke",
				"--prefix", "smk",
				"--server",
				"--server-host", "spire-dolt.spire-smoke.svc",
				"--server-port", "3306",
				"--server-user", "root",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildInitArgs(tt.opts)
			if !reflect.DeepEqual(got, tt.wantArgs) {
				t.Errorf("args = %v, want %v", got, tt.wantArgs)
			}
		})
	}
}

func TestBeadStatusConstants(t *testing.T) {
	if string(StatusOpen) != "open" {
		t.Errorf("StatusOpen = %q", StatusOpen)
	}
	if string(StatusInProgress) != "in_progress" {
		t.Errorf("StatusInProgress = %q", StatusInProgress)
	}
	if string(StatusClosed) != "closed" {
		t.Errorf("StatusClosed = %q", StatusClosed)
	}
	if string(StatusDone) != "done" {
		t.Errorf("StatusDone = %q", StatusDone)
	}
}

func TestBeadTypeConstants(t *testing.T) {
	if string(TypeTask) != "task" {
		t.Errorf("TypeTask = %q", TypeTask)
	}
	if string(TypeBug) != "bug" {
		t.Errorf("TypeBug = %q", TypeBug)
	}
	if string(TypeFeature) != "feature" {
		t.Errorf("TypeFeature = %q", TypeFeature)
	}
	if string(TypeEpic) != "epic" {
		t.Errorf("TypeEpic = %q", TypeEpic)
	}
	if string(TypeChore) != "chore" {
		t.Errorf("TypeChore = %q", TypeChore)
	}
}

func TestDoltRemoteUnmarshal(t *testing.T) {
	raw := `{"name": "origin", "url": "https://doltremoteapi.dolthub.com/org/db"}`
	var r DoltRemote
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if r.Name != "origin" {
		t.Errorf("Name = %q, want %q", r.Name, "origin")
	}
	if r.URL != "https://doltremoteapi.dolthub.com/org/db" {
		t.Errorf("URL = %q", r.URL)
	}
}

// --- helpers ---

func intPtr(i int) *int    { return &i }
func boolPtr(b bool) *bool { return &b }
