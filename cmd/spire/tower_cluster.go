package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	bdpkg "github.com/awell-health/spire/pkg/bd"
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
Used by the steward init container in the Helm chart. When
--bootstrap-if-blank is set, a blank database (no user tables) is
initialized with Spire's schema + identity + custom bead types first, so
installs without a DoltHub remote can land a usable tower.

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
		bootstrapIfBlank, _ := cmd.Flags().GetBool("bootstrap-if-blank")
		bundleStoreBackend, _ := cmd.Flags().GetString("bundle-store-backend")
		bundleStoreGCSBucket, _ := cmd.Flags().GetString("bundle-store-gcs-bucket")
		bundleStoreGCSPrefix, _ := cmd.Flags().GetString("bundle-store-gcs-prefix")
		return cmdTowerAttachCluster(dataDir, database, prefixFallback, dolthubRemote, waitDur, bootstrapIfBlank, bundleStoreBackend, bundleStoreGCSBucket, bundleStoreGCSPrefix)
	},
}

func init() {
	towerAttachClusterCmd.Flags().String("data-dir", "/data", "Directory containing .beads/ to seed (bootstrap mode)")
	towerAttachClusterCmd.Flags().String("database", "", "Dolt database name (bootstrap mode)")
	towerAttachClusterCmd.Flags().String("prefix", "", "Bead prefix fallback (bootstrap mode)")
	towerAttachClusterCmd.Flags().String("dolthub-remote", "", "DoltHub remote path; stored in TowerConfig for provenance (bootstrap mode)")
	towerAttachClusterCmd.Flags().Duration("dolt-wait", 120*time.Second, "How long to wait for dolt server reachability (bootstrap mode)")
	towerAttachClusterCmd.Flags().Bool("bootstrap-if-blank", false, "If the target database has no user tables, run the `spire tower create` ritual (schema, project_id, custom types) before attaching. Required for Use Case 2b (no DoltHub install).")

	// BundleStore configuration — persisted into the TowerConfig saved
	// by this command. Empty values leave BundleStoreConfig zero-valued,
	// which the runtime interprets as backend=local (the default). The
	// helm chart passes these unconditionally from .Values.bundleStore.*
	// — empty strings must be tolerated silently, not rejected.
	towerAttachClusterCmd.Flags().String("bundle-store-backend", "", "BundleStore backend to persist in tower config. Empty leaves the existing value untouched; \"local\" or \"gcs\" write-through to tower.bundle_store.backend. Helm passes this from .Values.bundleStore.backend so the steward's tower config reflects what the chart deployed.")
	towerAttachClusterCmd.Flags().String("bundle-store-gcs-bucket", "", "GCS bucket name for backend=gcs. Required when --bundle-store-backend=gcs. Stored as tower.bundle_store.gcs.bucket.")
	towerAttachClusterCmd.Flags().String("bundle-store-gcs-prefix", "", "Optional GCS object-name prefix for backend=gcs. Stored as tower.bundle_store.gcs.prefix.")

	towerAttachClusterCmd.Flags().String("namespace", "", "Kubernetes namespace for the cluster attachment (register mode)")
	towerAttachClusterCmd.Flags().String("kubeconfig", "", "Path to kubeconfig; defaults to $KUBECONFIG or ~/.kube/config (register mode)")
	towerAttachClusterCmd.Flags().String("context", "", "Kubeconfig context name; empty means current context (register mode)")
	towerAttachClusterCmd.Flags().Bool("in-cluster", false, "Use pod service account instead of a kubeconfig file (register mode)")
	towerAttachClusterCmd.Flags().String("tower", "", "Target tower name; defaults to the active tower (register mode)")

	towerCmd.AddCommand(towerAttachClusterCmd)
}

// clusterRunBdInit is the default RunBdInit wiring for cluster bootstrap.
// Shells out to `bd init` with the steward PV as cwd so the command's
// `.beads/` workspace lands on the PV. Forces server mode with the dolt
// connection resolved from pkg/dolt (which reads BEADS_DOLT_SERVER_HOST
// / BEADS_DOLT_SERVER_PORT set by the steward init container).
//
// Server mode is required here: the steward image is built CGO-disabled
// for a static binary, so bd's embedded Dolt engine is unavailable and
// embedded-mode init fails hard with "embedded Dolt requires CGO". The
// --server flag routes bd at the external dolt sql-server running in
// the cluster.
//
// Sandbox stays on: the cluster bootstrap path hits a blank DB with no
// remote configured, and without --sandbox bd would try to auto-sync
// and error.
//
// Declared as a package var so tests can swap it for a stub without
// shelling out to a real bd binary.
//
// Force is intentionally left off. Idempotency is owned by the upstream
// IsBlankDB guard in cmdTowerAttachCluster — a populated database short-
// circuits before this runs. Without Force, a concurrent restart that
// somehow slipped past the blank check (race on first boot) will fail
// loudly instead of clobbering an in-flight project_id seed. project_id
// stability is a spec-level invariant ("generate once and only once").
var clusterRunBdInit = func(database, prefix, runDir string) error {
	port, err := strconv.Atoi(dolt.Port())
	if err != nil {
		return fmt.Errorf("parse dolt port %q: %w", dolt.Port(), err)
	}
	client := bdpkg.NewClient()
	client.RunDir = runDir
	client.Sandbox = true // no remote is configured on the blank path
	return client.Init(bdpkg.InitOpts{
		Database:   database,
		Prefix:     prefix,
		Server:     true,
		ServerHost: dolt.Host(),
		ServerPort: port,
		ServerUser: "root",
	})
}

func cmdTowerAttachCluster(dataDir, database, prefixFallback, dolthubRemote string, waitDur time.Duration, bootstrapIfBlank bool, bundleStoreBackend, bundleStoreGCSBucket, bundleStoreGCSPrefix string) error {
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
	// Probe strength depends on mode. Normal attach assumes a cloned DB —
	// probing `<db>.metadata` ensures the clone has finished loading, not
	// just that dolt accepts connections. In --bootstrap-if-blank mode the
	// table will not exist yet, so we probe the schema listing instead so a
	// freshly-`dolt init`ed empty DB can still pass the readiness gate.
	var probe string
	if bootstrapIfBlank {
		probe = fmt.Sprintf("SELECT 1 FROM information_schema.schemata WHERE schema_name = '%s' LIMIT 1", database)
	} else {
		probe = fmt.Sprintf("SELECT 1 FROM `%s`.metadata LIMIT 1", database)
	}
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

	if bootstrapIfBlank {
		blank, err := towerpkg.IsBlankDB(dolt.RawQuery, database)
		if err != nil {
			return fmt.Errorf("blank-check: %w", err)
		}
		if !blank {
			fmt.Printf("[attach-cluster] database %q already populated — skipping bootstrap\n", database)
		} else {
			prefixForBootstrap := prefixFallback
			if prefixForBootstrap == "" {
				prefixForBootstrap = derivePrefixFromName(database)
			}
			fmt.Printf("[attach-cluster] bootstrap: blank DB, running ritual (database=%s, prefix=%s)\n", database, prefixForBootstrap)
			if err := towerpkg.BootstrapBlank(dolt.RawQuery, towerpkg.BlankBootstrapOpts{
				Database:          database,
				Prefix:            prefixForBootstrap,
				DataDir:           dataDir,
				RunBdInit:         clusterRunBdInit,
				EnsureCustomTypes: ensureBootstrapCustomTypesFn,
			}); err != nil {
				return fmt.Errorf("bootstrap blank tower: %w", err)
			}
			fmt.Printf("[attach-cluster] bootstrap: ritual complete\n")
		}
	}

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
		Apprentice: config.ApprenticeConfig{
			Transport: config.ApprenticeTransportBundle,
		},
		// attach-cluster is the cluster-bootstrap path. Stamp the mode
		// explicitly so the persisted tower config matches the topology
		// the pod is actually running in, instead of defaulting to
		// local-native via the loader fallback. Idempotent: re-running
		// attach-cluster on helm upgrade re-saves the same value.
		DeploymentMode: config.DeploymentModeClusterNative,
	}

	// Persist BundleStore selection when the chart supplied one. Empty
	// backend leaves the field zero-valued so the existing in-process
	// defaults in pkg/bundlestore still apply (local backend under
	// $XDG_DATA_HOME). When backend=gcs, both the steward (this pod) and
	// every apprentice/wizard pod the operator spawns read the persisted
	// config — we're writing through the chart's selection to the cluster
	// dispatch path via the tower config on the shared PVC.
	if bundleStoreBackend != "" {
		tower.BundleStore = config.BundleStoreConfig{
			Backend: bundleStoreBackend,
			GCS: config.BundleStoreGCSConfig{
				Bucket: bundleStoreGCSBucket,
				Prefix: bundleStoreGCSPrefix,
			},
		}
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
