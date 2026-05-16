# host-agent

Per-host bundle: **one container**, s6-supervised, that runs the Dell
PowerEdge fan controller AND the per-host Prometheus exporters. The
same image drops onto every Linux Docker host — Dell or not, GPU or
not, Ubuntu or Unraid — and each sub-service probes its hardware on
start, gracefully `sleep infinity`'ing if its prerequisites aren't
present. From the moment a host has this compose in place, every
future update flows via watchtower bumping the image tag. No further
per-host action.

## What's inside

| sub-service | port | runs when | otherwise |
|---|---|---|---|
| `fan-controller` | — | `/dev/ipmi0` exists AND BMC is Dell | sleep |
| `node_exporter` | 9100 | always | always |
| `cadvisor` | 8089 | `/var/run/docker.sock` mounted | sleep |
| `ipmi_exporter` | 9290 | `/dev/ipmi0` exists | sleep |
| `smartctl_exporter` | 9633 | always (auto-discovers) | always |
| `nvidia_gpu_exporter` | 9835 | `nvidia-smi` is present | sleep |

`node_exporter`'s `--collector.textfile.directory` is `/var/lib/fan-controller/state`,
which is where the fan controller writes its own `metrics.prom` each
cycle — so controller state (setpoints, per-class temps, EWMA baseline,
binding source) is exposed to Prometheus through node-exporter on the
same :9100 endpoint.

## Compose (paste this on every host)

```yaml
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
      - PROMETHEUS_REMOTE_WRITE_URL=${PROMETHEUS_REMOTE_WRITE_URL:?PROMETHEUS_REMOTE_WRITE_URL must be set}
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
      - /var/lib/fan-controller:/var/lib/fan-controller
    labels:
      - com.centurylinklabs.watchtower.enable=true
```

**Required env**:

| var | what it is |
|---|---|
| `HOST_AGENT_IMAGE` | image to pull, e.g. `ghcr.io/<user>/host-agent:latest` |
| `PROMETHEUS_REMOTE_WRITE_URL` | your receiver's `/api/v1/write` endpoint |

**GPU hosts**: set `HOST_AGENT_RUNTIME=nvidia` so the NVIDIA Container
Runtime injects `nvidia-smi` at start. Default `runc` is harmless on
hosts without nvidia-container-toolkit.

**Auth** (optional; bearer XOR basic):

```sh
# Bearer (preferred for Grafana Cloud, hosted Prometheus, etc.)
PROMETHEUS_REMOTE_WRITE_BEARER_TOKEN=...

# OR HTTP basic auth
PROMETHEUS_REMOTE_WRITE_USERNAME=...
PROMETHEUS_REMOTE_WRITE_PASSWORD=...

# Self-signed TLS on the receiver
PROMETHEUS_REMOTE_WRITE_TLS_INSECURE_SKIP_VERIFY=true
```

The receiver needs Prometheus 2.33+ run with
`--web.enable-remote-write-receiver`, or any compatible TSDB
(VictoriaMetrics, Grafana Mimir, Cortex, Grafana Cloud).

**One-time host setup**: load the IPMI kernel module on Dell hosts, and
make sure `/var/lib/fan-controller` exists. `infra/host-requirements.sh`
does this — DR runs it automatically; on a new host run it once by
hand.

## Why one container with sub-processes

The cloud-native default is one container per concern (the
sidecar-per-exporter pattern monitoring stacks usually ship as). For a
bare-metal, single-node fleet that pattern is mostly overhead:

- **One image to update.** Bump the image once → every host's
  watchtower picks it up. No 5× the image pulls, no drift between
  containers.
- **One restart unit per host.** s6 supervises each sub-process
  individually — a crashed exporter is restarted in isolation without
  taking down its siblings — but the operator-facing unit is a single
  container.
- **One compose to paste.** Adding a new host = paste 15 lines, done.
  Adding a new sub-process to the stack = no compose change on any
  host.
- **No cross-container coordination.** Sub-services share the
  container's view of `/dev`, `/sys`, `/proc`, `/var/run/docker.sock`
  — no per-exporter mount duplication.

Trade-off: a leaking sub-process shares its memory pressure with the
others (the container has unified cgroups). For these particular
workloads — all of them sit <1% CPU and <50 MB resident at idle —
that's a non-issue in practice.

## Build / release

`.github/workflows/build-host-agent.yml` builds + pushes the image on
every push under `host-agent/**`. Weekly cron rebuild picks up
base-image and upstream exporter security fixes. No manual `build.sh`.

Pinned upstream versions are in the `ARG ...` lines at the top of the
Dockerfile — bump those and commit to roll forward.

## Fan controller (sub-service detail)

Replaces iDRAC's automatic curve with three independent per-class PIDs
(CPU + passive GPU + HDD); the worst-offender class wins each cycle.
Self-tunes its baseline (EWMA persisted to
`/var/lib/fan-controller/state/`) over 24–48 hrs.

Inner loop runs every `INTERVAL=15s`:

```
read CPU temps (coretemp via /sys, fallback to IPMI entity 3.x)
read GPU temps (nvidia-smi), classify each: passive vs active
  - passive (no own fan, e.g. Tesla P4) → its own PID
  - active  (own fan, e.g. RTX A5500)   → assist + emergency, NOT a PID
read HDD temps (smartctl --scan + smartctl -A -n standby per drive)
  - cached every HDD_READ_INTERVAL (60s), drives in standby are skipped

# Per-class emergency: hitting any class's EMERGENCY → fans 100%.
if cpu_max         ≥ CPU_EMERGENCY            (80°C)
or passive_gpu_max ≥ GPU_EMERGENCY            (85°C)
or active_gpu_max  ≥ ACTIVE_GPU_EMERGENCY     (88°C)
or hdd_max         ≥ HDD_EMERGENCY            (50°C): fans → 100%

# Three independent PIDs, each producing a candidate fan speed.
for class in {cpu, passive_gpu, hdd}:
    candidate = clamp(current + P×err + D×d_temp, MIN_FAN, MAX_FAN)
    # asymmetric deadband: positive errors always step up, even
    # within the deadband; negative errors in deadband drift toward
    # the EWMA base. Cooldown bias — a heat-soaked chassis stays
    # loud until it actually cools to target.

# Active-GPU assist: chassis can't cool the die directly, but it
# can lower the air the GPU's own fan pulls in.
if active_gpu_max > ACTIVE_GPU_TARGET:
    assist_floor = MIN_FAN + (active_gpu_max - ACTIVE_GPU_TARGET) × ASSIST_GAIN

# Per-class proximity floor: silent until temp is within
# class_APPROACH_WINDOW of class_EMERGENCY, then ramps to MAX_FAN.
# Catches fast spikes the proportional loop can't react to.
for class in {cpu, passive_gpu, active_gpu, hdd}:
    proximity_floor[class] = linear_ramp(class_max, class_EMERGENCY - WINDOW, class_EMERGENCY)

new_speed = max(cpu_candidate, pg_candidate, hdd_candidate,
                cpu_pf, pg_pf, ag_pf, hdd_pf, ag_assist)
```

### Per-chassis profiles

`profiles/<model>.env`, auto-loaded by `dmidecode` product_name. Model
profile + `default.env` use `: "${VAR:=value}"` — container env wins
over profile, profile wins over default.

```
profiles/
├── default.env    # conservative fallback for unknown Dell models
├── r730xd.env     # MIN_FAN=10 (BMC clamps lower values to safety RPM)
└── r410.env       # MIN_FAN=20 (BMC obeys lower values literally → fan stall)
```

Override autodetection with `PROFILE=foo` env var.

### BMC quirks worth knowing

**R730xd:** Dell BMC clamps PWM <10% to a safety RPM (5040). Going
lower has no effect. No fan-stall risk at low values.

**R410:** Dell BMC obeys low PWM literally. Probed values:
- 5% → 0 RPM (fans STOP)
- 10% → 840 RPM (suspicious, possibly partial stall)
- 15% → 2520 RPM (BMC flags Lower Critical alarm)
- 20% → 3600 RPM (operational floor)

R410 must stay at MIN_FAN=20.

## Operational

**Logs** (interleaved by sub-service prefix):
```sh
sudo docker logs host-agent --tail 50 -f
```

**Controller state**:
```sh
sudo cat /var/lib/fan-controller/state/base
# base_speed=22.4515   ← EWMA of equilibrium fan speed
# last_speed=45        ← actual fan speed at last write
# samples=109          ← cycles since restart
# last_updated=…
```

**Per-host tuning override**: drop env vars in `infra/.env`
(e.g. `CPU_TARGET=72` to run quieter). Container env beats profile
beats default.

## Source layout

```
host-agent/
├── README.md                    # this file
├── Dockerfile                   # multi-stage: go-builder + exporter-fetch + s6-overlay
├── go.mod                       # zero external deps; vendor-free build
├── cmd/fan-controller/               # controller entry point
├── internal/                    # PID/proximity/EWMA + sensors + IPMI + state + metrics
├── profiles/                    # per-chassis tuning (r730xd, r730, r410, xc730xd-12, default)
├── s6/                          # s6-overlay service tree
│   ├── fan-controller/               (controller)
│   ├── node-exporter/           (:9100)
│   ├── cadvisor/                (:8089)
│   ├── ipmi-exporter/           (:9290)
│   ├── smartctl-exporter/       (:9633)
│   ├── nvidia-gpu-exporter/     (:9835)
│   ├── vmagent/                 (push to central Prometheus)
│   └── user/contents.d/         (enables each service in default bundle)
└── infra/
    ├── docker-compose.yml       # single service, paste-anywhere
    ├── deploy.sh                # thin wrapper for reconcile timer
    └── host-requirements.sh     # one-time host setup (ipmi_devintf, state dir)
```
