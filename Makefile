.PHONY: build build-steward build-agent load deploy apply restart logs status clean smoke-test-helm test-observability crd-check e2e lifecycle-gate

NAMESPACE ?= spire

# --- Build ---
#
# SPIRE_VERSION is injected via -ldflags "-X main.version=..." inside
# the Dockerfiles. Without it the binary stays at "dev" and
# decideVersionAction in cmd/spire/version_check.go re-runs every
# migration on every restart instead of memoising stored == binary.
# Defaults to the most recent git tag (or short SHA when there are no
# tags) so dev `make build` produces something that decides as a real
# release. Override via `make build SPIRE_VERSION=v0.49.0-rc1`.

SPIRE_VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

build: build-steward build-agent

build-steward:
	docker build --build-arg SPIRE_VERSION=$(SPIRE_VERSION) -f Dockerfile.steward -t spire-steward:dev .

build-agent:
	docker build --build-arg SPIRE_VERSION=$(SPIRE_VERSION) -f Dockerfile.agent -t spire-agent:dev .

# --- Deploy ---

deploy: build load apply restart

load:
	minikube image load spire-steward:dev
	minikube image load spire-agent:dev

apply:
	kubectl apply -k k8s/

restart:
	kubectl rollout restart deployment/spire-steward -n $(NAMESPACE)
	kubectl rollout restart deployment/spire-operator -n $(NAMESPACE)

# --- Shortcuts ---

steward: build-steward
	minikube image load spire-steward:dev
	kubectl rollout restart deployment/spire-steward -n $(NAMESPACE)

agent: build-agent
	minikube image load spire-agent:dev

operator: build-steward
	minikube image load spire-steward:dev
	kubectl rollout restart deployment/spire-operator -n $(NAMESPACE)

# --- Observe ---

logs:
	kubectl logs -n $(NAMESPACE) deploy/spire-steward -f --all-containers

logs-operator:
	kubectl logs -n $(NAMESPACE) deploy/spire-operator -f

status:
	@echo "=== Pods ==="
	@kubectl get pods -n $(NAMESPACE)
	@echo ""
	@echo "=== Guilds ==="
	@kubectl get wizardguild -n $(NAMESPACE)
	@echo ""
	@echo "=== Workloads ==="
	@kubectl get spireworkload -n $(NAMESPACE)

# --- Cleanup ---

clean:
	kubectl delete namespace $(NAMESPACE) --ignore-not-found

# --- Smoke tests ---

# Run the multi-tenant helm smoke test. Installs two releases into
# separate namespaces (spire-a, spire-b) with different bead prefixes
# and verifies isolation. See docs/HELM.md for env-var options.
smoke-test-helm:
	bash hack/multi-tenant-smoke-test.sh

# --- CRD drift guard ---

# controller-gen generates CRDs into helm/spire/crds/ (see
# operator/api/v1alpha1/generate.go); the same go:generate directive
# mirrors them into k8s/crds/ so `helm install` and `kubectl apply -f
# k8s/crds/` produce the same schema. Drift here silently strips unknown
# fields (see spi-8fvhv: missing spec.cache schema dropped Cache from
# every applied WizardGuild CR). The guard catches hand-edits that bypass
# `go generate`; the regenerate-and-diff pattern catches stale checked-in
# CRDs.
crd-check:
	@diff -r k8s/crds/ helm/spire/crds/ \
		|| { echo "CRD drift: k8s/crds and helm/spire/crds disagree; run 'go generate ./api/...' in operator/ to regenerate both."; exit 1; }

# --- Lifecycle gate (spi-sqqero) ---

# Fails when any direct bead.status write is detected outside pkg/lifecycle.
# Hardened in Landing 2 (spi-g8a1nz) — no grandfathered allowlist; pkg/lifecycle
# is the sole sanctioned writer of bead.status.
# See pkg/lifecycle/README.md for migration guidance.
lifecycle-gate:
	bash scripts/check-lifecycle-gate.sh

# --- Observability regression suite ---

# Layered regression tests for the observability pipeline: OTEL
# ingestion (pkg/otel), OLAP storage contract (pkg/olap), and
# bead-scoped CLI readers (cmd/spire). An observability regression is
# caught by exactly one layer of this target, not by inspection of
# production metrics. See scripts/test-observability.sh for scope.
test-observability:
	bash scripts/test-observability.sh

# --- End-to-end cache-recovery test (spi-p18tr) ---

# Acceptance gate for the pinned-identity + wisp recovery epic
# (spi-w860i). Requires a running minikube with spire-steward:dev and
# spire-agent:dev loaded. The test helm-installs a fresh namespace
# per run and tears it down on exit. See test/e2e/README.md for the
# full data flow, prerequisites, and debugging tips.
e2e: build load
	go test -tags=e2e -timeout 30m ./test/e2e/... -v
