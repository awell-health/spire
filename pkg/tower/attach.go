package tower

import (
	"fmt"

	"github.com/awell-health/spire/pkg/config"
)

// ClusterAttachment re-exports config.ClusterAttachment so callers in the
// tower package can reference the type without importing pkg/config.
type ClusterAttachment = config.ClusterAttachment

// AttachOptions carries the fields a caller has parsed from flags/env. No
// ambient state — parents construct this explicitly so both the CLI and a
// future Kubernetes post-install Job can call AttachCluster the same way.
type AttachOptions struct {
	Tower      string // when non-empty, target this tower; otherwise use active tower
	Namespace  string
	Kubeconfig string
	Context    string
	InCluster  bool
}

// AttachCluster records a ClusterAttachment on a tower config. When
// opts.Tower is empty the active tower (per config.ActiveTowerConfig) is
// used. Existing entries with the same namespace are replaced so the call is
// idempotent — re-running the Helm post-install Job does not duplicate rows.
func AttachCluster(opts AttachOptions) error {
	if opts.Namespace == "" {
		return fmt.Errorf("namespace is required")
	}
	if opts.InCluster && (opts.Kubeconfig != "" || opts.Context != "") {
		return fmt.Errorf("--in-cluster cannot be combined with --kubeconfig or --context")
	}

	var tc *config.TowerConfig
	var err error
	if opts.Tower != "" {
		tc, err = config.LoadTowerConfig(opts.Tower)
	} else {
		tc, err = config.ActiveTowerConfig()
	}
	if err != nil {
		return fmt.Errorf("resolve tower: %w", err)
	}

	att := ClusterAttachment{
		Namespace:  opts.Namespace,
		Kubeconfig: opts.Kubeconfig,
		Context:    opts.Context,
		InCluster:  opts.InCluster,
	}

	replaced := false
	for i, c := range tc.Clusters {
		if c.Namespace == opts.Namespace {
			tc.Clusters[i] = att
			replaced = true
			break
		}
	}
	if !replaced {
		tc.Clusters = append(tc.Clusters, att)
	}

	if err := config.SaveTowerConfig(tc); err != nil {
		return fmt.Errorf("save tower config: %w", err)
	}
	return nil
}
