package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/awell-health/spire/pkg/agent"
	"github.com/awell-health/spire/pkg/beadlifecycle"
	"github.com/awell-health/spire/pkg/config"
	"github.com/awell-health/spire/pkg/executor"
	"github.com/awell-health/spire/pkg/wizard"
	"github.com/awell-health/spire/pkg/wizardregistry"
)

// TestCmdSummon_UnsupportedHeader surfaces the single user-facing invariant
// the epic is strict about: unknown `-H` header names are rejected, never
// silently forwarded.
func TestCmdSummon_UnsupportedHeader(t *testing.T) {
	err := cmdSummon([]string{"1", "-H", "authorization: Bearer foo"})
	if err == nil {
		t.Fatal("expected error for unsupported header")
	}
	if !strings.Contains(err.Error(), "authorization") || !strings.Contains(err.Error(), "supported") {
		t.Fatalf("expected unsupported-header error mentioning name and supported list, got: %v", err)
	}
}

// TestCmdSummon_MissingHeaderValue checks the happy error when `-H` is the
// last arg with no value — a common scripted-invocation typo.
func TestCmdSummon_MissingHeaderValue(t *testing.T) {
	err := cmdSummon([]string{"1", "-H"})
	if err == nil {
		t.Fatal("expected error when -H has no value")
	}
	if !strings.Contains(err.Error(), "-H") || !strings.Contains(err.Error(), "header value") {
		t.Fatalf("expected missing-value error, got: %v", err)
	}
}

// TestCmdSummon_TurboConflictsWithAuth is the early-exit guard for the
// mutually-exclusive pair. The user passing both means they're telling us
// two different things — better to surface it at parse time than quietly
// pick one and spawn with the wrong credential.
func TestCmdSummon_TurboConflictsWithAuth(t *testing.T) {
	err := cmdSummon([]string{"1", "--turbo", "--auth", "subscription"})
	if err == nil {
		t.Fatal("expected error when --turbo and --auth=subscription are combined")
	}
	if !strings.Contains(err.Error(), "--turbo") {
		t.Fatalf("expected error mentioning --turbo, got: %v", err)
	}
}

// TestCmdSummon_MissingAuthValue checks that `--auth` with no value gets a
// clear error instead of swallowing the next positional as the slot name.
func TestCmdSummon_MissingAuthValue(t *testing.T) {
	err := cmdSummon([]string{"1", "--auth"})
	if err == nil {
		t.Fatal("expected error when --auth has no value")
	}
	if !strings.Contains(err.Error(), "--auth") {
		t.Fatalf("expected missing-slot error, got: %v", err)
	}
}

// TestAttachAuthToRunState_FreshWritesPreliminary verifies the fresh-spawn
// path: no prior state → write a preliminary GraphState with only Auth,
// BeadID, and AgentName. The wizard subprocess's NewGraph loader then
// merges this into a fresh graph state. File must be 0600 — the Auth
// field holds secrets.
func TestAttachAuthToRunState_FreshWritesPreliminary(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("SPIRE_CONFIG_DIR", tmp)

	auth := &config.AuthContext{
		Active:           &config.AuthCredential{Slot: config.AuthSlotAPIKey, Secret: "sk-ant-api"},
		APIKey:           &config.AuthCredential{Slot: config.AuthSlotAPIKey, Secret: "sk-ant-api"},
		AutoPromoteOn429: true,
	}
	if err := attachAuthToRunState("wizard-spi-abc", auth, nil); err != nil {
		t.Fatalf("attachAuthToRunState err = %v", err)
	}

	path := filepath.Join(tmp, "runtime", "wizard-spi-abc", "graph_state.json")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("graph_state.json not written: %v", err)
	}
	if got := info.Mode().Perm(); got != 0600 {
		t.Errorf("graph_state.json perm = %o, want 0600 (contains secret)", got)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var gs executor.GraphState
	if err := json.Unmarshal(data, &gs); err != nil {
		t.Fatalf("unmarshal graph_state.json: %v", err)
	}
	if gs.BeadID != "spi-abc" {
		t.Errorf("BeadID = %q, want spi-abc", gs.BeadID)
	}
	if gs.AgentName != "wizard-spi-abc" {
		t.Errorf("AgentName = %q, want wizard-spi-abc", gs.AgentName)
	}
	if gs.Auth == nil || gs.Auth.Active == nil || gs.Auth.Active.Secret != "sk-ant-api" {
		t.Errorf("Auth not attached: %+v", gs.Auth)
	}
	// Preliminary state must have no Steps/Formula — that's the signal
	// NewGraph uses to materialize a real state from the graph def.
	if len(gs.Steps) != 0 {
		t.Errorf("preliminary state has Steps = %v, want none", gs.Steps)
	}
	if gs.Formula != "" {
		t.Errorf("preliminary state has Formula = %q, want empty", gs.Formula)
	}
}

// TestAttachAuthToRunState_ResumeUpdatesExisting verifies the resumption
// path: when a prior GraphState already exists (a wizard is being
// resummoned), attach must update the Auth field in place and preserve
// Steps, Formula, Counters, etc. If we clobbered the existing state we'd
// lose progress on the current run.
func TestAttachAuthToRunState_ResumeUpdatesExisting(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("SPIRE_CONFIG_DIR", tmp)

	existing := &executor.GraphState{
		BeadID:    "spi-xyz",
		AgentName: "wizard-spi-xyz",
		Formula:   "task-default",
		Entry:     "implement",
		Steps:     map[string]executor.StepState{"implement": {Status: "active"}},
		Counters:  map[string]int{"retries": 2},
		Auth: &config.AuthContext{
			Active: &config.AuthCredential{Slot: config.AuthSlotSubscription, Secret: "old-tok"},
		},
	}

	newAuth := &config.AuthContext{
		Active:           &config.AuthCredential{Slot: config.AuthSlotAPIKey, Secret: "new-key"},
		APIKey:           &config.AuthCredential{Slot: config.AuthSlotAPIKey, Secret: "new-key"},
		AutoPromoteOn429: true,
	}
	if err := attachAuthToRunState("wizard-spi-xyz", newAuth, existing); err != nil {
		t.Fatalf("attachAuthToRunState err = %v", err)
	}

	path := filepath.Join(tmp, "runtime", "wizard-spi-xyz", "graph_state.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var gs executor.GraphState
	if err := json.Unmarshal(data, &gs); err != nil {
		t.Fatal(err)
	}
	if gs.Auth == nil || gs.Auth.Active.Secret != "new-key" {
		t.Errorf("Auth not updated: %+v", gs.Auth)
	}
	// Pre-existing non-Auth fields must survive.
	if gs.Formula != "task-default" {
		t.Errorf("Formula clobbered: %q", gs.Formula)
	}
	if len(gs.Steps) != 1 || gs.Steps["implement"].Status != "active" {
		t.Errorf("Steps clobbered: %+v", gs.Steps)
	}
	if gs.Counters["retries"] != 2 {
		t.Errorf("Counters clobbered: %+v", gs.Counters)
	}
}

// TestAttachAuthToRunState_NilAuthNoop checks the defensive branch: a nil
// AuthContext is a no-op, not an error. Callers that chose to skip auth
// shouldn't crash.
func TestAttachAuthToRunState_NilAuthNoop(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("SPIRE_CONFIG_DIR", tmp)

	if err := attachAuthToRunState("wizard-spi-noop", nil, nil); err != nil {
		t.Fatalf("attachAuthToRunState(nil) err = %v, want nil (no-op)", err)
	}
	path := filepath.Join(tmp, "runtime", "wizard-spi-noop", "graph_state.json")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("nil-auth attach must not write a file, got stat err = %v", err)
	}
}

// TestCmdSummon_AuthPlumbingReachesSummonLocal verifies that the --auth flag
// actually flows from cmdSummon's arg loop into summonLocal. We can't fully
// exercise the spawn path without real infra, but we can stub
// selectAuthReadConfig and assert SelectAuth sees the flag via the failure
// path (configured subscription only; --auth=api-key should error with the
// "api-key slot required" message mentioning --auth=api-key).
func TestCmdSummon_AuthPlumbingReachesSummonLocal(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("SPIRE_CONFIG_DIR", tmp)

	prevRead := selectAuthReadConfig
	defer func() { selectAuthReadConfig = prevRead }()
	selectAuthReadConfig = func() (*config.AuthConfig, error) {
		return &config.AuthConfig{
			AutoPromoteOn429: true,
			Subscription:     &config.AuthCredential{Slot: config.AuthSlotSubscription, Secret: "tok"},
			// APIKey intentionally nil so --auth=api-key fails with our canonical error.
		}, nil
	}

	prevGet := storeGetBeadFunc
	defer func() { storeGetBeadFunc = prevGet }()
	storeGetBeadFunc = func(id string) (Bead, error) {
		return Bead{ID: id, Status: "in_progress", Title: "t"}, nil
	}

	prevResolve := wizardResolveRepoForSummon
	defer func() { wizardResolveRepoForSummon = prevResolve }()
	wizardResolveRepoForSummon = func(id string) (string, string, string, error) {
		return "/tmp/fake", "git@example:fake.git", "main", nil
	}

	prevK8s := isK8sAvailableFunc
	defer func() { isK8sAvailableFunc = prevK8s }()
	isK8sAvailableFunc = func() bool { return false }

	prevBegin := summonBeginWorkFunc
	defer func() { summonBeginWorkFunc = prevBegin }()
	summonBeginWorkFunc = func(_ beadlifecycle.Deps, _ wizardregistry.Registry, _ string, _ beadlifecycle.BeginOpts) (string, error) {
		return "attempt-stub", nil
	}

	err := cmdSummon([]string{"spi-abc", "--auth", "api-key"})
	if err == nil {
		t.Fatal("expected error because api-key slot is not configured")
	}
	if !strings.Contains(err.Error(), "api-key") {
		t.Errorf("expected api-key in error: %v", err)
	}
	if !strings.Contains(err.Error(), "--auth=api-key") {
		t.Errorf("expected --auth=api-key (the trigger) in error: %v", err)
	}
}

// TestCmdSummon_HeaderPlumbingReachesSummonLocal is the -H twin of the --auth
// plumbing test: verifies that -H reaches summonLocal and SelectAuth picks
// the ephemeral path. Uses the spawn seam to short-circuit after SelectAuth
// succeeds so we don't need a live dolt / kubectl.
func TestCmdSummon_HeaderPlumbingReachesSummonLocal(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("SPIRE_CONFIG_DIR", tmp)

	prevRead := selectAuthReadConfig
	defer func() { selectAuthReadConfig = prevRead }()
	selectAuthReadConfig = func() (*config.AuthConfig, error) {
		return &config.AuthConfig{AutoPromoteOn429: true}, nil
	}

	prevGet := storeGetBeadFunc
	defer func() { storeGetBeadFunc = prevGet }()
	storeGetBeadFunc = func(id string) (Bead, error) {
		return Bead{ID: id, Status: "in_progress", Title: "t", Priority: 2}, nil
	}

	prevResolve := wizardResolveRepoForSummon
	defer func() { wizardResolveRepoForSummon = prevResolve }()
	wizardResolveRepoForSummon = func(id string) (string, string, string, error) {
		return "/tmp/fake", "git@example:fake.git", "main", nil
	}

	prevK8s := isK8sAvailableFunc
	defer func() { isK8sAvailableFunc = prevK8s }()
	isK8sAvailableFunc = func() bool { return false }

	prevBegin := summonBeginWorkFunc
	defer func() { summonBeginWorkFunc = prevBegin }()
	summonBeginWorkFunc = func(_ beadlifecycle.Deps, _ wizardregistry.Registry, _ string, _ beadlifecycle.BeginOpts) (string, error) {
		return "attempt-stub", nil
	}

	prevSpawn := summonSpawnFunc
	defer func() { summonSpawnFunc = prevSpawn }()
	var spawned bool
	summonSpawnFunc = func(b AgentBackend, cfg SpawnConfig) (agent.Handle, error) {
		spawned = true
		// Return a benign error — we don't need a real process for the
		// assertion. The attach-to-run-state call must have fired first.
		return nil, os.ErrInvalid
	}

	_ = cmdSummon([]string{"spi-xyz", "-H", "x-anthropic-api-key: sk-ant-inline"})

	if !spawned {
		t.Fatal("expected summonSpawnFunc to be called (SelectAuth should succeed with inline header)")
	}
	// Verify attachAuthToRunState wrote the ephemeral context.
	path := filepath.Join(tmp, "runtime", "wizard-spi-xyz", "graph_state.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("graph_state.json missing: %v", err)
	}
	var gs executor.GraphState
	if err := json.Unmarshal(data, &gs); err != nil {
		t.Fatal(err)
	}
	if gs.Auth == nil || !gs.Auth.Ephemeral {
		t.Errorf("Auth not attached or not ephemeral: %+v", gs.Auth)
	}
	if gs.Auth.Active == nil || gs.Auth.Active.Secret != "sk-ant-inline" {
		t.Errorf("Active secret wrong: %+v", gs.Auth.Active)
	}
}

// Compile-time check: wizard.SelectFlags must be the type summonLocal takes.
// Guards against accidental rename/refactor that would silently relax the
// wiring contract.
var _ = func(f wizard.SelectFlags) error {
	return summonLocal(1, []string{"spi-a"}, "", f)
}
