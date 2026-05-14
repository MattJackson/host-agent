# dell-fans

Dell PowerEdge IPMI raw fan controller. Replaces iDRAC's automatic curve
with a setpoint controller that finds the quietest fan speed which holds
chassis-cooled temps at TARGET. Self-tunes its baseline over 24-48 hrs
and persists across restarts.

Runs on classe (R730xd) and docker-2 (R410). Same image works on any
Dell PowerEdge that accepts the `raw 0x30 0x30 0x01/0x02` Dell fan
commands. Refuses to start on non-Dell BMCs.

## Why this instead of iDRAC

iDRAC's automatic curve is conservative and noisy — it ramps fans
aggressively for component temps that are nowhere near limit. The static
script that lived at `/usr/local/bin/dell-fan-controller.sh` for a year
was quieter but didn't adapt: same threshold table whether it's January
or August, AI lane idle or under load. This stack does both:

- **Quieter than iDRAC**: holds chassis-cooled max-temp at TARGET (default
  70°C), drops fan speed proportionally when it's cooler.
- **Adapts**: EWMA of equilibrium fan speed is the new "base" — over
  24-48 hrs the controller learns the speed needed for current
  conditions (ambient temp, drive count, dust). Add a HDD next month →
  base creeps up automatically.
- **Per-chassis tuned**: each Dell model has different fan-stall
  thresholds, BMC behavior, sensor layouts. Profile system handles that
  with one file per model.

## Control logic

Three independent PIDs (CPU, passive GPU, HDD) each compute a candidate
fan speed every cycle. The chassis fans are driven by the **max** across
all candidates plus per-class proximity floors plus active-GPU assist —
the worst-offender class wins, without coupling its state to the others.

Inner loop runs every `INTERVAL=15s`:

```
read CPU temps (coretemp via /sys/class/hwmon, fallback to IPMI entity 3.x)
read GPU temps (nvidia-smi if present), classify each: passive vs active
  - passive (no own fan, e.g. Tesla P4) → its own PID
  - active  (own fan, e.g. RTX A5500)   → assist + emergency (NOT a PID)
read HDD temps (smartctl --scan + smartctl -A -n standby per drive)
  - cached, refreshed every HDD_READ_INTERVAL (60s default)
  - SCSI/SATA/NVMe — three parse formats handled
  - drives in standby are skipped, not woken

cpu_max         = max(CPU cores)
passive_gpu_max = max(passive GPU dies)
active_gpu_max  = max(active GPU dies)
hdd_max         = max(HDD temps)

# Per-class emergency: any class hitting its own EMERGENCY → 100%
if cpu_max         ≥ CPU_EMERGENCY            (80°C)
or passive_gpu_max ≥ GPU_EMERGENCY            (85°C)
or active_gpu_max  ≥ ACTIVE_GPU_EMERGENCY     (88°C)
or hdd_max         ≥ HDD_EMERGENCY            (50°C):
    fans → 100%
    on next non-emergency cycle: snap fan to max(base, proximity floors)

# Three independent PIDs, each producing a candidate fan speed.
for class in {cpu, passive_gpu, hdd}:
    err = class_max - class_TARGET
    d_temp = class_max - last_cycle_class_temp   # 0 on first cycle
    if err ≤ 0 and |err| ≤ class_DEADBAND:
        # In deadband + at-or-below target: candidate drifts toward
        # learned base_speed by DEADBAND_DRIFT_RATE per cycle.
        candidate = current ± DEADBAND_DRIFT_RATE (toward base, capped at base)
    else:
        # Above target OR well below: P+D step from current_speed.
        # P reacts to distance from target; D reacts to rate-of-change.
        # Asymmetric deadband: positive errors ALWAYS take this branch,
        # even within the deadband. Cooldown bias — a heat-soaked
        # chassis stays loud until it actually cools to target, not
        # just until temp stops climbing.
        step = round(err × FAN_GAIN + d_temp × DERIVATIVE_GAIN)
        candidate = clamp(current + step, MIN_FAN, MAX_FAN)

# Active-GPU assist: chassis fans can't cool an active GPU's die directly,
# but they can lower the air the GPU's own fan pulls in.
if active_gpu_max > ACTIVE_GPU_TARGET:
    assist_floor = MIN_FAN + round((active_gpu_max - ACTIVE_GPU_TARGET) × ASSIST_GAIN)

# Per-class proximity floor — see below.
for class in {cpu, passive_gpu, active_gpu, hdd}:
    proximity_floor[class] = (linear ramp from MIN_FAN to MAX_FAN as
                              class_max climbs the last <class>_APPROACH_WINDOW
                              degrees up to class_EMERGENCY)

new_speed = max(cpu_candidate, pg_candidate, hdd_candidate,
                cpu_pf, pg_pf, ag_pf, hdd_pf, ag_assist)
```

With `ASSIST_GAIN=3` and `ACTIVE_GPU_TARGET=78`: A5500 at 82°C → assist
floor 22%, at 85°C → 31%, at 88°C → emergency (100%). If any other
class's PID or proximity floor already wants more than the assist
floor, the assist is a no-op.

**Proximity-to-emergency floor** (per class, with per-class window):

```
for each class with its own EMERGENCY and APPROACH_WINDOW:
    if temp ≤ EMERGENCY - APPROACH_WINDOW:
        floor = MIN_FAN
    else:
        floor = MIN_FAN + ((temp - (EMERGENCY - WINDOW)) / WINDOW) × (MAX_FAN - MIN_FAN)
```

For P4 (passive, EMERGENCY=85, WINDOW=10): silent ≤75°C, then 10% →
55% as temp climbs 75→80, → 91% at 84°C, → emergency 100% at 85°C.
For HDDs (EMERGENCY=50, WINDOW=5, narrower because drives operate
closer to their limit in normal use): silent ≤45°C, then ramps to
MAX_FAN at 50°C. Solves the spike problem: the PID's P term reacts
to "how far over TARGET" (and FAN_GAIN=0.5 only gives +5% per +10°C
of error — useless when P4 spikes +14°C in one cycle); the D term
helps but is capped by sensor cadence. The proximity floor commits
chassis based on remaining safety budget, independent of rate-of-change.
Sustained events get sustained cooling, not click-on/click-off
oscillation.

Emergency exit uses the proximity floor too: `exit_speed = max(base,
all_proximity_floors)` instead of just `base`. Otherwise we'd free-fall
to 29% with P4 still at 84°C and re-trip emergency 15s later.

Outer (every cycle, slow): `base = (1 - α) × base + α × current_speed`
with `ADAPT_ALPHA=0.001`. State (base, last_speed, samples, timestamp)
persisted to `/var/lib/dell-fans/state/base` every 60s. On restart,
controller resumes from last_speed, not base — avoids cold-start
overshoot during transients (e.g. emergency-then-recreate scenarios).

## Why per-class targets

Different device classes have different "comfortable" temps and
different relationships to chassis airflow. A CPU at 74°C and a P4 at
74°C and an HDD at 74°C look the same to a single-target controller,
but the CPU is fine (TJ ~85-95°C), the P4 is approaching its 91°C
ceiling, and the HDD is already 14°C past its manufacturer max.

- **CPU** (`CPU_TARGET=70`, `CPU_EMERGENCY=80`, `CPU_APPROACH_WINDOW=10`):
  Xeon E5-2600 v3/v4 TJ-throttle at 85-95°C. Fleet idles 45-58°C under
  qwen load — 80 means actual trouble.
- **Passive GPU** (`GPU_TARGET=72`, `GPU_EMERGENCY=85`,
  `GPU_APPROACH_WINDOW=10`): own PID because chassis fans are the only
  cooling. P4 ceiling is 91°C; 85 leaves ~6°C buffer.
- **HDD** (`HDD_TARGET=40`, `HDD_EMERGENCY=50`, `HDD_APPROACH_WINDOW=5`):
  own PID. Defaults sit inside the research consensus — Google FAST 2007
  found optimal failure rates in the 37-46°C band; U. Virginia /
  Microsoft 2013 found failure rate ~3× higher at 50°C vs 27°C and
  recommends not steadily exceeding 47°C; enterprise drive specs
  (Seagate Exos, WD RE) max at 55-60°C. Narrower approach window (5°C)
  because drives operate naturally close to their emergency — a wider
  window would have the proximity floor active during normal operation.
- **Active GPU** (`ACTIVE_GPU_TARGET=78`, `ACTIVE_GPU_EMERGENCY=88`,
  `ACTIVE_GPU_APPROACH_WINDOW=10`): excluded from the PID set because
  chassis fans can't cool an active die directly. Instead, exceeding
  `ACTIVE_GPU_TARGET` lifts the chassis floor proportionally
  (`ASSIST_GAIN=3` % per °C) so the GPU's own fan pulls cooler air.
  A5500 normally operates 75-85°C under qwen load (TJ-throttle ~93°C);
  78 means "starting to work hard," 88 leaves 5°C of TJ buffer for the
  chassis to bail it out. Separate from passive thresholds so normal
  A5500 inference doesn't trip the P4's 85°C emergency.

If active GPU temps drove a PID, chassis fans would pin at MAX trying
to chase a target they can't reach (the GPU's own fan does the actual
cooling). The assist band gives us the realistic version: help when we
can, don't pretend chassis fans cool an A5500 directly.

## Per-chassis profiles

`profiles/<model>.env`, auto-loaded at startup based on
`/sys/class/dmi/id/product_name`. Model profile + `default.env` use
`: "${VAR:=value}"` — container env wins over profile, profile wins over
default.

```
/etc/dell-fans/profiles/
├── default.env    # conservative fallback for any unknown Dell
├── r730xd.env     # MIN_FAN=10 (BMC clamps lower values to safety RPM)
└── r410.env       # MIN_FAN=20 (BMC obeys lower values literally → fan stall)
```

Adding a new chassis: drop `profiles/<lowercase model>.env` containing
overrides, rebuild, redeploy. Model name comes from
`cat /sys/class/dmi/id/product_name | tr 'A-Z' 'a-z'` with `PowerEdge_`
prefix stripped. Override autodetection by setting `PROFILE=foo` env var
in compose.

## BMC quirks worth knowing

**R730xd:** Dell BMC clamps PWM <10% to a safety RPM (5040). Going
lower has no effect — true minimum is 10% / 3840 RPM. **No fan-stall
risk** at low values.

**R410:** Dell BMC obeys low PWM literally. Probed values:
- 5% → **0 RPM (fans STOP, no airflow)**
- 10% → 840 RPM (suspicious, possibly partial stall)
- 15% → 2520 RPM (BMC flags "lcr" Lower Critical, IPMI returns errors)
- 20% → 3600 RPM (operational floor — BMC ok)

R410 must stay at MIN_FAN=20. Lower values risk stall AND trigger BMC
alarms that flood IPMI. This is why per-chassis profiles exist.

## Operational

**Build + push image** (only needed when `dell-fan-controller.sh`,
profiles, or Dockerfile change):

```sh
ssh docker
cd /srv/dell-fans
sudo bash build.sh
```

Builds locally on classe, pushes to `registry.docker.pq.io/dell-fans:latest`.
Watchtower on each host (`com.centurylinklabs.watchtower.enable=true`)
auto-pulls within its poll interval.

**Deploy / redeploy** (idempotent, run by `reconcile.sh` every 30 min):

```sh
ssh docker
sudo bash /srv/dell-fans/infra/deploy.sh
```

Or on docker-2 directly. Deploy script auto-detects GPU presence on the
host and links `docker-compose.override.yml → docker-compose.gpu.yml`
when nvidia-smi is available, so the same source set works on
GPU-equipped (classe) and headless (docker-2) hosts.

**Per-host tuning override**: drop env vars in `/srv/dell-fans/infra/.env`
(e.g. `CPU_TARGET=72` or `GPU_TARGET=68` to run a specific host
quieter or cooler). Container env
beats both profile and default. Will require touch + redeploy.

**State persistence**: `/var/lib/dell-fans/state/base`. Bind-mounted, so
survives container restarts and image updates. Loss of this file is
non-critical — controller relearns over 24-48 hrs from MIN_FAN. Not
backed up (label-discovered backups skip direct bind mounts).

**Reading state**:
```sh
sudo cat /var/lib/dell-fans/state/base
# base_speed=22.4515   ← EWMA of equilibrium fan speed
# last_speed=45        ← actual fan speed at last write
# samples=109          ← cycles since restart
# last_updated=2026-05-10T21:28:02Z
```

**Logs**:
```sh
sudo docker logs dell-fans --tail 20 -f
```

Each cycle: per-sensor temp readings, setpoint/active_gpu summary,
current fan %, status (`ok`, `up→N%`, `dn→N%`, `at MIN_FAN`, `EMERGENCY`),
running base.

## What kills it / what to check first

- **Fans pinned at 100%**: container is in safety mode (temp read
  failed) or emergency. `docker logs` shows which. If "Temp read
  failed", `/dev/ipmi0` mapping or `ipmi_devintf` module is broken.
  If "EMERGENCY", read the temps in the same line.
- **Won't start, "FATAL: not a Dell BMC"**: vendor guard fired.
  Container doesn't run on Supermicro/HP — by design.
- **Won't start, "Manufacturer Name" empty**: `/dev/ipmi0` missing.
  Run `host-requirements.sh` to load `ipmi_devintf` and persist.
- **GPU lane shows `Ga0:NN@MM%` but you don't have an active GPU**:
  `nvidia-smi --query-gpu=fan.speed` returned a number for that GPU.
  Verify with `docker exec dell-fans nvidia-smi --query-gpu=index,name,fan.speed --format=csv`.
- **"HDD monitoring disabled" but you have drives**: container can't
  see drives. Verify `/dev` is bind-mounted in compose, and from inside
  the container `docker exec dell-fans smartctl --scan` lists them.
  PERC-backed drives appear as `/dev/bus/0 -d megaraid,N`; SATA as
  `/dev/sdX -d scsi`; NVMe as `/dev/nvme0 -d nvme`.
- **HDD reads show `d0:?`**: smartctl ran but no temperature parsed.
  Run `docker exec dell-fans smartctl -A -d <spec> <dev>` to see the
  raw output; the parser handles SCSI/SATA-194/NVMe but a controller
  reporting in an unusual format would fall through. Open an issue
  with that output.
- **HDD reads show `d0:zZ`**: drive is in standby. By design — we use
  `-n standby` to avoid waking drives just to read temps. They'll be
  read again next refresh once active.

## Source layout

```
dell-fans/
├── README.md                    # this file
├── Dockerfile                   # debian:stable-slim + ipmitool + smartmontools + tini + mawk
├── dell-fan-controller.sh       # the controller (one bash script, ~3 PIDs)
├── build.sh                     # docker build + push to registry
├── profiles/
│   ├── default.env              # conservative fallback (incl. HDD class + per-class windows)
│   ├── r730xd.env               # R730xd-specific overrides
│   └── r410.env                 # R410-specific overrides
└── infra/
    ├── docker-compose.yml       # base, no GPU (mounts /dev for smartctl)
    ├── docker-compose.gpu.yml   # GPU overlay (nvidia runtime + visible_devices)
    ├── deploy.sh                # idempotent, links override.yml conditionally
    └── host-requirements.sh     # modprobe ipmi_devintf, persist, mkdir state
```
