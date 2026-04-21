package agent

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// K8sBackend implements Backend for Kubernetes pod execution.
// Each agent runs as a one-shot Pod with labels for discovery and
// secret references for credentials.
type K8sBackend struct {
	client        kubernetes.Interface
	namespace     string
	image         string // agent container image
	secretName    string // k8s Secret holding ANTHROPIC_API_KEY_DEFAULT / GITHUB_TOKEN
	database      string // dolt database name (tower name); wizard tower-attach --database
	prefix        string // bead prefix; wizard tower-attach --prefix
	dolthubRemote string // dolthub remote path; wizard tower-attach --dolthub-remote
}

// NewK8sBackend creates a K8sBackend using in-cluster config with
// kubeconfig fallback. Reads SPIRE_K8S_NAMESPACE (default: namespace
// from serviceaccount token), SPIRE_AGENT_IMAGE (required), and
// SPIRE_CREDENTIALS_SECRET (optional; falls back to "spire-credentials"
// for backward compat with installs that pre-date the helm chart's
// release-scoped secret naming). Wizard-pod attach-cluster flags are
// sourced from BEADS_DATABASE, BEADS_PREFIX, and DOLTHUB_REMOTE — the
// same envs the helm chart already injects into the steward so init
// containers across the cluster share one config source.
func NewK8sBackend() (*K8sBackend, error) {
	image := os.Getenv("SPIRE_AGENT_IMAGE")
	if image == "" {
		return nil, fmt.Errorf("SPIRE_AGENT_IMAGE env is required for k8s backend")
	}
	secretName := os.Getenv("SPIRE_CREDENTIALS_SECRET")
	if secretName == "" {
		secretName = "spire-credentials"
	}

	config, err := rest.InClusterConfig()
	if err != nil {
		// Fallback to kubeconfig for local development.
		rules := clientcmd.NewDefaultClientConfigLoadingRules()
		config, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
			rules, &clientcmd.ConfigOverrides{},
		).ClientConfig()
		if err != nil {
			return nil, fmt.Errorf("k8s config: %w", err)
		}
	}

	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("k8s client: %w", err)
	}

	ns := os.Getenv("SPIRE_K8S_NAMESPACE")
	if ns == "" {
		// Try to read the namespace from the serviceaccount mount.
		if data, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace"); err == nil {
			ns = strings.TrimSpace(string(data))
		}
	}
	if ns == "" {
		ns = "default"
	}

	return &K8sBackend{
		client:        client,
		namespace:     ns,
		image:         image,
		secretName:    secretName,
		database:      os.Getenv("BEADS_DATABASE"),
		prefix:        os.Getenv("BEADS_PREFIX"),
		dolthubRemote: os.Getenv("DOLTHUB_REMOTE"),
	}, nil
}

// NewK8sBackendFromClient creates a K8sBackend with an injected client.
// Used for testing with the k8s fake client. Wizard-pod attach-cluster
// flags read from BEADS_DATABASE/BEADS_PREFIX/DOLTHUB_REMOTE so tests
// can exercise the wizard branch with t.Setenv.
func NewK8sBackendFromClient(client kubernetes.Interface, namespace, image string) *K8sBackend {
	return &K8sBackend{
		client:        client,
		namespace:     namespace,
		image:         image,
		secretName:    "spire-credentials",
		database:      os.Getenv("BEADS_DATABASE"),
		prefix:        os.Getenv("BEADS_PREFIX"),
		dolthubRemote: os.Getenv("DOLTHUB_REMOTE"),
	}
}

// Spawn creates a one-shot k8s Pod for the given agent config.
//
// RoleWizard pods take the canonical wizard pod contract: a tower-attach
// init container stages .beads into /data/<db>/.beads, /data and /workspace
// emptyDir volumes are mounted on the main container, DOLT_DATA_DIR and
// SPIRE_CONFIG_DIR env vars are added so resolveBeadsDir() finds the staged
// store, and the main container uses WizardResources() — enough headroom
// to fan out apprentices.
//
// All other roles keep the flat executor pod shape that ships today
// (byte-for-byte identical to the pre-wizard-branch code path).
func (b *K8sBackend) Spawn(cfg SpawnConfig) (Handle, error) {
	subcmd, err := roleToSubcmd(cfg.Role)
	if err != nil {
		return nil, err
	}

	args := append([]string{}, subcmd...)
	args = append(args, cfg.BeadID, "--name", cfg.Name)
	args = append(args, cfg.ExtraArgs...)

	env := b.buildEnvVars(cfg)

	// If custom prompt is non-empty, pass as env var.
	if cfg.CustomPrompt != "" {
		env = append(env, corev1.EnvVar{
			Name:  "SPIRE_CUSTOM_PROMPT",
			Value: cfg.CustomPrompt,
		})
	}

	// Secret references. Key names match what `helm/spire/templates/secret.yaml`
	// writes (uppercase env-var-style, not lowercase kebab). GITHUB_TOKEN is
	// optional so installs without a github token (e.g. smoke tests that
	// don't push) don't block pod creation on a missing key.
	optional := true
	env = append(env,
		corev1.EnvVar{
			Name: "ANTHROPIC_API_KEY",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: b.secretName},
					Key:                  "ANTHROPIC_API_KEY_DEFAULT",
				},
			},
		},
		corev1.EnvVar{
			Name: "GITHUB_TOKEN",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: b.secretName},
					Key:                  "GITHUB_TOKEN",
					Optional:             &optional,
				},
			},
		},
	)

	// Sanitize the agent name for use as a pod name (must be DNS-compatible).
	podName := sanitizePodName(cfg.Name)

	var pod *corev1.Pod
	if cfg.Role == RoleWizard {
		pod = b.buildWizardPod(cfg, podName, args, env)
	} else {
		resources := resourcesForRole(cfg.Role)
		pod = &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      podName,
				Namespace: b.namespace,
				Labels: map[string]string{
					"spire.agent":      "true",   // fixed value for network policy selectors
					"spire.agent.name": cfg.Name, // actual agent name for lookups
					"spire.bead":       cfg.BeadID,
					"spire.role":       string(cfg.Role),
					"spire.tower":      cfg.Tower,
				},
			},
			Spec: corev1.PodSpec{
				RestartPolicy:     corev1.RestartPolicyNever,
				PriorityClassName: "spire-agent-default",
				Containers: []corev1.Container{
					{
						Name:      "agent",
						Image:     b.image,
						Command:   append([]string{"spire"}, args...),
						Env:       env,
						Resources: resources,
					},
				},
			},
		}
	}

	created, err := b.client.CoreV1().Pods(b.namespace).Create(
		context.Background(), pod, metav1.CreateOptions{},
	)
	if err != nil {
		return nil, fmt.Errorf("create pod %s: %w", podName, err)
	}

	return &K8sHandle{
		client:    b.client,
		namespace: b.namespace,
		podName:   created.Name,
		name:      cfg.Name,
	}, nil
}

// buildWizardPod produces the canonical wizard pod: a tower-attach init
// container that stages .beads and tower config onto emptyDir /data, a
// /workspace emptyDir for later apprentice bundle flows, matching volume
// mounts on the main container, DOLT_DATA_DIR and SPIRE_CONFIG_DIR env
// vars so resolveBeadsDir() finds the staged store, and WizardResources()
// for the main container.
//
// Database/prefix/dolthub-remote for the init container command come from
// the same env source the steward deployment already uses — see
// NewK8sBackend. Database falls back to cfg.Tower (tower name == database
// name, per pkg/tower/attach-cluster).
func (b *K8sBackend) buildWizardPod(cfg SpawnConfig, podName string, args []string, env []corev1.EnvVar) *corev1.Pod {
	db := b.database
	if db == "" {
		db = cfg.Tower
	}

	// Main container needs these two so resolveBeadsDir() finds the bead
	// store the init container stages at /data/<db>/.beads. Without them
	// `spire execute` dies with "no .beads directory found".
	env = append(env,
		corev1.EnvVar{Name: "DOLT_DATA_DIR", Value: "/data"},
		corev1.EnvVar{Name: "SPIRE_CONFIG_DIR", Value: "/data/spire-config"},
	)

	dataMount := corev1.VolumeMount{Name: "data", MountPath: "/data"}
	workspaceMount := corev1.VolumeMount{Name: "workspace", MountPath: "/workspace"}

	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: b.namespace,
			Labels: map[string]string{
				"spire.agent":      "true",   // fixed value for network policy selectors
				"spire.agent.name": cfg.Name, // actual agent name for lookups
				"spire.bead":       cfg.BeadID,
				"spire.role":       string(cfg.Role),
				"spire.tower":      cfg.Tower,
			},
		},
		Spec: corev1.PodSpec{
			RestartPolicy:     corev1.RestartPolicyNever,
			PriorityClassName: "spire-agent-default",
			Volumes: []corev1.Volume{
				{
					Name:         "data",
					VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
				},
				{
					Name:         "workspace",
					VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
				},
			},
			InitContainers: []corev1.Container{{
				Name:  "tower-attach",
				Image: b.image,
				Command: []string{
					"spire", "tower", "attach-cluster",
					"--data-dir=/data/" + db,
					"--database=" + db,
					"--prefix=" + b.prefix,
					"--dolthub-remote=" + b.dolthubRemote,
				},
				VolumeMounts: []corev1.VolumeMount{dataMount},
			}},
			Containers: []corev1.Container{
				{
					Name:         "agent",
					Image:        b.image,
					Command:      append([]string{"spire"}, args...),
					Env:          env,
					Resources:    WizardResources(),
					VolumeMounts: []corev1.VolumeMount{dataMount, workspaceMount},
				},
			},
		},
	}
}

// List returns Info for all Spire agent pods in the namespace.
func (b *K8sBackend) List() ([]Info, error) {
	pods, err := b.client.CoreV1().Pods(b.namespace).List(
		context.Background(),
		metav1.ListOptions{LabelSelector: "spire.agent=true"},
	)
	if err != nil {
		return nil, fmt.Errorf("list agent pods: %w", err)
	}

	infos := make([]Info, 0, len(pods.Items))
	for _, pod := range pods.Items {
		alive := pod.Status.Phase == corev1.PodRunning || pod.Status.Phase == corev1.PodPending
		infos = append(infos, Info{
			Name:       pod.Labels["spire.agent.name"],
			BeadID:     pod.Labels["spire.bead"],
			Phase:      pod.Labels["spire.role"],
			Alive:      alive,
			Identifier: pod.Name,
			StartedAt:  pod.CreationTimestamp.Time,
			Tower:      pod.Labels["spire.tower"],
		})
	}
	return infos, nil
}

// Logs returns a follow-stream of logs for the named agent's pod.
// Returns os.ErrNotExist if no pod is found.
func (b *K8sBackend) Logs(name string) (io.ReadCloser, error) {
	podName, err := b.findPod(name)
	if err != nil {
		return nil, err
	}

	follow := true
	stream, err := b.client.CoreV1().Pods(b.namespace).GetLogs(podName, &corev1.PodLogOptions{
		Follow: follow,
	}).Stream(context.Background())
	if err != nil {
		return nil, fmt.Errorf("get logs for pod %s: %w", podName, err)
	}

	return stream, nil
}

// Kill deletes the named agent's pod with a 10-second grace period.
func (b *K8sBackend) Kill(name string) error {
	podName, err := b.findPod(name)
	if err != nil {
		return err
	}

	grace := int64(10)
	return b.client.CoreV1().Pods(b.namespace).Delete(
		context.Background(), podName,
		metav1.DeleteOptions{GracePeriodSeconds: &grace},
	)
}

// findPod locates a pod by the spire.agent=<name> label.
// Returns os.ErrNotExist if not found.
func (b *K8sBackend) findPod(name string) (string, error) {
	pods, err := b.client.CoreV1().Pods(b.namespace).List(
		context.Background(),
		metav1.ListOptions{
			LabelSelector: fmt.Sprintf("spire.agent.name=%s", name),
		},
	)
	if err != nil {
		return "", fmt.Errorf("find pod for agent %s: %w", name, err)
	}
	if len(pods.Items) == 0 {
		return "", os.ErrNotExist
	}
	return pods.Items[0].Name, nil
}

// buildEnvVars constructs the standard environment variables for an agent pod,
// mirroring the process spawner's env setup.
func (b *K8sBackend) buildEnvVars(cfg SpawnConfig) []corev1.EnvVar {
	env := []corev1.EnvVar{
		{Name: "OTEL_EXPORTER_OTLP_ENDPOINT", Value: fmt.Sprintf("http://spire-steward.%s.svc:4317", b.namespace)},
		{Name: "CLAUDE_CODE_ENABLE_TELEMETRY", Value: "1"},
		{Name: "CLAUDE_CODE_ENHANCED_TELEMETRY_BETA", Value: "1"},
		{Name: "OTEL_TRACES_EXPORTER", Value: "otlp"},
		{Name: "OTEL_LOGS_EXPORTER", Value: "otlp"},
		{Name: "OTEL_EXPORTER_OTLP_PROTOCOL", Value: "grpc"},
		{Name: "BEADS_DOLT_SERVER_HOST", Value: fmt.Sprintf("spire-dolt.%s.svc", b.namespace)},
		{Name: "BEADS_DOLT_SERVER_PORT", Value: "3307"},
	}

	if cfg.Tower != "" {
		env = append(env, corev1.EnvVar{Name: "SPIRE_TOWER", Value: cfg.Tower})
	}
	if cfg.Provider != "" {
		env = append(env, corev1.EnvVar{Name: "SPIRE_PROVIDER", Value: cfg.Provider})
	}
	if cfg.Role != "" {
		env = append(env, corev1.EnvVar{Name: "SPIRE_ROLE", Value: string(cfg.Role)})
	}

	// Apprentice identity env vars. Transport-agnostic: the apprentice reads
	// them to resolve which bead to write to and what role to claim at
	// submit time.
	if cfg.BeadID != "" {
		env = append(env, corev1.EnvVar{Name: "SPIRE_BEAD_ID", Value: cfg.BeadID})
	}
	if cfg.AttemptID != "" {
		env = append(env, corev1.EnvVar{Name: "SPIRE_ATTEMPT_ID", Value: cfg.AttemptID})
	}
	if cfg.ApprenticeIdx != "" {
		env = append(env, corev1.EnvVar{Name: "SPIRE_APPRENTICE_IDX", Value: cfg.ApprenticeIdx})
	}

	// OTEL resource attributes for trace/log correlation.
	var resAttrs []string
	if cfg.BeadID != "" {
		resAttrs = append(resAttrs, "bead.id="+cfg.BeadID)
	}
	if cfg.Name != "" {
		resAttrs = append(resAttrs, "agent.name="+cfg.Name)
	}
	if cfg.Step != "" {
		resAttrs = append(resAttrs, "step="+cfg.Step)
	}
	if cfg.Tower != "" {
		resAttrs = append(resAttrs, "tower="+cfg.Tower)
	}
	if len(resAttrs) > 0 {
		env = append(env, corev1.EnvVar{
			Name:  "OTEL_RESOURCE_ATTRIBUTES",
			Value: strings.Join(resAttrs, ","),
		})
	}

	return env
}

// resourcesForRole returns CPU/memory requests and limits based on agent role.
func resourcesForRole(role SpawnRole) corev1.ResourceRequirements {
	switch role {
	case RoleApprentice:
		return corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceMemory: resource.MustParse("1Gi"),
				corev1.ResourceCPU:    resource.MustParse("500m"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceMemory: resource.MustParse("4Gi"),
				corev1.ResourceCPU:    resource.MustParse("2000m"),
			},
		}
	case RoleSage:
		return corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceMemory: resource.MustParse("256Mi"),
				corev1.ResourceCPU:    resource.MustParse("250m"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceMemory: resource.MustParse("1Gi"),
				corev1.ResourceCPU:    resource.MustParse("1000m"),
			},
		}
	case RoleWizard, RoleExecutor:
		return corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceMemory: resource.MustParse("256Mi"),
				corev1.ResourceCPU:    resource.MustParse("250m"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceMemory: resource.MustParse("512Mi"),
				corev1.ResourceCPU:    resource.MustParse("500m"),
			},
		}
	default:
		// Fallback to wizard-tier resources.
		return corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceMemory: resource.MustParse("256Mi"),
				corev1.ResourceCPU:    resource.MustParse("250m"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceMemory: resource.MustParse("512Mi"),
				corev1.ResourceCPU:    resource.MustParse("500m"),
			},
		}
	}
}

// sanitizePodName converts an agent name to a valid k8s pod name.
// Pod names must be lowercase, alphanumeric, or '-', max 253 chars.
func sanitizePodName(name string) string {
	name = strings.ToLower(name)
	name = strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			return r
		}
		return '-'
	}, name)
	// Trim leading/trailing dashes.
	name = strings.Trim(name, "-")
	if len(name) > 253 {
		name = name[:253]
	}
	if name == "" {
		name = "spire-agent"
	}

	// Add a timestamp suffix to avoid name collisions.
	suffix := fmt.Sprintf("-%d", time.Now().UnixMilli()%100000)
	if len(name)+len(suffix) > 253 {
		name = name[:253-len(suffix)]
	}
	return name + suffix
}
