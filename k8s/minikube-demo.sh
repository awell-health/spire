#!/usr/bin/env bash
set -e

PROFILE="${MINIKUBE_PROFILE:-spire}"

echo "=== Spire Mayor — Minikube Demo (profile: $PROFILE) ==="
echo ""

# Check prerequisites
for cmd in minikube kubectl docker; do
    if ! command -v "$cmd" &>/dev/null; then
        echo "Error: $cmd is required but not installed."
        exit 1
    fi
done

# Check minikube is running
if ! minikube status -p "$PROFILE" &>/dev/null; then
    echo "Starting minikube (profile: $PROFILE)..."
    minikube start -p "$PROFILE"
fi

# Point docker to minikube's daemon
echo "Configuring docker to use minikube..."
eval $(minikube docker-env -p "$PROFILE")

# Build the mayor image
echo "Building spire-mayor:dev..."
cd "$(dirname "$0")/.."
docker build -f Dockerfile.mayor -t spire-mayor:dev .
echo "Image built."

# Apply namespace
echo ""
echo "Creating namespace..."
kubectl apply -f k8s/namespace.yaml

# Apply CRDs
echo "Applying CRDs..."
kubectl apply -f k8s/crds/

# Create secrets (prompt for values)
echo ""
echo "Setting up secrets..."
if kubectl get secret spire-credentials -n spire &>/dev/null; then
    echo "  Secrets already exist. Skipping."
else
    read -p "  DoltHub username: " DOLT_USER
    read -sp "  DoltHub token: " DOLT_PASS
    echo ""

    kubectl create secret generic spire-credentials \
        --namespace spire \
        --from-literal=DOLT_REMOTE_USER="$DOLT_USER" \
        --from-literal=DOLT_REMOTE_PASSWORD="$DOLT_PASS" \
        --from-literal=ANTHROPIC_API_KEY_DEFAULT="${ANTHROPIC_API_KEY:-not-set}"

    echo "  Secrets created."
fi

# Deploy
echo ""
echo "Deploying mayor..."
kubectl apply -f k8s/mayor.yaml

# Apply example config
echo "Applying SpireConfig..."
kubectl apply -f k8s/examples/config.yaml

# Register yourself as an external agent
echo "Registering external agent..."
kubectl apply -f k8s/examples/agent-external.yaml

# Wait for rollout
echo ""
echo "Waiting for mayor to start..."
kubectl rollout status -n spire deployment/spire-mayor --timeout=120s

echo ""
echo "=== Demo running! ==="
echo ""
echo "Watch the mayor logs:"
echo "  kubectl logs -n spire deploy/spire-mayor -f"
echo ""
echo "Check the board:"
echo "  kubectl get spireworkloads -n spire"
echo "  kubectl get spireagents -n spire"
echo ""
echo "Create a test bead locally and push:"
echo "  spire file 'Test mayor assignment' -t task -p 2"
echo "  bd dolt push"
echo ""
echo "Then watch it appear:"
echo "  kubectl get spireworkloads -n spire -w"
echo ""
echo "Tear down:"
echo "  kubectl delete namespace spire"
echo "  minikube stop -p $PROFILE"
