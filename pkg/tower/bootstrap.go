package tower

import (
	"errors"
	"fmt"
	"path/filepath"

	"github.com/awell-health/spire/pkg/config"
)

// BlankBootstrapOpts configures BootstrapBlank. Callers pass every
// dependency explicitly so the function has no coupling to local config,
// keychain, or stdin — it is safe to call from a pod's init container.
type BlankBootstrapOpts struct {
	// Database is the dolt database name (e.g. "spi"). Required.
	Database string

	// Prefix is the bead prefix seeded via `bd init --prefix`. Required —
	// a first-boot database must land with a deterministic prefix so
	// later `spire tower attach-cluster` reads match the chart value.
	Prefix string

	// DataDir is the directory on the steward PV where bd init will root
	// its `.beads/` workspace (via `bd init` cwd). Required.
	DataDir string

	// RunBdInit shells out to the bd binary. Separated so the local and
	// cluster paths can wire different bd invocations and tests can
	// substitute a stub. Required.
	RunBdInit func(database, prefix, runDir string) error

	// EnsureCustomTypes registers Spire's custom bead types in the seeded
	// `.beads/` workspace. Optional — callers that register types later
	// can leave this nil.
	EnsureCustomTypes func(beadsDir string) error
}

// IsBlankDB returns true when the given database has no user tables.
// Used as a guard before bootstrap so the operation is idempotent on pod
// restart — a populated database must never be re-bootstrapped.
//
// Counts rows in information_schema.tables scoped to the database. A
// zero row count (or an unparseable result) is treated as blank.
func IsBlankDB(exec SQLExec, database string) (bool, error) {
	if database == "" {
		return false, errors.New("IsBlankDB: database is required")
	}
	out, err := exec(fmt.Sprintf(
		"SELECT COUNT(*) AS cnt FROM information_schema.tables WHERE table_schema = '%s'",
		database))
	if err != nil {
		return false, fmt.Errorf("count tables in %s: %w", database, err)
	}
	val := config.ExtractSQLValue(out)
	return val == "" || val == "0", nil
}

// BootstrapBlank runs the first-boot ritual against a blank tower database:
// `bd init` to create Spire's schema and seed `_project_id`, verify the
// project_id landed in the metadata table, then register Spire's custom
// bead types.
//
// BootstrapBlank is *not* idempotent on its own — the caller must
// blank-check with IsBlankDB first. Keeping the check outside the function
// lets callers log "DB already populated, skipping bootstrap" in the
// no-op case and lets tests drive known state without stubbing the
// check itself.
//
// The function does not write `.beads/metadata.json` — `bd init` writes
// one in embedded-mode, and the caller (typically immediately after) is
// expected to overwrite it via BootstrapBeadsDir with authoritative
// server-mode values. Keeping those concerns separate means the local and
// cluster paths converge on BootstrapBeadsDir rather than duplicating the
// metadata-file shape here.
func BootstrapBlank(exec SQLExec, opts BlankBootstrapOpts) error {
	if opts.Database == "" {
		return errors.New("BootstrapBlank: Database is required")
	}
	if opts.Prefix == "" {
		return errors.New("BootstrapBlank: Prefix is required")
	}
	if opts.DataDir == "" {
		return errors.New("BootstrapBlank: DataDir is required")
	}
	if opts.RunBdInit == nil {
		return errors.New("BootstrapBlank: RunBdInit is required")
	}

	if err := opts.RunBdInit(opts.Database, opts.Prefix, opts.DataDir); err != nil {
		return fmt.Errorf("bd init: %w", err)
	}

	// Verify bd init seeded _project_id in the metadata table. Without
	// this, a silently misconfigured bd (wrong DB, dropped table) would
	// leave the tower identity-less, and the first `spire file` would
	// fail with a confusing schema error.
	projectID, _, err := ReadMetadata(exec, opts.Database)
	if err != nil {
		return fmt.Errorf("verify project_id after bd init: %w", err)
	}
	if projectID == "" {
		return fmt.Errorf("bd init completed but %s.metadata has no _project_id", opts.Database)
	}

	if opts.EnsureCustomTypes != nil {
		beadsDir := filepath.Join(opts.DataDir, ".beads")
		if err := opts.EnsureCustomTypes(beadsDir); err != nil {
			return fmt.Errorf("register custom bead types: %w", err)
		}
	}

	return nil
}
