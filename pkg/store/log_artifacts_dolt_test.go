//go:build dolt_integration

// This file is built only under the `dolt_integration` tag. It runs
// ListLogArtifactsForBead against a real, ephemeral Dolt sql-server so the
// recency ORDER BY — which uses a MySQL window function — is verified
// against the actual engine rather than sqlmock (which does not execute
// ORDER BY at all). It is excluded from the default `go test ./...` run.
//
// Run locally with:
//
//	go test -tags dolt_integration -run RealDolt ./pkg/store/
//
// The test skips (not fails) when the `dolt` binary is not on PATH, so a
// developer without Dolt installed never gets a red build.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

// startEphemeralDolt boots a throwaway dolt sql-server in a temp repo on a
// free port, returns an open *sql.DB pointed at it, and registers cleanup
// (kill server, close db, remove dir) on the test. It skips the test if the
// dolt binary is missing.
func startEphemeralDolt(t *testing.T) *sql.DB {
	t.Helper()

	bin, err := exec.LookPath("dolt")
	if err != nil {
		t.Skip("dolt binary not on PATH; skipping real-Dolt ordering test")
	}

	dir := t.TempDir()
	repo := filepath.Join(dir, "logtest") // db name = "logtest"
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}

	// Dolt requires a configured identity before `init`.
	for _, kv := range [][2]string{
		{"user.name", "spire-test"},
		{"user.email", "spire-test@example.com"},
	} {
		cfg := exec.Command(bin, "config", "--global", "--add", kv[0], kv[1])
		if out, err := cfg.CombinedOutput(); err != nil {
			t.Fatalf("dolt config %s: %v\n%s", kv[0], err, out)
		}
	}

	initCmd := exec.Command(bin, "init")
	initCmd.Dir = repo
	if out, err := initCmd.CombinedOutput(); err != nil {
		t.Fatalf("dolt init: %v\n%s", err, out)
	}

	port := freePort(t)
	srv := exec.Command(bin, "sql-server", "-H", "127.0.0.1", "-P", fmt.Sprintf("%d", port))
	srv.Dir = repo
	if err := srv.Start(); err != nil {
		t.Fatalf("start dolt sql-server: %v", err)
	}
	t.Cleanup(func() {
		_ = srv.Process.Kill()
		_, _ = srv.Process.Wait()
	})

	dsn := fmt.Sprintf("root@tcp(127.0.0.1:%d)/logtest?parseTime=true", port)
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// Wait for the server to accept connections.
	deadline := time.Now().Add(20 * time.Second)
	for {
		if err := db.Ping(); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("dolt sql-server not reachable on port %d within deadline", port)
		}
		time.Sleep(200 * time.Millisecond)
	}
	return db
}

// freePort grabs an OS-assigned free TCP port and immediately releases it
// for dolt to bind. The brief window between release and bind is acceptable
// for a single-process integration test.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// TestListLogArtifactsForBead_RecencyOrdering_RealDolt proves the window
// function actually orders attempt groups by recency on a live Dolt engine.
// The fixture is the load-bearing case: attempt IDs sort in the OPPOSITE
// direction to their timestamps, so an attempt_id-based ordering would put
// the rows in the wrong order. The newest attempt (spi-att-zzz) must lead.
func TestListLogArtifactsForBead_RecencyOrdering_RealDolt(t *testing.T) {
	db := startEphemeralDolt(t)
	ctx := context.Background()

	if _, err := db.ExecContext(ctx, AgentLogArtifactsTableSQL); err != nil {
		t.Fatalf("create table: %v", err)
	}

	// Insert in a deliberately scrambled order to prove ordering is the
	// query's job, not insertion order. Note the inversion: spi-att-aaa is
	// the OLDER attempt (Jan), spi-att-zzz the NEWER (Jun).
	type fix struct {
		id, attempt, run string
		seq              int
		createdAt        string
	}
	fixtures := []fix{
		{"log-a1", "spi-att-aaa", "run-1", 1, "2026-01-01 10:05:00"},
		{"log-z1", "spi-att-zzz", "run-2", 1, "2026-06-01 09:02:00"},
		{"log-a0", "spi-att-aaa", "run-1", 0, "2026-01-01 10:00:00"},
		{"log-z0", "spi-att-zzz", "run-2", 0, "2026-06-01 09:00:00"},
	}
	for _, f := range fixtures {
		_, err := db.ExecContext(ctx,
			`INSERT INTO agent_log_artifacts
			 (id, tower, bead_id, attempt_id, run_id, agent_name, role, phase,
			  provider, stream, sequence, object_uri, status, created_at, updated_at)
			 VALUES (?, 'awell', 'spi-x', ?, ?, 'wizard-spi-x', 'wizard', 'implement',
			         'claude', 'transcript', ?, ?, ?, ?, ?)`,
			f.id, f.attempt, f.run, f.seq,
			"file:///"+f.id+".jsonl", LogArtifactStatusFinalized, f.createdAt, f.createdAt,
		)
		if err != nil {
			t.Fatalf("insert %s: %v", f.id, err)
		}
	}

	got, err := ListLogArtifactsForBead(ctx, db, "spi-x")
	if err != nil {
		t.Fatalf("ListLogArtifactsForBead: %v", err)
	}

	wantIDs := []string{"log-z0", "log-z1", "log-a0", "log-a1"}
	if len(got) != len(wantIDs) {
		t.Fatalf("got %d rows, want %d", len(got), len(wantIDs))
	}
	for i, want := range wantIDs {
		if got[i].ID != want {
			t.Errorf("row %d: id = %q, want %q (full order: %s)", i, got[i].ID, want, ids(got))
		}
	}
}

func ids(recs []LogArtifactRecord) string {
	out := make([]string, len(recs))
	for i, r := range recs {
		out[i] = r.ID
	}
	return fmt.Sprint(out)
}
