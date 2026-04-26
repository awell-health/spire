package main

import (
	"strings"
	"testing"
)

func TestParseUpArgs_DefaultsStartSteward(t *testing.T) {
	opts, err := parseUpArgs(nil)
	if err != nil {
		t.Fatalf("parseUpArgs(nil): %v", err)
	}
	if !opts.startSteward {
		t.Errorf("startSteward = false, want true (steward must start by default)")
	}
	if opts.interval != "2m" {
		t.Errorf("interval = %q, want %q", opts.interval, "2m")
	}
	if opts.backendName != "" {
		t.Errorf("backendName = %q, want empty", opts.backendName)
	}
	if opts.metricsPort != "" {
		t.Errorf("metricsPort = %q, want empty", opts.metricsPort)
	}
}

func TestParseUpArgs_NoStewardOptsOut(t *testing.T) {
	opts, err := parseUpArgs([]string{"--no-steward"})
	if err != nil {
		t.Fatalf("parseUpArgs --no-steward: %v", err)
	}
	if opts.startSteward {
		t.Errorf("startSteward = true, want false after --no-steward")
	}
}

func TestParseUpArgs_StewardIsBackCompatNoOp(t *testing.T) {
	// --steward is the deprecated flag; it must still parse cleanly,
	// and steward must remain enabled (since enabled is the new default).
	opts, err := parseUpArgs([]string{"--steward"})
	if err != nil {
		t.Fatalf("parseUpArgs --steward: %v", err)
	}
	if !opts.startSteward {
		t.Errorf("startSteward = false, want true (--steward should not turn off the new default)")
	}
}

func TestParseUpArgs_NoStewardWinsOverSteward(t *testing.T) {
	opts, err := parseUpArgs([]string{"--steward", "--no-steward"})
	if err != nil {
		t.Fatalf("parseUpArgs --steward --no-steward: %v", err)
	}
	if opts.startSteward {
		t.Errorf("startSteward = true, want false (--no-steward must opt out even alongside --steward)")
	}
}

func TestParseUpArgs_AllFlags(t *testing.T) {
	opts, err := parseUpArgs([]string{
		"--interval", "5m",
		"--backend", "process",
		"--metrics-port", "9090",
	})
	if err != nil {
		t.Fatalf("parseUpArgs: %v", err)
	}
	if opts.interval != "5m" {
		t.Errorf("interval = %q, want %q", opts.interval, "5m")
	}
	if opts.backendName != "process" {
		t.Errorf("backendName = %q, want %q", opts.backendName, "process")
	}
	if opts.metricsPort != "9090" {
		t.Errorf("metricsPort = %q, want %q", opts.metricsPort, "9090")
	}
	if !opts.startSteward {
		t.Errorf("startSteward = false, want true")
	}
}

func TestParseUpArgs_UnknownFlagAdvertisesNoSteward(t *testing.T) {
	_, err := parseUpArgs([]string{"--bogus"})
	if err == nil {
		t.Fatalf("expected error for --bogus, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "--no-steward") {
		t.Errorf("usage string should advertise --no-steward, got: %q", msg)
	}
	if strings.Contains(msg, "[--steward]") {
		t.Errorf("usage string should not advertise the deprecated --steward, got: %q", msg)
	}
}

func TestParseUpArgs_MissingValues(t *testing.T) {
	cases := []string{"--interval", "--backend", "--metrics-port"}
	for _, flag := range cases {
		if _, err := parseUpArgs([]string{flag}); err == nil {
			t.Errorf("%s with no value: expected error, got nil", flag)
		}
	}
}
