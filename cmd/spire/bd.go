package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

// bd runs a bd command and returns stdout. Stderr is included in error on failure.
func bd(args ...string) (string, error) {
	cmd := exec.Command("bd", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		return "", fmt.Errorf("bd %s: %s\n%s", strings.Join(args, " "), err, stderr.String())
	}
	return strings.TrimSpace(stdout.String()), nil
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
