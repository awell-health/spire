//go:build e2e

package helpers

import (
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/steveyegge/beads"

	"github.com/awell-health/spire/pkg/store"
)

// OpenStoreViaPortForward wires pkg/store to the dolt database inside
// the test cluster. The flow is:
//
//  1. Set BEADS_DOLT_SERVER_HOST / BEADS_DOLT_SERVER_PORT so the beads
//     library uses the port-forwarded socket instead of the laptop's
//     default dolt server on 3307.
//  2. Build a scratch .beads/ directory containing a minimal dolt
//     config so beads.OpenFromConfig can open a remote connection.
//  3. Call store.OpenAt(scratchDir); defer store.Reset via t.Cleanup.
//
// The scratch directory is placed under t.TempDir so test cleanup
// handles its removal. Intentionally uses the pkg/store API — the user's
// standing rule is that tests must read through the store API rather
// than the bd subprocess (memory: feedback_store_api_not_bd.md).
func OpenStoreViaPortForward(t *testing.T, towerName, doltHost string, doltPort int) {
	t.Helper()

	t.Setenv("BEADS_DOLT_SERVER_HOST", doltHost)
	t.Setenv("BEADS_DOLT_SERVER_PORT", fmt.Sprintf("%d", doltPort))

	scratch := t.TempDir()
	beadsDir := scratch + "/.beads"
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", beadsDir, err)
	}
	// Minimal config.yaml the beads library can consume. The database
	// name mirrors the tower — helm install spire sets up a dolt
	// database matching the tower name (see SyncTowerDerivedConfigs).
	cfg := fmt.Sprintf("database: %s\nremote: true\n", towerName)
	if err := os.WriteFile(beadsDir+"/config.yaml", []byte(cfg), 0o644); err != nil {
		t.Fatalf("write %s/config.yaml: %v", beadsDir, err)
	}

	if _, err := store.OpenAt(beadsDir); err != nil {
		t.Fatalf("store.OpenAt(%s): %v", beadsDir, err)
	}
	t.Cleanup(store.Reset)
}

// GetPinnedIdentityBead returns the pinned-identity bead that the
// operator provisions for a WizardGuild/<namespace>/<name>/cache
// resource. The bead is discovered by its stable label set
// (`pinned-identity`, `resource:wizardguild-cache`, `guild:<name>`)
// rather than by ID, because the Status-stamped ID can be wiped
// between reconciles while the label shape stays constant.
//
// Returns the Bead when exactly one match exists. Fatals on zero or
// multiple matches — both are acceptance-criterion violations the test
// must surface loudly.
func GetPinnedIdentityBead(t *testing.T, guildName string) store.Bead {
	t.Helper()

	beadsList, err := store.ListBeads(beads.IssueFilter{
		Labels: []string{"pinned-identity", "guild:" + guildName},
	})
	if err != nil {
		t.Fatalf("list pinned-identity beads for guild=%s: %v", guildName, err)
	}
	if len(beadsList) == 0 {
		t.Fatalf("no pinned-identity bead for guild=%s — operator may not have reconciled yet", guildName)
	}
	if len(beadsList) > 1 {
		ids := make([]string, 0, len(beadsList))
		for _, b := range beadsList {
			ids = append(ids, b.ID)
		}
		t.Fatalf("expected exactly 1 pinned-identity bead for guild=%s, got %d: %v",
			guildName, len(beadsList), ids)
	}
	return beadsList[0]
}

// GetOpenWispsFor returns the set of open wisp recovery beads whose
// caused-by edge points at pinnedID. The filter mirrors
// operator/controllers/pinned_identity.go:listOpenWispsTargeting so
// tests assert against the same inventory the finalizer operates on.
func GetOpenWispsFor(t *testing.T, pinnedID string) []store.Bead {
	t.Helper()

	deps, err := store.GetDependentsWithMeta(pinnedID)
	if err != nil {
		t.Fatalf("get dependents for pinned=%s: %v", pinnedID, err)
	}
	var wisps []store.Bead
	for _, d := range deps {
		if string(d.DependencyType) != store.DepCausedBy {
			continue
		}
		if !d.Ephemeral {
			continue
		}
		if d.Status == "closed" {
			continue
		}
		b, err := store.GetBead(d.ID)
		if err != nil {
			t.Logf("skip dependent %s (cannot fetch): %v", d.ID, err)
			continue
		}
		wisps = append(wisps, b)
	}
	return wisps
}

// WaitForOpenWisp polls GetOpenWispsFor until exactly one open wisp is
// visible, then returns it. Fatals on timeout OR on seeing more than
// one open wisp (the failure-injection path is deterministic — one
// BackoffLimitExceeded Job should produce one wisp).
func WaitForOpenWisp(t *testing.T, pinnedID string, timeout time.Duration) store.Bead {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		wisps := GetOpenWispsFor(t, pinnedID)
		if len(wisps) == 1 {
			return wisps[0]
		}
		if len(wisps) > 1 {
			ids := make([]string, 0, len(wisps))
			for _, w := range wisps {
				ids = append(ids, w.ID)
			}
			t.Fatalf("expected exactly 1 open wisp for pinned=%s, got %d: %v",
				pinnedID, len(wisps), ids)
		}
		time.Sleep(3 * time.Second)
	}
	t.Fatalf("no open wisp observed for pinned=%s within %s", pinnedID, timeout)
	return store.Bead{}
}

// WaitForBeadStatus polls a bead's Status field until it matches `want`
// or timeout elapses. Fatals with the last-observed status on timeout
// to make flake triage easier.
func WaitForBeadStatus(t *testing.T, beadID, want string, timeout time.Duration) store.Bead {
	t.Helper()
	var last store.Bead
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		b, err := store.GetBead(beadID)
		if err != nil {
			t.Logf("get bead %s: %v", beadID, err)
			time.Sleep(2 * time.Second)
			continue
		}
		last = b
		if b.Status == want {
			return b
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("bead %s: status=%q never reached %q within %s",
		beadID, last.Status, want, timeout)
	return last
}

// GetBeadByID is a thin wrapper around store.GetBead that fatals on
// error. Useful for inline assertions where a bead is expected to
// exist by the time the test checks.
func GetBeadByID(t *testing.T, id string) store.Bead {
	t.Helper()
	b, err := store.GetBead(id)
	if err != nil {
		t.Fatalf("get bead %s: %v", id, err)
	}
	return b
}

// GetRecoveryLearningsByResourceURI queries the recovery_learnings table
// for rows whose bead metadata includes a source_resource_uri matching
// `uri`. The join is done through source_bead → bead metadata because
// recovery_learnings itself is keyed by source_bead; SourceResourceURI
// is persisted as a column on the wisp bead's metadata.
//
// Returns the rows in resolved_at-descending order. An empty slice means
// no learnings have been recorded yet — either because the recovery has
// not completed, or because an upstream task has not wired the cleric's
// learn step to stamp source_resource_uri into the SQL path.
//
// The helper uses a direct *sql.DB for the join because pkg/store does
// not surface a resource-URI lookup (the store-level helpers are keyed
// by SourceBead + FailureClass — see pkg/store/recovery_learnings.go).
// If the W2 acceptance criterion requires direct resource-URI lookup
// and this helper comes back empty, file a follow-up to add
// GetRecoveryLearningsByResourceURI on pkg/store itself; the test is
// permitted to t.Fatal here with that signal.
func GetRecoveryLearningsByResourceURI(t *testing.T, db *sql.DB, uri string) []store.RecoveryLearningRow {
	t.Helper()

	// The recovery_learnings table does not carry source_resource_uri
	// as its own column — it records source_bead + failure_class. We
	// join on the wisp bead's metadata to find learnings whose source
	// wisp referenced this resource. If the join returns nothing after
	// a successful recovery, that's signal the learn step is not yet
	// stamping the needed linkage.
	query := `
		SELECT l.id, l.recovery_bead, l.source_bead, l.failure_class,
		       l.failure_sig, l.resolution_kind, l.outcome,
		       l.learning_summary, l.reusable, l.resolved_at,
		       COALESCE(l.expected_outcome, ''),
		       COALESCE(l.mechanical_recipe, ''),
		       COALESCE(l.demoted_at, '')
		FROM recovery_learnings l
		LEFT JOIN issues_metadata m
		  ON m.issue_id = l.source_bead AND m.key = 'source_resource_uri'
		WHERE m.value = ?
		ORDER BY l.resolved_at DESC`

	rows, err := db.Query(query, uri)
	if err != nil {
		t.Fatalf("query recovery_learnings for resource=%s: %v", uri, err)
	}
	defer rows.Close()

	var out []store.RecoveryLearningRow
	for rows.Next() {
		var r store.RecoveryLearningRow
		var reusable int
		var resolvedAt, demotedAt string
		if err := rows.Scan(
			&r.ID, &r.RecoveryBead, &r.SourceBead, &r.FailureClass,
			&r.FailureSig, &r.ResolutionKind, &r.Outcome,
			&r.LearningSummary, &reusable, &resolvedAt,
			&r.ExpectedOutcome, &r.MechanicalRecipe, &demotedAt,
		); err != nil {
			t.Fatalf("scan recovery_learnings row: %v", err)
		}
		r.Reusable = reusable != 0
		r.ResolvedAt, _ = time.Parse("2006-01-02 15:04:05", resolvedAt)
		if demotedAt != "" {
			r.DemotedAt, _ = time.Parse("2006-01-02 15:04:05", demotedAt)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate recovery_learnings rows: %v", err)
	}
	return out
}

// OpenDoltSQL returns a *sql.DB connected to the forwarded dolt socket.
// Callers close the returned handle via t.Cleanup. Used for direct SQL
// against recovery_learnings and for wisp-GC verification where the
// store API does not expose the needed surface.
func OpenDoltSQL(t *testing.T, host string, port int, database string) *sql.DB {
	t.Helper()
	dsn := fmt.Sprintf("root:@tcp(%s:%d)/%s", host, port, database)
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("open dolt at %s: %v", dsn, err)
	}
	db.SetConnMaxLifetime(30 * time.Second)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// BeadExistsInWispsTable reports whether the wisp bead row still lives
// in the `wisps` table (as opposed to having been reaped by the wisp
// GC). Used by the LearningsSurviveWispGC test block to assert the
// wisp row is gone while the learning row survives.
//
// Uses the forwarded dolt handle rather than the bead store API
// because the store's GetBead auto-unions issues and wisps under the
// hood, which would hide reaping.
func BeadExistsInWispsTable(t *testing.T, db *sql.DB, wispID string) bool {
	t.Helper()
	var id string
	err := db.QueryRow(`SELECT id FROM wisps WHERE id = ? LIMIT 1`, wispID).Scan(&id)
	if err == sql.ErrNoRows {
		return false
	}
	if err != nil {
		// Missing table is a deployment-level problem, not a pass signal —
		// surface it so the reader doesn't misread an empty result.
		if strings.Contains(err.Error(), "wisps") {
			t.Fatalf("query wisps table (schema may be missing): %v", err)
		}
		t.Fatalf("query wisps table for %s: %v", wispID, err)
	}
	return id != ""
}

// WaitForWispReaped polls BeadExistsInWispsTable until the wisp row is
// gone OR the timeout elapses. Returns true on successful reap.
func WaitForWispReaped(t *testing.T, db *sql.DB, wispID string, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !BeadExistsInWispsTable(t, db, wispID) {
			return true
		}
		time.Sleep(3 * time.Second)
	}
	return false
}
