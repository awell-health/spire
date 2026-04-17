package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/awell-health/spire/pkg/config"
	"github.com/awell-health/spire/pkg/dolt"
	towerpkg "github.com/awell-health/spire/pkg/tower"
	"github.com/spf13/cobra"
)

// cmd/spire/tower_cluster.go
//
// Non-interactive tower attach for cluster bootstrap. Runs inside a steward
// init container after the dolt StatefulSet's init container has cloned the
// tower DB from DoltHub and the dolt main container is serving. Reads
// project_id and prefix from the live dolt server, writes an authoritative
// .beads/ workspace to the steward PV, and registers Spire's custom bead
// types.
//
// Kept separate from cmdTowerAttach so the cluster path never drags in
// interactive prompts, keychain reads, global-config writes, or local dolt
// server lifecycle — all of which would be wrong in a pod.

var towerAttachClusterCmd = &cobra.Command{
	Use:   "attach-cluster",
	Short: "Non-interactive tower attach for cluster bootstrap",
	Long: `Seeds .beads/ in --data-dir after reading the attached tower's
project_id and prefix from the live dolt server. Expects the dolt server to
be reachable via DOLT_HOST/DOLT_PORT env (or --host/--port) and the database
to already exist on it (cloned by a separate init container).`,
	RunE: func(cmd *cobra.Command, args []string) error {
		dataDir, _ := cmd.Flags().GetString("data-dir")
		database, _ := cmd.Flags().GetString("database")
		prefixFallback, _ := cmd.Flags().GetString("prefix")
		dolthubRemote, _ := cmd.Flags().GetString("dolthub-remote")
		waitDur, _ := cmd.Flags().GetDuration("dolt-wait")
		return cmdTowerAttachCluster(dataDir, database, prefixFallback, dolthubRemote, waitDur)
	},
}

func init() {
	towerAttachClusterCmd.Flags().String("data-dir", "/data", "Directory containing .beads/ to seed")
	towerAttachClusterCmd.Flags().String("database", "", "Dolt database name (required)")
	towerAttachClusterCmd.Flags().String("prefix", "", "Bead prefix fallback (used if DB lacks prefix metadata)")
	towerAttachClusterCmd.Flags().String("dolthub-remote", "", "DoltHub remote path (e.g. awell/awell); stored in TowerConfig for provenance")
	towerAttachClusterCmd.Flags().Duration("dolt-wait", 120*time.Second, "How long to wait for dolt server reachability")
	towerCmd.AddCommand(towerAttachClusterCmd)
}

func cmdTowerAttachCluster(dataDir, database, prefixFallback, dolthubRemote string, waitDur time.Duration) error {
	if database == "" {
		return fmt.Errorf("--database is required")
	}
	if !isValidDatabaseName(database) {
		return fmt.Errorf("invalid database name %q", database)
	}
	if dataDir == "" {
		return fmt.Errorf("--data-dir must not be empty")
	}

	host, port := dolt.Host(), dolt.Port()
	fmt.Printf("[attach-cluster] waiting for database %q on %s:%s (up to %s)\n", database, host, port, waitDur)
	// Probe the database specifically — a bare `SELECT 1` succeeds as soon
	// as dolt accepts connections, but the DB may still be loading from the
	// clone the dolt StatefulSet init container ran. Querying the metadata
	// table ensures we only proceed once the cloned DB is actually served.
	probe := fmt.Sprintf("SELECT 1 FROM `%s`.metadata LIMIT 1", database)
	deadline := time.Now().Add(waitDur)
	for {
		if _, err := dolt.RawQuery(probe); err == nil {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("database %q not reachable on %s:%s after %s", database, host, port, waitDur)
		}
		time.Sleep(2 * time.Second)
	}
	fmt.Printf("[attach-cluster] database %q reachable\n", database)

	fmt.Printf("[attach-cluster] reading tower metadata from database %q\n", database)
	projectID, prefix, err := towerpkg.ReadMetadata(dolt.RawQuery, database)
	if err != nil {
		return err
	}
	if prefix == "" {
		prefix = prefixFallback
	}
	if prefix == "" {
		prefix = derivePrefixFromName(database)
	}
	if prefixFallback != "" && prefix != prefixFallback {
		fmt.Fprintf(os.Stderr, "[attach-cluster] warning: chart prefix=%q differs from DB prefix=%q — using DB value\n", prefixFallback, prefix)
	}

	tower := &config.TowerConfig{
		Name:          database,
		ProjectID:     projectID,
		HubPrefix:     prefix,
		DolthubRemote: dolthubRemote,
		Database:      database,
		CreatedAt:     time.Now().UTC().Format(time.RFC3339),
	}

	beadsDir := filepath.Join(dataDir, ".beads")
	fmt.Printf("[attach-cluster] seeding %s (project_id=%s, prefix=%s)\n", beadsDir, projectID, prefix)
	if err := towerpkg.BootstrapBeadsDir(towerpkg.BootstrapOpts{
		BeadsDir: beadsDir,
		Tower:    tower,
		DoltHost: host,
		DoltPort: port,
		Prefix:   prefix,
		AutoPush: false,
	}); err != nil {
		return err
	}

	// ensureCustomBeadTypes shells to bd against the seeded .beads dir.
	// Non-fatal: the tower's DB may already have everything registered from
	// `spire tower create`; we log and continue on error rather than crashloop.
	fmt.Println("[attach-cluster] ensuring Spire custom bead types")
	if err := ensureBootstrapCustomTypesFn(beadsDir); err != nil {
		fmt.Fprintf(os.Stderr, "[attach-cluster] warning: register custom bead types: %v\n", err)
	}

	fmt.Println("[attach-cluster] done")
	return nil
}
