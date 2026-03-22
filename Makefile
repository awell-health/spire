.PHONY: build build-steward build-agent load deploy apply restart logs status clean

NAMESPACE ?= spire

# --- Build ---

build: build-steward build-agent

build-steward:
	docker build -f Dockerfile.steward -t spire-steward:dev .

build-agent:
	docker build -f Dockerfile.agent -t spire-agent:dev .

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
	@echo "=== Agents ==="
	@kubectl get spireagent -n $(NAMESPACE)
	@echo ""
	@echo "=== Workloads ==="
	@kubectl get spireworkload -n $(NAMESPACE)

# --- Cleanup ---

clean:
	kubectl delete namespace $(NAMESPACE) --ignore-not-found
