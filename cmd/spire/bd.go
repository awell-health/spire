// bd.go — Subprocess execution for bd CLI commands.
//
// Most bead operations have been migrated to the beads library (see store.go).
// The functions below are retained for Tier 3 operations that are not yet
// available on the Storage interface or require CLI orchestration:
//
// bd() callers:
//   - push.go:  dolt remote list/add/remove, vc status/commit, bd status
//   - sync.go:  bd status, dolt remote/pull/push/fetch/reset, vc ops, count, export/import, sql (schema checks)
//   - focus.go: bd cook, bd mol pour, bd mol progress
//   - grok.go:  bd mol progress
//   - init.go:  bd config get, bd init
//   - spire_test.go: bd close --force (no store equivalent), bd mol progress
//
// bdJSON() callers:
//   - spire_test.go: TestIntegrationBdJSON
// NOTE: board.go and watch.go have been migrated to the store API + storeGetBlockedIssues.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"time"
)

// bdVerbose gates bd command logging. Background services set SPIRE_BD_LOG=1;
// interactive CLI stays quiet unless the user opts in.
var bdVerbose = os.Getenv("SPIRE_BD_LOG") != ""

// bd runs a bd command and returns stdout. Stderr is included in error on failure.
func bd(args ...string) (string, error) {
	label := "bd " + strings.Join(args, " ")
	if bdVerbose {
		log.Printf("[bd] exec: %s", label)
	}
	start := time.Now()

	cmd := exec.Command("bd", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()

	dur := time.Since(start).Seconds()
	if err != nil {
		errStr := strings.TrimSpace(stderr.String())
		if bdVerbose {
			log.Printf("[bd] FAIL (%.1fs): %s — %s", dur, label, errStr)
		}
		return "", fmt.Errorf("bd %s: %s\n%s", strings.Join(args, " "), err, errStr)
	}

	out := strings.TrimSpace(stdout.String())
	if bdVerbose {
		log.Printf("[bd] OK (%.1fs): %s — %d bytes", dur, label, len(out))
	}
	return out, nil
}

// bdJSON runs a bd command with --json and unmarshals the output.
func bdJSON(result any, args ...string) error {
	args = append(args, "--json")
	out, err := bd(args...)
	if err != nil {
		return err
	}
	if out == "" {
		return nil
	}
	return json.Unmarshal([]byte(out), result)
}

// ensureProjectID reads the local .beads/metadata.json project_id and the
// dolt server's _project_id, then updates the local file if they disagree.
// Called once at startup before the first steward cycle (steward.go:126).
//
// Tier 3 operation — uses raw dolt SQL, not the Storage interface.
func ensureProjectID() {
	metaPath := ".beads/metadata.json"
	data, err := os.ReadFile(metaPath)
	if err != nil {
		log.Printf("[project-id] cannot read %s: %s", metaPath, err)
		return
	}
	var meta map[string]any
	if err := json.Unmarshal(data, &meta); err != nil {
		log.Printf("[project-id] cannot parse %s: %s", metaPath, err)
		return
	}
	localPID, _ := meta["project_id"].(string)
	log.Printf("[project-id] local: %s", localPID)

	host := doltHost()
	port := doltPort()

	out, err := exec.Command(doltBin(), "sql",
		"--host", host, "--port", port,
		"--user", "root", "-p", "", "--no-tls",
		"-q", fmt.Sprintf("USE `%s`; SELECT value FROM metadata WHERE `key`='_project_id'", detectDBName()),
		"-r", "csv").Output()
	if err != nil {
		log.Printf("[project-id] cannot query server at %s:%s: %s", host, port, err)
		return
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) < 2 {
		log.Printf("[project-id] unexpected server response: %s", string(out))
		return
	}
	serverPID := strings.TrimSpace(lines[len(lines)-1])
	log.Printf("[project-id] server: %s", serverPID)

	if localPID == serverPID {
		log.Printf("[project-id] aligned")
		return
	}

	log.Printf("[project-id] MISMATCH — updating local %s → %s", localPID, serverPID)
	meta["project_id"] = serverPID
	updated, _ := json.MarshalIndent(meta, "", "  ")
	if err := os.WriteFile(metaPath, updated, 0644); err != nil {
		log.Printf("[project-id] cannot write %s: %s", metaPath, err)
		return
	}
	log.Printf("[project-id] realigned successfully")
}
