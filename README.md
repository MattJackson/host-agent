# host-agent

[![CI](https://github.com/mattjackson/host-agent/actions/workflows/test.yml/badge.svg)](https://github.com/mattjackson/host-agent/actions/workflows/test.yml)
[![Release](https://img.shields.io/github/v/release/mattjackson/host-agent?display_name=tag&sort=semver)](https://github.com/mattjackson/host-agent/releases)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

Single-container, drop-on-any-Linux-host bundle that does two things at once:

1. **Adaptive Dell PowerEdge fan control** — replaces iDRAC's stock fan curve with per-class PIDs (CPU, passive GPU, active GPU, HDD, SSD), an EWMA-tracked equilibrium baseline, and a proximity-to-emergency safety floor.
2. **Full per-host metrics** — bundles `node_exporter`, `cadvisor`, `ipmi_exporter`, `smartctl_exporter`, `nvidia_gpu_exporter`, and a `vmagent` that pushes everything to your Prometheus via remote_write.

Each sub-service probes its hardware on start and self-disables if its prerequisites are missing, so the *same image* runs on a Dell R730xd with a Tesla GPU, an Unraid box on consumer hardware, and a plain Debian VM with nothing exotic attached. No per-host build, no per-host config — set two env vars (image source, Prometheus URL) and it runs.

```
┌──────────────────────── your host ────────────────────────┐
│                                                           │
│   host-agent (single container, s6-supervised)            │
│   ├─ fan-controller   (Dell IPMI PID controller)          │
│   ├─ node_exporter    :9100                               │
│   ├─ cadvisor         :8089                               │
│   ├─ ipmi_exporter    :9290                               │
│   ├─ smartctl_exporter:9633                               │
│   ├─ nvidia_gpu_expo. :9835                               │
│   └─ vmagent ──────────── remote_write ──┐                │
│                                          │                │
└──────────────────────────────────────────┼────────────────┘
                                           │
                              ┌────────────▼────────────┐
                              │  your Prometheus        │
                              │  (or VictoriaMetrics,   │
                              │   Grafana Cloud, Mimir) │
                              └─────────────────────────┘
```

## Quick start

> **On Unraid?** Skip this block and go straight to [Option C](#option-c--unraid-xml-template-direct-from-github) — the install path is different (and simpler) because the URL lives in appdata, not env.

```sh
# 1. Pick where you're pushing metrics.
export HOST_AGENT_IMAGE=ghcr.io/mattjackson/host-agent:latest
export PROMETHEUS_REMOTE_WRITE_URL=https://your.prometheus/api/v1/write

# 2. (Dell hosts only) load the IPMI kernel module + create state dir.
sudo modprobe ipmi_devintf
echo ipmi_devintf | sudo tee /etc/modules-load.d/ipmi.conf
sudo install -d -m 755 /var/lib/host-agent/state

# 3. Run.
docker run -d \
  --name host-agent \
  --restart unless-stopped \
  --privileged \
  --network host \
  --cgroupns host \
  -e PROMETHEUS_REMOTE_WRITE_URL \
  -v /:/host:ro,rslave \
  -v /sys:/sys:ro \
  -v /run/docker.sock:/run/docker.sock \
  -v /run/containerd:/run/containerd:ro \
  -v /var/lib/docker:/var/lib/docker:ro \
  -v /dev:/dev \
  -v /var/lib/host-agent:/var/lib/host-agent \
  "$HOST_AGENT_IMAGE"
```

The container will appear in your Prometheus on its first scrape, labeled with `host=<kernel-hostname>`. There's no central config to edit when you add a new box — the agent introduces itself.

## What's inside

| sub-service | port | runs when | otherwise |
|---|---|---|---|
| `fan-controller`     | —    | `/dev/ipmi0` exists AND BMC is Dell | sleep |
| `node_exporter`      | 9100 | always                              | always |
| `cadvisor`           | 8089 | `/run/docker.sock` mounted          | sleep |
| `ipmi_exporter`      | 9290 | `/dev/ipmi0` exists                 | sleep |
| `smartctl_exporter`  | 9633 | always (auto-discovers)             | always |
| `nvidia_gpu_exporter`| 9835 | `nvidia-smi` is present             | sleep |
| `unraid-disks`       | —    | `/host/etc/unraid-version` exists   | sleep |
| `vmagent`            | 8429 | `PROMETHEUS_REMOTE_WRITE_URL` set   | sleep |

`unraid-disks` emits a textfile metric `unraid_disk_info{device,slot}` mapping Unraid's array slot labels (`disk1`, `parity`, `cache`, ...) to Linux device names by parsing `/var/local/emhttp/disks.ini`. Dashboards join on `(host, device)` to display the slot label instead of the bare `sdX` letter — matches the bay labeling in Unraid's own UI.

`node_exporter` runs with `--collector.textfile.directory=/var/lib/host-agent/state`, so the fan controller's own state metrics (setpoint, EWMA baseline, per-class temps & targets) are emitted alongside the standard node metrics on `:9100`. The dashboard treats them as native Prometheus series.

## Install

### Option A — single `docker run` (most hosts)

See [Quick start](#quick-start). Idempotent re-runs of the same `docker run` aren't (Docker will error on the name conflict); use `docker rm -f host-agent` then re-run, or use `install/install.sh` which does that automatically.

### Option B — docker compose

```yaml
# docker-compose.yml
services:
  host-agent:
    image: ${HOST_AGENT_IMAGE:?HOST_AGENT_IMAGE must be set}
    container_name: host-agent
    restart: unless-stopped
    privileged: true
    network_mode: host
    cgroup: host
    runtime: ${HOST_AGENT_RUNTIME:-runc}
    environment:
      - NVIDIA_VISIBLE_DEVICES=all
      - NVIDIA_DRIVER_CAPABILITIES=utility
      - PROMETHEUS_REMOTE_WRITE_URL=${PROMETHEUS_REMOTE_WRITE_URL:?must be set}
      - PROMETHEUS_REMOTE_WRITE_BEARER_TOKEN=${PROMETHEUS_REMOTE_WRITE_BEARER_TOKEN:-}
      - PROMETHEUS_REMOTE_WRITE_USERNAME=${PROMETHEUS_REMOTE_WRITE_USERNAME:-}
      - PROMETHEUS_REMOTE_WRITE_PASSWORD=${PROMETHEUS_REMOTE_WRITE_PASSWORD:-}
      - PROMETHEUS_REMOTE_WRITE_TLS_INSECURE_SKIP_VERIFY=${PROMETHEUS_REMOTE_WRITE_TLS_INSECURE_SKIP_VERIFY:-}
    volumes:
      - /:/host:ro,rslave
      - /sys:/sys:ro
      - /run/docker.sock:/run/docker.sock
      - /run/containerd:/run/containerd:ro
      - /var/lib/docker:/var/lib/docker:ro
      - /dev:/dev
      - /var/lib/host-agent:/var/lib/host-agent
```

Drop the two env vars in an `.env` next to the compose, then `docker compose up -d`. The `infra/deploy.sh` wrapper in this repo handles the "auto-detect nvidia runtime" case.

### Option C — Unraid (XML template direct from GitHub)

The `install/host-agent.xml` template is consumable directly from GitHub — no Community Applications submission needed. **The URL is set via a file in appdata, not the template** — that decouples your config from Unraid's Force Update behavior.

**One-time SSH setup** (does both the URL file and the template fetch in one go):

```sh
mkdir -p /mnt/user/appdata/host-agent/config
echo 'http://your-prometheus:9090/api/v1/write' \
  > /mnt/user/appdata/host-agent/config/remote_write_url
curl -sfLO --output-dir /boot/config/plugins/dockerMan/templates-user \
  https://raw.githubusercontent.com/mattjackson/host-agent/main/install/host-agent.xml
```

**Then in the web UI**: Docker tab → **Add Container** → Template dropdown → pick **host-agent** under "User templates" → **Apply**.

That's it. No env vars to fill in. If your Prometheus needs auth, toggle **Advanced View** in the form to expose the optional bearer-token / basic-auth / TLS-skip fields.

All paths and the `--cgroupns=host` flag are pre-baked into the template. To get listed in Community Applications search, submit the XML to [Squidly271/AppFeed](https://github.com/Squidly271/AppFeed) — not required for personal use.

**Future updates** — just click **Force Update** in the Docker tab. The URL is in appdata; Unraid can't lose it. If a release adds new template fields (rare), the release notes will tell you to re-run the curl above; otherwise leave the template alone.

### Option D — `install.sh` one-shot (curl-pipe)

```sh
export HOST_AGENT_IMAGE=ghcr.io/mattjackson/host-agent:latest
export PROMETHEUS_REMOTE_WRITE_URL=https://your.prometheus/api/v1/write
curl -sf https://raw.githubusercontent.com/mattjackson/host-agent/main/install/install.sh | sh
```

Idempotent (stops + removes any existing `host-agent` container first). All flags hardcoded — to change them, fork or build a new image, don't edit the script per host.

### Option E — from a blank Ubuntu host

End-to-end walkthrough for a freshly installed Ubuntu (or Debian) server with nothing on it yet. Goes from `apt update` to host-agent reporting in your Prometheus. Tested on Ubuntu 24.04 LTS server, fresh install.

```sh
# 1. Set the hostname FIRST — host-agent stamps every metric series with
#    `host=$(hostname -s)`, so this determines how the box appears in
#    Prometheus and Grafana. Skipping this step gives you a series labeled
#    "ubuntu" (or, on some setups, with no host label at all).
sudo hostnamectl set-hostname <your-host>

# 2. Ubuntu prerequisites
sudo apt update && sudo apt install -y ca-certificates curl gnupg lsb-release

# 3. Docker engine (official repo, not the snap)
sudo install -m 0755 -d /etc/apt/keyrings
curl -fsSL https://download.docker.com/linux/ubuntu/gpg | \
  sudo gpg --dearmor -o /etc/apt/keyrings/docker.gpg
sudo chmod a+r /etc/apt/keyrings/docker.gpg
echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.gpg] \
  https://download.docker.com/linux/ubuntu $(lsb_release -cs) stable" | \
  sudo tee /etc/apt/sources.list.d/docker.list
sudo apt update && sudo apt install -y docker-ce docker-ce-cli containerd.io \
  docker-buildx-plugin docker-compose-plugin
sudo usermod -aG docker $USER && newgrp docker

# 4. (Dell hosts only) load the IPMI kernel module so the fan controller
#    can talk to the BMC. Harmless skip on non-Dell hardware.
sudo modprobe ipmi_devintf
echo ipmi_devintf | sudo tee /etc/modules-load.d/ipmi.conf

# 5. State dir (persists fan-controller learnings + vmagent WAL)
sudo install -d -m 755 /var/lib/host-agent/state

# 6. host-agent — adjust PROMETHEUS_REMOTE_WRITE_URL to your receiver
sudo docker run -d \
  --name host-agent \
  --restart unless-stopped \
  --privileged \
  --network host \
  --cgroupns host \
  -e PROMETHEUS_REMOTE_WRITE_URL=https://your.prometheus/api/v1/write \
  -l com.centurylinklabs.watchtower.enable=true \
  -v /:/host:ro,rslave \
  -v /sys:/sys:ro \
  -v /run/docker.sock:/run/docker.sock \
  -v /run/containerd:/run/containerd:ro \
  -v /var/lib/docker:/var/lib/docker:ro \
  -v /dev:/dev \
  -v /var/lib/host-agent:/var/lib/host-agent \
  ghcr.io/mattjackson/host-agent:latest

# 7. Watchtower — auto-pulls host-agent updates from ghcr.io (label-gated,
#    only touches containers with watchtower.enable=true, which host-agent
#    sets in step 6).
sudo docker run -d --name watchtower --restart unless-stopped \
  -v /var/run/docker.sock:/var/run/docker.sock \
  containrrr/watchtower --label-enable --interval 300
```

Within ~1 minute, the box appears in your Prometheus tagged `host=<your-host>`. From then on, every host-agent release lands automatically (Watchtower's 5-min poll on `:latest`).

> **Watch-outs:**
> - **Hostname set late.** If you set hostname *after* starting the container, restart it (`docker restart host-agent`) so vmagent re-reads `hostname -s` for its external label.
> - **All 4 docker mounts are required.** Skipping any of `/`, `/run/docker.sock`, `/run/containerd`, or `/var/lib/docker` silently breaks cadvisor (no container metrics) and gives node-exporter the container's `/proc` view instead of the host's. The minimal `-v /sys:/sys:ro -v /dev:/dev` set is *not* sufficient — it'll start, it'll push, but half your panels will be empty.
> - **Auth.** If your Prometheus needs auth, add `-e PROMETHEUS_REMOTE_WRITE_BEARER_TOKEN=...` (or the basic-auth pair). See [Configuration](#configuration).

## Configuration

Every knob is an env var. Required:

| var | what it is |
|---|---|
| `HOST_AGENT_IMAGE` | image to pull, e.g. `ghcr.io/mattjackson/host-agent:latest` |
| `PROMETHEUS_REMOTE_WRITE_URL` | your receiver's `/api/v1/write` endpoint |

**Persistent URL fallback** (recommended on Unraid and any platform with a managed container UI): write the URL to a file inside the state mount instead of (or in addition to) the env var:

```sh
mkdir -p /var/lib/host-agent/config   # on Unraid: /mnt/user/appdata/host-agent/config/
echo "https://your.prometheus/api/v1/write" > /var/lib/host-agent/config/remote_write_url
```

vmagent reads from this file if `PROMETHEUS_REMOTE_WRITE_URL` is unset or equal to the example.com placeholder. The file lives in the appdata mount, so it survives container recreations, image updates, and template re-curls — set it once, never reconfigure.

Optional — Prometheus push auth (bearer XOR basic):

| var | default | what |
|---|---|---|
| `PROMETHEUS_REMOTE_WRITE_BEARER_TOKEN` | — | sent as `Authorization: Bearer …` |
| `PROMETHEUS_REMOTE_WRITE_USERNAME` / `_PASSWORD` | — | HTTP basic auth |
| `PROMETHEUS_REMOTE_WRITE_TLS_INSECURE_SKIP_VERIFY` | `false` | self-signed certs |

Optional — GPU runtime:

| var | default | what |
|---|---|---|
| `HOST_AGENT_RUNTIME` | `runc` | set to `nvidia` to inject `nvidia-smi` via NVIDIA Container Runtime |

Optional — per-class fan controller overrides (advanced; override the chassis profile):

| var | default (from `profiles/default.env`) | what |
|---|---|---|
| `CPU_TARGET` / `CPU_EMERGENCY` / `CPU_DEADBAND` / `CPU_APPROACH_WINDOW` | `70` / `80` / `5` / `5` | CPU class PID |
| `GPU_TARGET` / `GPU_EMERGENCY` / `GPU_DEADBAND` / `GPU_APPROACH_WINDOW` | `83` / `90` / `2` / `7` | passive GPU PID |
| `ACTIVE_GPU_OWN_FAN_THRESHOLD` / `ACTIVE_GPU_EMERGENCY` | `85` / `88` | active GPU (own-fan-driven) assist threshold + temp safety net |
| `HDD_TARGET` / `HDD_EMERGENCY` | `40` / `50` | HDD PID |
| `SSD_TARGET` / `SSD_EMERGENCY` | `50` / `65` | SSD PID |
| `PROFILE` | autodetected | force a profile slug (e.g. `r730xd`); overrides dmidecode |

See `profiles/default.env` for the full reference + tuning notes.

## Server side (your Prometheus)

The receiver needs Prometheus 2.33+ with `--web.enable-remote-write-receiver`, or any compatible TSDB:

- Prometheus
- VictoriaMetrics (single-node or cluster)
- Grafana Mimir
- Cortex
- Grafana Cloud (hosted)

A minimal local setup (`examples/server-side/` in this repo):

```yaml
# prometheus.yml
global:
  scrape_interval: 15s
# Hosts push to us; we don't scrape them.
scrape_configs: []

rule_files:
  - rules.yml
```

```yaml
# rules.yml — populates the Grafana host dropdown
groups:
  - name: host-agent
    rules:
      - record: hosts_active
        expr: group by (host) (count_over_time(up[5m]))
```

Run Prometheus with `--web.enable-remote-write-receiver` and the agent's `vmagent` will push to `/api/v1/write` automatically.

## Dashboard

A pre-built Grafana dashboard for `host-agent` data lives in `examples/grafana/server-overview.json`. Import via Grafana → Dashboards → New → Import → paste JSON. Sections:

- **Temperatures** — CPU per-socket, IPMI inlet/exhaust, GPU die, per-drive SMART
- **Fan setpoint + RPM** — controller's commanded % vs measured RPM per fan
- **CPU / RAM / disk / network** — standard `node_exporter` panels
- **GPU util / mem / power** — from `nvidia_gpu_exporter`
- **Build panel** — which container build + chassis profile each host reports

The dashboard is hardware-agnostic (uses `class=~"cpu1|cpu2"` and `device=~".+"` patterns) so it adapts to whatever each host actually exposes.

## How the fan controller works

```
read CPU temps  (coretemp via /sys, IPMI entity 3.x fallback)
read GPU temps  (nvidia-smi), classify passive vs active
read HDD/SSD    (smartctl --scan, then -A -n standby per drive)
                  ├─ HDD: classified by rotation_rate > 0
                  └─ SSD/NVMe: rotation_rate = 0

Emergency override: any class >= its class_EMERGENCY ⇒ fans = 100%, stay
                    until ALL classes drop below {emergency - hysteresis}.

Per-class PID candidates (CPU, passive_GPU, HDD, SSD):
    candidate = clamp(current_speed + P×err + D×d_temp,
                      MIN_FAN, MAX_FAN)
    # Asymmetric deadband: positive errors always step up, even inside
    # the deadband. Negative errors inside deadband drift toward EWMA
    # base ⇒ heat-soaked chassis stays loud until it actually cools.

Per-class proximity floor (linear ramp from approach_window edge to emergency):
    pf[class] = ramp(class_temp,
                     EMERGENCY - APPROACH_WINDOW,  EMERGENCY,
                     MIN_FAN,                       MAX_FAN)
    # State-free — catches fast spikes the PID step can't react to.

Active-GPU assist (own-fan cards like RTX A5500 — chassis can't cool the
die, only the inlet air):
    if active_temp > ACTIVE_GPU_TARGET:
        assist = MIN_FAN + (active_temp - ACTIVE_GPU_TARGET) × ASSIST_GAIN

final_speed = max(cpu_cand, pg_cand, hdd_cand, ssd_cand,
                  cpu_pf, pg_pf, ag_pf, hdd_pf, ssd_pf,
                  ag_assist)

EWMA baseline (persisted to /var/lib/host-agent/state/base):
    base_speed = α × current_speed + (1-α) × base_speed
    # α = 0.001/cycle → settles over 24-48 hrs of operation.
```

The whole loop runs every `INTERVAL_SEC=15s`. All temperature thresholds, gains, and timing constants come from the chassis profile + class defaults — no hardcoded fan values, no lookup tables, no piecewise rules.

### Per-chassis profiles

`profiles/<model>.env` files are autoloaded by `dmidecode product_name`. Currently shipped:

| chassis | MIN_FAN | notes |
|---|---|---|
| `r730xd` | 10 | BMC clamps PWM <10 to a safety RPM (5040). No fan-stall risk. |
| `r730`   | 10 | Same firmware family as R730xd. |
| `r410`   | 20 | BMC obeys low PWM literally — going lower stalls fans. |
| `xc730xd_12` | 10 | Dell XC = Nutanix OEM rebadge of R730xd. Same BMC. |
| `default` | 15 | Conservative fallback for unknown Dell models. |

Override autodetection with `PROFILE=foo`. Adding a chassis: drop a `profiles/<slug>.env` with `MIN_FAN` and (optionally) the `SENSOR_*` IPMI sensor mapping. No code change needed.

## Tested hardware

| class | tested | likely works | won't work |
|---|---|---|---|
| **Chassis (fan ctrl)** | Dell PowerEdge R730xd, R730, R410, XC730xd-12 | R720, R630, R740 (same iDRAC family) | Non-Dell BMCs (Supermicro, HPE, Lenovo — fan controller sleeps, exporters keep running) |
| **CPU** | Xeon E5 v3/v4 | any with `coretemp` driver | — |
| **GPUs (passive)** | Tesla P4 | T4, M40, P40, A2, L4, A10, RTX 4000 Ada SFF | — |
| **GPUs (active)** | RTX A5500 | any consumer/workstation card with own fan | — |
| **Drives** | SATA HDD, SATA SSD, NVMe, MegaRAID passthrough | anything `smartctl --scan` enumerates | — |
| **OS** | Debian, Ubuntu, Unraid | any Linux + Docker | — |

Even on non-Dell hardware, **exporters still work** — `node_exporter`, `cadvisor`, `smartctl_exporter`, `nvidia_gpu_exporter` all run. Only the `fan-controller` and `ipmi_exporter` self-disable.

## Why one container with sub-processes

The cloud-native default is one container per concern. For a single-node fleet that pattern is mostly overhead:

- **One image to update.** Bump the tag → every host's watchtower picks it up. No 5× image pulls, no drift between containers.
- **One restart unit per host.** s6 supervises each sub-process individually — a crashed exporter is restarted in isolation — but the operator-facing unit is a single container.
- **One compose to paste.** Adding a new host = paste 15 lines. Adding a new sub-process to the agent = no compose change on any host.
- **No cross-container coordination.** Sub-services share the container's view of `/dev`, `/sys`, `/proc`, `/run/docker.sock` — no per-exporter mount duplication.

Tradeoff: a leaking sub-process shares memory pressure with its siblings (unified cgroup). For these workloads (all <1% CPU and <50 MB resident at idle), that's a non-issue.

## Operational

**Logs** (s6 prefixes each line with the sub-service):
```sh
docker logs host-agent --tail 100 -f
```

**Controller state**:
```sh
sudo cat /var/lib/host-agent/state/base
# base_speed=22.4515  ← EWMA of equilibrium fan speed
# last_speed=45       ← actual fan speed at last write
# samples=109         ← cycles since restart
# last_updated=…
```

**Per-host tuning** (advanced): drop env vars in `.env` next to the compose (e.g. `CPU_TARGET=72` to run quieter). Container env beats profile beats default. Most users should never need this — the chassis profile auto-loads sane defaults.

## Source layout

```
host-agent/
├── README.md                  # this file
├── LICENSE                    # MIT
├── CHANGELOG.md               # release notes
├── CONTRIBUTING.md            # dev setup, code style, profile contributions
├── SECURITY.md                # vulnerability reporting
├── Dockerfile                 # multi-stage: go-builder + exporter-fetch + s6-overlay
├── go.mod                     # zero external Go deps; vendor-free build
├── cmd/fan-controller/        # binary entry point
├── internal/                  # PID + sensors + IPMI + state + metrics
├── profiles/                  # per-chassis env files
├── s6/                        # s6-overlay service tree
├── examples/                  # bundled server-side compose + Grafana dashboard
├── install/                   # Unraid CA template + curl-pipe installer
├── infra/                     # docker-compose + deploy.sh + host-requirements.sh
└── .github/                   # CI workflows, issue + PR templates
```

## Development & contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). TL;DR:

```sh
cd host-agent
go test ./...                 # unit + e2e tests; no Docker needed
docker build -t host-agent:dev .
```

The fan controller is pure Go with zero external dependencies — the build is reproducible, the binary is ~2.4 MB, all logic is unit-testable as functions of inputs.

## License

[MIT](LICENSE) © Matthew Jackson
