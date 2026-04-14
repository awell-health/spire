package main

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	// Isolate all tests from the real dolt server. Tests must never hit a
	// live database — use pkg/store mocks or skip with doltIsReachable().
	// Both env vars needed: BEADS_DOLT_SERVER_PORT is checked by dolt.SQL()
	// directly, DOLT_PORT is checked by dolt.Port() (used by IsReachable).
	os.Setenv("BEADS_DOLT_SERVER_PORT", "19999")
	os.Setenv("DOLT_PORT", "19999")
	os.Exit(m.Run())
}

// restoreDoltPort undoes the TestMain isolation so integration tests gated by
// doltIsReachable() can reach the real server. Call at the top of any test
// that intentionally needs a live dolt connection.
func restoreDoltPort(t *testing.T) {
	t.Helper()
	t.Setenv("BEADS_DOLT_SERVER_PORT", "")
	t.Setenv("DOLT_PORT", "")
	os.Unsetenv("BEADS_DOLT_SERVER_PORT")
	os.Unsetenv("DOLT_PORT")
}
