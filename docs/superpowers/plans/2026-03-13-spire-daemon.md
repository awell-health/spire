# Spire Daemon Implementation Plan

> **For agentic workers:** REQUIRED: Use superpowers:subagent-driven-development (if subagents available) or superpowers:executing-plans to implement this plan. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a `spire daemon` subcommand that runs a pull/push sync loop and processes Linear webhook event beads — creating/updating epic beads and sending spire mail notifications.

**Architecture:** New subcommand in the existing Go binary. Shells out to `bd` for all database operations. Two new files: `daemon.go` (loop + CLI) and `webhook.go` (event processing logic). Signal-safe shutdown.

**Tech Stack:** Go 1.26 (stdlib only), beads CLI (`bd`), existing `spire` infrastructure

**Spec:** `docs/superpowers/specs/2026-03-13-spire-daemon-design.md`

---

## File Structure

```
cmd/spire/
  daemon.go      — spire daemon subcommand: flag parsing, tick loop, pull/push
  webhook.go     — webhook event parsing, label-to-rig mapping, epic bead sync
  main.go        — add "daemon" case to switch (one-line change)
  spire_test.go  — add unit + integration tests

setup.sh         — add LaunchAgent plist for spire daemon
```

---

## Chunk 1: Webhook Event Processing Logic

### Task 1: webhook.go — parsing, mapping, and epic sync

**Files:**
- Create: `cmd/spire/webhook.go`

- [ ] **Step 1: Write webhook.go with types and mapping functions**

Create `cmd/spire/webhook.go`:

```go
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
)

// linearEvent represents the relevant fields from a Linear webhook payload.
type linearEvent struct {
	Action string `json:"action"`
	Type   string `json:"type"`
	Data   struct {
		ID         string `json:"id"`
		Identifier string `json:"identifier"`
		Title      string `json:"title"`
		Priority   int    `json:"priority"`
		Labels     []struct {
			Name string `json:"name"`
		} `json:"labels"`
		Assignee *struct {
			Name  string `json:"name"`
			Email string `json:"email"`
		} `json:"assignee"`
	} `json:"data"`
}

// labelRigMap maps exact Linear label names to rig prefixes.
var labelRigMap = map[string]string{
	"Workstream: Platform": "awp",
}

// labelPrefixRigMap maps Linear label prefixes to rig prefixes.
var labelPrefixRigMap = map[string]string{
	"Panels": "pan",
	"Grove":  "gro",
}

// linearToBeadsPriority converts Linear priority (0-4) to beads priority (0-4).
// Linear: 0=none, 1=urgent, 2=high, 3=medium, 4=low
// Beads:  0=P0,   1=P1,     2=P2,   3=P3,      4=P4
func linearToBeadsPriority(linearPri int) int {
	switch linearPri {
	case 1:
		return 0
	case 2:
		return 1
	case 3:
		return 2
	case 4:
		return 3
	default:
		return 3 // no priority -> P3
	}
}

// mapLabelsToRig determines the rig prefix from Linear issue labels.
// Returns the rig prefix and true if a match is found, or "" and false.
func mapLabelsToRig(labels []string) (string, bool) {
	// Exact match first
	for _, label := range labels {
		if rig, ok := labelRigMap[label]; ok {
			return rig, true
		}
	}
	// Prefix match
	for _, label := range labels {
		for prefix, rig := range labelPrefixRigMap {
			if strings.HasPrefix(label, prefix) {
				return rig, true
			}
		}
	}
	return "", false
}

// parseWebhookPayload parses a Linear webhook JSON payload from a bead description.
func parseWebhookPayload(description string) (linearEvent, error) {
	var event linearEvent
	if err := json.Unmarshal([]byte(description), &event); err != nil {
		return event, fmt.Errorf("parse webhook payload: %w", err)
	}
	if event.Data.Identifier == "" {
		return event, fmt.Errorf("parse webhook payload: missing identifier")
	}
	return event, nil
}

// processWebhookEvent processes a single webhook event bead.
// Returns an error only if the event should be retried (not closed).
func processWebhookEvent(eventBead Bead) error {
	// Extract event type and linear identifier from labels
	eventType := hasLabel(eventBead, "event:")
	linearID := hasLabel(eventBead, "linear:")

	if linearID == "" {
		log.Printf("[daemon] event %s: no linear: label, skipping", eventBead.ID)
		return nil // close it, don't retry
	}

	// Parse the payload from description
	event, err := parseWebhookPayload(eventBead.Description)
	if err != nil {
		log.Printf("[daemon] event %s: %s, skipping", eventBead.ID, err)
		return nil // close it, malformed payload
	}

	// Extract Linear labels as strings
	var linearLabels []string
	for _, l := range event.Data.Labels {
		linearLabels = append(linearLabels, l.Name)
	}

	// Map to rig prefix
	rig, found := mapLabelsToRig(linearLabels)
	if !found {
		log.Printf("[daemon] event %s: no rig match for labels %v, skipping", eventBead.ID, linearLabels)
		return nil // close it, no rig match
	}

	// Ensure epic bead exists
	epicID, err := ensureEpicBead(rig, event)
	if err != nil {
		return fmt.Errorf("ensure epic bead: %w", err) // retry
	}

	// Notify owner if claimed
	err = notifyOwnerIfClaimed(epicID, linearID, eventType)
	if err != nil {
		// Non-fatal: notification failure should not prevent closing the event
		log.Printf("[daemon] event %s: notification failed: %s", eventBead.ID, err)
	}

	return nil
}

// ensureEpicBead finds or creates an epic bead for the given Linear issue.
// Returns the bead ID.
func ensureEpicBead(rig string, event linearEvent) (string, error) {
	identifier := event.Data.Identifier
	title := event.Data.Title
	priority := linearToBeadsPriority(event.Data.Priority)

	// Look for existing epic with this linear identifier
	var existing []Bead
	err := bdJSON(&existing, "list", fmt.Sprintf("--rig=%s", rig), "--label", fmt.Sprintf("linear:%s", identifier), "--type", "epic")
	if err != nil {
		return "", fmt.Errorf("search for epic linear:%s: %w", identifier, err)
	}

	if len(existing) > 0 {
		epicBead := existing[0]
		// Update title/priority if changed
		needsUpdate := false
		var updateArgs []string
		updateArgs = append(updateArgs, "update", epicBead.ID)

		if epicBead.Title != title {
			updateArgs = append(updateArgs, "--title", title)
			needsUpdate = true
		}
		if epicBead.Priority != priority {
			updateArgs = append(updateArgs, "-p", fmt.Sprintf("%d", priority))
			needsUpdate = true
		}

		if needsUpdate {
			_, err := bd(updateArgs...)
			if err != nil {
				return "", fmt.Errorf("update epic %s: %w", epicBead.ID, err)
			}
			log.Printf("[daemon] updated epic %s (%s): title/priority synced", epicBead.ID, identifier)
		}

		return epicBead.ID, nil
	}

	// Create new epic bead
	id, err := bdSilent(
		"create",
		fmt.Sprintf("--rig=%s", rig),
		"--type=epic",
		"--title", title,
		"-p", fmt.Sprintf("%d", priority),
		"--labels", fmt.Sprintf("linear:%s", identifier),
	)
	if err != nil {
		return "", fmt.Errorf("create epic for %s: %w", identifier, err)
	}

	log.Printf("[daemon] created epic %s for %s in rig %s", id, identifier, rig)
	return id, nil
}

// notifyOwnerIfClaimed sends spire mail to the epic owner if someone has claimed it.
func notifyOwnerIfClaimed(epicID, linearID, eventType string) error {
	// Fetch the epic bead to check for owner
	out, err := bd("show", epicID, "--json")
	if err != nil {
		return fmt.Errorf("show epic %s: %w", epicID, err)
	}

	epicBead, err := parseBead([]byte(out))
	if err != nil {
		return fmt.Errorf("parse epic %s: %w", epicID, err)
	}

	// Check for owner — look for the owner field or a claimed-by label
	owner := hasLabel(epicBead, "owner:")
	if owner == "" {
		// No owner, no notification
		return nil
	}

	// Send notification via spire send
	msg := fmt.Sprintf("%s updated (%s)", linearID, eventType)
	return cmdSend([]string{"--as", "spi", owner, msg, "--ref", epicID})
}
```

- [ ] **Step 2: Verify it compiles**

```bash
cd /Users/jb/awell/spire && go build -o /tmp/spire ./cmd/spire
```

Expected: compiles successfully (webhook.go is just library code, no main changes yet).

- [ ] **Step 3: Commit**

```bash
git add cmd/spire/webhook.go
git commit -m "feat(daemon): add webhook event parsing, label-to-rig mapping, and epic sync"
```

---

## Chunk 2: Daemon Subcommand

### Task 2: daemon.go — tick loop with pull/push and event processing

**Files:**
- Create: `cmd/spire/daemon.go`
- Modify: `cmd/spire/main.go` (add daemon case)

- [ ] **Step 1: Write daemon.go**

Create `cmd/spire/daemon.go`:

```go
package main

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func cmdDaemon(args []string) error {
	// Parse flags
	interval := 2 * time.Minute
	once := false

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--interval":
			if i+1 >= len(args) {
				return fmt.Errorf("--interval requires a value (e.g., 2m, 30s, 5m)")
			}
			i++
			d, err := time.ParseDuration(args[i])
			if err != nil {
				// Try parsing as plain seconds
				secs, serr := strconv.Atoi(args[i])
				if serr != nil {
					return fmt.Errorf("--interval: invalid duration %q", args[i])
				}
				d = time.Duration(secs) * time.Second
			}
			interval = d
		case "--once":
			once = true
		default:
			return fmt.Errorf("unknown flag: %s\nusage: spire daemon [--interval 2m] [--once]", args[i])
		}
	}

	log.Printf("[daemon] starting (interval=%s, once=%v)", interval, once)

	// Run first cycle immediately
	runCycle()

	if once {
		log.Printf("[daemon] --once mode, exiting")
		return nil
	}

	// Set up signal handling for graceful shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			runCycle()
		case sig := <-sigCh:
			log.Printf("[daemon] received %s, shutting down", sig)
			return nil
		}
	}
}

// runCycle executes one pull -> process -> push cycle.
func runCycle() {
	log.Printf("[daemon] cycle start")

	// Step 1: Pull
	_, err := bd("dolt", "pull")
	if err != nil {
		// Non-fatal: log and continue. Remote may not be configured.
		if !strings.Contains(err.Error(), "no remotes") {
			log.Printf("[daemon] pull warning: %s", err)
		}
	}

	// Step 2: Process webhook events
	processed, errors := processWebhookEvents()
	if processed > 0 || errors > 0 {
		log.Printf("[daemon] processed %d events (%d errors)", processed, errors)
	}

	// Step 3: Push
	_, err = bd("dolt", "push")
	if err != nil {
		if !strings.Contains(err.Error(), "no remotes") {
			log.Printf("[daemon] push warning: %s", err)
		}
	}

	log.Printf("[daemon] cycle complete")
}

// processWebhookEvents queries for unprocessed webhook event beads and processes them.
// Returns (processed count, error count).
func processWebhookEvents() (int, int) {
	var events []Bead
	err := bdJSON(&events, "list", "--label", "webhook", "--status=open")
	if err != nil {
		log.Printf("[daemon] list webhook events: %s", err)
		return 0, 0
	}

	if len(events) == 0 {
		return 0, 0
	}

	log.Printf("[daemon] found %d unprocessed webhook events", len(events))

	processed := 0
	errors := 0

	for _, event := range events {
		err := processWebhookEvent(event)
		if err != nil {
			// Don't close — will be retried next cycle
			log.Printf("[daemon] event %s: error (will retry): %s", event.ID, err)
			errors++
			continue
		}

		// Close the event bead (mark processed)
		_, closeErr := bd("close", event.ID)
		if closeErr != nil {
			log.Printf("[daemon] event %s: close failed: %s", event.ID, closeErr)
			errors++
			continue
		}

		processed++
	}

	return processed, errors
}
```

- [ ] **Step 2: Update main.go to add daemon case**

Add the `"daemon"` case to the switch in `cmd/spire/main.go`:

```go
	case "daemon":
		err = cmdDaemon(args)
```

Also update `printUsage()` to include the daemon command:

```
  daemon              Run sync daemon (--interval, --once)
```

- [ ] **Step 3: Build and test basic invocation**

```bash
cd /Users/jb/awell/spire && go build -o /tmp/spire ./cmd/spire

# Test help
/tmp/spire daemon --help 2>&1
# Expected: unknown flag error showing usage

# Test --once mode (should run one cycle and exit)
/tmp/spire daemon --once 2>&1
# Expected: logs cycle start/complete, exits cleanly
```

- [ ] **Step 4: Commit**

```bash
git add cmd/spire/daemon.go cmd/spire/main.go
git commit -m "feat(daemon): add spire daemon subcommand with pull/push tick loop"
```

---

## Chunk 3: Tests

### Task 3: Unit tests for webhook parsing and mapping

**Files:**
- Modify: `cmd/spire/spire_test.go`

- [ ] **Step 1: Add unit tests**

Append to `cmd/spire/spire_test.go`:

```go
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
```

- [ ] **Step 2: Run tests**

```bash
cd /Users/jb/awell/spire && go test ./cmd/spire/ -run "TestLinear|TestMapLabels|TestParseWebhook" -v
```

Expected: all tests pass.

- [ ] **Step 3: Add integration test for processWebhookEvents**

Append to `cmd/spire/spire_test.go`:

```go
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
```

- [ ] **Step 4: Run integration test**

```bash
cd /Users/jb/awell/spire && go test ./cmd/spire/ -run "TestIntegrationProcessWebhook" -v
```

Expected: test passes (creates webhook event bead, processes it, verifies epic was created).

- [ ] **Step 5: Commit**

```bash
git add cmd/spire/spire_test.go
git commit -m "test(daemon): add unit and integration tests for webhook processing"
```

---

## Chunk 4: LaunchAgent and setup.sh

### Task 4: Add LaunchAgent plist for spire daemon to setup.sh

**Files:**
- Modify: `setup.sh`

- [ ] **Step 1: Add step 8 to setup.sh — spire daemon LaunchAgent**

After the "Build spire CLI" section (step 7), before the "Setup complete" summary, add:

```bash
# ── Step 8: Spire daemon LaunchAgent ──────────────────────────────────────
echo "── 8. Spire daemon LaunchAgent ──"

SPIRE_PLIST_NAME="com.awell.spire-daemon"
SPIRE_PLIST_PATH="$HOME/Library/LaunchAgents/$SPIRE_PLIST_NAME.plist"
SPIRE_BIN="$HOME/.local/bin/spire"

if [ -x "$SPIRE_BIN" ]; then
  cat > "$SPIRE_PLIST_PATH" << EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN"
  "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>$SPIRE_PLIST_NAME</string>
    <key>ProgramArguments</key>
    <array>
        <string>$SPIRE_BIN</string>
        <string>daemon</string>
        <string>--interval</string>
        <string>2m</string>
    </array>
    <key>WorkingDirectory</key>
    <string>$HUB_DIR</string>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>StandardOutPath</key>
    <string>$DOLT_LOG_DIR/spire-daemon.log</string>
    <key>StandardErrorPath</key>
    <string>$DOLT_LOG_DIR/spire-daemon.error.log</string>
    <key>EnvironmentVariables</key>
    <dict>
        <key>SPIRE_IDENTITY</key>
        <string>spi</string>
        <key>BEADS_DOLT_SERVER_HOST</key>
        <string>127.0.0.1</string>
        <key>BEADS_DOLT_SERVER_PORT</key>
        <string>$DOLT_PORT</string>
        <key>BEADS_DOLT_SERVER_MODE</key>
        <string>1</string>
        <key>BEADS_DOLT_AUTO_START</key>
        <string>0</string>
    </dict>
</dict>
</plist>
EOF
  ok "LaunchAgent written ($SPIRE_PLIST_PATH)"

  # (Re)load the LaunchAgent
  launchctl bootout "gui/$(id -u)/$SPIRE_PLIST_NAME" 2>/dev/null || true
  launchctl bootstrap "gui/$(id -u)" "$SPIRE_PLIST_PATH"
  ok "Spire daemon started"
else
  warn "Spire binary not found at $SPIRE_BIN — skipping daemon LaunchAgent"
fi

echo ""
```

- [ ] **Step 2: Verify setup.sh syntax**

```bash
bash -n /Users/jb/awell/spire/setup.sh
```

Expected: no syntax errors.

- [ ] **Step 3: Commit**

```bash
git add setup.sh
git commit -m "feat(daemon): add LaunchAgent plist for spire daemon autostart"
```

---

## Chunk 5: DoltHub Remote Setup

### Task 5: Add DoltHub remote configuration to setup.sh

**Files:**
- Modify: `setup.sh`

- [ ] **Step 1: Add remote setup after beads init (step 4)**

After the beads hub initialization in step 4, add remote configuration:

```bash
# Configure DoltHub remote if not already set
REMOTE_COUNT=$(bd dolt remote list 2>/dev/null | grep -c "origin" || echo "0")
if [ "$REMOTE_COUNT" = "0" ]; then
  info "No DoltHub remote configured."
  info "To enable daemon sync, run:"
  info "  cd $HUB_DIR && bd dolt remote add origin <dolthub-url>"
else
  ok "DoltHub remote 'origin' configured"
fi
```

Note: This is informational only — the actual remote URL must be provided by the user since it depends on their DoltHub org. The daemon handles missing remotes gracefully.

- [ ] **Step 2: Commit**

```bash
git add setup.sh
git commit -m "feat(daemon): add DoltHub remote setup guidance to setup.sh"
```

---

## Chunk 6: Final verification

### Task 6: End-to-end test and build verification

- [ ] **Step 1: Build and run all tests**

```bash
cd /Users/jb/awell/spire && go build -o /tmp/spire ./cmd/spire && go test ./cmd/spire/ -v
```

Expected: all tests pass, binary builds.

- [ ] **Step 2: Test daemon --once**

```bash
/tmp/spire daemon --once 2>&1
```

Expected: logs one cycle (pull warning if no remote, 0 events processed, push warning if no remote), exits.

- [ ] **Step 3: Test daemon with short interval (manual stop with Ctrl-C)**

```bash
/tmp/spire daemon --interval 5s &
sleep 12
kill %1
```

Expected: runs 2-3 cycles, then cleanly shuts down on signal.

- [ ] **Step 4: Verify full help text**

```bash
/tmp/spire help
```

Expected: shows daemon command in the list.

- [ ] **Step 5: Final commit if any fixes needed**

Only if changes were required during testing:

```bash
git add -A
git commit -m "fix(daemon): integration test fixes"
```
