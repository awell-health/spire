package v1alpha1

import (
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
}

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
