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

// bdSilent runs a bd command with --silent and returns the trimmed output (typically an ID).
func bdSilent(args ...string) (string, error) {
	args = append(args, "--silent")
	return bd(args...)
}
