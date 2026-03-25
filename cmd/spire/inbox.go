package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// inboxMessage is a single message in the inbox file.
type inboxMessage struct {
	ID        string `json:"id"`
	From      string `json:"from"`
	Ref       string `json:"ref,omitempty"`
	Text      string `json:"text"`
	Priority  int    `json:"priority"`
	CreatedAt string `json:"created_at"`
}

// inboxFile is the structure of the inbox.json file.
type inboxFile struct {
	Agent     string         `json:"agent"`
	UpdatedAt string         `json:"updated_at"`
	Messages  []inboxMessage `json:"messages"`
}

func cmdInbox(args []string) error {
	if d := resolveBeadsDir(); d != "" {
		os.Setenv("BEADS_DIR", d)
	}

	var (
		check      bool
		watch      bool
		jsonOut    bool
		timeout    = 5 * time.Minute
		remaining  []string
	)

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--check":
			check = true
		case "--watch":
			watch = true
		case "--json":
			jsonOut = true
		case "--timeout":
			if i+1 >= len(args) {
				return fmt.Errorf("--timeout requires a value")
			}
			i++
			d, err := time.ParseDuration(args[i])
			if err != nil {
				return fmt.Errorf("--timeout: invalid duration %q", args[i])
			}
			timeout = d
		default:
			remaining = append(remaining, args[i])
		}
	}

	// Determine agent name
	var agentName string
	if len(remaining) > 0 {
		agentName = remaining[0]
	} else {
		var err error
		agentName, err = detectIdentity("")
		if err != nil {
			return fmt.Errorf("inbox: could not determine agent name: %w", err)
		}
	}

	if watch {
		return inboxWatch(agentName, timeout, jsonOut)
	}

	if check {
		return inboxCheck(agentName)
	}

	// Default: read and display
	return inboxRead(agentName, jsonOut)
}

// inboxRead reads and displays the inbox file.
func inboxRead(agentName string, jsonOut bool) error {
	data, err := readInboxFile(agentName)
	if err != nil {
		if os.IsNotExist(err) {
			if jsonOut {
				fmt.Println("[]")
				return nil
			}
			fmt.Println("No messages.")
			return nil
		}
		return fmt.Errorf("inbox: %w", err)
	}

	var inbox inboxFile
	if err := json.Unmarshal(data, &inbox); err != nil {
		return fmt.Errorf("inbox: parse: %w", err)
	}

	if jsonOut {
		fmt.Println(string(data))
		return nil
	}

	if len(inbox.Messages) == 0 {
		fmt.Println("No messages.")
		return nil
	}

	fmt.Printf("%d message(s):\n\n", len(inbox.Messages))
	for _, m := range inbox.Messages {
		fmt.Printf("  %s  [from:%s]", m.ID, m.From)
		if m.Ref != "" {
			fmt.Printf("  [ref:%s]", m.Ref)
		}
		fmt.Printf("  %s\n", m.Text)
	}
	fmt.Printf("\nRun `spire read <id>` to mark as read.\n")
	return nil
}

// inboxCheck is for PostToolUse hooks: silent if empty, prints if new messages.
// Tracks last check time to avoid re-injecting the same messages.
func inboxCheck(agentName string) error {
	path := inboxPath(agentName)
	info, err := os.Stat(path)
	if err != nil {
		return nil // no file = no messages = silent
	}

	// Check if file has been modified since last check
	lastCheckPath := inboxLastCheckPath(agentName)
	lastCheck := time.Time{}
	if lcData, err := os.ReadFile(lastCheckPath); err == nil {
		lastCheck, _ = time.Parse(time.RFC3339Nano, strings.TrimSpace(string(lcData)))
	}

	if !lastCheck.IsZero() && !info.ModTime().After(lastCheck) {
		return nil // file hasn't changed since last check
	}

	// Read and parse inbox
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}

	var inbox inboxFile
	if err := json.Unmarshal(data, &inbox); err != nil {
		return nil
	}

	if len(inbox.Messages) == 0 {
		return nil // empty = silent
	}

	// Update last check time
	os.MkdirAll(filepath.Dir(lastCheckPath), 0755)
	os.WriteFile(lastCheckPath, []byte(time.Now().Format(time.RFC3339Nano)), 0644)

	// Print messages (stdout — Claude Code hook injects this)
	fmt.Printf("[spire inbox] %d new message(s) for %s:\n", len(inbox.Messages), agentName)
	for _, m := range inbox.Messages {
		fmt.Printf("  from:%s", m.From)
		if m.Ref != "" {
			fmt.Printf(" ref:%s", m.Ref)
		}
		fmt.Printf(" — %s\n", m.Text)
	}
	return nil
}

// inboxWatch blocks until new messages appear or timeout.
func inboxWatch(agentName string, timeout time.Duration, jsonOut bool) error {
	path := inboxPath(agentName)
	deadline := time.Now().Add(timeout)
	pollInterval := 2 * time.Second

	// Get initial state
	var lastMod time.Time
	if info, err := os.Stat(path); err == nil {
		lastMod = info.ModTime()
	}

	// Also check if there are already messages
	if hasNewMessages(path) {
		return inboxRead(agentName, jsonOut)
	}

	for time.Now().Before(deadline) {
		time.Sleep(pollInterval)

		info, err := os.Stat(path)
		if err != nil {
			continue
		}

		if info.ModTime().After(lastMod) {
			// File changed — check if it has messages
			if hasNewMessages(path) {
				return inboxRead(agentName, jsonOut)
			}
			lastMod = info.ModTime()
		}
	}

	// Timeout — no new messages
	if jsonOut {
		fmt.Println("[]")
	}
	return nil
}

// hasNewMessages checks if the inbox file has any messages.
func hasNewMessages(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var inbox inboxFile
	if err := json.Unmarshal(data, &inbox); err != nil {
		return false
	}
	return len(inbox.Messages) > 0
}

// --- Path helpers ---

// inboxPath returns the path to an agent's inbox file.
func inboxPath(agentName string) string {
	dir, err := configDir()
	if err != nil {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".config", "spire")
	}
	return filepath.Join(dir, "runtime", agentName, "inbox.json")
}

// inboxLastCheckPath returns the path to the last-check timestamp file.
func inboxLastCheckPath(agentName string) string {
	dir, err := configDir()
	if err != nil {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".config", "spire")
	}
	return filepath.Join(dir, "runtime", agentName, "inbox.last")
}

// readInboxFile reads the raw inbox file bytes.
func readInboxFile(agentName string) ([]byte, error) {
	return os.ReadFile(inboxPath(agentName))
}

// writeInboxFile writes the inbox file for an agent. Used by the daemon.
func writeInboxFile(agentName string, data []byte) error {
	path := inboxPath(agentName)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}
