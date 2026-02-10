# Runtime image with crictl and required tools
FROM ubuntu:24.04

LABEL maintainer="Aviral Singh <aviral_s@mt.iitr.ac.in>"
LABEL description="Packet Capture Controller for Antrea LFX Mentorship"

ARG CRICTL_VERSION="v1.29.0"
ARG TARGETARCH

# Install required packages (--allow-releaseinfo-change for clock skew issues)
RUN apt-get -o Acquire::Check-Valid-Until=false -o Acquire::Check-Date=false update && \
    apt-get install -y --no-install-recommends \
    tcpdump \
    bash \
    util-linux \
    procps \
    curl \
    ca-certificates \
    && rm -rf /var/lib/apt/lists/*

# Install crictl
RUN ARCH="$(dpkg --print-architecture)"; \
    if [ -n "${TARGETARCH}" ]; then ARCH="${TARGETARCH}"; fi; \
    case "${ARCH}" in amd64|arm64) ;; *) echo "Unsupported arch: ${ARCH}"; exit 1;; esac; \
    curl -L https://github.com/kubernetes-sigs/cri-tools/releases/download/${CRICTL_VERSION}/crictl-${CRICTL_VERSION}-linux-${ARCH}.tar.gz -o crictl.tar.gz && \
    tar -xzf crictl.tar.gz -C /usr/local/bin && \
    rm crictl.tar.gz && \
    chmod +x /usr/local/bin/crictl

# Copy pre-built binary from local build
COPY bin/capture-controller /usr/local/bin/

ENTRYPOINT ["/usr/local/bin/capture-controller"]
