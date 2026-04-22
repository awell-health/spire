package controllers

// CR / resolver identity parity tests (spi-pg632).
//
// These tests pin the spi-njzmg identity invariant: in cluster-native
// mode, cluster repo identity MUST resolve via
// pkg/steward/identity.ClusterIdentityResolver. When a CR (WizardGuild
// or the WorkloadIntent projection) carries URL/BaseBranch/Prefix
// fields, they are treated as projection-only — the resolver's output
// wins, and any divergence is surfaced as a drift log line (or a
// typed error, per the wave-1 design).
//
// The tests cover both operator paths that touch identity:
//
//   - AgentMonitor.buildWorkloadPod (legacy wizard pod shape used by
//     managed guilds). Wires a ClusterIdentityResolver and asserts
//     parity → silent, drift → logged + resolver wins.
//
//   - IntentWorkloadReconciler.canonicalIdentity (canonical apprentice
//     pod shape). Same contract, different code path.
//
// Together they enforce that no scheduling decision is ever made from
// CR-only fields alone.

import (
	"context"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"

	"github.com/awell-health/spire/pkg/steward/identity"
	"github.com/awell-health/spire/pkg/steward/intent"
)

// TestClusterIdentityParity_AgentMonitor_CRMatchesResolver is the
// parity path: the CR advertises the same repo identity that shared
// repo registration (the resolver) would return. The reconciler must
// use the resolver's output and emit NO drift log line — parity is
// silent.
func TestClusterIdentityParity_AgentMonitor_CRMatchesResolver(t *testing.T) {
	const (
		ns         = "spire"
		prefix     = "spi"
		repoURL    = "git@example.com:spire-test/repo.git"
		baseBranch = "main"
	)

	// CR carries the same values the resolver will return — this is
	// the canonical state for a correctly-registered cluster install.
	guild := makeAgent("core", ns, nil)
	guild.Spec.Repo = repoURL
	guild.Spec.RepoBranch = baseBranch
	guild.Spec.Prefixes = []string{prefix}

	sink := &capturingSink{}
	resolver := &fakeIdentityResolver{out: identity.ClusterRepoIdentity{
		URL:        repoURL,
		BaseBranch: baseBranch,
		Prefix:     prefix,
	}}

	m := &AgentMonitor{
		Log:          logr.New(sink),
		Namespace:    ns,
		StewardImage: "spire-agent:dev",
		Database:     "spire",
		Prefix:       prefix,
		Resolver:     resolver,
	}

	pod := m.buildWorkloadPod(guild, "spi-abc", nil)
	if pod == nil {
		t.Fatalf("buildWorkloadPod returned nil; resolver parity path should build a pod")
	}

	// Resolver output must flow into the pod. SPIRE_REPO_URL and
	// SPIRE_REPO_BRANCH are the authoritative env that the
	// repo-bootstrap init container clones from.
	main := pod.Spec.Containers[0]
	env := envMap(main.Env)
	if got := envValue(env, "SPIRE_REPO_URL"); got != repoURL {
		t.Errorf("SPIRE_REPO_URL = %q, want %q (resolver URL)", got, repoURL)
	}
	if got := envValue(env, "SPIRE_REPO_BRANCH"); got != baseBranch {
		t.Errorf("SPIRE_REPO_BRANCH = %q, want %q (resolver branch)", got, baseBranch)
	}
	if got := envValue(env, "SPIRE_REPO_PREFIX"); got != prefix {
		t.Errorf("SPIRE_REPO_PREFIX = %q, want %q", got, prefix)
	}

	// Parity is silent: no drift log line should be emitted when CR
	// and resolver agree. Any other Info lines (cycle start / builder
	// logs) are fine — we only forbid the drift-specific markers.
	if sink.hasInfoContaining("drifts from canonical resolver") {
		t.Errorf("parity case must not emit drift log; got messages: %v", sink.infoMessages())
	}
}

// TestClusterIdentityParity_AgentMonitor_CRDriftsFromResolver is the
// drift path: the CR advertises URL/branch that diverge from shared
// repo registration. The reconciler must log drift AND use the
// resolver's value in the built pod — the CR never wins scheduling
// decisions, even when it holds "newer-looking" fields.
func TestClusterIdentityParity_AgentMonitor_CRDriftsFromResolver(t *testing.T) {
	const (
		ns             = "spire"
		prefix         = "spi"
		crRepoURL      = "git@example.com:stale/repo.git"
		crBranch       = "old-main"
		canonicalURL   = "git@example.com:canonical/repo.git"
		canonicalBase  = "main"
	)

	guild := makeAgent("core", ns, nil)
	// CR fields are divergent from the resolver's canonical output.
	// This simulates a CR that was apply'd with fields that race or
	// contradict shared repo registration.
	guild.Spec.Repo = crRepoURL
	guild.Spec.RepoBranch = crBranch
	guild.Spec.Prefixes = []string{prefix}

	sink := &capturingSink{}
	resolver := &fakeIdentityResolver{out: identity.ClusterRepoIdentity{
		URL:        canonicalURL,
		BaseBranch: canonicalBase,
		Prefix:     prefix,
	}}

	m := &AgentMonitor{
		Log:          logr.New(sink),
		Namespace:    ns,
		StewardImage: "spire-agent:dev",
		Database:     "spire",
		Prefix:       prefix,
		Resolver:     resolver,
	}

	pod := m.buildWorkloadPod(guild, "spi-abc", nil)
	if pod == nil {
		t.Fatalf("buildWorkloadPod returned nil under drift; reconciler should still produce a pod using resolver output")
	}

	// Resolver wins: the pod must carry canonical URL/branch values,
	// not the CR's divergent projection.
	main := pod.Spec.Containers[0]
	env := envMap(main.Env)
	if got := envValue(env, "SPIRE_REPO_URL"); got != canonicalURL {
		t.Errorf("SPIRE_REPO_URL = %q, want canonical %q (resolver must win over CR)", got, canonicalURL)
	}
	if got := envValue(env, "SPIRE_REPO_BRANCH"); got != canonicalBase {
		t.Errorf("SPIRE_REPO_BRANCH = %q, want canonical %q (resolver must win over CR)", got, canonicalBase)
	}

	// Drift lines must be on the log so operators can see the
	// divergence. Separate lines for URL and branch drift are a
	// readability choice in buildWorkloadPod; both should fire.
	if !sink.hasInfoContaining("Repo drifts from canonical resolver") {
		t.Errorf("drift case must log URL drift; got messages: %v", sink.infoMessages())
	}
	if !sink.hasInfoContaining("RepoBranch drifts from canonical resolver") {
		t.Errorf("drift case must log BaseBranch drift; got messages: %v", sink.infoMessages())
	}

	// The CR's divergent URL must NOT leak into the pod env under any
	// container. This is the scheduling-decision invariant from
	// spi-njzmg: CR fields are projection-only.
	for _, c := range pod.Spec.Containers {
		for _, e := range c.Env {
			if e.Value == crRepoURL {
				t.Errorf("CR URL %q leaked into container %q env %q", crRepoURL, c.Name, e.Name)
			}
			if e.Value == crBranch {
				t.Errorf("CR branch %q leaked into container %q env %q", crBranch, c.Name, e.Name)
			}
		}
	}
	for _, c := range pod.Spec.InitContainers {
		for _, e := range c.Env {
			if e.Value == crRepoURL {
				t.Errorf("CR URL %q leaked into init container %q env %q", crRepoURL, c.Name, e.Name)
			}
		}
	}
}

// TestClusterIdentityParity_AgentMonitor_NoResolverFallback documents
// the nil-resolver bring-up path: without a wired resolver, the
// reconciler falls back to CR fields verbatim. This is the wave-0
// default used while the resolver is still being materialized from
// shared registration. The test pins the fallback so nobody
// accidentally makes the resolver mandatory before every install has
// one wired.
func TestClusterIdentityParity_AgentMonitor_NoResolverFallback(t *testing.T) {
	const (
		ns         = "spire"
		prefix     = "spi"
		crRepoURL  = "git@example.com:fallback/repo.git"
		crBranch   = "trunk"
	)

	guild := makeAgent("core", ns, nil)
	guild.Spec.Repo = crRepoURL
	guild.Spec.RepoBranch = crBranch
	guild.Spec.Prefixes = []string{prefix}

	sink := &capturingSink{}
	m := &AgentMonitor{
		Log:          logr.New(sink),
		Namespace:    ns,
		StewardImage: "spire-agent:dev",
		Database:     "spire",
		Prefix:       prefix,
		// Resolver intentionally nil — wave-0 fallback.
	}

	pod := m.buildWorkloadPod(guild, "spi-abc", nil)
	if pod == nil {
		t.Fatalf("buildWorkloadPod returned nil with nil resolver; fallback should use CR")
	}
	env := envMap(pod.Spec.Containers[0].Env)
	if got := envValue(env, "SPIRE_REPO_URL"); got != crRepoURL {
		t.Errorf("SPIRE_REPO_URL = %q, want %q (CR fallback when resolver nil)", got, crRepoURL)
	}
	if got := envValue(env, "SPIRE_REPO_BRANCH"); got != crBranch {
		t.Errorf("SPIRE_REPO_BRANCH = %q, want %q (CR fallback when resolver nil)", got, crBranch)
	}

	// No drift line — there's nothing to compare against.
	if sink.hasInfoContaining("drifts from canonical resolver") {
		t.Errorf("nil-resolver fallback must not emit drift log; got: %v", sink.infoMessages())
	}
}

// TestClusterIdentityParity_IntentReconciler_ResolverWins covers the
// canonical apprentice path. IntentWorkloadReconciler.canonicalIdentity
// reconciles the WorkloadIntent's projected RepoIdentity against the
// shared resolver — on parity the resolver output is returned silently;
// on drift a log line fires and the resolver value wins.
func TestClusterIdentityParity_IntentReconciler_ResolverWins(t *testing.T) {
	const (
		prefix        = "spi"
		intentURL     = "git@example.com:projected/repo.git"
		intentBranch  = "projection"
		canonicalURL  = "git@example.com:canonical/repo.git"
		canonicalBase = "main"
	)

	sink := &capturingSink{}
	resolver := &fakeIdentityResolver{out: identity.ClusterRepoIdentity{
		URL:        canonicalURL,
		BaseBranch: canonicalBase,
		Prefix:     prefix,
	}}

	r := &IntentWorkloadReconciler{
		Log:       logr.New(sink),
		Namespace: "spire",
		Image:     "spire-agent:dev",
		Tower:     "spire",
		Resolver:  resolver,
	}

	got, err := r.canonicalIdentity(context.Background(), intent.RepoIdentity{
		URL:        intentURL,
		BaseBranch: intentBranch,
		Prefix:     prefix,
	})
	if err != nil {
		t.Fatalf("canonicalIdentity: unexpected error: %v", err)
	}

	if got.URL != canonicalURL {
		t.Errorf("canonicalIdentity.URL = %q, want %q (resolver wins)", got.URL, canonicalURL)
	}
	if got.BaseBranch != canonicalBase {
		t.Errorf("canonicalIdentity.BaseBranch = %q, want %q (resolver wins)", got.BaseBranch, canonicalBase)
	}
	if got.Prefix != prefix {
		t.Errorf("canonicalIdentity.Prefix = %q, want %q", got.Prefix, prefix)
	}

	// Drift must be logged. The reconciler does not wrap the drift
	// into a typed error — per wave-1 design (spi-njzmg note 4) the
	// resolver value is authoritative and the divergence is logged so
	// operators can audit it without the scheduler stalling.
	if !sink.hasInfoContaining("drifts from canonical resolver") {
		t.Errorf("intent reconciler drift path must log drift; got: %v", sink.infoMessages())
	}
}

// TestClusterIdentityParity_IntentReconciler_ParityIsSilent covers
// the mirror case: when the WorkloadIntent's projection agrees with
// the resolver, canonicalIdentity returns the resolver value without
// emitting a drift log.
func TestClusterIdentityParity_IntentReconciler_ParityIsSilent(t *testing.T) {
	const (
		prefix        = "spi"
		canonicalURL  = "git@example.com:canonical/repo.git"
		canonicalBase = "main"
	)

	sink := &capturingSink{}
	resolver := &fakeIdentityResolver{out: identity.ClusterRepoIdentity{
		URL:        canonicalURL,
		BaseBranch: canonicalBase,
		Prefix:     prefix,
	}}

	r := &IntentWorkloadReconciler{
		Log:       logr.New(sink),
		Namespace: "spire",
		Image:     "spire-agent:dev",
		Tower:     "spire",
		Resolver:  resolver,
	}

	got, err := r.canonicalIdentity(context.Background(), intent.RepoIdentity{
		URL:        canonicalURL,
		BaseBranch: canonicalBase,
		Prefix:     prefix,
	})
	if err != nil {
		t.Fatalf("canonicalIdentity: unexpected error: %v", err)
	}
	if got.URL != canonicalURL || got.BaseBranch != canonicalBase || got.Prefix != prefix {
		t.Fatalf("canonicalIdentity: got %+v, want URL=%q branch=%q prefix=%q",
			got, canonicalURL, canonicalBase, prefix)
	}

	if sink.hasInfoContaining("drifts from canonical resolver") {
		t.Errorf("parity case must not emit drift log; got: %v", sink.infoMessages())
	}
}

// TestClusterIdentityParity_IntentReconciler_EmptyPrefixErrors pins
// the typed-error surface from canonicalIdentity. The resolver contract
// requires a non-empty prefix; pushing an intent with an empty prefix
// should surface as an explicit error so the reconciler drops the
// intent rather than materialize a pod with no identity.
func TestClusterIdentityParity_IntentReconciler_EmptyPrefixErrors(t *testing.T) {
	r := &IntentWorkloadReconciler{
		Log:       logr.New(&capturingSink{}),
		Namespace: "spire",
		Image:     "spire-agent:dev",
		Tower:     "spire",
		Resolver:  &fakeIdentityResolver{},
	}

	_, err := r.canonicalIdentity(context.Background(), intent.RepoIdentity{
		URL:        "git@example.com:x/y.git",
		BaseBranch: "main",
		Prefix:     "",
	})
	if err == nil {
		t.Fatalf("canonicalIdentity: expected error for empty prefix, got nil")
	}
	if !strings.Contains(err.Error(), "empty repo prefix") {
		t.Errorf("canonicalIdentity error = %v, want containing 'empty repo prefix'", err)
	}
}

// envValue returns the Value field of env[name] or "" when the name
// is absent. Pulled out so the assertions above stay readable.
func envValue(env map[string]corev1.EnvVar, name string) string {
	return env[name].Value
}
