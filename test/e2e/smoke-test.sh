#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"

echo "=== Running DRA Resctrl Driver E2E Smoke Test ==="

NODE_NAME="$(kubectl get nodes -o jsonpath='{.items[0].metadata.name}')"
echo "Found Kubernetes node: ${NODE_NAME}"

# Label node for resctrl DaemonSet scheduling
echo "Labeling node ${NODE_NAME} with resctrl.fabiendupont.io/resctrl-cat=true..."
kubectl label node "${NODE_NAME}" resctrl.fabiendupont.io/resctrl-cat="true" --overwrite

# Install Helm chart with the local CI image
echo "Deploying dra-resctrl via Helm..."
helm install dra-resctrl "${REPO_ROOT}/deploy/helm/dra-resctrl/" \
  -n dra-resctrl --create-namespace \
  --set driver.image.repository=dra-resctrl \
  --set driver.image.tag=ci \
  --set driver.image.pullPolicy=Never \
  --set nfd.enabled=false

# Wait for DaemonSet rollout
echo "Waiting for DaemonSet rollout..."
kubectl rollout status daemonset/dra-resctrl -n dra-resctrl --timeout=120s

# Check pods
echo "Checking dra-resctrl pods..."
kubectl get pods -n dra-resctrl -o wide

# Check DaemonSet logs
echo "=== DaemonSet Logs ==="
kubectl logs -n dra-resctrl daemonset/dra-resctrl --tail=50

# Verify ResourceSlice is published
echo "Checking published ResourceSlices..."
kubectl get resourceslices -o wide

SLICE_COUNT=$(kubectl get resourceslices -o json | jq -r '[.items[] | select(.spec.driver == "resctrl.fabiendupont.io")] | length')
if [ "$SLICE_COUNT" -eq 0 ]; then
  echo "ERROR: No ResourceSlice published by resctrl.fabiendupont.io"
  exit 1
fi
echo "Successfully found ${SLICE_COUNT} ResourceSlice(s) published by resctrl.fabiendupont.io"

# Verify devices within ResourceSlice
DEVICE_COUNT=$(kubectl get resourceslices -o json | jq -r '[.items[] | select(.spec.driver == "resctrl.fabiendupont.io") | .spec.devices[]] | length')
echo "Found ${DEVICE_COUNT} total devices published across slices."
if [ "$DEVICE_COUNT" -le 0 ]; then
  echo "ERROR: ResourceSlice contains no devices"
  exit 1
fi

# Verify resctrl groups were created in mock resctrl directory
echo "Verifying mock resctrl groups..."
ls -d /tmp/fake-resctrl/cache*-part* || {
  echo "ERROR: Expected resctrl group directories not found in /tmp/fake-resctrl"
  exit 1
}

echo "Resctrl groups created:"
ls -d /tmp/fake-resctrl/cache*-part*

# Verify uninstall cleans up
echo "Uninstalling dra-resctrl Helm release..."
helm uninstall dra-resctrl -n dra-resctrl

echo "Waiting for pods to terminate..."
kubectl wait --for=delete pod -l app.kubernetes.io/name=dra-resctrl -n dra-resctrl --timeout=60s || true

echo "=== E2E Smoke Test Passed Successfully! ==="
