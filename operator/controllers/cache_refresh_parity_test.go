package controllers

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/awell-health/spire/pkg/agent"
)

// TestCacheRefreshParity_ProducerFeedsConsumer is the load-bearing
// assertion for spi-yzmq0: the actual refresh script the operator
// would render, given the canonical intra-PVC layout env vars, must
// produce a cache tree that the unmodified worker-side consumer
// (agent.MaterializeWorkspaceFromCache) can read with no path
// munging. This test is why the producer and consumer share the
// pkg/agent.Cache* constants in the first place — if they ever drift
// again, this test fails before the drift reaches a cluster.
//
// The test does NOT mock: it runs the real shell script body in a
// temp dir with a temp /cache mount and a local seed repository
// standing in for the upstream. That exercises:
//
//   - the atomic rename of .tmp → revision marker
//   - the mirror/ subdirectory layout
//   - `git clone --no-hardlinks <mount>/mirror` in the consumer
//   - checkCacheReady's validation of the marker
//
// Gated on bash + git availability so CI environments without them
// skip rather than fail.
func TestCacheRefreshParity_ProducerFeedsConsumer(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-based parity test not supported on windows")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skipf("bash not on PATH (%v); skipping parity test", err)
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git not on PATH (%v); skipping parity test", err)
	}

	// 1. Seed a local "upstream" repository the refresh script will
	//    `git clone --mirror` from. A single committed file is
	//    sufficient — the contract is "the clone produces a
	//    worker-consumable tree", not any particular branch shape.
	seed := makeSeedRepo(t, "PARITY.md", "hello from parity\n")

	// 2. Stand up a fresh /cache mount in tempdir. The refresh
	//    script's SPIRE_CACHE_MOUNT env points here, so every
	//    intra-PVC path the script touches is under this directory.
	cache := t.TempDir()

	// 3. Assemble the env the reconciler would attach to the refresh
	//    Job container, plus repo identity vars the script also
	//    reads. cacheRefreshEnv(cache) overrides the mount path so
	//    the script can target the temp cache dir instead of /cache,
	//    and we override the termination-log path to a writable temp
	//    file because /dev/termination-log only exists inside a pod.
	termLog := filepath.Join(t.TempDir(), "termination.log")
	env := append(os.Environ(),
		"SPIRE_REPO_URL="+seed,
		"SPIRE_BRANCH_PIN=",
	)
	for _, e := range cacheRefreshEnv(cache) {
		if e.Name == "SPIRE_CACHE_TERMINATION_LOG" {
			env = append(env, e.Name+"="+termLog)
			continue
		}
		env = append(env, e.Name+"="+e.Value)
	}

	// 4. Execute the real script body in bash.
	cmd := exec.Command("bash", "-c", cacheRefreshScript)
	cmd.Env = env
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("refresh script exec failed: %v", err)
	}

	// 5. Producer-side sanity: the layout the refresh Job published
	//    should match what the consumer is about to read. Explicit
	//    assertions here make a layout-drift failure debuggable —
	//    otherwise the consumer just fails with ErrCacheUnavailable
	//    and the operator has no idea which side moved.
	if _, err := os.Stat(filepath.Join(cache, agent.CacheMirrorSubdir)); err != nil {
		t.Fatalf("refresh script did not create %s subdir under %s: %v",
			agent.CacheMirrorSubdir, cache, err)
	}
	if _, err := os.Stat(filepath.Join(cache, agent.CacheRevisionMarkerName)); err != nil {
		t.Fatalf("refresh script did not publish %s at %s: %v",
			agent.CacheRevisionMarkerName, cache, err)
	}
	// The .tmp sibling must be gone after the atomic rename. If it
	// lingers, the rename did not happen and any "ready" signal is a
	// lie.
	if _, err := os.Stat(filepath.Join(cache, agent.CacheRevisionTmpMarkerName)); err == nil {
		t.Errorf("refresh script left %s behind; atomic rename did not complete",
			agent.CacheRevisionTmpMarkerName)
	}

	// 6. Hand the published tree directly to the consumer — no
	//    translation, no path rewriting. If the producer and
	//    consumer agree on layout, this succeeds; if they do not, it
	//    fails with ErrCacheUnavailable or a clone error.
	workspace := filepath.Join(t.TempDir(), "ws")
	if err := agent.MaterializeWorkspaceFromCache(context.Background(), cache, workspace, "spi"); err != nil {
		if errors.Is(err, agent.ErrCacheUnavailable) {
			t.Fatalf("consumer rejected producer's cache tree as unavailable: %v\n"+
				"this is the exact drift bug (spi-yzmq0) the parity test exists to catch", err)
		}
		t.Fatalf("MaterializeWorkspaceFromCache failed on real producer output: %v", err)
	}

	// 7. The workspace must contain the seeded file so "successful
	//    consumer" isn't just "clone exited zero": we're also
	//    confirming the content flowed end-to-end through the
	//    producer's mirror layout.
	content, err := os.ReadFile(filepath.Join(workspace, "PARITY.md"))
	if err != nil {
		t.Fatalf("seeded file missing from materialized workspace: %v", err)
	}
	if string(content) != "hello from parity\n" {
		t.Errorf("PARITY.md content = %q, want %q", string(content), "hello from parity\n")
	}
}

// makeSeedRepo creates a local non-bare git repository containing a
// single commit of (relPath → content). Used as the SPIRE_REPO_URL
// the refresh script's `git clone --mirror` targets. `git clone
// --mirror <local-path>` works against a non-bare repo because git
// internally resolves <path>/.git when the target is bare-less; that
// matches how a real refresh Job would treat a remote URL, just
// without the network.
func makeSeedRepo(t *testing.T, relPath, content string) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	run("config", "user.email", "parity@example.com")
	run("config", "user.name", "parity")
	if err := os.WriteFile(filepath.Join(dir, relPath), []byte(content), 0o644); err != nil {
		t.Fatalf("write seed file: %v", err)
	}
	run("add", relPath)
	run("commit", "-q", "-m", "seed")
	return dir
}
