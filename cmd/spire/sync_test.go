package main

import (
	"strings"
	"testing"
)

func TestCmdSyncNoArgs(t *testing.T) {
	// No args: prints usage and returns nil.
	err := cmdSync([]string{})
	if err != nil {
		t.Errorf("cmdSync([]): expected nil, got %v", err)
	}
}

func TestCmdSyncHelp(t *testing.T) {
	err := cmdSync([]string{"--help"})
	if err != nil {
		t.Errorf("cmdSync(--help): expected nil, got %v", err)
	}
}

func TestCmdSyncHelpShort(t *testing.T) {
	err := cmdSync([]string{"-h"})
	if err != nil {
		t.Errorf("cmdSync(-h): expected nil, got %v", err)
	}
}

func TestCmdSyncUnknownFlag(t *testing.T) {
	err := cmdSync([]string{"--unknown"})
	if err == nil {
		t.Fatal("cmdSync(--unknown): expected error, got nil")
	}
	if !strings.Contains(err.Error(), "unknown flag") {
		t.Errorf("expected 'unknown flag' in error, got: %v", err)
	}
}

func TestRunDoltSyncNoRemote(t *testing.T) {
	// Tower with no DolthubRemote — should be a no-op (no panic, no error).
	tower := TowerConfig{Name: "test", Database: "testdb", DolthubRemote: ""}
	runDoltSync(tower) // should return immediately without side effects
}

func TestDoltCLIFetchMergeNoDolt(t *testing.T) {
	// If dolt is not installed, doltCLIFetchMerge must return a descriptive error.
	if doltBin() != "" {
		t.Skip("dolt binary present — skipping no-dolt path")
	}
	_, err := doltCLIFetchMerge(t.TempDir())
	if err == nil {
		t.Fatal("expected error when dolt is not found")
	}
	if !strings.Contains(err.Error(), "dolt not found") {
		t.Errorf("expected 'dolt not found' in error, got: %v", err)
	}
}
