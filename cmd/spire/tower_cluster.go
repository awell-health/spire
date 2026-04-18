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
	Long: `attach-cluster has two modes.

Bootstrap mode (--data-dir/--database): seeds .beads/ in --data-dir after
reading the attached tower's project_id and prefix from the live dolt server.
Used by the steward init container in the Helm chart.

Register mode (--namespace): records a ClusterAttachment on the tower config
so later dispatch code can route work to a specific Kubernetes namespace.
When --in-cluster is set, uses the pod service account instead of a
kubeconfig file.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		namespace, _ := cmd.Flags().GetString("namespace")
		if namespace != "" {
			towerName, _ := cmd.Flags().GetString("tower")
			kubeconfig, _ := cmd.Flags().GetString("kubeconfig")
			kubeContext, _ := cmd.Flags().GetString("context")
			inCluster, _ := cmd.Flags().GetBool("in-cluster")
			return towerAttachCluster(towerpkg.AttachOptions{
				Tower:      towerName,
				Namespace:  namespace,
				Kubeconfig: kubeconfig,
				Context:    kubeContext,
				InCluster:  inCluster,
			})
		}
		dataDir, _ := cmd.Flags().GetString("data-dir")
		database, _ := cmd.Flags().GetString("database")
		prefixFallback, _ := cmd.Flags().GetString("prefix")
		dolthubRemote, _ := cmd.Flags().GetString("dolthub-remote")
		waitDur, _ := cmd.Flags().GetDuration("dolt-wait")
		return cmdTowerAttachCluster(dataDir, database, prefixFallback, dolthubRemote, waitDur)
	},
}

func init() {
	towerAttachClusterCmd.Flags().String("data-dir", "/data", "Directory containing .beads/ to seed (bootstrap mode)")
	towerAttachClusterCmd.Flags().String("database", "", "Dolt database name (bootstrap mode)")
	towerAttachClusterCmd.Flags().String("prefix", "", "Bead prefix fallback (bootstrap mode)")
	towerAttachClusterCmd.Flags().String("dolthub-remote", "", "DoltHub remote path; stored in TowerConfig for provenance (bootstrap mode)")
	towerAttachClusterCmd.Flags().Duration("dolt-wait", 120*time.Second, "How long to wait for dolt server reachability (bootstrap mode)")

	towerAttachClusterCmd.Flags().String("namespace", "", "Kubernetes namespace for the cluster attachment (register mode)")
	towerAttachClusterCmd.Flags().String("kubeconfig", "", "Path to kubeconfig; defaults to $KUBECONFIG or ~/.kube/config (register mode)")
	towerAttachClusterCmd.Flags().String("context", "", "Kubeconfig context name; empty means current context (register mode)")
	towerAttachClusterCmd.Flags().Bool("in-cluster", false, "Use pod service account instead of a kubeconfig file (register mode)")
	towerAttachClusterCmd.Flags().String("tower", "", "Target tower name; defaults to the active tower (register mode)")

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

	// Persist the tower config so `spire daemon` (local mode, in a
	// sidecar) can find and iterate it via config.ListTowerConfigs().
	// Without this, the cluster daemon reports "no towers configured,
	// skipping cycle" and nothing syncs.
	if err := config.SaveTowerConfig(tower); err != nil {
		fmt.Fprintf(os.Stderr, "[attach-cluster] warning: save tower config: %v\n", err)
	} else {
		fmt.Printf("[attach-cluster] saved tower config for %q\n", tower.Name)
	}

	fmt.Println("[attach-cluster] done")
	return nil
}
