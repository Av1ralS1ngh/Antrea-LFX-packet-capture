# Antrea LFX Packet Capture Controller

A Kubernetes controller that performs on-demand packet capture on Pods using `tcpdump`. Developed as part of the LFX Mentorship 2026_01 task.

## Overview

This controller watches for Pods with the annotation `tcpdump.antrea.io=<max-files>` and starts a packet capture on the Pod's `eth0` interface. The captures are stored in the controller's container.

### Features

- **On-demand Capture**: Triggered by adding an annotation to a Pod.
- **Dynamic Configuration**: Adjust the number of rotated files dynamically by updating the annotation value.
- **Automatic Cleanup**: Stops the capture process and removes pcap files when the annotation is removed.
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
   Annotate the Pod with the desired number of rotated files (e.g., 3 files of 1MB each):

   ```bash
   kubectl annotate pod traffic-generator tcpdump.antrea.io=3
   ```

3. **Verify Capture**
   Check the controller logs:

   ```bash
   kubectl logs -n kube-system -l app=capture-controller
   ```

   Check generated files in the controller pod:

   ```bash
   kubectl exec -n kube-system <controller-pod-name> -- ls -l /captures
   ```

4. **Update Capture Config**
   Change the max file limit:

   ```bash
   kubectl annotate pod traffic-generator tcpdump.antrea.io=5 --overwrite
   ```

5. **Stop Capture**
   Remove the annotation:
   ```bash
   kubectl annotate pod traffic-generator tcpdump.antrea.io-
   ```

## Development

- **Build Binary**: `make build`
- **Unit Tests**: `make test`
- **Build Docker Image**: `make docker-build`

## Architecture

- **Controller**: Watches Pod events.
- **ProcessManager**: Manages `tcpdump` processes using `nsenter` to access Pod network namespaces.
- **Host Access**: Uses `hostPID: true` and mounts `/run/containerd/containerd.sock` to resolve container PIDs via `crictl`.

## License

MIT
