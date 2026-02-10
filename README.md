# Antrea LFX Packet Capture Controller

A Kubernetes controller that performs on-demand packet capture on Pods using `tcpdump`. Developed as part of the LFX Mentorship 2026_01 task.

## Overview

This controller watches Pod annotations and starts a packet capture on matching Pods' `eth0` interfaces. The captures are stored on the node in files named `/captures/capture-<pod>.pcap` (with rotation if `-C/-W` is used).

### Features

- **On-demand Capture**: Triggered by Pod annotation `tcpdump.antrea.io: "<N>"`.
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
   Annotate the Pod with the required key and max file count:

   ```bash
   kubectl annotate pod traffic-generator tcpdump.antrea.io="5"
   ```

3. **Verify Capture**
   Check generated files in the controller pod:

   ```bash
   kubectl exec -n kube-system <controller-pod-name> -- ls -l /captures/capture-*
   ```

4. **Stop Capture**
   Remove the annotation:

   ```bash
   kubectl annotate pod traffic-generator tcpdump.antrea.io-
   ```

## Development

- **Build Binary**: `make build`
- **Unit Tests**: `make test`
- **Build Docker Image**: `make docker-build`

## Automated E2E Testing

The `make e2e` target creates a Kind cluster, deploys the controller, starts a capture via annotation, generates traffic, and verifies it contains more than 0 packets.

```bash
make e2e
```

Optional environment variables:

- `CLUSTER_NAME`: Kind cluster name (default: `capture-test`)
- `KIND_CONFIG`: Kind config file path (defaults to Kind's built-in config)

## Architecture

- **Controller**: Watches Pod events and reacts to the `tcpdump.antrea.io` annotation.
- **ProcessManager**: Manages `tcpdump` processes using `nsenter` to access Pod network namespaces.
- **Host Access**: Uses `hostPID: true` and mounts `/run/containerd/containerd.sock` to resolve container PIDs via `crictl`.

## Important Notes

### File Naming

When using `tcpdump -W <N>`, tcpdump automatically appends numeric suffixes to capture files. For example, with `-w /captures/capture-traffic-generator.pcap -W 5`, the files will be named:
- `/captures/capture-traffic-generator.pcap0`
- `/captures/capture-traffic-generator.pcap1`
- `/captures/capture-traffic-generator.pcap2`
- etc.

This is standard tcpdump behavior for file rotation.

## Deliverables

This repository contains all required deliverables for the LFX Mentorship task:

1. **Go source code**: `cmd/` and `pkg/` directories
2. **Dockerfile**: `Dockerfile` (uses ubuntu:24.04 base image)
3. **Makefile**: `Makefile` for build automation
4. **Capture DaemonSet manifest**: `deploy/daemonset.yaml` (includes RBAC)
5. **Test Pod manifest**: `deploy/test-pod.yaml`
6. **Pod describe output**: `deliverables/pod-describe.txt`
7. **Pods listing**: `deliverables/pods.txt`
8. **Capture files listing**: `deliverables/capture-files.txt`
9. **Extracted pcap file**: `deliverables/capture.pcap`
10. **Pcap text output**: `deliverables/capture-output.txt`
11. **Cleanup verification**: `deliverables/cleanup-verification.txt`

## License

MIT

