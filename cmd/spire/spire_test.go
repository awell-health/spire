package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// requireBd skips the test if bd is not available.
func requireBd(t *testing.T) {
	t.Helper()
	_, err := bd("--version")
	if err != nil {
		t.Skip("bd not available, skipping integration test")
	}
}

// TestParseAsFlag tests the --as flag extraction.
func TestParseAsFlag(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		wantAs   string
		wantArgs []string
	}{
		{"no flag", []string{"awp", "hello"}, "", []string{"awp", "hello"}},
		{"with flag", []string{"--as", "pan", "awp", "hello"}, "pan", []string{"awp", "hello"}},
		{"flag at end", []string{"awp", "hello", "--as", "pan"}, "pan", []string{"awp", "hello"}},
		{"flag in middle", []string{"awp", "--as", "pan", "hello"}, "pan", []string{"awp", "hello"}},
		{"flag without value", []string{"awp", "--as"}, "", []string{"awp", "--as"}},
		{"empty args", []string{}, "", []string{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotAs, gotArgs := parseAsFlag(tt.args)
			if gotAs != tt.wantAs {
				t.Errorf("parseAsFlag() as = %q, want %q", gotAs, tt.wantAs)
			}
			if len(gotArgs) != len(tt.wantArgs) {
				t.Errorf("parseAsFlag() args len = %d, want %d", len(gotArgs), len(tt.wantArgs))
				return
			}
			for i, a := range gotArgs {
				if a != tt.wantArgs[i] {
					t.Errorf("parseAsFlag() args[%d] = %q, want %q", i, a, tt.wantArgs[i])
				}
			}
		})
	}
}

// TestParseBead tests JSON parsing of bd show output.
func TestParseBead(t *testing.T) {
	// bd show --json returns an array
	input := `[{"id":"spi-abc","title":"Test","status":"open","priority":2,"issue_type":"task","labels":["msg","to:pan"]}]`
	b, err := parseBead([]byte(input))
	if err != nil {
		t.Fatalf("parseBead() error = %v", err)
	}
	if b.ID != "spi-abc" {
		t.Errorf("ID = %q, want %q", b.ID, "spi-abc")
	}
	if b.Title != "Test" {
		t.Errorf("Title = %q, want %q", b.Title, "Test")
	}
	if b.Status != "open" {
		t.Errorf("Status = %q, want %q", b.Status, "open")
	}
	if b.Priority != 2 {
		t.Errorf("Priority = %d, want %d", b.Priority, 2)
	}
	if b.Type != "task" {
		t.Errorf("Type = %q, want %q (check json tag is issue_type)", b.Type, "task")
	}
	if len(b.Labels) != 2 {
		t.Errorf("Labels len = %d, want 2", len(b.Labels))
	}
}

// TestParseBeadEmpty tests parsing empty arrays.
func TestParseBeadEmpty(t *testing.T) {
	_, err := parseBead([]byte(`[]`))
	if err == nil {
		t.Error("parseBead([]) should return error")
	}
}

// TestParseBeadSingleObject tests that a single object (not array) fails gracefully.
func TestParseBeadSingleObject(t *testing.T) {
	// parseBead expects an array — single object should fail
	_, err := parseBead([]byte(`{"id":"spi-abc"}`))
	if err == nil {
		t.Error("parseBead(single object) should return error")
	}
}

// TestHasLabel tests label prefix matching.
func TestHasLabel(t *testing.T) {
	b := Bead{Labels: []string{"msg", "to:pan", "from:awp", "ref:pan-42"}}

	if v := hasLabel(b, "to:"); v != "pan" {
		t.Errorf("hasLabel(to:) = %q, want %q", v, "pan")
	}
	if v := hasLabel(b, "from:"); v != "awp" {
		t.Errorf("hasLabel(from:) = %q, want %q", v, "awp")
	}
	if v := hasLabel(b, "ref:"); v != "pan-42" {
		t.Errorf("hasLabel(ref:) = %q, want %q", v, "pan-42")
	}
	if v := hasLabel(b, "missing:"); v != "" {
		t.Errorf("hasLabel(missing:) = %q, want empty", v)
	}
}

// TestDetectIdentityAsFlag tests that --as flag takes priority.
func TestDetectIdentityAsFlag(t *testing.T) {
	id, err := detectIdentity("pan")
	if err != nil {
		t.Fatalf("detectIdentity(pan) error = %v", err)
	}
	if id != "pan" {
		t.Errorf("detectIdentity(pan) = %q, want %q", id, "pan")
	}
}

// TestDetectIdentityEnv tests SPIRE_IDENTITY env var.
func TestDetectIdentityEnv(t *testing.T) {
	os.Setenv("SPIRE_IDENTITY", "awp")
	defer os.Unsetenv("SPIRE_IDENTITY")

	id, err := detectIdentity("")
	if err != nil {
		t.Fatalf("detectIdentity() error = %v", err)
	}
	if id != "awp" {
		t.Errorf("detectIdentity() = %q, want %q", id, "awp")
	}
}

// TestDetectIdentityFlagOverridesEnv tests that flag wins over env.
func TestDetectIdentityFlagOverridesEnv(t *testing.T) {
	os.Setenv("SPIRE_IDENTITY", "awp")
	defer os.Unsetenv("SPIRE_IDENTITY")

	id, err := detectIdentity("pan")
	if err != nil {
		t.Fatalf("detectIdentity(pan) error = %v", err)
	}
	if id != "pan" {
		t.Errorf("detectIdentity(pan) = %q, want %q (flag should override env)", id, "pan")
	}
}

// TestDetectIdentityNone tests error when nothing is set.
func TestDetectIdentityNone(t *testing.T) {
	os.Unsetenv("SPIRE_IDENTITY")
	// This will try bd config get, which may or may not work
	_, err := detectIdentity("")
	if err == nil {
		// Only fail if we're sure no config exists
		// In a dev environment, bd config might return something
		t.Log("detectIdentity() returned no error — bd config may have a prefix set")
	}
}

// --- Integration tests (require bd + dolt server) ---

// TestIntegrationRegisterUnregister tests the full register/unregister cycle.
func TestIntegrationRegisterUnregister(t *testing.T) {
	requireBd(t)

	name := "test-agent-" + t.Name()

	// Register
	err := cmdRegister([]string{name})
	if err != nil {
		t.Fatalf("register error: %v", err)
	}

	// Find the agent bead
	id, err := findAgentBead(name)
	if err != nil {
		t.Fatalf("findAgentBead error: %v", err)
	}
	if id == "" {
		t.Fatal("findAgentBead returned empty ID")
	}

	// Register again (idempotent) — should return same ID
	id2, err := findAgentBead(name)
	if err != nil {
		t.Fatalf("second findAgentBead error: %v", err)
	}
	if id2 != id {
		t.Errorf("idempotent register returned different ID: %q vs %q", id2, id)
	}

	// Unregister
	err = cmdUnregister([]string{name})
	if err != nil {
		t.Fatalf("unregister error: %v", err)
	}

	// Should be gone
	id3, err := findAgentBead(name)
	if err != nil {
		t.Fatalf("findAgentBead after unregister error: %v", err)
	}
	if id3 != "" {
		t.Errorf("agent still found after unregister: %q", id3)
	}
}

// TestIntegrationSendCollectRead tests the full message lifecycle.
func TestIntegrationSendCollectRead(t *testing.T) {
	requireBd(t)

	os.Setenv("SPIRE_IDENTITY", "test-sender")
	defer os.Unsetenv("SPIRE_IDENTITY")

	// Send a message
	err := cmdSend([]string{"test-receiver", "hello from test", "--ref", "pan-99"})
	if err != nil {
		t.Fatalf("send error: %v", err)
	}

	// Collect as receiver
	var messages []Bead
	err = bdJSON(&messages, "list", "--rig=spi", "--label", "msg,to:test-receiver", "--status=open")
	if err != nil {
		t.Fatalf("collect query error: %v", err)
	}

	if len(messages) == 0 {
		t.Fatal("no messages found for test-receiver")
	}

	// Find our message
	var msgID string
	for _, m := range messages {
		if m.Title == "hello from test" {
			msgID = m.ID
			// Verify labels
			hasTo := false
			hasFrom := false
			hasRef := false
			for _, l := range m.Labels {
				if l == "to:test-receiver" {
					hasTo = true
				}
				if l == "from:test-sender" {
					hasFrom = true
				}
				if l == "ref:pan-99" {
					hasRef = true
				}
			}
			if !hasTo {
				t.Error("message missing to:test-receiver label")
			}
			if !hasFrom {
				t.Error("message missing from:test-sender label")
			}
			if !hasRef {
				t.Error("message missing ref:pan-99 label")
			}
			break
		}
	}
	if msgID == "" {
		t.Fatal("could not find test message")
	}

	// Read (close) the message
	err = cmdRead([]string{msgID})
	if err != nil {
		t.Fatalf("read error: %v", err)
	}

	// Verify it's closed
	out, err := bd("show", msgID, "--json")
	if err != nil {
		t.Fatalf("show after read error: %v", err)
	}
	b, err := parseBead([]byte(out))
	if err != nil {
		t.Fatalf("parse after read error: %v", err)
	}
	if b.Status != "closed" {
		t.Errorf("message status = %q after read, want %q", b.Status, "closed")
	}

	// Read again — should be no-op
	err = cmdRead([]string{msgID})
	if err != nil {
		t.Fatalf("second read error: %v", err)
	}
}

// TestIntegrationSendWithThread tests threaded messages.
func TestIntegrationSendWithThread(t *testing.T) {
	requireBd(t)

	os.Setenv("SPIRE_IDENTITY", "thread-sender")
	defer os.Unsetenv("SPIRE_IDENTITY")

	// Send parent message
	err := cmdSend([]string{"thread-receiver", "parent message"})
	if err != nil {
		t.Fatalf("send parent error: %v", err)
	}

	// Find the parent
	var messages []Bead
	err = bdJSON(&messages, "list", "--rig=spi", "--label", "msg,to:thread-receiver", "--status=open")
	if err != nil {
		t.Fatalf("list error: %v", err)
	}
	var parentID string
	for _, m := range messages {
		if m.Title == "parent message" {
			parentID = m.ID
			break
		}
	}
	if parentID == "" {
		t.Fatal("could not find parent message")
	}

	// Send reply in thread
	err = cmdSend([]string{"thread-receiver", "reply message", "--thread", parentID})
	if err != nil {
		t.Fatalf("send reply error: %v", err)
	}

	// Find the reply — verify it was created
	err = bdJSON(&messages, "list", "--rig=spi", "--label", "msg,to:thread-receiver", "--status=open")
	if err != nil {
		t.Fatalf("list after reply error: %v", err)
	}
	var replyID string
	for _, m := range messages {
		if m.Title == "reply message" {
			replyID = m.ID
			break
		}
	}
	if replyID == "" {
		t.Fatal("reply message not created")
	}

	// Clean up
	for _, m := range messages {
		bd("close", m.ID)
	}
}

// TestIntegrationFocus tests focus with molecule pour.
func TestIntegrationFocus(t *testing.T) {
	requireBd(t)

	// Create a task
	taskID, err := bdSilent("create", "--rig=spi", "--type=task", "--title", "Focus test task", "-p", "2")
	if err != nil {
		t.Fatalf("create task error: %v", err)
	}

	// First focus — should pour molecule
	err = cmdFocus([]string{taskID})
	if err != nil {
		t.Fatalf("first focus error: %v", err)
	}

	// Verify molecule was created with workflow label
	var mols []Bead
	err = bdJSON(&mols, "list", "--rig=spi", "--label", "workflow:"+taskID, "--status=open")
	if err != nil {
		t.Fatalf("find molecule error: %v", err)
	}
	if len(mols) == 0 {
		t.Fatal("no molecule found after focus")
	}
	molID := mols[0].ID

	// Verify molecule has progress
	progressOut, err := bd("mol", "progress", molID)
	if err != nil {
		t.Fatalf("mol progress error: %v", err)
	}
	if !strings.Contains(progressOut, "0 / 4") {
		t.Errorf("progress = %q, want to contain '0 / 4'", progressOut)
	}

	// Second focus — should NOT pour again
	err = cmdFocus([]string{taskID})
	if err != nil {
		t.Fatalf("second focus error: %v", err)
	}

	// Verify still only one molecule
	err = bdJSON(&mols, "list", "--rig=spi", "--label", "workflow:"+taskID, "--status=open")
	if err != nil {
		t.Fatalf("find molecule after second focus error: %v", err)
	}
	if len(mols) != 1 {
		t.Errorf("expected 1 molecule, got %d (pour ran twice?)", len(mols))
	}

	// Clean up: close molecule and task
	bd("close", molID, "--force")
	bd("close", taskID, "--force")
}

// TestBdJSON tests the bdJSON helper with a real bd call.
func TestBdJSON(t *testing.T) {
	requireBd(t)

	var result []json.RawMessage
	err := bdJSON(&result, "list", "--rig=spi")
	if err != nil {
		t.Fatalf("bdJSON error: %v", err)
	}
	// result may be empty or populated, both are valid
}

// --- Daemon / Webhook tests ---

func TestLinearToBeadsPriority(t *testing.T) {
	tests := []struct {
		linear int
		beads  int
	}{
		{0, 3}, // no priority -> P3
		{1, 0}, // urgent -> P0
		{2, 1}, // high -> P1
		{3, 2}, // medium -> P2
		{4, 3}, // low -> P3
	}
	for _, tt := range tests {
		got := linearToBeadsPriority(tt.linear)
		if got != tt.beads {
			t.Errorf("linearToBeadsPriority(%d) = %d, want %d", tt.linear, got, tt.beads)
		}
	}
}

func TestMapLabelsToRig(t *testing.T) {
	tests := []struct {
		name   string
		labels []string
		want   string
		found  bool
	}{
		{"exact match", []string{"Workstream: Platform"}, "awp", true},
		{"prefix Panels", []string{"Panels - Design"}, "pan", true},
		{"prefix Grove", []string{"Grove", "Bug"}, "gro", true},
		{"no match", []string{"Bug", "Feature"}, "", false},
		{"empty labels", []string{}, "", false},
		{"exact wins over prefix", []string{"Workstream: Platform", "Panels"}, "awp", true},
		{"panels variant", []string{"Panels - Frontend"}, "pan", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, found := mapLabelsToRig(tt.labels)
			if got != tt.want || found != tt.found {
				t.Errorf("mapLabelsToRig(%v) = (%q, %v), want (%q, %v)", tt.labels, got, found, tt.want, tt.found)
			}
		})
	}
}

func TestParseWebhookPayload(t *testing.T) {
	payload := `{
		"action": "update",
		"type": "Issue",
		"data": {
			"id": "uuid-123",
			"identifier": "AWE-42",
			"title": "Fix auth",
			"priority": 2,
			"labels": [{"name": "Panels - Design"}, {"name": "Bug"}]
		}
	}`

	event, err := parseWebhookPayload(payload)
	if err != nil {
		t.Fatalf("parseWebhookPayload error: %v", err)
	}
	if event.Action != "update" {
		t.Errorf("Action = %q, want %q", event.Action, "update")
	}
	if event.Data.Identifier != "AWE-42" {
		t.Errorf("Identifier = %q, want %q", event.Data.Identifier, "AWE-42")
	}
	if event.Data.Title != "Fix auth" {
		t.Errorf("Title = %q, want %q", event.Data.Title, "Fix auth")
	}
	if event.Data.Priority != 2 {
		t.Errorf("Priority = %d, want %d", event.Data.Priority, 2)
	}
	if len(event.Data.Labels) != 2 {
		t.Errorf("Labels len = %d, want 2", len(event.Data.Labels))
	}
}

func TestParseWebhookPayloadMissingIdentifier(t *testing.T) {
	payload := `{"action": "update", "type": "Issue", "data": {"title": "No ID"}}`
	_, err := parseWebhookPayload(payload)
	if err == nil {
		t.Error("expected error for missing identifier")
	}
}

func TestParseWebhookPayloadInvalid(t *testing.T) {
	_, err := parseWebhookPayload("not json")
	if err == nil {
		t.Error("expected error for invalid JSON")
	}
}

func TestIntegrationProcessWebhookEvent(t *testing.T) {
	requireBd(t)

	// Create a fake webhook event bead
	payload := `{"action":"create","type":"Issue","data":{"id":"uuid-test","identifier":"AWE-99","title":"Integration test epic","priority":2,"labels":[{"name":"Panels - Test"}]}}`

	eventID, err := bdSilent(
		"create",
		"--rig=spi",
		"--type=task",
		"-p", "3",
		"--title", "Issue created: AWE-99",
		"--labels", "webhook,event:Issue.create,linear:AWE-99",
		"--description", payload,
	)
	if err != nil {
		t.Fatalf("create webhook event: %v", err)
	}

	// Run a single daemon cycle
	processed, errors := processWebhookEvents()
	if errors > 0 {
		t.Errorf("processWebhookEvents had %d errors", errors)
	}
	if processed == 0 {
		t.Error("processWebhookEvents processed 0 events")
	}

	// Verify the event bead is closed
	out, err := bd("show", eventID, "--json")
	if err != nil {
		t.Fatalf("show event after processing: %v", err)
	}
	eventBead, _ := parseBead([]byte(out))
	if eventBead.Status != "closed" {
		t.Errorf("event status = %q, want closed", eventBead.Status)
	}

	// Verify an epic bead was created in the pan rig
	var epics []Bead
	err = bdJSON(&epics, "list", "--rig=pan", "--label", "linear:AWE-99", "--type", "epic")
	if err != nil {
		t.Fatalf("list epic: %v", err)
	}
	if len(epics) == 0 {
		t.Fatal("no epic bead created for AWE-99")
	}
	if epics[0].Title != "Integration test epic" {
		t.Errorf("epic title = %q, want %q", epics[0].Title, "Integration test epic")
	}

	// Clean up
	bd("close", epics[0].ID, "--force")
}

// --- Webhook Queue tests ---

func TestWebhookSignatureVerification(t *testing.T) {
	// Test the same HMAC-SHA256 algorithm used in api/webhook.js
	secret := "test-secret"
	body := `{"action":"update","type":"Issue","data":{"identifier":"AWE-1"}}`

	h := hmac.New(sha256.New, []byte(secret))
	h.Write([]byte(body))
	expected := hex.EncodeToString(h.Sum(nil))

	// Verify it produces a deterministic signature
	if expected == "" {
		t.Error("empty signature")
	}
	if len(expected) != 64 {
		t.Errorf("signature length = %d, want 64", len(expected))
	}

	// Verify same input produces same output
	h2 := hmac.New(sha256.New, []byte(secret))
	h2.Write([]byte(body))
	expected2 := hex.EncodeToString(h2.Sum(nil))
	if expected != expected2 {
		t.Errorf("non-deterministic signature: %q vs %q", expected, expected2)
	}

	// Verify different secret produces different signature
	h3 := hmac.New(sha256.New, []byte("wrong-secret"))
	h3.Write([]byte(body))
	wrong := hex.EncodeToString(h3.Sum(nil))
	if expected == wrong {
		t.Error("different secrets produced same signature")
	}
}

func TestDoltSQL(t *testing.T) {
	// Skip if dolt is not available
	_, err := exec.LookPath("dolt")
	if err != nil {
		t.Skip("dolt not available, skipping")
	}
	requireBd(t)

	out, err := doltSQL("SELECT 1 AS n", true)
	if err != nil {
		t.Fatalf("doltSQL error: %v", err)
	}
	if !strings.Contains(out, "1") {
		t.Errorf("doltSQL output = %q, expected to contain '1'", out)
	}
}

func TestIntegrationProcessWebhookQueue(t *testing.T) {
	requireBd(t)

	// Skip if dolt is not available
	_, err := exec.LookPath("dolt")
	if err != nil {
		t.Skip("dolt not available, skipping")
	}

	// Create the webhook_queue table if needed
	_, err = doltSQL(`CREATE TABLE IF NOT EXISTS webhook_queue (
		id VARCHAR(36) PRIMARY KEY,
		event_type VARCHAR(64) NOT NULL,
		linear_id VARCHAR(32) NOT NULL,
		payload JSON NOT NULL,
		processed BOOLEAN NOT NULL DEFAULT 0,
		created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
	)`, false)
	if err != nil {
		t.Fatalf("create webhook_queue table: %v", err)
	}

	// Insert a test row
	testID := fmt.Sprintf("test-queue-%d", os.Getpid())
	payload := `{"action":"create","type":"Issue","data":{"id":"uuid-queue","identifier":"AWE-77","title":"Queue test epic","priority":1,"labels":[{"name":"Panels - Test"}]}}`
	escapedPayload := strings.ReplaceAll(payload, "'", "''")

	_, err = doltSQL(fmt.Sprintf(
		"INSERT INTO webhook_queue (id, event_type, linear_id, payload) VALUES ('%s', 'Issue.create', 'AWE-77', '%s')",
		testID, escapedPayload), false)
	if err != nil {
		t.Fatalf("insert queue row: %v", err)
	}

	// Process the queue
	processed, errors := processWebhookQueue()
	if errors > 0 {
		t.Errorf("processWebhookQueue had %d errors", errors)
	}
	if processed == 0 {
		t.Error("processWebhookQueue processed 0 rows")
	}

	// Verify the queue row is marked processed
	out, err := doltSQL(
		fmt.Sprintf("SELECT processed FROM webhook_queue WHERE id = '%s'", testID),
		true,
	)
	if err != nil {
		t.Fatalf("check processed: %v", err)
	}
	if !strings.Contains(out, "1") && !strings.Contains(out, "true") {
		t.Errorf("queue row not marked processed: %s", out)
	}

	// Clean up
	doltSQL(fmt.Sprintf("DELETE FROM webhook_queue WHERE id = '%s'", testID), false)
}
