# Unified host-agent image. One container, s6-supervised, bundles:
#
#   fan-controller         — adaptive Dell PowerEdge fan PID
#   node_exporter          — :9100  host CPU/mem/disk/net + textfile
#   cadvisor               — :8089  per-container metrics
#   ipmi_exporter          — :9290  chassis sensors
#   smartctl_exporter      — :9633  drive SMART
#   nvidia_gpu_exporter    — :9835  GPU temp/util/mem/power
#   vmagent                — push all of the above to central Prometheus
#                            (hardcoded remote_write URL, host label =
#                             kernel hostname). No per-host config; the
#                             dashboard is a pure receiver.
#
# Each sub-service probes its hardware on start and `sleep infinity` if
# absent — same image runs on Dell + non-Dell, GPU + headless, Debian +
# Unraid hosts with no per-host build.
#
# Base: Alpine (was Debian-slim through v0.2.0).
#   - The fan-controller is pure Go static (CGO_ENABLED=0); the upstream
#     exporter binaries are also Go-static. None require glibc.
#   - nvidia-smi IS injected glibc-linked by the NVIDIA Container
#     Runtime on GPU hosts. We install `gcompat` (musl→glibc shim) so
#     nvidia_gpu_exporter's nvidia-smi shellouts still work.
#   - Save ~60 MB on the base layer vs debian-slim.
#
# UPX: all Go binaries are compressed with `upx --best --lzma`. Costs
# ~50ms decompression-on-exec. For long-running services, free 30-50%
# image shrink. Skipped if upx fails on a particular binary (some
# binaries with complex section layouts refuse to pack).

# -------- go-builder: compile the Go v2 fan controller --------
# Same source tree the bash script lives in; we copy host-agent/go.mod
# + the Go source dirs. Built static (CGO_ENABLED=0) + stripped so the
# final image stays small. VERSION is stamped into main.version via
# -ldflags so `fan-controller --version` (if we ever add the flag)
# matches /etc/host-agent-version.
FROM golang:1.26-alpine AS go-builder
ARG VERSION=dev
RUN apk add --no-cache upx
WORKDIR /src
COPY go.mod ./
COPY cmd/      cmd/
COPY internal/ internal/
RUN CGO_ENABLED=0 go build \
      -trimpath \
      -ldflags="-s -w -X main.version=${VERSION}" \
      -o /fan-controller \
      ./cmd/fan-controller \
 && upx --best --lzma /fan-controller || true

# -------- builder: fetch the Go binaries from upstream releases --------
# Run UPX --best --lzma against each. `|| true` so a refusal on any
# single binary doesn't abort the whole build — we get the uncompressed
# version in that case.
FROM alpine:3 AS builder

ARG NODE_EXPORTER_VERSION=1.8.2
ARG CADVISOR_VERSION=0.55.1
ARG IPMI_EXPORTER_VERSION=1.10.0
ARG SMARTCTL_EXPORTER_VERSION=0.13.0
ARG NVIDIA_GPU_EXPORTER_VERSION=1.4.1
ARG VMAGENT_VERSION=1.108.0

RUN apk add --no-cache curl ca-certificates tar xz upx

WORKDIR /tmp
RUN curl -fsSL -o node.tgz \
      "https://github.com/prometheus/node_exporter/releases/download/v${NODE_EXPORTER_VERSION}/node_exporter-${NODE_EXPORTER_VERSION}.linux-amd64.tar.gz" \
 && tar -xzf node.tgz \
 && cp "node_exporter-${NODE_EXPORTER_VERSION}.linux-amd64/node_exporter" /node_exporter \
 && upx --best --lzma /node_exporter || true

RUN curl -fsSL -o /cadvisor \
      "https://github.com/google/cadvisor/releases/download/v${CADVISOR_VERSION}/cadvisor-v${CADVISOR_VERSION}-linux-amd64" \
 && chmod +x /cadvisor \
 && upx --best --lzma /cadvisor || true

RUN curl -fsSL -o ipmi.tgz \
      "https://github.com/prometheus-community/ipmi_exporter/releases/download/v${IPMI_EXPORTER_VERSION}/ipmi_exporter-${IPMI_EXPORTER_VERSION}.linux-amd64.tar.gz" \
 && tar -xzf ipmi.tgz \
 && cp "ipmi_exporter-${IPMI_EXPORTER_VERSION}.linux-amd64/ipmi_exporter" /ipmi_exporter \
 && upx --best --lzma /ipmi_exporter || true

RUN curl -fsSL -o smartctl.tgz \
      "https://github.com/prometheus-community/smartctl_exporter/releases/download/v${SMARTCTL_EXPORTER_VERSION}/smartctl_exporter-${SMARTCTL_EXPORTER_VERSION}.linux-amd64.tar.gz" \
 && tar -xzf smartctl.tgz \
 && cp "smartctl_exporter-${SMARTCTL_EXPORTER_VERSION}.linux-amd64/smartctl_exporter" /smartctl_exporter \
 && upx --best --lzma /smartctl_exporter || true

RUN curl -fsSL -o nvidia.tgz \
      "https://github.com/utkuozdemir/nvidia_gpu_exporter/releases/download/v${NVIDIA_GPU_EXPORTER_VERSION}/nvidia_gpu_exporter_${NVIDIA_GPU_EXPORTER_VERSION}_linux_x86_64.tar.gz" \
 && tar -xzf nvidia.tgz \
 && cp nvidia_gpu_exporter /nvidia_gpu_exporter \
 && upx --best --lzma /nvidia_gpu_exporter || true

RUN curl -fsSL -o vmutils.tgz \
      "https://github.com/VictoriaMetrics/VictoriaMetrics/releases/download/v${VMAGENT_VERSION}/vmutils-linux-amd64-v${VMAGENT_VERSION}.tar.gz" \
 && tar -xzf vmutils.tgz vmagent-prod \
 && mv vmagent-prod /vmagent \
 && upx --best --lzma /vmagent || true


# -------- runtime --------
FROM alpine:3

# Short git SHA baked at build time. vmagent reads /etc/host-agent-version
# and stamps it onto every sample as the `version` external_label, so the
# dashboard can show which build is running per host. Defaults to "dev"
# for local builds.
ARG VERSION=dev
ARG S6_OVERLAY_VERSION=3.2.0.2
RUN echo "$VERSION" > /etc/host-agent-version

# OS tools needed by sub-services:
#   ipmitool, freeipmi    — chassis sensors / Dell raw fan commands
#   smartmontools         — drive SMART (smartctl_exporter shells out)
#   dmidecode             — chassis detection by fan-controller
#   gcompat               — musl→glibc shim (lets glibc-linked
#                           nvidia-smi run on Alpine; nvidia-smi is
#                           injected by NVIDIA Container Runtime on
#                           GPU hosts, not bundled here)
#   ca-certificates       — TLS to GitHub releases, push endpoints
#   mawk, xz, tini, procps — shell tooling for s6 run scripts +
#                            startup probes
RUN apk add --no-cache \
      ipmitool freeipmi smartmontools dmidecode \
      gcompat \
      ca-certificates mawk xz tini procps

# s6-overlay (PID 1, restarts crashed sub-services individually)
ADD "https://github.com/just-containers/s6-overlay/releases/download/v${S6_OVERLAY_VERSION}/s6-overlay-noarch.tar.xz" /tmp/
ADD "https://github.com/just-containers/s6-overlay/releases/download/v${S6_OVERLAY_VERSION}/s6-overlay-x86_64.tar.xz" /tmp/
RUN tar -Jxpf /tmp/s6-overlay-noarch.tar.xz -C / \
 && tar -Jxpf /tmp/s6-overlay-x86_64.tar.xz -C / \
 && rm /tmp/s6-overlay-*.tar.xz

# Exporter binaries (already UPX-compressed in builder stage)
COPY --from=builder /node_exporter        /usr/local/bin/node_exporter
COPY --from=builder /cadvisor             /usr/local/bin/cadvisor
COPY --from=builder /ipmi_exporter        /usr/local/bin/ipmi_exporter
COPY --from=builder /smartctl_exporter    /usr/local/bin/smartctl_exporter
COPY --from=builder /nvidia_gpu_exporter  /usr/local/bin/nvidia_gpu_exporter
COPY --from=builder /vmagent              /usr/local/bin/vmagent

# Fan controller (Go v2, UPX-compressed) + per-chassis profiles.
COPY --from=go-builder /fan-controller /usr/local/bin/fan-controller
COPY profiles/                              /etc/fan-controller/profiles/
RUN chmod +x /usr/local/bin/fan-controller

# s6 service definitions: one per sub-service, each probes its hardware
COPY s6/ /etc/s6-overlay/s6-rc.d/

ENV S6_KEEP_ENV=1 \
    S6_BEHAVIOUR_IF_STAGE2_FAILS=2 \
    S6_CMD_WAIT_FOR_SERVICES_MAXTIME=0

ENTRYPOINT ["/init"]
