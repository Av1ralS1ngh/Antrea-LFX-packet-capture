# Antrea LFX Packet Capture Controller

A Kubernetes controller that performs on-demand packet capture on Pods using `tcpdump`. Developed as part of the LFX Mentorship 2026_01 task.

## Overview

This controller watches `PacketCapture` custom resources and starts a packet capture on matching Pods' `eth0` interfaces. The captures are stored in the controller's container.

Design note: I intentionally implemented capture triggers using a CRD rather than Pod annotations to support better validation, status reporting, and lifecycle management, which are best practices for Kubernetes controllers.

### Features

- **On-demand Capture**: Triggered by creating a `PacketCapture` custom resource.
- **Timed Captures**: Stop captures automatically after the configured `spec.timeout`.
- **Stable Status**: Status updates are owned by the node running the capture to avoid cross-node overwrites.
- **Automatic Cleanup**: Stops the capture process and removes pcap files when the `PacketCapture` is removed.
- **Resource Management**: Uses a bounded worker queue to limit concurrent captures.
- **Containerized**: Runs as a DaemonSet for node-local captures.

## Prerequisites

- Kubernetes cluster (Kind recommended)
- `kubectl`
- `docker`
- `make`

## Installation

1. **Deploy Kind Cluster**

   ```bash
   make kind-load
   ```

   (This target builds the binary, builds the Docker image, creates the cluster if needed, and loads the image)

2. **Deploy Controller**
   ```bash
   make deploy
   ```
   This applies the DaemonSet and RBAC manifests to the `kube-system` namespace.

## Usage

1. **Deploy a Test Pod**

   ```bash
   kubectl run traffic-generator --image=curlimages/curl -- /bin/sh -c "while true; do curl -s google.com; sleep 1; done"
   ```

2. **Start Capture**
   Apply the CRD and create a PacketCapture resource:

   ```bash
   kubectl apply -f deploy/packetcapture-crd.yaml
   kubectl apply -f deploy/packetcapture.yaml
   ```

3. **Verify Capture**
   Check the controller logs:

   ```bash
   kubectl logs -n kube-system -l app=capture-controller
   ```

   Check PacketCapture status:

   ```bash
   kubectl get packetcaptures
   kubectl describe packetcapture capture-db-traffic
   ```

   The status includes the node name that owns the capture updates.

   Check generated files in the controller pod:

   ```bash
   kubectl exec -n kube-system <controller-pod-name> -- ls -l /captures
   ```

4. **Stop Capture**
   Delete the PacketCapture:
   ```bash
   kubectl delete packetcapture capture-db-traffic
   ```

5. **Download Capture**
   The controller exposes a simple HTTP server for downloading files.

   ```bash
   kubectl proxy &
   kubectl get pods -n kube-system -l app=capture-controller
   curl -o capture.pcap "http://127.0.0.1:8001/api/v1/namespaces/kube-system/pods/<controller-pod-name>:8090/proxy/captures/capture-<packetcapture-name>-<pod-name>.pcap"
   ```

## Development

- **Build Binary**: `make build`
- **Unit Tests**: `make test`
- **Build Docker Image**: `make docker-build`

## Automated E2E Testing

The `make e2e` target creates a Kind cluster, deploys the controller, starts a capture, generates traffic, downloads the PCAP, and verifies it contains more than 0 packets.

```bash
make e2e
```

Optional environment variables:

- `CLUSTER_NAME`: Kind cluster name (default: `capture-test`)
- `KIND_CONFIG`: Kind config file path (defaults to Kind's built-in config)

## Architecture

- **Controller**: Watches Pod events.
- **ProcessManager**: Manages `tcpdump` processes using `nsenter` to access Pod network namespaces.
- **Host Access**: Uses `hostPID: true` and mounts `/run/containerd/containerd.sock` to resolve container PIDs via `crictl`.

## License

MIT
