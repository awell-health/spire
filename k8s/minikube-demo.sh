#!/usr/bin/env bash
set -e

PROFILE="${MINIKUBE_PROFILE:-spire}"

echo "=== Spire Steward — Minikube Demo (profile: $PROFILE) ==="
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

# Build the steward image
echo "Building spire-steward:dev..."
cd "$(dirname "$0")/.."
docker build -f Dockerfile.steward -t spire-steward:dev .
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
echo "Deploying steward..."
kubectl apply -f k8s/steward.yaml

# Apply example config
echo "Applying SpireConfig..."
kubectl apply -f k8s/examples/config.yaml

# Register yourself as an external agent
echo "Registering external agent..."
kubectl apply -f k8s/examples/agent-external.yaml

# Wait for rollout
echo ""
echo "Waiting for steward to start..."
kubectl rollout status -n spire deployment/spire-steward --timeout=120s

echo ""
echo "=== Demo running! ==="
echo ""
echo "Watch the steward logs:"
echo "  kubectl logs -n spire deploy/spire-steward -f"
echo ""
echo "Check the board:"
echo "  kubectl get spireworkloads -n spire"
echo "  kubectl get spireagents -n spire"
echo ""
echo "Create a test bead locally and push:"
echo "  spire file 'Test steward assignment' -t task -p 2"
echo "  bd dolt push"
echo ""
echo "Then watch it appear:"
echo "  kubectl get spireworkloads -n spire -w"
echo ""
echo "Tear down:"
echo "  kubectl delete namespace spire"
echo "  minikube stop -p $PROFILE"
