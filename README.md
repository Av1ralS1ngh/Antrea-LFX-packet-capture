# Antrea LFX Packet Capture Controller

A Kubernetes controller that performs on-demand packet capture on Pods using tcpdump.

## Overview

Watches Pod annotations and starts packet capture when `tcpdump.antrea.io: "<N>"` is added. Captures are stored as `/capture-<pod>.pcap` with automatic rotation and cleanup.

## Quick Start

```bash
make kind-load
```

```bash
make deploy
```

```bash
kubectl annotate pod <pod-name> tcpdump.antrea.io="5"
```

```bash
kubectl exec -n kube-system <controller-pod> -- ls -l /capture-*
```

```bash
kubectl annotate pod <pod-name> tcpdump.antrea.io-
```

## Installation

**Prerequisites:** Kubernetes cluster, kubectl, docker, make

1. **Build and Deploy:**

```bash
make kind-load
```

```bash
make deploy
```

2. **Deploy Test Pod:**

```bash
kubectl run traffic-generator --image=curlimages/curl \
  -- /bin/sh -c "while true; do curl -s google.com; sleep 1; done"
```

3. **Annotate Pod:**

```bash
kubectl annotate pod traffic-generator tcpdump.antrea.io="5"
```

## How It Works

- Controller watches Pods on the same node via informers
- When annotation is detected, starts `tcpdump` via `nsenter` into Pod's network namespace
- Uses `crictl` to resolve container PID for namespace access
- Invokes: `tcpdump -C 1M -W <N> -w /capture-<pod>.pcap -i eth0`
- Cleans up pcap files when annotation is removed or Pod deleted

## Implementation

- **Controller:** Standard K8s controller with informers and work queue
- **Process Manager:** Manages tcpdump processes with semaphore-based concurrency control
- **Multi-container Pods:** Always selects first container (`spec.containers[0]`)

## Development

```bash
make build
```

```bash
make test
```

```bash
make docker-build
```

```bash
make e2e
```

## Deliverables

All required files in `deliverables/`:
- `pod-describe.txt` - kubectl describe output
- `pods.txt` - kubectl get pods -A output
- `capture-files.txt` - ls output showing pcap files
- `capture.pcap` - Extracted pcap file
- `capture-output.txt` - tcpdump -r output
- `cleanup-verification.txt` - Proof files are deleted

Source code in `cmd/` and `pkg/`. Manifests in `deploy/`.

## License

MIT
