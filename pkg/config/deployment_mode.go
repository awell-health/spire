package config

import "fmt"

// DeploymentMode names the control-plane topology a tower targets. It is the
// single switch that decides whether scheduling, dispatch, and ownership run
// locally on the archmage's machine or against a remote cluster surface.
//
// Deployment mode is deliberately orthogonal to two neighboring concerns:
//
//   - Worker backend — how an individual agent process is launched. Controlled
//     by agent.backend (process / docker / k8s). A local-native tower can run
//     agents via docker; a cluster-native tower can still shell out to a
//     process-backed worker in tests. Mode does not imply backend.
//   - Sync transport — how dolt data moves between peers. Controlled by the
//     remote-kind / transport selection (syncer, remotesapi, DoltHub). Mode
//     does not imply transport; a local-native tower may sync over DoltHub,
//     and a cluster-native tower may sync over remotesapi.
//
// Callers that need to branch on topology MUST read DeploymentMode rather than
// inferring it from backend or transport.
type DeploymentMode string

// Deployment modes recognized by Spire. The zero value is intentionally
// invalid — use Default() to obtain the canonical default.
const (
	// DeploymentModeLocalNative runs the full control plane on the archmage's
	// machine: local scheduler, local dispatch, local worker launch. This is
	// the canonical single-host topology and the default for new towers.
	DeploymentModeLocalNative DeploymentMode = "local-native"

	// DeploymentModeClusterNative runs scheduling and dispatch inside a
	// Kubernetes cluster: the steward emits WorkloadIntent, the operator
	// reconciles it into pods, and workers execute in-cluster. The archmage's
	// machine is a client of the cluster control plane.
	DeploymentModeClusterNative DeploymentMode = "cluster-native"

	// DeploymentModeAttachedReserved is reserved for a future track in which a
	// local control plane drives a remote cluster execution surface through an
	// explicit attached seam. No execution path is implemented for this mode
	// today; selecting it is a declaration of intent, not a runnable
	// configuration. Consumers that observe this value MUST return a typed
	// "not implemented" error rather than silently falling back to another
	// mode.
	DeploymentModeAttachedReserved DeploymentMode = "attached-reserved"

	// DeploymentModeUnknown is the sentinel returned by
	// TowerConfig.EffectiveDeploymentMode when the tower has no
	// DeploymentMode set. Callers that switch on the deployment mode MUST
	// handle this case explicitly — typically by erroring with a clear
	// "tower X has no DeploymentMode set; configure it in
	// ~/.config/spire/towers/X.json" message — rather than letting a
	// missing mode silently dispatch through LocalNative machinery.
	// Filed under spi-eep81n; the recurring spi-od41sr-class regression
	// (in-memory TowerConfig{} bypassing LoadTowerConfig fell into the
	// LocalNative branch and SIGINT-killed every wizard in the local
	// registry) is the motivating incident.
	DeploymentModeUnknown DeploymentMode = "unknown"
)

// Default returns the canonical default deployment mode (local-native). This
// is the value used when a tower config does not specify deployment_mode, and
// it is the value new tower configs should be initialized with unless the
// archmage explicitly opts into another mode.
func Default() DeploymentMode {
	return DeploymentModeLocalNative
}

// Validate parses an arbitrary string into a DeploymentMode. It accepts the
// exact wire values ("local-native", "cluster-native", "attached-reserved")
// and rejects anything else — including the empty string — with a descriptive
// error. Callers that want to treat empty as "use the default" should check
// for "" themselves before calling Validate, or call Default() instead.
func Validate(s string) (DeploymentMode, error) {
	switch DeploymentMode(s) {
	case DeploymentModeLocalNative,
		DeploymentModeClusterNative,
		DeploymentModeAttachedReserved:
		return DeploymentMode(s), nil
	default:
		return "", fmt.Errorf("invalid deployment mode %q: must be one of %q, %q, %q",
			s,
			DeploymentModeLocalNative,
			DeploymentModeClusterNative,
			DeploymentModeAttachedReserved)
	}
}

// String returns the wire form of the mode. It lets DeploymentMode participate
// in fmt.Stringer-aware formatting without leaking the underlying string type.
func (m DeploymentMode) String() string {
	return string(m)
}
