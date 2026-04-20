package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SpireAgent defines an agent that can execute work.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
type SpireAgent struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SpireAgentSpec   `json:"spec,omitempty"`
	Status SpireAgentStatus `json:"status,omitempty"`
}

type SpireAgentSpec struct {
	DisplayName   string   `json:"displayName,omitempty"`
	Mode          string   `json:"mode"` // "external" or "managed"
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
	Image      string                `json:"image,omitempty"`
	Repo       string                `json:"repo,omitempty"`
	RepoBranch string                `json:"repoBranch,omitempty"`
	Resources  *AgentResourceRequirements `json:"resources,omitempty"`
}

type AgentResourceRequirements struct {
	Requests map[string]string `json:"requests,omitempty"`
	Limits   map[string]string `json:"limits,omitempty"`
}

type SpireAgentStatus struct {
	Phase          string   `json:"phase,omitempty"` // Idle, Working, Stale, Offline, Provisioning
	Registered     bool     `json:"registered,omitempty"`
	LastSeen       string   `json:"lastSeen,omitempty"`
	CurrentWork    []string `json:"currentWork,omitempty"`
	CompletedCount int      `json:"completedCount,omitempty"`
	PodName        string   `json:"podName,omitempty"`
	Message        string   `json:"message,omitempty"`
}

// +kubebuilder:object:root=true
type SpireAgentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SpireAgent `json:"items"`
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
