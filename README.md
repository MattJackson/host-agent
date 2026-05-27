# host-agent

[![CI](https://github.com/MattJackson/host-agent/actions/workflows/test.yml/badge.svg)](https://github.com/MattJackson/host-agent/actions/workflows/test.yml)
[![Release](https://img.shields.io/github/v/release/MattJackson/host-agent?display_name=tag&sort=semver)](https://github.com/MattJackson/host-agent/releases)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)
[![Image](https://img.shields.io/badge/image-ghcr.io%2Fmattjackson%2Fhost--agent-1f6feb?logo=docker)](https://github.com/MattJackson/host-agent/pkgs/container/host-agent)

Single-container, drop-on-any-Linux-host bundle that does two things at once: replaces Dell PowerEdge stock fan curves with a per-class adaptive PID, and ships a full per-host Prometheus exporter stack (`node_exporter`, `cadvisor`, `ipmi_exporter`, `smartctl_exporter`, `nvidia_gpu_exporter`, `vmagent`) in the same image. Each sub-service probes its hardware on start and self-disables if absent, so the *same image* runs on a Dell R730xd with a Tesla GPU, an Unraid box on consumer hardware, and a plain Debian VM with nothing exotic attached. Set two env vars (image, Prometheus URL) and it runs.

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

**At a glance**

| | |
|---|---|
| **Image** | `ghcr.io/mattjackson/host-agent:latest` (`linux/amd64`) |
| **Chassis profiles shipped** | 13 (4 tested on real hardware, 9 conservative defaults) |
| **External Go dependencies** | zero (stdlib only, no `go.sum`, vendor-free build) |

## Table of contents

- [Quick start](#quick-start)
- [What's inside](#whats-inside)
- [Install](#install)
- [Configuration](#configuration)
- [Dashboard](#dashboard)
- [Fan controller](#fan-controller)
- [Per-chassis profiles](#per-chassis-profiles)
- [Tested hardware](#tested-hardware)
- [FAQ](#faq)
- [Comparison vs alternatives](#comparison-vs-alternatives)
- [Architecture rationale](#architecture-rationale)
- [Operational](#operational)
- [Development](#development)
- [License](#license)

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

The container appears in your Prometheus on its first scrape, labeled with `host=<kernel-hostname>`. There's no central config to edit when you add a new box — the agent introduces itself.

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
| `apt-status`         | —    | host has `/usr/lib/update-notifier/apt-check` (Debian/Ubuntu) | sleep |
| `vmagent`            | 8429 | `PROMETHEUS_REMOTE_WRITE_URL` set   | sleep |

`unraid-disks` emits a textfile metric `unraid_disk_info{device,slot}` mapping Unraid's array slot labels (`disk1`, `parity`, `cache`, ...) to Linux device names by parsing `/var/local/emhttp/disks.ini`. Dashboards join on `(host, device)` to display the slot label instead of the bare `sdX` letter — matches the bay labeling in Unraid's own UI.

`apt-status` runs `chroot /host /usr/lib/update-notifier/apt-check` once an hour and emits `host_apt_updates_pending{type="all"|"security"}` plus `host_reboot_required` (0/1, gated on `/var/run/reboot-required`). Useful when you've disabled `unattended-upgrades` on production hosts and want a "pending updates" panel as your planned-patch-cycle to-do list; the reboot-required flag is the single best post-upgrade safety check.

`node_exporter` runs with `--collector.textfile.directory=/var/lib/host-agent/state`, so the fan controller's own state metrics (setpoint, EWMA baseline, per-class temps & targets, adaptive targets) are emitted alongside the standard node metrics on `:9100`. The dashboard treats them as native Prometheus series.

**Adaptive controller (v0.2.0+).** The fan controller has two layers. A fast PID layer cycles every 15s and emits per-class fan-speed candidates; max() across all candidates plus per-class proximity floors drives the chassis. A slow intent layer (`HOST_AGENT_MODE=max-cool|balanced|min-noise|eco`) sits above it and drifts per-class targets every 10 min toward the chassis's actual equilibrium — within hardware envelopes encoded in the agent (CPU TJunction, NVIDIA passive datacenter envelope, Google HDD optimal band, NAND specs). Operators state intent; the agent picks numbers. Per-class env-var overrides (`CPU_TARGET=70` etc.) still win and disable adaptive on that class. See [`docs/adaptive-controller-v2.md`](docs/adaptive-controller-v2.md) for the full design.

## Install

### Option A — single `docker run` (most hosts)

See [Quick start](#quick-start). Idempotent re-runs of the same `docker run` aren't (Docker errors on the name conflict); use `docker rm -f host-agent` then re-run, or use `install/install.sh` which does that automatically.

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

Drop the two env vars in an `.env` next to the compose, then `docker compose up -d`. The `infra/deploy.sh` wrapper in this repo handles the auto-detect-nvidia-runtime case.

### Option C — Unraid (XML template direct from GitHub)

The `install/host-agent.xml` template is consumable directly from GitHub — no Community Applications submission needed. **The URL is set via a file in appdata, not the template** — that decouples your config from Unraid's Force Update behavior.

**One-time SSH setup** (does both the URL file and the template fetch in one go):

```sh
mkdir -p /mnt/user/appdata/host-agent/config
echo 'http://your-prometheus:9090/api/v1/write' \
  > /mnt/user/appdata/host-agent/config/remote_write_url
curl -sfLO --output-dir /boot/config/plugins/dockerMan/templates-user \
  https://raw.githubusercontent.com/MattJackson/host-agent/main/install/host-agent.xml
```

**Then in the web UI**: Docker tab → **Add Container** → Template dropdown → pick **host-agent** under "User templates" → **Apply**.

That's it. No env vars to fill in. If your Prometheus needs auth, toggle **Advanced View** in the form to expose the optional bearer-token / basic-auth / TLS-skip fields.

All paths and the `--cgroupns=host` flag are pre-baked into the template. To get listed in Community Applications search, submit the XML to [Squidly271/AppFeed](https://github.com/Squidly271/AppFeed) — not required for personal use.

**Future updates** — just click **Force Update** in the Docker tab. The URL is in appdata; Unraid can't lose it. If a release adds new template fields (rare), the release notes will tell you to re-run the curl above; otherwise leave the template alone.

### Option D — `install.sh` one-shot (curl-pipe)

```sh
export HOST_AGENT_IMAGE=ghcr.io/mattjackson/host-agent:latest
export PROMETHEUS_REMOTE_WRITE_URL=https://your.prometheus/api/v1/write
curl -sf https://raw.githubusercontent.com/MattJackson/host-agent/main/install/install.sh | sh
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
> - **All 4 docker mounts are required.** Skipping any of `/`, `/run/docker.sock`, `/run/containerd`, or `/var/lib/docker` silently breaks cadvisor (no container metrics) and gives node-exporter the container's `/proc` view instead of the host's. The minimal `-v /sys:/sys:ro -v /dev:/dev` set is *not* sufficient — it starts, it pushes, but half your panels are empty.
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

Optional — adaptive controller intent (v0.2.0+):

| var | default | what |
|---|---|---|
| `HOST_AGENT_MODE` | unset (v1 fixed targets) | one of `max-cool`, `balanced`, `min-noise`, `eco`. Replaces per-class fixed targets with envelope-derived initials, then drifts them every 10 min toward observed equilibrium. |
| `HOST_AGENT_ADAPTIVE_DISABLED` | `false` | kill-switch — keeps the observer off entirely. Falls back to pure v1 fixed-target behavior. |
| `ADAPTIVE_CYCLE_MINUTES` | `10` | reconcile cadence |
| `OBSERVER_WINDOW_MINUTES` | `120` | rolling sample window per class |

Optional — per-class fan controller overrides (advanced; override the chassis profile AND disable adaptive on that class):

| var | default (from `profiles/default.env`) | what |
|---|---|---|
| `CPU_TARGET` / `CPU_EMERGENCY` / `CPU_DEADBAND` / `CPU_APPROACH_WINDOW` | `70` / `80` / `3` / `10` | CPU class PID |
| `GPU_TARGET` / `GPU_EMERGENCY` / `GPU_DEADBAND` / `GPU_APPROACH_WINDOW` | `83` / `90` / `2` / `7` | passive GPU PID |
| `ACTIVE_GPU_OWN_FAN_THRESHOLD` / `ACTIVE_GPU_EMERGENCY` | `85` / `88` | active GPU (own-fan-driven) assist threshold + temp safety net |
| `HDD_TARGET` / `HDD_EMERGENCY` / `HDD_DEADBAND` / `HDD_APPROACH_WINDOW` | `40` / `50` / `3` / `5` | HDD PID |
| `SSD_TARGET` / `SSD_EMERGENCY` / `SSD_DEADBAND` / `SSD_APPROACH_WINDOW` | `50` / `65` / `5` / `8` | SSD PID |
| `PROFILE` | autodetected | force a profile slug (e.g. `r730xd`); overrides dmidecode |

See `profiles/default.env` for the full reference + tuning notes.

### Server side (your Prometheus)

The receiver needs Prometheus 2.33+ with `--web.enable-remote-write-receiver`, or any compatible TSDB:

- Prometheus
- VictoriaMetrics (single-node or cluster)
- Grafana Mimir
- Cortex
- Grafana Cloud (hosted)

A minimal local setup lives in [`examples/server-side/`](examples/server-side/) — a tiny Prometheus + Grafana compose that takes a fresh user end-to-end in ~5 min. The bundled Prometheus runs with `--web.enable-remote-write-receiver` and `--enable-feature=promql-experimental-functions` (the dashboard uses `sort_by_label` for natural Unraid drive ordering).

## Dashboard

A pre-built Grafana dashboard for host-agent data lives at [`examples/server-side/grafana/dashboards/server-overview.json`](examples/server-side/grafana/dashboards/server-overview.json) (auto-provisioned by the bundled server-side compose; or import via Grafana → Dashboards → New → Import → paste JSON). Sections:

- **Temperatures** — CPU per-socket, IPMI inlet/exhaust, GPU die, per-drive SMART
- **Fan setpoint + RPM** — controller's commanded % vs measured RPM per fan
- **CPU / RAM / disk / network** — standard `node_exporter` panels
- **GPU util / mem / power** — from `nvidia_gpu_exporter`
- **Drives** — per-drive temperature timeseries with Unraid slot labels where applicable
- **Build panel** — which container build + chassis profile each host reports

The dashboard is hardware-agnostic (uses `class=~"cpu1|cpu2"` and `device=~".+"` patterns) so it adapts to whatever each host actually exposes. An Adaptive Controller dashboard row (per-class targets vs envelope, drift events, fit score per mode) lights up when `HOST_AGENT_MODE` is set.

## Fan controller

```
read CPU temps  (coretemp via /sys, IPMI entity 3.x fallback)
read GPU temps  (nvidia-smi), classify passive vs active
read HDD/SSD    (smartctl --scan, then -A -n standby per drive)
                  ├─ HDD: classified by rotation_rate > 0
                  └─ SSD/NVMe: rotation_rate = 0

Re-assert manual fan mode every cycle: iDRAC's third-party PCIe cooling
                response silently flips the BMC back to auto (→ 100% fans)
                within ~30s when a non-Dell GPU/HBA is present. Idempotent
                IPMI call; ~10ms per cycle.

Emergency override: any class >= its class_EMERGENCY ⇒ fans = 100%, stay
                    until ALL classes drop below {emergency - hysteresis}.

Per-class PID candidates (CPU, passive_GPU, HDD, SSD):
    candidate = clamp(current_speed + P*err + D*d_temp,
                      MIN_FAN, MAX_FAN)
    # Asymmetric deadband: positive errors always step up, even inside
    # the deadband. Negative errors inside deadband drift toward MIN_FAN
    # by DEADBAND_DRIFT_RATE per cycle.

Saturation escape (v0.3.7+): when above target AND already at MAX_FAN
                AND temp is not rising, drift candidate DOWN by
                DEADBAND_DRIFT_RATE to probe whether less fan also
                holds equilibrium. Self-correcting — if load really is
                near max cooling capacity, next cycle's P+D pushes
                back up. Pairs with adaptive's saturation penalty so
                target rises to the achievable equilibrium and error
                closes.

Per-class proximity floor (linear ramp from approach_window edge to emergency):
    pf[class] = ramp(class_temp,
                     EMERGENCY - APPROACH_WINDOW,  EMERGENCY,
                     MIN_FAN,                       MAX_FAN)
    # State-free — catches fast spikes the PID step can't react to.

Active-GPU assist (own-fan cards like RTX A5500 — chassis can't cool the
die, only the inlet air; signal is the card's OWN fan saturation, not
die temp):
    if own_fan >= ACTIVE_GPU_OWN_FAN_THRESHOLD:
        assist = ramp(own_fan,
                      ACTIVE_GPU_OWN_FAN_THRESHOLD, 100,
                      MIN_FAN, MAX_FAN)

final_speed = max(cpu_cand, pg_cand, hdd_cand, ssd_cand,
                  cpu_pf,  pg_pf,   hdd_pf,   ssd_pf,
                  ag_assist)
SetFan(final_speed)  # unconditional every cycle, not on change —
                     # BMC's revert-watchdog tracks the SetFan command.

EWMA baseline (persisted to /var/lib/host-agent/state/base):
    base_speed = (1 - α) * base_speed + α * current_speed
    # α = 0.001/cycle → settles over 24-48 hrs of operation.
```

The whole loop runs every `INTERVAL_SEC=15s`. All temperature thresholds, gains, and timing constants come from the chassis profile + class defaults — no hardcoded fan values, no lookup tables, no piecewise rules.

**Adaptive intent layer** (slow loop, every `ADAPTIVE_CYCLE_MINUTES=10`). With `HOST_AGENT_MODE` set, an observer accumulates per-class (temp, fan-demand, inlet) samples over a 120-min rolling window. Each reconcile cycle scores three projected futures (target now / +1°C / -1°C) using the mode's score function against (mean, stddev, fan-change-rate, fan-demand-mean) and drifts target by the winning delta — bounded by the class's envelope `[PreferredLow, PreferredHigh]` (hardware-derived), rate-limited to ±1°C per cycle, and reset to mode-initial on stddev > 5°C. The score functions encode two complementary signals: v0.3.4's PID-engagement relief (raising target reduces variance + fan-change-rate, not just mean) lets adaptive find equilibrium inside the satisficing band, and v0.3.7's quadratic saturation penalty on fan-demand-mean breaks the "fan stuck at 100 but in-band" tie that previously read as "settled" — so target rises promptly when the chassis can't physically meet it, instead of after an hour of drift. State persists to `/var/lib/host-agent/state/adaptive.json` and survives container restarts; the observer window also persists, so a restart doesn't cost a 2-hour warmup. See [`docs/adaptive-controller-v2.md`](docs/adaptive-controller-v2.md) for the full design.

### Per-chassis profiles

`profiles/<model>.env` files are autoloaded by `dmidecode product_name`. Currently shipped:

| chassis | MIN_FAN | tested | notes |
|---|---|---|---|
| `r730xd` | 10 | yes | BMC clamps PWM <10 to a safety RPM (5040). No fan-stall risk. |
| `r730`   | 10 | yes | Same firmware family as R730xd. |
| `r410`   | 20 | yes | BMC obeys low PWM literally — going lower stalls fans. |
| `dell_xc730xd_12` | 10 | yes | Dell XC = Nutanix OEM rebadge of R730xd. Same BMC. |
| `r310` / `r510` / `r610` / `r710` / `r720` / `r720xd` | 20 | no | Conservative — older BMC firmware behavior not probed. |
| `r630` / `r740` / `r740xd` | 20 | no | Conservative — iDRAC9 behavior likely similar to gen 13 but not verified. |
| `default` | 20 | — | Fallback for unknown Dell models. |

Override autodetection with `PROFILE=foo`. Adding a chassis: drop a `profiles/<slug>.env` with `MIN_FAN` and (optionally) the `SENSOR_*` IPMI sensor mapping. No code change needed. See [CONTRIBUTING.md § Adding a chassis profile](CONTRIBUTING.md#adding-a-chassis-profile).

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

## FAQ

**Does it work on non-Dell hardware?**
Yes for everything except fan control. `node_exporter`, `cadvisor`, `smartctl_exporter`, `nvidia_gpu_exporter`, `vmagent`, `unraid-disks`, and `apt-status` all run on any Linux + Docker host. The fan controller refuses to start on non-Dell BMCs (it issues Dell-specific `0x30 0x30` raw IPMI commands and won't try them blindly on other vendors) and `ipmi_exporter` self-disables without `/dev/ipmi0`. Same image, everything else still works.

**Why one container instead of N?**
One image to update, one restart unit, one compose to paste per new host, no per-exporter mount duplication. s6-overlay still supervises each sub-process individually — a crashed exporter is restarted in isolation. Full reasoning in [Architecture rationale](#architecture-rationale).

**How do I add a new chassis profile?**
Drop a file at `profiles/<slug>.env` with at minimum a `MIN_FAN` value, and optionally `SENSOR_*` IPMI sensor mappings for the dashboard's per-CPU/inlet/exhaust panels. No code change needed. Full procedure in [CONTRIBUTING.md § Adding a chassis profile](CONTRIBUTING.md#adding-a-chassis-profile).

**What if my Prometheus needs auth?**
Set `PROMETHEUS_REMOTE_WRITE_BEARER_TOKEN` (for token auth) or the `PROMETHEUS_REMOTE_WRITE_USERNAME`/`_PASSWORD` pair (for HTTP basic). Bearer and basic are mutually exclusive. Self-signed cert? Set `PROMETHEUS_REMOTE_WRITE_TLS_INSECURE_SKIP_VERIFY=true`. See [Configuration](#configuration).

**Will this damage my server's fans / void warranty?**
The controller writes fan PWM via the standard Dell IPMI raw commands (`0x30 0x30 0x01` / `0x30 0x30 0x02`) — the same commands documented for `ipmitool`-based fan control and the same mechanism iDRAC itself uses internally. PWM values are clamped to `[MIN_FAN, MAX_FAN]` (default `20–100`), and per-chassis profiles set a `MIN_FAN` known not to stall fans on that BMC family (R7xx series the BMC clamps low PWM to a safety RPM anyway; R4xx series the profile sets a higher floor). Worst case if the controller crashes or the container exits: it issues a `HandbackAuto` IPMI command on shutdown, and even without that, the BMC re-asserts auto mode within ~30s on its own.

**How much CPU / memory does it use?**
The fan-controller binary is ~2.4 MB and idle CPU is sub-1% on the development hardware. Per-sub-service idle footprint is on the order of single-digit MB resident (`node_exporter`/`cadvisor`/`vmagent` are all Go-static with bounded heaps). Exact numbers depend on number of drives (smartctl scan cost), number of containers (cadvisor watch cost), and scrape interval. Compare to running 6 separate exporter containers — same total cost, one PID 1 supervising it all.

**Can I run two of these per host?**
You wouldn't want to. They'd race on the IPMI fan setpoint (last one to call `SetFan` wins, every 15s, in random order — fans flap), both bind `:9100` / `:8089` / `:9290` / etc. and conflict on host network, and both write to `/var/lib/host-agent/state` and corrupt each other's EWMA. The container is designed to be the one host-agent on a host. For separate metric routing, use vmagent relabeling or a downstream Prometheus federation, not a second container.

**Does it work behind a corporate proxy / air-gapped?**
vmagent inherits `HTTP_PROXY` / `HTTPS_PROXY` / `NO_PROXY` from the container env, so push behind a proxy works. Air-gapped: pre-pull `ghcr.io/mattjackson/host-agent:latest` somewhere with internet, push to your internal registry, and set `HOST_AGENT_IMAGE` to the internal tag. Nothing inside the image reaches out at runtime beyond `remote_write`.

**What about ARM / non-x86?**
Currently `linux/amd64` only — see `.github/workflows/build.yml` and the `tar -xzf ... amd64.tar.gz` calls in the Dockerfile. The Go binary itself is trivially cross-compilable, but the bundled upstream exporters (`node_exporter`, `cadvisor`, `ipmi_exporter`, `smartctl_exporter`, `nvidia_gpu_exporter`, `vmagent`) are fetched as amd64 release tarballs. PRs welcome to multi-arch the fetch step.

**Why no `go.sum`?**
Zero external Go dependencies by design. The fan controller is stdlib only, so there's nothing to lock. See [CONTRIBUTING.md § What's out of scope](CONTRIBUTING.md#whats-out-of-scope) — adding `import "github.com/..."` needs a strong justification, and `go.sum` will only appear in the repo if that happens.

## Comparison vs alternatives

| | Dell stock iDRAC curve | Custom shell + cron | `node_exporter` alone | Prometheus + N exporters | **host-agent** |
|---|---|---|---|---|---|
| Adaptive PID over time | no | rarely | n/a | n/a | yes (EWMA + v2 intent layer) |
| Per-class control (CPU/GPU/HDD/SSD) | no — single chassis curve | maybe | n/a | n/a | yes |
| Auto-detect chassis | n/a (in BMC) | no | n/a | n/a | yes (dmidecode → profile) |
| Safety floor (proximity-to-emergency) | n/a | rarely | n/a | n/a | yes (per-class ramp) |
| Bundled metrics (node + cadvisor + ipmi + smart + nvidia) | none | usually none | partial (no GPU, no drives, no chassis) | yes (after wiring N containers) | yes |
| One container per host | n/a | n/a (cron script) | yes | no (5-6 containers) | yes |
| Self-disables on missing hardware | n/a | rarely | n/a | per-container, manual | yes (same image runs anywhere) |
| Survives container restart cleanly | n/a | per-script | yes | yes | yes (EWMA + observer persist) |
| Operational overhead per new host | none | per-host script tweak | one compose | N composes + N image bumps | one compose, one image bump |
| OSS | n/a (vendor firmware) | usually | yes (Apache 2) | yes (Apache 2) | yes (MIT) |
| No extra software | yes | no | no | no | no |

Where stock iDRAC wins: zero install footprint and no operator responsibility. Where it loses: fixed thermal table, no adaptation to the actual chassis airflow or the actual workload pattern, ramps loud the moment a third-party PCIe card confuses its thermal table (the "100% fans randomly" iDRAC problem this project was built to fix).

## Architecture rationale

The cloud-native default is one container per concern. For a single-node fleet that pattern is mostly overhead:

- **One image to update.** Bump the tag → every host's Watchtower picks it up. No 5x image pulls, no drift between containers.
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

**Adaptive state** (only when `HOST_AGENT_MODE` is set):

```sh
sudo cat /var/lib/host-agent/state/adaptive.json
# per-class target + deadband, with last-change direction
```

To reset adaptive targets to mode-initial (e.g. after upgrading or switching modes):

```sh
sudo docker stop host-agent
sudo rm /var/lib/host-agent/state/adaptive.json
sudo docker start host-agent
```

The observer window (`observer.json`) is mode-agnostic and does not need to be cleared.

**Per-host tuning** (advanced): drop env vars in `.env` next to the compose (e.g. `CPU_TARGET=72` to run quieter). Container env beats profile beats default. Most users should never need this — the chassis profile auto-loads sane defaults, and `HOST_AGENT_MODE` covers the "I want quieter / cooler" intent without per-class numbers.

## Development

See [CONTRIBUTING.md](CONTRIBUTING.md). TL;DR:

```sh
git clone https://github.com/MattJackson/host-agent
cd host-agent
go test ./...                 # unit + e2e tests; no Docker needed
docker build -t host-agent:dev .
```

The fan controller is pure Go with zero external dependencies — the build is reproducible, the binary is ~2.4 MB, all logic is unit-testable as functions of inputs.

## License

[MIT](LICENSE) © Matthew Jackson
