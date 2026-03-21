package main

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"
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

	// Write our PID file so spire down can find us
	writePID(daemonPIDPath(), os.Getpid())

	// Ensure webhook_queue table exists
	ensureWebhookQueue()

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

// runCycle executes one daemon cycle: process webhooks and sync epics.
// DoltHub remote sync (pull/push) is handled by the dedicated spire-syncer pod.
func runCycle() {
	log.Printf("[daemon] cycle start")

	// Step 1: Sync unsynced epics to Linear
	epicsSynced := syncEpicsToLinear()
	if epicsSynced > 0 {
		log.Printf("[daemon] synced %d epic(s) to Linear", epicsSynced)
	}

	// Step 2: Process webhook queue (from spire serve or serverless functions)
	qProcessed, qErrors := processWebhookQueue()
	if qProcessed > 0 || qErrors > 0 {
		log.Printf("[daemon] queue: processed %d rows (%d errors)", qProcessed, qErrors)
	}

	// Step 3: Process webhook event beads (legacy/direct bead creation)
	processed, errors := processWebhookEvents()
	if processed > 0 || errors > 0 {
		log.Printf("[daemon] processed %d events (%d errors)", processed, errors)
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

// ensureWebhookQueue creates the webhook_queue table if it doesn't exist.
func ensureWebhookQueue() {
	_, err := doltSQL(`CREATE TABLE IF NOT EXISTS webhook_queue (
		id          VARCHAR(36) PRIMARY KEY,
		event_type  VARCHAR(64) NOT NULL,
		linear_id   VARCHAR(32) NOT NULL,
		payload     JSON NOT NULL,
		processed   BOOLEAN NOT NULL DEFAULT 0,
		created_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
	)`, false)
	if err != nil {
		log.Printf("[daemon] ensure webhook_queue: %s", err)
	}
}
