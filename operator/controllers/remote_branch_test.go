package controllers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-logr/logr/testr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	spirev1 "github.com/awell-health/spire/operator/api/v1alpha1"
)

func TestParseGitHubOwnerRepo(t *testing.T) {
	cases := []struct {
		name      string
		url       string
		wantOwner string
		wantRepo  string
		wantOK    bool
	}{
		{name: "https with .git", url: "https://github.com/awell-health/spire.git", wantOwner: "awell-health", wantRepo: "spire", wantOK: true},
		{name: "https without .git", url: "https://github.com/awell-health/spire", wantOwner: "awell-health", wantRepo: "spire", wantOK: true},
		{name: "https trailing slash", url: "https://github.com/awell-health/spire/", wantOwner: "awell-health", wantRepo: "spire", wantOK: true},
		{name: "ssh shorthand", url: "git@github.com:awell-health/spire.git", wantOwner: "awell-health", wantRepo: "spire", wantOK: true},
		{name: "ssh shorthand no .git", url: "git@github.com:awell-health/spire", wantOwner: "awell-health", wantRepo: "spire", wantOK: true},
		{name: "ssh url scheme", url: "ssh://git@github.com/awell-health/spire.git", wantOwner: "awell-health", wantRepo: "spire", wantOK: true},
		{name: "http", url: "http://github.com/awell-health/spire.git", wantOwner: "awell-health", wantRepo: "spire", wantOK: true},
		{name: "with whitespace", url: "  https://github.com/awell-health/spire  ", wantOwner: "awell-health", wantRepo: "spire", wantOK: true},
		{name: "gitlab", url: "https://gitlab.com/awell-health/spire.git", wantOK: false},
		{name: "bitbucket", url: "https://bitbucket.org/awell-health/spire.git", wantOK: false},
		{name: "empty", url: "", wantOK: false},
		{name: "missing repo", url: "https://github.com/awell-health", wantOK: false},
		{name: "extra path", url: "https://github.com/awell-health/spire/tree/main", wantOK: false},
		{name: "missing owner", url: "https://github.com//spire", wantOK: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotOwner, gotRepo, gotOK := parseGitHubOwnerRepo(tc.url)
			if gotOK != tc.wantOK {
				t.Fatalf("ok=%v want %v", gotOK, tc.wantOK)
			}
			if !tc.wantOK {
				return
			}
			if gotOwner != tc.wantOwner || gotRepo != tc.wantRepo {
				t.Fatalf("got (%q, %q) want (%q, %q)", gotOwner, gotRepo, tc.wantOwner, tc.wantRepo)
			}
		})
	}
}

func TestDeleteRemoteFeatBranch(t *testing.T) {
	ns := "spire"
	const wantPath = "/repos/awell-health/spire/git/refs/heads/feat/spi-3x7h7"

	cases := []struct {
		name        string
		status      int
		token       string
		repo        string
		wantErr     bool
		wantHit     bool
	}{
		{name: "204 deleted", status: http.StatusNoContent, token: "ghp_x", repo: "https://github.com/awell-health/spire.git", wantErr: false, wantHit: true},
		{name: "404 already gone", status: http.StatusNotFound, token: "ghp_x", repo: "https://github.com/awell-health/spire.git", wantErr: false, wantHit: true},
		{name: "422 never existed", status: http.StatusUnprocessableEntity, token: "ghp_x", repo: "git@github.com:awell-health/spire.git", wantErr: false, wantHit: true},
		{name: "500 unexpected", status: http.StatusInternalServerError, token: "ghp_x", repo: "https://github.com/awell-health/spire", wantErr: true, wantHit: true},
		{name: "no token", status: http.StatusNoContent, token: "", repo: "https://github.com/awell-health/spire", wantErr: false, wantHit: false},
		{name: "non-github repo", status: http.StatusNoContent, token: "ghp_x", repo: "https://gitlab.com/awell-health/spire", wantErr: false, wantHit: false},
		{name: "empty repo", status: http.StatusNoContent, token: "ghp_x", repo: "", wantErr: false, wantHit: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var hit bool
			var gotPath, gotMethod, gotAuth string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hit = true
				gotPath = r.URL.Path
				gotMethod = r.Method
				gotAuth = r.Header.Get("Authorization")
				w.WriteHeader(tc.status)
			}))
			defer srv.Close()

			origBase := githubAPIBase
			githubAPIBase = srv.URL
			defer func() { githubAPIBase = origBase }()

			objs := []client.Object{}
			cfg := &spirev1.SpireConfig{
				ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: ns},
				Spec:       spirev1.SpireConfigSpec{DoltHub: spirev1.DoltHubConfig{CredentialsSecret: "dolthub-creds"}},
			}
			objs = append(objs, cfg)
			if tc.token != "" {
				objs = append(objs, &corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{Name: "dolthub-creds", Namespace: ns},
					Data:       map[string][]byte{"GITHUB_TOKEN": []byte(tc.token)},
				})
			}

			c := fake.NewClientBuilder().
				WithScheme(newTestScheme(t)).
				WithObjects(objs...).
				Build()

			m := &AgentMonitor{Client: c, Log: testr.New(t), Namespace: ns}
			agent := &spirev1.SpireAgent{
				ObjectMeta: metav1.ObjectMeta{Name: "test-agent", Namespace: ns},
				Spec:       spirev1.SpireAgentSpec{Repo: tc.repo},
			}

			err := m.deleteRemoteFeatBranch(context.Background(), agent, "spi-3x7h7", cfg)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, tc.wantErr)
			}
			if hit != tc.wantHit {
				t.Fatalf("server hit=%v want %v", hit, tc.wantHit)
			}
			if !tc.wantHit {
				return
			}
			if gotMethod != http.MethodDelete {
				t.Fatalf("method=%q want DELETE", gotMethod)
			}
			if gotPath != wantPath {
				t.Fatalf("path=%q want %q", gotPath, wantPath)
			}
			if gotAuth != "Bearer "+tc.token {
				t.Fatalf("auth=%q want Bearer %s", gotAuth, tc.token)
			}
		})
	}
}

func TestDeleteRemoteFeatBranch_NoConfig(t *testing.T) {
	ns := "spire"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("server should not be hit when cfg is nil")
	}))
	defer srv.Close()

	origBase := githubAPIBase
	githubAPIBase = srv.URL
	defer func() { githubAPIBase = origBase }()

	c := fake.NewClientBuilder().WithScheme(newTestScheme(t)).Build()
	m := &AgentMonitor{Client: c, Log: testr.New(t), Namespace: ns}
	agent := &spirev1.SpireAgent{
		ObjectMeta: metav1.ObjectMeta{Name: "test-agent", Namespace: ns},
		Spec:       spirev1.SpireAgentSpec{Repo: "https://github.com/awell-health/spire"},
	}

	if err := m.deleteRemoteFeatBranch(context.Background(), agent, "spi-3x7h7", nil); err != nil {
		t.Fatalf("expected nil err for nil cfg, got %v", err)
	}
}
