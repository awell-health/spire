//go:build integration

// This file is built only under the `integration` tag. It runs the
// shared olaptest contract harness against a real ClickHouse server
// pointed at by SPIRE_CLICKHOUSE_DSN. If the env is unset the test
// skips — unit runs on developer laptops never block on an external
// service. CI starts a ClickHouse service container (see
// .github/workflows/ci.yml) and exports SPIRE_CLICKHOUSE_DSN to
// trigger the full contract.
package clickhouse

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/awell-health/spire/pkg/olap"
	"github.com/awell-health/spire/pkg/olap/olaptest"
)

const envDSN = "SPIRE_CLICKHOUSE_DSN"

// TestContract runs the shared Writer + TraceReader contract against a
// live ClickHouse. Each subtest gets a fresh Store pointed at a unique
// database so subtests can't cross-pollute state.
func TestContract(t *testing.T) {
	dsn := os.Getenv(envDSN)
	if dsn == "" {
		t.Skipf("%s not set; skipping integration contract tests", envDSN)
	}

	// Verify the DSN is reachable before we spin up subtests so the
	// failure mode is obvious ("can't reach ClickHouse") rather than
	// dozens of subtest failures with opaque messages.
	if err := pingDSN(dsn); err != nil {
		t.Fatalf("ping %s: %v", envDSN, err)
	}

	olaptest.RunContractTests(t, pairFactory(t, dsn))
}

// pairFactory returns a PairFactory that creates a fresh Store against
// an isolated per-subtest database name, so tests don't contaminate
// each other. ClickHouse's CREATE DATABASE IF NOT EXISTS is fast so
// this is cheap.
func pairFactory(t *testing.T, baseDSN string) olaptest.PairFactory {
	t.Helper()
	counter := 0
	return func(tt *testing.T) (olap.Writer, olap.TraceReader) {
		tt.Helper()
		counter++
		dbName := fmt.Sprintf("spire_ct_%d_%d", time.Now().UnixNano(), counter)
		subtestDSN, err := replaceDatabase(baseDSN, dbName)
		if err != nil {
			tt.Fatalf("derive subtest DSN: %v", err)
		}
		store, err := Open(olap.Config{DSN: subtestDSN})
		if err != nil {
			tt.Fatalf("open clickhouse: %v", err)
		}
		tt.Cleanup(func() {
			_ = dropDatabase(baseDSN, dbName)
		})
		return store, store
	}
}

// pingDSN verifies the server accepts a connection without attempting
// schema initialisation. It targets the always-present `default`
// bootstrap DB rather than the DSN's configured database, which may
// not exist yet (per-subtest DBs are created later by pairFactory).
// Any failure here is reported by TestContract as a hard stop since
// running the rest of the harness would produce misleading failures.
func pingDSN(dsn string) error {
	u, err := url.Parse(dsn)
	if err != nil {
		return fmt.Errorf("parse %s: %w", envDSN, err)
	}
	u.Path = "/default"
	db, err := sql.Open("clickhouse", u.String())
	if err != nil {
		return err
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return db.PingContext(ctx)
}

// replaceDatabase swaps the /database segment of a clickhouse DSN.
// E.g. clickhouse://host:9000/default → clickhouse://host:9000/<name>.
func replaceDatabase(dsn, name string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", err
	}
	u.Path = "/" + name
	return u.String(), nil
}

// dropDatabase connects to /default and drops the per-subtest database.
// Best-effort cleanup — errors are non-fatal.
func dropDatabase(dsn, name string) error {
	u, err := url.Parse(dsn)
	if err != nil {
		return err
	}
	u.Path = "/default"
	db, err := sql.Open("clickhouse", u.String())
	if err != nil {
		return err
	}
	defer db.Close()
	_, err = db.Exec("DROP DATABASE IF EXISTS " + name)
	return err
}
