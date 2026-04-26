package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	bdpkg "github.com/awell-health/spire/pkg/bd"
	"github.com/awell-health/spire/pkg/config"
	"github.com/awell-health/spire/pkg/dolt"
	"github.com/awell-health/spire/pkg/gatewayclient"
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
	Long: `attach-cluster has three modes.

Gateway mode (--url/--token): attaches a local CLI to a remote tower over
HTTPS. Fetches tower identity from GET /api/v1/tower on the gateway,
verifies the returned Name matches --tower, persists the bearer token in
the OS keychain, and writes a gateway-mode tower config locally. Every
subsequent bead/message op against this tower tunnels through the gateway.

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
		url, _ := cmd.Flags().GetString("url")
		token, _ := cmd.Flags().GetString("token")
		// Gateway mode is selected when either --url or --token is set. Both
		// must be provided together — refusing a half-configured invocation
		// avoids partially-written state (e.g. keychain without tower config).
		if url != "" || token != "" {
			if url == "" {
				return fmt.Errorf("--url is required when --token is set")
			}
			if token == "" {
				return fmt.Errorf("--token is required when --url is set")
			}
			towerName, _ := cmd.Flags().GetString("tower")
			if towerName == "" {
				return fmt.Errorf("--tower is required for gateway attach")
			}
			localAlias, _ := cmd.Flags().GetString("name")
			return cmdTowerAttachClusterGateway(url, token, towerName, localAlias)
		}

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
	towerAttachClusterCmd.Flags().String("tower", "", "Target tower name; defaults to the active tower (register mode / required for gateway mode)")

	// Gateway-mode flags. --url + --token must be provided together; --name
	// optionally overrides the local alias (default is --tower).
	towerAttachClusterCmd.Flags().String("url", "", "Gateway base URL (https://...) for gateway-mode attach; pairs with --token")
	towerAttachClusterCmd.Flags().String("token", "", "Gateway bearer token for gateway-mode attach; pairs with --url")
	towerAttachClusterCmd.Flags().String("name", "", "Local alias for the attached gateway tower; defaults to --tower (gateway mode)")

	towerCmd.AddCommand(towerAttachClusterCmd)
}

// gatewayAttachFetchTower fetches the remote tower identity via the gateway
// HTTPS API. Declared as a package variable so tests can substitute a fake
// that avoids building a real *http.Client. The default implementation is a
// thin wrapper around gatewayclient.NewClient + GetTower.
var gatewayAttachFetchTower = func(ctx context.Context, url, token string) (gatewayclient.TowerInfo, error) {
	return gatewayclient.NewClient(url, token).GetTower(ctx)
}

// gatewayAttachSetToken persists the bearer token in the OS keychain. Wrapped
// in a package variable so tests can stub out keychain access without shelling
// out to `security` (macOS) or `secret-tool` (Linux).
var gatewayAttachSetToken = config.SetTowerToken

// checkGatewayTowerCollision rejects gateway-mode attach when the local
// config already resolves the gateway tower's prefix or local alias. The
// spec is explicit: refuse and instruct the user to remove the existing
// tower first. There is no auto-conversion path.
//
// The check compares (case-insensitive, trimmed):
//   - gateway.Prefix vs every existing tower's HubPrefix
//   - gateway.Prefix vs every cfg.Instances entry's Prefix (or map key)
//   - localAlias vs every existing tower's Name
//
// On collision, returns an error whose message includes a copy-pasteable
// `spire tower remove <name>` command. Prefix collisions take precedence
// over name collisions in the message because that is the resolution path
// most likely to silently route mutations to the wrong tower (CWD →
// instance → tower); name collisions are rarer but equally fatal because
// SaveTowerConfig would overwrite the existing config file.
func checkGatewayTowerCollision(cfg *SpireConfig, gateway gatewayclient.TowerInfo, localAlias string) error {
	gwPrefix := normalizeForCollision(gateway.Prefix)
	gwAlias := normalizeForCollision(localAlias)

	// 1. Prefix collisions on existing tower configs. A same-prefix
	// direct/local tower will route CWD-resolved store dispatch to the
	// wrong place (see spi-43q7hp); refuse before any write.
	if gwPrefix != "" {
		towers, err := listTowerConfigs()
		if err != nil {
			return fmt.Errorf("list towers: %w", err)
		}
		for _, t := range towers {
			if normalizeForCollision(t.HubPrefix) == gwPrefix {
				return collisionError(t.Name, "prefix", t.HubPrefix)
			}
		}
	}

	// 2. Prefix collisions on existing instance entries. Instances are a
	// parallel source of CWD → tower resolution; a stale instance entry
	// can shadow a freshly-attached gateway tower even with no matching
	// tower config on disk.
	if gwPrefix != "" {
		for key, inst := range cfg.Instances {
			if inst == nil {
				continue
			}
			instPrefix := normalizeForCollision(inst.Prefix)
			if instPrefix == "" {
				instPrefix = normalizeForCollision(key)
			}
			if instPrefix == gwPrefix {
				name := inst.Tower
				if name == "" {
					name = key
				}
				return collisionError(name, "prefix", inst.Prefix)
			}
		}
	}

	// 3. Local alias / name collisions. SaveTowerConfig writes
	// ~/.config/spire/towers/<alias>.json, which would silently
	// overwrite an existing tower config of the same name.
	if gwAlias != "" {
		towers, err := listTowerConfigs()
		if err != nil {
			return fmt.Errorf("list towers: %w", err)
		}
		for _, t := range towers {
			if normalizeForCollision(t.Name) == gwAlias {
				return collisionError(t.Name, "name", t.Name)
			}
		}
	}

	return nil
}

// normalizeForCollision lower-cases and trims a config string for
// case-insensitive comparison. Aliases and prefixes are typically
// stored lowercase already, but operators occasionally type
// `--tower=Spi` and we should not let a single capital letter slip
// the collision check.
func normalizeForCollision(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// collisionError builds the user-facing error for a tower collision.
// Single-line so the suggested remove command is easy to copy-paste
// from a terminal.
func collisionError(existingName, kind, value string) error {
	return fmt.Errorf(
		"tower %q already uses %s %q. Remove it first: spire tower remove %s, then re-run spire tower attach-cluster.",
		existingName, kind, value, existingName,
	)
}

// cmdTowerAttachClusterGateway runs the gateway-mode flow of attach-cluster:
// verifies the remote tower identity, stores the bearer token in the OS
// keychain, and persists a gateway-mode tower config + instance entry so
// later CLI commands route bead/message ops through the gateway. Returns
// cleanly diagnosable errors on mismatch; callers should not retry with
// the same arguments after a name mismatch.
func cmdTowerAttachClusterGateway(url, token, towerName, localAlias string) error {
	if url == "" {
		return fmt.Errorf("--url is required for gateway attach")
	}
	if token == "" {
		return fmt.Errorf("--token is required for gateway attach")
	}
	if towerName == "" {
		return fmt.Errorf("--tower is required for gateway attach")
	}

	effectiveName := towerName
	if localAlias != "" {
		effectiveName = localAlias
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	info, err := gatewayAttachFetchTower(ctx, url, token)
	if err != nil {
		return fmt.Errorf("fetch tower from gateway at %s: %w", url, err)
	}

	if info.Name != towerName {
		return fmt.Errorf("tower name mismatch: --tower=%q but gateway reports %q", towerName, info.Name)
	}

	// Load the spire config once so the collision check and the later
	// instance write both see the same on-disk snapshot. Loading before
	// any persistence means a load failure cannot leave partial state.
	cfg, err := loadConfig()
	if err != nil {
		return fmt.Errorf("load spire config: %w", err)
	}
	if cfg.Instances == nil {
		cfg.Instances = make(map[string]*Instance)
	}

	// Refuse the attach if the local config already resolves to the
	// gateway tower's prefix or alias. Runs before any persistence so
	// no partial state (keychain entry, tower file, instance entry) can
	// leak when a collision is detected.
	if err := checkGatewayTowerCollision(cfg, info, effectiveName); err != nil {
		return err
	}

	tower := &config.TowerConfig{
		Name:      effectiveName,
		HubPrefix: info.Prefix,
		Archmage:  config.ArchmageConfig{Name: info.Archmage},
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
		Mode:      config.TowerModeGateway,
		URL:       url,
		TokenRef:  effectiveName,
	}

	// Write order: tower config → spire config (instance + active tower)
	// → keychain. Keychain is last so a config-save failure cannot leave
	// an orphan token entry. If keychain write fails, roll back the
	// tower-file and active-tower fields so the next attach starts clean.
	if err := config.SaveTowerConfig(tower); err != nil {
		return fmt.Errorf("save tower config: %w", err)
	}

	// Mirror Mode/URL/TokenRef onto an Instance entry keyed by tower prefix
	// so CWD-based tower resolution and any code path that reads the
	// parallel instance config stays coherent with the tower file.
	cfg.Instances[info.Prefix] = &Instance{
		Prefix:   info.Prefix,
		Tower:    effectiveName,
		Mode:     config.TowerModeGateway,
		URL:      url,
		TokenRef: effectiveName,
	}
	prevActive := cfg.ActiveTower
	cfg.ActiveTower = effectiveName
	if err := saveConfig(cfg); err != nil {
		// Tower file already written; roll it back so a failed attach
		// does not leave dangling tower config on disk.
		_ = config.DeleteTowerConfig(effectiveName)
		return fmt.Errorf("save spire config: %w", err)
	}

	if err := gatewayAttachSetToken(effectiveName, token); err != nil {
		// Roll back both tower file and instance/active-tower config so
		// no orphan keychain reference is left dangling. Best-effort: if
		// rollback itself fails we surface the original token error,
		// since that is the reason the attach is aborting.
		_ = config.DeleteTowerConfig(effectiveName)
		delete(cfg.Instances, info.Prefix)
		cfg.ActiveTower = prevActive
		_ = saveConfig(cfg)
		return fmt.Errorf("store bearer token in keychain: %w", err)
	}

	fmt.Printf("Tower attached via gateway: %s\n", tower.Name)
	fmt.Printf("  prefix:      %s\n", tower.HubPrefix)
	if info.Archmage != "" {
		fmt.Printf("  archmage:    %s\n", info.Archmage)
	}
	fmt.Printf("  url:         %s\n", tower.URL)
	fmt.Printf("  local alias: %s\n", effectiveName)
	return nil
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
