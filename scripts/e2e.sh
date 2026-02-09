#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CLUSTER_NAME="${CLUSTER_NAME:-capture-test}"
KIND_CONFIG="${KIND_CONFIG:-}"
TMP_DIR=""

require_cmd() {
  if ! command -v "$1" >/dev/null 2>&1; then
    echo "ERROR: Missing required command: $1" >&2
    exit 1
  fi
}

cleanup() {
  if [[ -n "$TMP_DIR" && -d "$TMP_DIR" ]]; then
    rm -rf "$TMP_DIR"
  fi
}

trap cleanup EXIT

require_cmd kind
require_cmd kubectl
require_cmd docker
require_cmd make

cd "$ROOT_DIR"

if ! kind get clusters | grep -qx "$CLUSTER_NAME"; then
  echo "==> Creating Kind cluster: $CLUSTER_NAME"
  if [[ -n "$KIND_CONFIG" ]]; then
    kind create cluster --name "$CLUSTER_NAME" --config "$KIND_CONFIG"
  else
    kind create cluster --name "$CLUSTER_NAME"
  fi
fi

kubectl config use-context "kind-$CLUSTER_NAME" >/dev/null

echo "==> Building and loading image into Kind"
CLUSTER_NAME="$CLUSTER_NAME" make kind-load

echo "==> Deploying controller and test Pod"
kubectl apply -f deploy/daemonset.yaml -f deploy/test-pod.yaml

echo "==> Restarting controller DaemonSet to pick up new image"
kubectl -n kube-system rollout restart daemonset/capture-controller

echo "==> Waiting for controller DaemonSet rollout"
kubectl -n kube-system rollout status daemonset/capture-controller

echo "==> Waiting for traffic generator to be ready"
kubectl wait --for=condition=Ready pod/traffic-generator --timeout=120s

echo "==> Starting capture"
kubectl annotate pod traffic-generator tcpdump.antrea.io="5" --overwrite

traffic_node="$(kubectl get pod traffic-generator -o jsonpath='{.spec.nodeName}')"
controller_pod="$(kubectl -n kube-system get pods -l app=capture-controller -o jsonpath='{range .items[?(@.spec.nodeName=="'"$traffic_node"'")]}{.metadata.name}{"\n"}{end}' | head -n 1)"

if [[ -z "$controller_pod" ]]; then
  echo "ERROR: No controller pod found on node $traffic_node" >&2
  exit 1
fi

echo "==> Controller pod on traffic node: $controller_pod"

echo "==> Waiting for PCAP data"
start_time="$(date +%s)"
file_location=""
packet_count="0"
while true; do
  file_location="$(kubectl -n kube-system exec "$controller_pod" -- sh -c "ls -t /capture-traffic-generator.pcap* 2>/dev/null | head -n 1" | tr -d '\r')"
  if [[ -n "$file_location" ]] && kubectl -n kube-system exec "$controller_pod" -- sh -c "test -f '$file_location'" >/dev/null 2>&1; then
    packet_count="$(kubectl -n kube-system exec "$controller_pod" -- sh -c "tcpdump -r '$file_location' -nn 2>/dev/null | wc -l" | tr -d ' ')"
    if [[ -n "$packet_count" && "$packet_count" -gt 0 ]]; then
      break
    fi
  fi

  now="$(date +%s)"
  if (( now - start_time > 120 )); then
    echo "ERROR: Timed out waiting for PCAP to contain packets" >&2
    exit 1
  fi
  sleep 2
done

TMP_DIR="$(mktemp -d)"
local_pcap="$TMP_DIR/capture.pcap"

echo "==> Downloading PCAP"
kubectl cp -n kube-system "$controller_pod:$file_location" "$local_pcap"

echo "==> PCAP verification passed ($packet_count packets)"

echo "==> Cleaning up capture"
kubectl annotate pod traffic-generator tcpdump.antrea.io- >/dev/null 2>&1 || true
