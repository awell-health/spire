package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// WizardGuild defines a guild of wizards (agents) that can execute work.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=wizardguilds,singular=wizardguild,shortName=wg;guild
type WizardGuild struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   WizardGuildSpec   `json:"spec,omitempty"`
	Status WizardGuildStatus `json:"status,omitempty"`
}

type WizardGuildSpec struct {
	DisplayName string `json:"displayName,omitempty"`
	Mode        string `json:"mode"` // "external" or "managed"
	// Capabilities is reserved for future use (likely tool/skill provisioning).
	// No code path consumes it today — values set here are a no-op.
	Capabilities  []string `json:"capabilities,omitempty"`
	Prefixes      []string `json:"prefixes,omitempty"`
	Token         string   `json:"token,omitempty"`
	MaxConcurrent int      `json:"maxConcurrent,omitempty"`
	// MaxApprentices caps the number of concurrent apprentice subprocesses
	// that a single wizard pod will spawn during wave dispatch. Pointer so
	// unset (nil) is distinguishable from zero: when nil, the operator will
	// not inject SPIRE_MAX_APPRENTICES and the wizard falls back to the
	// spire.yaml value (or the built-in default of 3).
	//
	// Intended to migrate to a future WizardGuild CRD; landed here per the
	// forward-compat note in operator/controllers/agent_monitor.go.
	MaxApprentices *int `json:"maxApprentices,omitempty"`

	// Managed mode fields
	Image      string                     `json:"image,omitempty"`
	Repo       string                     `json:"repo,omitempty"`
	RepoBranch string                     `json:"repoBranch,omitempty"`
	Resources  *GuildResourceRequirements `json:"resources,omitempty"`

	// SharedWorkspace opts the guild in to the borrowed-worktree k8s
	// spawn path by setting SPIRE_K8S_SHARED_WORKSPACE=1 on the wizard
	// pod. When true, child apprentice/sage pods spawned by the wizard
	// mount the parent wizard's PVC (labeled
	// `spire.io/owning-wizard-pod=<name>`) at /workspace; when false
	// (default), /workspace is backed by an emptyDir and borrowed
	// worktrees are not supported on the k8s backend.
	//
	// IMPORTANT: setting this to true WITHOUT production PVC
	// provisioning will cause child pod spawns to fail with
	// ErrSharedWorkspacePVCNotFound. The operator does not create the
	// PVC today — see spi-cslm8 for the bug and the follow-up task for
	// PVC provisioning. Until that lands, leave this unset (default
	// false) to keep the operator's behavior correct.
	//
	// Pointer so unset (nil) is distinguishable from explicit false,
	// matching the MaxApprentices precedence pattern above.
	SharedWorkspace *bool `json:"sharedWorkspace,omitempty"`

	// Cache declares a guild-owned repo cache that wizard pods derive
	// their read-only repo substrate from, instead of each pod cloning
	// from origin. The operator reconciles this into a PVC plus a
	// refresh Job (see spi-myzn5). Repo identity is NOT declared here —
	// it stays authoritative via tower/shared registration (spi-xplwy).
	//
	// Pointer so unset (nil) keeps the pre-cache behavior: no PVC
	// provisioned, and wizard pods bootstrap without the cache mount.
	Cache *CacheSpec `json:"cache,omitempty"`
}

// CacheSpec declares the storage and refresh contract for a
// WizardGuild's repo cache. It intentionally contains no repo URL —
// repo identity stays authoritative via tower/shared registration
// (see spi-xplwy). The operator resolves the repo to clone from the
// guild's existing configuration at reconcile time.
type CacheSpec struct {
	// StorageClassName names the StorageClass used for the cache PVC.
	// When empty, the operator falls back to the cluster default.
	StorageClassName string `json:"storageClassName,omitempty"`

	// Size is the requested capacity for the cache PVC as a
	// resource.Quantity (e.g. "10Gi").
	Size resource.Quantity `json:"size"`

	// AccessMode is the PVC access mode. Defaults to ReadOnlyMany so
	// many wizard pods can mount the same cache in parallel.
	// +kubebuilder:default=ReadOnlyMany
	AccessMode corev1.PersistentVolumeAccessMode `json:"accessMode,omitempty"`

	// RefreshInterval is how often the operator schedules a fetch
	// against the cache. Defaults to 5m.
	// +kubebuilder:default="5m"
	RefreshInterval metav1.Duration `json:"refreshInterval,omitempty"`

	// BranchPin, when set, constrains the cache to a specific git
	// branch. When nil, the cache tracks the guild's default branch.
	BranchPin *string `json:"branchPin,omitempty"`
}

type GuildResourceRequirements struct {
	Requests map[string]string `json:"requests,omitempty"`
	Limits   map[string]string `json:"limits,omitempty"`
}

type WizardGuildStatus struct {
	Phase          string   `json:"phase,omitempty"` // Idle, Working, Stale, Offline, Provisioning
	Registered     bool     `json:"registered,omitempty"`
	LastSeen       string   `json:"lastSeen,omitempty"`
	CurrentWork    []string `json:"currentWork,omitempty"`
	CompletedCount int      `json:"completedCount,omitempty"`
	PodName        string   `json:"podName,omitempty"`
	Message        string   `json:"message,omitempty"`

	// Cache reports the lifecycle state of the guild-owned repo cache
	// when Spec.Cache is set. Nil when no cache is declared.
	Cache *CacheStatus `json:"cache,omitempty"`
}

// CacheStatus reports the observed state of a WizardGuild's repo
// cache. It is set and maintained by the cache reconciler.
type CacheStatus struct {
	// Phase is one of Pending, Ready, Refreshing, Failed.
	// +kubebuilder:validation:Enum=Pending;Ready;Refreshing;Failed
	Phase string `json:"phase,omitempty"`

	// Revision is the git commit SHA the cache currently points at.
	// Empty until the first successful refresh completes.
	Revision string `json:"revision,omitempty"`

	// LastRefreshTime is when the cache was most recently refreshed.
	LastRefreshTime *metav1.Time `json:"lastRefreshTime,omitempty"`

	// RefreshError carries a human-readable message describing the
	// most recent refresh failure, if any. Cleared on the next
	// successful refresh.
	RefreshError string `json:"refreshError,omitempty"`
}

// Cache-related condition types used on WizardGuild.Status.Conditions
// (once conditions are wired). The set deliberately mirrors the
// CacheStatus.Phase values that represent durable states — an
// intermittent "Refreshing" is distinct from the terminal "Failed".
const (
	// CacheReady is True when the cache exists and has been refreshed
	// successfully at least once. A wizard pod can safely bootstrap
	// from the cache when this condition is True.
	CacheReady = "CacheReady"

	// CacheRefreshing is True while a refresh Job is in-flight. This
	// is informational — workers do not block on it, because the
	// reconciler serializes refresh so the cache never points at a
	// half-written snapshot.
	CacheRefreshing = "CacheRefreshing"

	// CacheFailed is True when the most recent refresh failed and no
	// newer successful refresh has taken its place. The Message on
	// this condition should carry the same detail as
	// CacheStatus.RefreshError.
	CacheFailed = "CacheFailed"
)

// +kubebuilder:object:root=true
type WizardGuildList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []WizardGuild `json:"items"`
}

// SpireWorkload represents a bead assignment.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
type SpireWorkload struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SpireWorkloadSpec   `json:"spec,omitempty"`
	Status SpireWorkloadStatus `json:"status,omitempty"`
}

type SpireWorkloadSpec struct {
	BeadID   string   `json:"beadId"`
	Title    string   `json:"title,omitempty"`
	Priority int      `json:"priority,omitempty"`
	Type     string   `json:"type,omitempty"`
	Prefixes []string `json:"prefixes,omitempty"`
	Token    string   `json:"token,omitempty"`
}

type SpireWorkloadStatus struct {
	Phase       string `json:"phase,omitempty"` // Pending, Assigned, InProgress, Done, Stale, Failed
	AssignedTo  string `json:"assignedTo,omitempty"`
	AssignedAt  string `json:"assignedAt,omitempty"`
	StartedAt   string `json:"startedAt,omitempty"`
	CompletedAt string `json:"completedAt,omitempty"`
	LastProgress string `json:"lastProgress,omitempty"`
	Attempts    int    `json:"attempts,omitempty"`
	Message     string `json:"message,omitempty"`
}

// +kubebuilder:object:root=true
type SpireWorkloadList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SpireWorkload `json:"items"`
}

// SpireConfig is the cluster-wide configuration singleton.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
type SpireConfig struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SpireConfigSpec   `json:"spec,omitempty"`
	Status SpireConfigStatus `json:"status,omitempty"`
}

type SpireConfigSpec struct {
	DoltHub      DoltHubConfig       `json:"dolthub"`
	Polling      PollingConfig       `json:"polling,omitempty"`
	Tokens       map[string]TokenRef `json:"tokens,omitempty"`
	Routing      []RoutingRule       `json:"routing,omitempty"`
	DefaultToken string              `json:"defaultToken,omitempty"`
}

type DoltHubConfig struct {
	Remote            string `json:"remote"`
	CredentialsSecret string `json:"credentialsSecret"`
}

type PollingConfig struct {
	Interval           string `json:"interval,omitempty"`
	StaleThreshold     string `json:"staleThreshold,omitempty"`
	ReassignThreshold  string `json:"reassignThreshold,omitempty"`
}

type TokenRef struct {
	Secret string `json:"secret"`
	Key    string `json:"key"`
}

type RoutingRule struct {
	Match map[string]string `json:"match,omitempty"`
	Token string            `json:"token"`
}

type SpireConfigStatus struct {
	LastSync      string `json:"lastSync,omitempty"`
	BeadCount     int    `json:"beadCount,omitempty"`
	AgentCount    int    `json:"agentCount,omitempty"`
	WorkloadCount int    `json:"workloadCount,omitempty"`
	Message       string `json:"message,omitempty"`
}

// +kubebuilder:object:root=true
type SpireConfigList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SpireConfig `json:"items"`
}
