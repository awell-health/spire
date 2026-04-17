package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/awell-health/spire/pkg/config"
	"github.com/awell-health/spire/pkg/dolt"
	"github.com/awell-health/spire/pkg/store"
)

const systemPrompt = `You are the steward's sidecar — an intelligent message router and coordinator for the Spire agent system. You process messages sent to the steward and translate them into bead operations.

## Your role

You sit between human operators and the Spire work graph. When someone sends a message to the steward, you:
1. Parse the intent — what do they want to happen?
2. Resolve the scope — which beads, agents, or epics are involved?
3. Execute — use your tools to make it happen (label beads, send messages, steer wizards, create dependencies, etc.)
4. Confirm — respond to the sender describing what you did.

## Capabilities

You can:
- **Apply directives** across beads: "make sure wizards on the auth epic reference the design doc"
  → Find children of the epic, add ref labels, steer active wizards
- **Re-prioritize work**: "drop everything, there's a prod bug in payments"
  → Create a P0 bead, identify who to interrupt, pause/steer them, assign the bug
- **Coordinate across agents**: "the API and frontend changes need to land together"
  → Re-parent beads under a shared epic (the artificer handles co-merge)
- **Broadcast decisions with semantic targeting**: "we're switching to Redis, tell data layer wizards"
  → Search beads by title/description to identify "data layer" work, send contextual messages
- **Track conditions**: "when the payments team finishes, assign the integration task"
  → Note the tracking condition in your state, check it each round

## State management

After processing each batch of messages, update your state by describing:
- **directives**: standing instructions you've applied (what, to which beads, who was steered)
- **tracking**: conditions you're watching for (when X happens, do Y)
- **pending**: actions you've taken that await confirmation (steered wizard-3, awaiting ack)
- **recent_decisions**: judgment calls with rationale (interrupted wizard-2 because their work was lower priority)

Keep your state concise. If a directive has been fully applied (all beads labeled, all wizards steered), you can drop it. If a tracking condition has been satisfied, remove it.

## Principles

- **Externalize immediately**: when you decide something, write it to the bead graph (labels, deps, comments) so it persists even if this session restarts.
- **Bead graph is truth**: your state file only holds in-flight reasoning. Everything durable goes into beads.
- **Semantic matching**: when a request mentions concepts ("data layer", "onboarding flow"), use list_beads to read titles and match them yourself.
- **Contextual messages**: when steering wizards, tailor the message to what THAT wizard is working on. Don't send generic broadcasts.
- **Judgment over rules**: you're here because some decisions can't be expressed as simple label queries or round-robin. Use your judgment.

## Message format

Incoming messages have this structure:
- id: message bead ID
- title: the message text
- labels: includes from:<sender>, ref:<bead-id> (optional), priority
- priority: 0=critical, 1=high, 2=medium, 3=normal, 4=backlog

## When there are no messages

If the inbox is empty, check your tracking conditions — any of them may have become satisfied since last round. Use list_beads or show_bead to check.

If nothing needs action, simply say "No action needed." to end the round.`

func main() {
	commsDir := flag.String("comms-dir", "/comms", "shared communication directory")
	pollInterval := flag.Duration("poll-interval", 30*time.Second, "inbox poll interval")
	port := flag.Int("port", 8081, "health endpoint port")
	// haiku-4-5 is the right model for this subsystem: routing, classification,
	// and tool-call selection for a message pipe. Sonnet-level reasoning is
	// overkill for "label these 3 beads" — the old default of sonnet-4-6
	// cost ~10x per call for no quality gain on the 90%+ routine traffic.
	model := flag.String("model", "claude-haiku-4-5-20251001", "Anthropic model to use")
	contextThreshold := flag.Float64("context-threshold", 0.45, "context usage fraction that triggers restart (0.0-1.0)")
	flag.Parse()

	log.Printf("[steward-sidecar] starting (comms=%s, poll=%s, model=%s, threshold=%.0f%%)",
		*commsDir, *pollInterval, *model, *contextThreshold*100)

	if err := os.MkdirAll(*commsDir, 0755); err != nil {
		log.Fatalf("failed to create comms dir: %v", err)
	}

	// Wire up store initialization (same pattern as cmd/spire/store_bridge.go).
	store.BeadsDirResolver = config.ResolveBeadsDir
	config.DoltDataDirFunc = dolt.DataDir
	config.StoreConfigGetterFunc = store.GetConfig
	if _, err := store.Ensure(config.ResolveBeadsDir()); err != nil {
		log.Printf("[steward-sidecar] store init: %v (tools will retry on first use)", err)
	}
	defer store.Reset()

	// Align project_id before the first inbox poll — ensures metadata.json
	// matches the dolt server even after restarts that change the ID.
	ensureProjectID()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	// Health server.
	mux := http.NewServeMux()
	var mu sync.RWMutex
	var lastSnapshot *StewardSnapshot

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "ok")
	})
	mux.HandleFunc("/status", func(w http.ResponseWriter, _ *http.Request) {
		mu.RLock()
		defer mu.RUnlock()
		w.Header().Set("Content-Type", "application/json")
		if lastSnapshot != nil {
			json.NewEncoder(w).Encode(lastSnapshot)
		} else {
			fmt.Fprint(w, "{}")
		}
	})

	server := &http.Server{
		Addr:              fmt.Sprintf(":%d", *port),
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("health server: %v", err)
		}
	}()

	// Heartbeat loop.
	go func() {
		heartbeatPath := filepath.Join(*commsDir, "heartbeat")
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(30 * time.Second):
				os.WriteFile(heartbeatPath, []byte(time.Now().UTC().Format(time.RFC3339)), 0644)
			}
		}
	}()

	// Main run loop — manages sessions with restart on context threshold.
	go func() {
		for {
			if ctx.Err() != nil {
				return
			}

			log.Println("[steward-sidecar] starting new session")
			runSession(ctx, *commsDir, *model, *pollInterval, *contextThreshold, &mu, &lastSnapshot)

			if ctx.Err() != nil {
				return
			}
			log.Println("[steward-sidecar] session ended, restarting in 5s")
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
		}
	}()

	// Wait for shutdown.
	<-sigCh
	log.Println("[steward-sidecar] shutting down")
	cancel()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	server.Shutdown(shutdownCtx)

	log.Println("[steward-sidecar] stopped")
}

// runSession runs a single LLM session until context threshold is reached or context is cancelled.
func runSession(ctx context.Context, commsDir, model string, pollInterval time.Duration, threshold float64, mu *sync.RWMutex, lastSnap **StewardSnapshot) {
	tools := NewStewardTools(commsDir)
	toolDefs := ToolDefinitions()

	// Load previous state for session restoration.
	prevState, err := ReadState(commsDir)
	if err != nil {
		log.Printf("[steward-sidecar] failed to read state: %v", err)
		prevState = &StewardState{}
	}

	checkpoint := FormatCheckpoint(prevState)
	var session *Session
	if checkpoint != "" && len(prevState.Directives)+len(prevState.Tracking)+len(prevState.Pending) > 0 {
		log.Println("[steward-sidecar] restoring from checkpoint")
		session = RestoreSession(model, systemPrompt, toolDefs, tools, checkpoint)
	} else {
		session = NewSession(model, systemPrompt, toolDefs, tools)
	}

	state := prevState
	round := 0

	// Process inbox on a timer.
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	// Run immediately on start.
	processInbox(session, commsDir, state, &round, mu, lastSnap, threshold)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Check for CHECKPOINT signal from runner (or external).
			controlPath := filepath.Join(commsDir, "control")
			if data, err := os.ReadFile(controlPath); err == nil {
				os.Remove(controlPath)
				cmd := strings.TrimSpace(string(data))
				if cmd == "CHECKPOINT" || cmd == "STOP" {
					log.Printf("[steward-sidecar] received %s signal, ending session", cmd)
					WriteState(commsDir, state)
					WriteSnapshot(commsDir, session, state, round)
					return
				}
			}

			processInbox(session, commsDir, state, &round, mu, lastSnap, threshold)

			// Check context threshold.
			if session.ContextUsage() >= threshold {
				log.Printf("[steward-sidecar] context at %.0f%%, checkpointing and restarting",
					session.ContextUsage()*100)
				WriteState(commsDir, state)
				WriteSnapshot(commsDir, session, state, round)
				return
			}
		}
	}
}

// processInbox collects messages and sends them to the LLM for processing.
func processInbox(session *Session, commsDir string, state *StewardState, round *int, mu *sync.RWMutex, lastSnap **StewardSnapshot, threshold float64) {
	// Collect inbox.
	inboxJSON, err := runSpire("collect", "--json", "steward")
	if err != nil {
		log.Printf("[steward-sidecar] collect failed: %v", err)
		return
	}

	// Parse messages.
	var messages []json.RawMessage
	if inboxJSON != "" {
		json.Unmarshal([]byte(inboxJSON), &messages)
	}

	// Mark messages as read.
	for _, raw := range messages {
		var msg struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(raw, &msg) == nil && msg.ID != "" {
			runSpire("read", msg.ID)
		}
	}

	// Idle gate: if nothing is going on, skip the LLM call entirely. The
	// sidecar's job is to react to messages and re-evaluate standing
	// conditions; with none of those active, there is literally nothing
	// for the model to do. Polling a 2,880-rounds/day tight loop against
	// an LLM endpoint for "no action needed." is pure waste.
	idle := len(messages) == 0 &&
		len(state.Tracking) == 0 &&
		len(state.Pending) == 0
	if idle {
		*round++
		log.Printf("[steward-sidecar] round %d: idle (no messages, tracking, or pending) — skipping LLM call", *round)
		return
	}

	// Build the prompt for this round.
	var prompt string
	if len(messages) > 0 {
		prompt = fmt.Sprintf("## Inbox (%d new messages)\n\n%s", len(messages), inboxJSON)
	} else {
		prompt = "No new messages. Check your tracking conditions."
	}

	// Add current state context.
	if len(state.Tracking) > 0 {
		tracking, _ := json.MarshalIndent(state.Tracking, "", "  ")
		prompt += fmt.Sprintf("\n\n## Active tracking conditions\n%s", string(tracking))
	}

	*round++
	log.Printf("[steward-sidecar] round %d: %d messages, context %.0f%%",
		*round, len(messages), session.ContextUsage()*100)

	// Send to LLM.
	response, err := session.Send(prompt)
	if err != nil {
		log.Printf("[steward-sidecar] LLM error: %v", err)
		return
	}

	log.Printf("[steward-sidecar] round %d response: %s", *round, truncateLog(response, 200))

	// Parse updated state from response if the LLM included it.
	// The LLM is instructed to maintain state, but we also try to extract
	// structured state if it outputs JSON in a known format.
	extractStateUpdate(response, state)

	// Write state and snapshot after every round (the "hook").
	WriteState(commsDir, state)
	WriteSnapshot(commsDir, session, state, *round)

	// Update in-memory snapshot for health endpoint.
	mu.Lock()
	snap := &StewardSnapshot{
		Timestamp:     time.Now().UTC().Format(time.RFC3339),
		ContextPct:    session.ContextUsage(),
		InputTokens:   session.totalInputTk,
		OutputTokens:  session.totalOutputTk,
		SessionRounds: *round,
		State:         state,
	}
	*lastSnap = snap
	mu.Unlock()
}

// extractStateUpdate tries to parse state updates from the LLM response.
// The LLM may update directives, tracking, pending, or decisions.
// We look for a JSON block in the response marked with a known key.
func extractStateUpdate(response string, state *StewardState) {
	// Look for a JSON state block in the response.
	// The LLM might output: ```json {"directives": [...], ...} ```
	// or include state inline.

	idx := strings.Index(response, `"directives"`)
	if idx < 0 {
		idx = strings.Index(response, `"tracking"`)
	}
	if idx < 0 {
		return
	}

	// Find the enclosing braces.
	start := strings.LastIndex(response[:idx], "{")
	if start < 0 {
		return
	}

	depth := 0
	for i := start; i < len(response); i++ {
		switch response[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				var update StewardState
				if json.Unmarshal([]byte(response[start:i+1]), &update) == nil {
					// Merge non-empty fields.
					if len(update.Directives) > 0 {
						state.Directives = update.Directives
					}
					if len(update.Tracking) > 0 {
						state.Tracking = update.Tracking
					}
					if len(update.Pending) > 0 {
						state.Pending = update.Pending
					}
					if len(update.RecentDecisions) > 0 {
						state.RecentDecisions = update.RecentDecisions
					}
				}
				return
			}
		}
	}
}

func truncateLog(s string, max int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
