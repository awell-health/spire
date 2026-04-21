package controllers

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	spirev1 "github.com/awell-health/spire/operator/api/v1alpha1"
)

// githubAPIBase is overridable in tests.
var githubAPIBase = "https://api.github.com"

// httpClient is overridable in tests.
var httpClient = &http.Client{Timeout: 10 * time.Second}

// parseGitHubOwnerRepo extracts (owner, repo) from a GitHub repository URL.
// Accepts https://github.com/owner/repo(.git)? and git@github.com:owner/repo(.git)?.
// Returns ok=false for non-GitHub or unparseable URLs.
func parseGitHubOwnerRepo(url string) (owner, repo string, ok bool) {
	s := strings.TrimSpace(url)
	if s == "" {
		return "", "", false
	}

	var path string
	switch {
	case strings.HasPrefix(s, "git@github.com:"):
		path = strings.TrimPrefix(s, "git@github.com:")
	case strings.HasPrefix(s, "ssh://git@github.com/"):
		path = strings.TrimPrefix(s, "ssh://git@github.com/")
	case strings.HasPrefix(s, "https://github.com/"):
		path = strings.TrimPrefix(s, "https://github.com/")
	case strings.HasPrefix(s, "http://github.com/"):
		path = strings.TrimPrefix(s, "http://github.com/")
	default:
		return "", "", false
	}

	path = strings.TrimSuffix(path, ".git")
	path = strings.TrimSuffix(path, "/")
	parts := strings.Split(path, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// loadGitHubToken returns the GITHUB_TOKEN from the DoltHub credentials secret,
// or "" if unconfigured / unreadable. Errors are logged at debug level only —
// missing tokens are an expected configuration state, not a hard failure.
func (m *AgentMonitor) loadGitHubToken(ctx context.Context, cfg *spirev1.SpireConfig) string {
	if cfg == nil || cfg.Spec.DoltHub.CredentialsSecret == "" {
		return ""
	}
	var secret corev1.Secret
	key := client.ObjectKey{Namespace: m.Namespace, Name: cfg.Spec.DoltHub.CredentialsSecret}
	if err := m.Client.Get(ctx, key, &secret); err != nil {
		if !errors.IsNotFound(err) {
			m.Log.V(1).Info("failed to read DoltHub credentials secret for GitHub token",
				"secret", cfg.Spec.DoltHub.CredentialsSecret, "err", err, "tower", m.Database, "prefix", m.Prefix, "backend", "operator-k8s")
		}
		return ""
	}
	if v, ok := secret.Data["GITHUB_TOKEN"]; ok {
		return strings.TrimSpace(string(v))
	}
	return ""
}

// deleteRemoteFeatBranch best-effort deletes origin/feat/<beadID> via the GitHub
// REST API. No-op when the token or repo info is missing or the remote isn't
// GitHub. Treats 204/404/422 as success (branch is gone or never existed).
// Returns an error only on transport failures or unexpected HTTP statuses.
func (m *AgentMonitor) deleteRemoteFeatBranch(ctx context.Context, agent *spirev1.WizardGuild, beadID string, cfg *spirev1.SpireConfig) error {
	if agent == nil || beadID == "" {
		return nil
	}
	owner, repo, ok := parseGitHubOwnerRepo(agent.Spec.Repo)
	if !ok {
		return nil
	}
	token := m.loadGitHubToken(ctx, cfg)
	if token == "" {
		return nil
	}

	url := fmt.Sprintf("%s/repos/%s/%s/git/refs/heads/feat/%s", githubAPIBase, owner, repo, beadID)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
	if err != nil {
		return fmt.Errorf("build delete request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("delete remote branch: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	switch resp.StatusCode {
	case http.StatusNoContent, http.StatusNotFound, http.StatusUnprocessableEntity:
		// 204: deleted. 404: ref already gone. 422: ref never existed.
		return nil
	default:
		return fmt.Errorf("github DELETE returned status %d", resp.StatusCode)
	}
}
