#!/bin/bash
# Dell PowerEdge adaptive fan controller (containerized).
#
# Three independent PIDs (CPU, passive GPU, HDD) each produce a candidate
# fan speed; max() across all candidates + per-class proximity floors +
# active-GPU assist drives the chassis fans. The worst-offender class wins
# every cycle without coupling its state to the others.
#
#   - CPU PID: chases CPU_TARGET, P+D on max core temp.
#   - Passive GPU PID: chases GPU_TARGET, P+D on max passive GPU die
#     (e.g. Tesla P4 — no own fan, chassis-cooled).
#   - HDD PID: chases HDD_TARGET, P+D on max drive temp from smartctl
#     (cached, refreshed every HDD_READ_INTERVAL seconds).
#   - Active-GPU assist: cards with their own fan (e.g. A5500) can't be
#     cooled by chassis fans directly — instead, exceeding
#     ACTIVE_GPU_TARGET lifts the chassis floor proportionally so the
#     card's own fan pulls cooler intake air. Stays as assist (not a PID)
#     because chassis fans don't cool the active die.
#   - Per-class proximity floor: silent until temp is within
#     <class>_APPROACH_WINDOW of that class's EMERGENCY, then ramps
#     linearly to MAX_FAN. Catches fast spikes the proportional loop
#     can't react to in time.
#   - Outer loop: EWMA of fan speed → "base" persisted to disk. On
#     restart, controller resumes from last_speed (recent operating
#     point) — adapts to chassis condition changes (added drive,
#     ambient drift, dust, etc) over ~24-48 hrs without manual retuning.
#   - Emergency: any class >= its own EMERGENCY threshold → instant 100%,
#     bypasses everything else.
#
# Temp sources:
#   - /sys/class/hwmon/* coretemp (Intel CPU die temps, all cores)
#   - IPMI entity 3.x (Processor) — fallback if coretemp unavailable
#   - nvidia-smi (all GPUs) — if GPU_AWARE=auto/true and runtime present.
#     Per-GPU classification by fan.speed reporting:
#       passive (no own fan, e.g. Tesla P4) → drives PID
#       active  (own fan,    e.g. RTX A5500) → drives assist + emergency
#   - smartctl --scan + smartctl -A -n standby (per drive) — if
#     HDD_AWARE=auto/true and smartmontools available. Skips drives in
#     standby (doesn't wake them). Handles SCSI (Current Drive
#     Temperature), SATA (attribute 194), and NVMe (Temperature: N Celsius).
#
# Profile system: /etc/dell-fans/profiles/<model>.env auto-loaded based
# on dmidecode product_name. PROFILE env var overrides. Falls back to
# default.env. Container env vars (compose) override profile values.
#
# Refuses to start on non-Dell BMCs (raw 0x30 0x30 commands are Dell-only).

set -u

PROFILE_DIR=/etc/dell-fans/profiles
STATE_DIR=/var/lib/dell-fans/state
STATE_FILE="$STATE_DIR/base"
METRICS_FILE="$STATE_DIR/metrics.prom"
METRICS_TMP="$STATE_DIR/.metrics.prom.tmp"
PERSIST_INTERVAL=60    # write state every 2 cycles. State file is tiny.
                       # Frequent writes ensure container restarts resume
                       # from a recent operating point, not stale state.

GPU_AWARE="${GPU_AWARE:-auto}"
GPU_ENABLED=0
HDD_AWARE="${HDD_AWARE:-auto}"
HDD_ENABLED=0
HDD_DEVICES=()         # entries are "<dev>|<smartctl -d spec>"
hdd_last_read=0
hdd_cached_max=0
hdd_cached_details=""

# Internal state.
current_speed=0
base_speed=0
samples=0
last_persist=0
in_emergency=0

# D-term state: previous-cycle temp per class. -1 = no prior reading
# (first cycle after restart), in which case d_temp is forced to 0.
# No last_ag_temp — active GPU is assist-only, not a PID.
last_cpu_temp=-1
last_pg_temp=-1
last_hdd_temp=-1

log() { echo "$(date '+%Y-%m-%d %H:%M:%S') - $1"; }

vendor_guard() {
    local vendor
    vendor=$(ipmitool mc info 2>/dev/null | awk -F': ' '/Manufacturer Name/{print $2; exit}')
    if [ -z "$vendor" ]; then
        log "FATAL: ipmitool mc info returned no Manufacturer Name. Is /dev/ipmi0 mapped in?"
        exit 1
    fi
    case "$vendor" in
        *Dell*) log "Vendor: $vendor" ;;
        *) log "FATAL: not a Dell BMC ($vendor). Refusing to issue Dell raw fan commands."; exit 1 ;;
    esac
}

detect_model() {
    # /sys/class/dmi/id/product_name → "PowerEdge R730xd"
    # → strip "PowerEdge ", lowercase → "r730xd"
    local raw
    raw=$(cat /sys/class/dmi/id/product_name 2>/dev/null | tr -d '\n')
    if [ -z "$raw" ]; then
        echo "unknown"
        return
    fi
    raw="${raw#PowerEdge }"
    echo "$raw" | tr 'A-Z' 'a-z' | tr -c 'a-z0-9' '_' | sed 's/_*$//'
}

load_profile() {
    local model="${PROFILE:-$(detect_model)}"
    local file="$PROFILE_DIR/$model.env"
    if [ -f "$file" ]; then
        log "Loading profile: $model"
        set -a; . "$file"; set +a
    else
        log "No profile for '$model' — using default"
    fi
    # Default profile fills in anything still unset (uses :=).
    set -a; . "$PROFILE_DIR/default.env"; set +a
    log "Active: CPU target=${CPU_TARGET}±${CPU_DEADBAND} emerg=${CPU_EMERGENCY}°C win=${CPU_APPROACH_WINDOW} | GPU(passive) target=${GPU_TARGET}±${GPU_DEADBAND} emerg=${GPU_EMERGENCY}°C win=${GPU_APPROACH_WINDOW} | GPU(active) assist=${ACTIVE_GPU_TARGET} emerg=${ACTIVE_GPU_EMERGENCY}°C win=${ACTIVE_GPU_APPROACH_WINDOW} | HDD target=${HDD_TARGET}±${HDD_DEADBAND} emerg=${HDD_EMERGENCY}°C win=${HDD_APPROACH_WINDOW} read=${HDD_READ_INTERVAL}s | FAN=${MIN_FAN}-${MAX_FAN}% P=${FAN_GAIN} D=${DERIVATIVE_GAIN} ASSIST_GAIN=${ASSIST_GAIN} DRIFT=${DEADBAND_DRIFT_RATE}%/cyc INTERVAL=${INTERVAL}s ALPHA=${ADAPT_ALPHA}"
}

probe_gpu() {
    case "$GPU_AWARE" in
        false) GPU_ENABLED=0; log "GPU monitoring disabled (GPU_AWARE=false)"; return ;;
        true)
            if ! command -v nvidia-smi >/dev/null 2>&1 || ! nvidia-smi -L >/dev/null 2>&1; then
                log "FATAL: GPU_AWARE=true but nvidia-smi not usable in container"
                exit 1
            fi
            GPU_ENABLED=1
            log "GPU monitoring: $(nvidia-smi --query-gpu=name --format=csv,noheader | paste -sd, -)"
            ;;
        auto|*)
            if command -v nvidia-smi >/dev/null 2>&1 && nvidia-smi -L >/dev/null 2>&1; then
                GPU_ENABLED=1
                log "GPU detected: $(nvidia-smi --query-gpu=name --format=csv,noheader | paste -sd, -)"
            else
                GPU_ENABLED=0
                log "No GPU detected (CPU-only mode)"
            fi
            ;;
    esac
}

# Discover drives via `smartctl --scan` once at startup. Scan output:
#   /dev/sda     -d scsi         # /dev/sda, SCSI device
#   /dev/bus/0   -d megaraid,0   # /dev/bus/0 [megaraid_disk_00], SCSI device
#   /dev/nvme0   -d nvme         # /dev/nvme0, NVMe device
# We keep "<dev>|<spec>" entries so the per-cycle read can `smartctl -A
# -d <spec> <dev>` each one. Refreshing the scan at runtime is not worth
# the complexity — hot-plug HDDs in a server are rare and a controller
# restart picks them up.
probe_hdd() {
    case "$HDD_AWARE" in
        false) HDD_ENABLED=0; log "HDD monitoring disabled (HDD_AWARE=false)"; return ;;
        true)
            if ! command -v smartctl >/dev/null 2>&1; then
                log "FATAL: HDD_AWARE=true but smartctl not available"
                exit 1
            fi
            ;;
        auto|*)
            if ! command -v smartctl >/dev/null 2>&1; then
                HDD_ENABLED=0
                log "No smartctl — HDD monitoring disabled"
                return
            fi
            ;;
    esac

    local scan
    scan=$(smartctl --scan 2>/dev/null)
    if [ -z "$scan" ]; then
        HDD_ENABLED=0
        log "smartctl --scan returned nothing — HDD monitoring disabled"
        return
    fi

    while IFS= read -r line; do
        [ -z "$line" ] && continue
        # Strip trailing comment.
        line="${line%%#*}"
        local dev spec
        dev=$(echo "$line"  | awk '{print $1}')
        spec=$(echo "$line" | awk '{print $3}')
        [ -z "$dev" ] && continue
        [ -z "$spec" ] && continue
        HDD_DEVICES+=("$dev|$spec")
    done <<< "$scan"

    if [ "${#HDD_DEVICES[@]}" -eq 0 ]; then
        HDD_ENABLED=0
        log "No drives parsed from smartctl --scan — HDD monitoring disabled"
        return
    fi

    HDD_ENABLED=1
    local list=""
    for entry in "${HDD_DEVICES[@]}"; do
        list+="${entry%|*}(${entry#*|}) "
    done
    log "HDD monitoring: ${#HDD_DEVICES[@]} drive(s) — $list"
}

# Read all CPU die temps from /sys/class/hwmon (coretemp driver). Echoes
# "<max>|<details>" or returns nonzero. Works on any Intel-based Dell.
read_coretemp() {
    local max=0 t mc dir name f details=""
    for dir in /sys/class/hwmon/hwmon*; do
        [ -r "$dir/name" ] || continue
        name=$(cat "$dir/name" 2>/dev/null)
        [ "$name" = "coretemp" ] || continue
        local pkg="${dir##*hwmon}"
        for f in "$dir"/temp*_input; do
            [ -r "$f" ] || continue
            mc=$(cat "$f" 2>/dev/null)
            [ -z "$mc" ] && continue
            t=$((mc / 1000))
            local idx="${f##*temp}"; idx="${idx%_input}"
            details+="P${pkg}.t${idx}:${t} "
            [ "$t" -gt "$max" ] && max=$t
        done
    done
    [ "$max" -gt 0 ] || return 1
    echo "$max|$details"
}

# Read CPU temps from IPMI entity 3.x (Processor). R730xd path. Some
# older Dells (R410) report these as "Disabled" — coretemp is the
# fallback. Echoes "<max>|<details>" or nonzero.
read_ipmi_cpu() {
    local lines max=0 t entity details=""
    lines=$(ipmitool sdr type temperature 2>/dev/null) || return 1
    [ -z "$lines" ] && return 1
    while IFS= read -r line; do
        entity=$(echo "$line" | awk -F'|' '{gsub(/[[:space:]]/,"",$4); print $4}')
        case "$entity" in 3.*) ;; *) continue ;; esac
        case "$line" in *Disabled*) continue ;; esac
        t=$(echo "$line" | grep -oP '\d+(?= degrees)')
        [ -z "$t" ] && continue
        details+="IPMI${entity}:${t} "
        [ "$t" -gt "$max" ] && max=$t
    done <<< "$lines"
    [ "$max" -gt 0 ] || return 1
    echo "$max|$details"
}

# Read per-GPU temp + classify by cooling type. nvidia-smi returns
# fan.speed = "[N/A]" for passive cards (Tesla P4) — no fan, depend on
# chassis airflow → drives PID. Numeric fan.speed = active card (A5500)
# — chassis fans can't directly cool die → drives assist band + emergency.
#
# Echoes "<passive_max>|<active_max>|<details>".
read_gpu_temps() {
    [ "$GPU_ENABLED" -eq 1 ] || return 1
    local data passive_max=0 active_max=0 details=""
    data=$(nvidia-smi --query-gpu=index,temperature.gpu,fan.speed --format=csv,noheader,nounits 2>/dev/null) || return 1
    while IFS= read -r line; do
        [ -z "$line" ] && continue
        local idx temp fan
        idx=$(echo "$line" | awk -F',' '{gsub(/[[:space:]]/,"",$1); print $1}')
        temp=$(echo "$line" | awk -F',' '{gsub(/[[:space:]]/,"",$2); print $2}')
        fan=$(echo "$line" | awk -F',' '{gsub(/[[:space:]%]/,"",$3); print $3}')
        [ -z "$temp" ] && continue
        case "$fan" in
            ''|'[N/A]'|'[NotSupported]')
                details+="Gp${idx}:${temp} "
                [ "$temp" -gt "$passive_max" ] && passive_max=$temp
                ;;
            *)
                details+="Ga${idx}:${temp}@${fan}% "
                [ "$temp" -gt "$active_max" ] && active_max=$temp
                ;;
        esac
    done <<< "$data"
    echo "$passive_max|$active_max|$details"
}

# Read per-drive temps via smartctl. Cached for HDD_READ_INTERVAL seconds
# — smartctl is slow against RAID controllers and HDD temps change slowly
# enough that 60s resolution is plenty. Won't wake drives in standby
# (`-n standby` returns 2 without reading).
#
# Parses three output formats:
#   SCSI:  "Current Drive Temperature:     31 C"
#   SATA:  "194 Temperature_Celsius     ...     <raw>"  (column 10)
#   NVMe:  "Temperature:                    32 Celsius"
#
# Echoes "<max>|<details>" or returns nonzero.
read_hdd_temps() {
    [ "$HDD_ENABLED" -eq 1 ] || return 1
    local now
    now=$(date +%s)
    if [ "$hdd_last_read" -gt 0 ] && [ $(( now - hdd_last_read )) -lt "$HDD_READ_INTERVAL" ]; then
        echo "$hdd_cached_max|$hdd_cached_details"
        return 0
    fi

    local max=0 details="" i=0
    local entry dev spec out rc t
    for entry in "${HDD_DEVICES[@]}"; do
        dev="${entry%|*}"
        spec="${entry#*|}"
        out=$(smartctl -A -n standby -d "$spec" "$dev" 2>/dev/null)
        rc=$?
        if [ "$rc" -eq 2 ]; then
            details+="d${i}:zZ "
        else
            t=$(echo "$out" | grep -oE 'Current Drive Temperature:[[:space:]]+[0-9]+' | grep -oE '[0-9]+' | head -1)
            if [ -z "$t" ]; then
                t=$(echo "$out" | awk '/^[[:space:]]*194[[:space:]]+Temperature/{print $10; exit}')
            fi
            if [ -z "$t" ]; then
                t=$(echo "$out" | grep -oE 'Temperature:[[:space:]]+[0-9]+[[:space:]]+Celsius' | grep -oE '[0-9]+' | head -1)
            fi
            if [ -n "$t" ] && [ "$t" -gt 0 ] 2>/dev/null; then
                details+="d${i}:${t} "
                [ "$t" -gt "$max" ] && max=$t
            else
                details+="d${i}:? "
            fi
        fi
        i=$(( i + 1 ))
    done

    hdd_cached_max=$max
    hdd_cached_details="$details"
    hdd_last_read=$now
    echo "$max|$details"
}

# Aggregates all temp sources, reporting each class separately so the
# main loop can run independent per-class PIDs and floors.
# Echoes "<cpu_max>|<passive_gpu_max>|<active_gpu_max>|<hdd_max>|<details>".
get_temps() {
    local cpu_result gpu_result hdd_result
    local cpu_max=0 passive_gpu_max=0 active_gpu_max=0 hdd_max=0 details=""
    if cpu_result=$(read_coretemp); then
        :
    elif cpu_result=$(read_ipmi_cpu); then
        :
    else
        return 1
    fi
    cpu_max="${cpu_result%%|*}"
    details="${cpu_result#*|}"

    if gpu_result=$(read_gpu_temps); then
        passive_gpu_max="${gpu_result%%|*}"
        local rest="${gpu_result#*|}"
        active_gpu_max="${rest%%|*}"
        details+="${rest#*|}"
    fi

    if hdd_result=$(read_hdd_temps); then
        hdd_max="${hdd_result%%|*}"
        details+="${hdd_result#*|}"
    fi

    echo "$cpu_max|$passive_gpu_max|$active_gpu_max|$hdd_max|$details"
}

# Set fan speed (decimal %, 0-100). Converts to hex byte at the BMC call.
set_fan() {
    local pct=$1
    local hex
    hex=$(printf "0x%02x" "$pct")
    ipmitool raw 0x30 0x30 0x02 0xff "$hex" >/dev/null 2>&1
}

clamp() {
    local v=$1 lo=$2 hi=$3
    [ "$v" -lt "$lo" ] && v=$lo
    [ "$v" -gt "$hi" ] && v=$hi
    echo "$v"
}

# Proximity-to-emergency floor for one device class. Silent until temp
# enters (emergency - window); then linear ramp from MIN_FAN at the
# window's outer edge to MAX_FAN at emergency. Echoes the floor as an
# integer %. Per-class window — each class has its own approach distance.
#
# Why: the per-class proportional loop reacts to "distance from target"
# — fine for slow drift but blind to fast spikes (P4: +14°C in 30s only
# earns +5% fan). The proximity floor commits chassis based on "how
# much safety budget remains," independent of how we got there.
proximity_floor() {
    local temp=$1 emergency=$2 window=$3
    awk -v t="$temp" -v e="$emergency" -v w="$window" \
        -v lo="$MIN_FAN" -v hi="$MAX_FAN" \
        'BEGIN {
            diff = t - (e - w);
            if (diff <= 0) { print lo; exit }
            f = lo + (diff/w) * (hi - lo);
            if (f > hi) f = hi;
            if (f < lo) f = lo;
            printf "%d", f + 0.5
         }'
}

# EWMA: new_base = (1-alpha) * old_base + alpha * sample.
# Uses awk because bash has no floating-point math.
ewma() {
    local prev=$1 sample=$2 alpha=$3
    awk -v p="$prev" -v s="$sample" -v a="$alpha" 'BEGIN { printf "%.4f", (1-a)*p + a*s }'
}

# Run one class's PID from temp + per-class config. Returns the
# candidate fan speed via stdout. The main loop takes max() across
# all class candidates (and proximity floors) — worst-offender wins.
#
# In-deadband-and-at-or-below-target: candidate drifts toward base_speed
# by DEADBAND_DRIFT_RATE. Above target OR well below: P × error + D ×
# rate-of-change. Asymmetric deadband (positive errors always do P+D
# even inside the deadband) gives the controller "cooldown bias":
# a heat-soaked chassis stays loud until it actually cools to target,
# not just until temp stops climbing.
#
# A class with no reading (temp <= 0) abstains by returning the current
# fan speed — a no-op in the max().
run_pid() {
    local temp=$1 target=$2 deadband=$3 last=$4

    if [ "$temp" -le 0 ]; then
        echo "$current_speed"
        return
    fi

    local error=$(( temp - target ))
    local abs_error=${error#-}
    local d_temp
    if [ "$last" -lt 0 ]; then
        d_temp=0
    else
        d_temp=$(( temp - last ))
    fi

    if [ "$error" -le 0 ] && [ "$abs_error" -le "$deadband" ]; then
        # At or below target, in deadband: trickle toward base_speed.
        local int_base
        int_base=$(printf "%.0f" "$base_speed")
        int_base=$(clamp "$int_base" "$MIN_FAN" "$MAX_FAN")
        local cand=$current_speed
        if [ "$current_speed" -gt "$int_base" ]; then
            cand=$(( current_speed - DEADBAND_DRIFT_RATE ))
            [ "$cand" -lt "$int_base" ] && cand=$int_base
        elif [ "$current_speed" -lt "$int_base" ]; then
            cand=$(( current_speed + DEADBAND_DRIFT_RATE ))
            [ "$cand" -gt "$int_base" ] && cand=$int_base
        fi
        echo "$cand"
        return
    fi

    # Step = P × error + D × d_temp. awk does the multiply (gains can be
    # floats), then we round half-away-from-zero so small fractions
    # still move the fan.
    local step cand
    step=$(awk -v e="$error" -v p="$FAN_GAIN" -v d="$d_temp" -v dg="$DERIVATIVE_GAIN" \
        'BEGIN { s = e*p + d*dg; printf "%d", (s >= 0 ? s + 0.5 : s - 0.5) }')
    cand=$(( current_speed + step ))
    cand=$(clamp "$cand" "$MIN_FAN" "$MAX_FAN")
    echo "$cand"
}

write_state() {
    mkdir -p "$STATE_DIR"
    cat > "$STATE_FILE" <<EOF
base_speed=$base_speed
last_speed=$current_speed
samples=$samples
last_updated=$(date -u +%Y-%m-%dT%H:%M:%SZ)
EOF
    last_persist=$(date +%s)
}

# Emit Prometheus textfile-collector metrics each cycle. Atomic write
# (temp file + rename) so a scraper never sees a torn file. Consumed by
# node-exporter's --collector.textfile.directory in the host-agent stack;
# absent that consumer, the file is just inert state. Failures are
# tolerated (|| true at call sites) so a malformed format string can never
# block fan control.
#
# Globals used: current_speed, base_speed, samples, in_emergency,
# CPU_MAX, PASSIVE_GPU_MAX, ACTIVE_GPU_MAX, HDD_MAX,
# CPU_TARGET / GPU_TARGET / ACTIVE_GPU_TARGET / HDD_TARGET,
# CPU_EMERGENCY / GPU_EMERGENCY / ACTIVE_GPU_EMERGENCY / HDD_EMERGENCY,
# cpu_cand / pg_cand / hdd_cand,
# cpu_pf / pg_pf / ag_pf / hdd_pf,
# ag_assist, src.
#
# Variables that may be unset during an emergency cycle (PIDs/floors not
# computed because we short-circuit to 100%) use ${var:-0} defaults so
# the format stays well-formed.
emit_metrics() {
    {
        cat <<EOF
# HELP dellfans_fan_setpoint_percent Current chassis fan setpoint commanded by controller.
# TYPE dellfans_fan_setpoint_percent gauge
dellfans_fan_setpoint_percent ${current_speed}

# HELP dellfans_base_speed_percent EWMA-smoothed baseline fan speed (24-48h adaptation).
# TYPE dellfans_base_speed_percent gauge
dellfans_base_speed_percent ${base_speed}

# HELP dellfans_samples_total Number of decision cycles since controller start.
# TYPE dellfans_samples_total counter
dellfans_samples_total ${samples}

# HELP dellfans_emergency_active Whether the controller is in emergency state (1=yes, 0=no).
# TYPE dellfans_emergency_active gauge
dellfans_emergency_active ${in_emergency}

# HELP dellfans_class_temp_celsius Max temperature observed for a hardware class this cycle.
# TYPE dellfans_class_temp_celsius gauge
dellfans_class_temp_celsius{class="cpu"} ${CPU_MAX:-0}
dellfans_class_temp_celsius{class="passive_gpu"} ${PASSIVE_GPU_MAX:-0}
dellfans_class_temp_celsius{class="active_gpu"} ${ACTIVE_GPU_MAX:-0}
dellfans_class_temp_celsius{class="hdd"} ${HDD_MAX:-0}

# HELP dellfans_class_target_celsius Per-class target temperature (deadband center) or assist threshold.
# TYPE dellfans_class_target_celsius gauge
dellfans_class_target_celsius{class="cpu"} ${CPU_TARGET}
dellfans_class_target_celsius{class="passive_gpu"} ${GPU_TARGET}
dellfans_class_target_celsius{class="active_gpu"} ${ACTIVE_GPU_TARGET}
dellfans_class_target_celsius{class="hdd"} ${HDD_TARGET}

# HELP dellfans_class_emergency_celsius Per-class emergency threshold — instant fans=100%.
# TYPE dellfans_class_emergency_celsius gauge
dellfans_class_emergency_celsius{class="cpu"} ${CPU_EMERGENCY}
dellfans_class_emergency_celsius{class="passive_gpu"} ${GPU_EMERGENCY}
dellfans_class_emergency_celsius{class="active_gpu"} ${ACTIVE_GPU_EMERGENCY}
dellfans_class_emergency_celsius{class="hdd"} ${HDD_EMERGENCY}

# HELP dellfans_class_candidate_percent Per-class PID candidate fan speed. max() across all classes drives fans.
# TYPE dellfans_class_candidate_percent gauge
dellfans_class_candidate_percent{class="cpu"} ${cpu_cand:-0}
dellfans_class_candidate_percent{class="passive_gpu"} ${pg_cand:-0}
dellfans_class_candidate_percent{class="hdd"} ${hdd_cand:-0}

# HELP dellfans_class_proximity_floor_percent Per-class proximity-to-emergency floor (silent until temp enters approach window).
# TYPE dellfans_class_proximity_floor_percent gauge
dellfans_class_proximity_floor_percent{class="cpu"} ${cpu_pf:-0}
dellfans_class_proximity_floor_percent{class="passive_gpu"} ${pg_pf:-0}
dellfans_class_proximity_floor_percent{class="active_gpu"} ${ag_pf:-0}
dellfans_class_proximity_floor_percent{class="hdd"} ${hdd_pf:-0}

# HELP dellfans_active_gpu_assist_percent Active-GPU intake-air assist contribution to chassis floor.
# TYPE dellfans_active_gpu_assist_percent gauge
dellfans_active_gpu_assist_percent ${ag_assist:-0}

# HELP dellfans_binding_source_info Which source bound the fan decision this cycle (max-wins). 1 for the active source.
# TYPE dellfans_binding_source_info gauge
dellfans_binding_source_info{source="${src:-emergency}"} 1
EOF
    } > "$METRICS_TMP" && mv -f "$METRICS_TMP" "$METRICS_FILE"
}

read_state() {
    if [ -f "$STATE_FILE" ]; then
        last_speed=""
        # shellcheck disable=SC1090
        . "$STATE_FILE"
        log "Restored state: base=${base_speed}%, last_speed=${last_speed:-?}%, samples=${samples}, last_updated=${last_updated:-unknown}"
        # Prefer last_speed (recent operating point) over base (slow EWMA
        # that lags real conditions). On a fresh image with old state file
        # last_speed may be missing — fall back to base.
        if [ -n "$last_speed" ]; then
            current_speed=$(clamp "$last_speed" "$MIN_FAN" "$MAX_FAN")
            log "Starting at ${current_speed}% (resumed from last_speed)"
        else
            local int_base
            int_base=$(printf "%.0f" "$base_speed")
            current_speed=$(clamp "$int_base" "$MIN_FAN" "$MAX_FAN")
            log "Starting at ${current_speed}% (legacy fallback to base)"
        fi
    else
        log "No persisted state — starting at MIN_FAN=${MIN_FAN}%"
        current_speed=$MIN_FAN
        base_speed=$MIN_FAN
        samples=0
    fi
}

shutdown_handback() {
    log "Shutting down — returning fan control to iDRAC automatic"
    write_state || true
    ipmitool raw 0x30 0x30 0x01 0x01 >/dev/null 2>&1
    exit 0
}

trap shutdown_handback SIGINT SIGTERM

vendor_guard
log "Detected model: $(detect_model)"
load_profile
probe_gpu
probe_hdd
read_state

# Engage manual control + apply initial fan speed.
ipmitool raw 0x30 0x30 0x01 0x00 >/dev/null 2>&1
set_fan "$current_speed"
log "Manual control engaged at ${current_speed}%"

while true; do
    if ! result=$(get_temps); then
        log "Temp read failed — fans 100% for safety"
        set_fan 100
        current_speed=100
        sleep "$INTERVAL"
        continue
    fi

    # Parse cpu|passive_gpu|active_gpu|hdd|details
    CPU_MAX="${result%%|*}";          rest="${result#*|}"
    PASSIVE_GPU_MAX="${rest%%|*}";    rest="${rest#*|}"
    ACTIVE_GPU_MAX="${rest%%|*}";     rest="${rest#*|}"
    HDD_MAX="${rest%%|*}";            DETAILS="${rest#*|}"

    # Emergency on anything: any class hitting its emergency threshold
    # snaps fans to 100%. CPU + passive GPU + HDDs share their respective
    # EMERGENCY thresholds because chassis fans are their only cooling.
    # Active GPU emergency means its own fan is genuinely struggling —
    # chassis at 100% then assists by lowering intake-air temp.
    if [ "$CPU_MAX" -ge "$CPU_EMERGENCY" ] \
       || [ "$PASSIVE_GPU_MAX" -ge "$GPU_EMERGENCY" ] \
       || [ "$ACTIVE_GPU_MAX" -ge "$ACTIVE_GPU_EMERGENCY" ] \
       || { [ "$HDD_MAX" -gt 0 ] && [ "$HDD_MAX" -ge "$HDD_EMERGENCY" ]; }; then
        if [ "$current_speed" -ne 100 ]; then
            set_fan 100
            current_speed=100
            log "EMERGENCY (cpu:${CPU_MAX}/${CPU_EMERGENCY} p_gpu:${PASSIVE_GPU_MAX}/${GPU_EMERGENCY} a_gpu:${ACTIVE_GPU_MAX}/${ACTIVE_GPU_EMERGENCY} hdd:${HDD_MAX}/${HDD_EMERGENCY}) — fans 100%"
        else
            log "EMERGENCY hold 100% — ${DETAILS}cpu:${CPU_MAX} p_gpu:${PASSIVE_GPU_MAX} a_gpu:${ACTIVE_GPU_MAX} hdd:${HDD_MAX}"
        fi
        in_emergency=1
        # PIDs/floors not computed in emergency — zero them so the
        # textfile reflects the actual short-circuited state.
        cpu_cand=0 pg_cand=0 hdd_cand=0
        cpu_pf=0 pg_pf=0 ag_pf=0 hdd_pf=0
        ag_assist=0
        src="emergency"
        emit_metrics || true
        sleep "$INTERVAL"
        continue
    fi

    # Just exited emergency — pick max(base, per-class proximity floors)
    # so we don't free-fall back to baseline while a device is still in
    # its approach zone (1-2°C below emergency). Otherwise a sustained
    # event oscillates: emergency → 100% → cooled to (emerg-1) → exit →
    # snap to base → re-spike → emergency. Hold near-emergency fan during
    # near-emergency temps.
    if [ "$in_emergency" -eq 1 ]; then
        int_base=$(printf "%.0f" "$base_speed")
        exit_speed=$(clamp "$int_base" "$MIN_FAN" "$MAX_FAN")
        cpu_pf=$(proximity_floor "$CPU_MAX" "$CPU_EMERGENCY" "$CPU_APPROACH_WINDOW")
        pg_pf=0
        [ "$PASSIVE_GPU_MAX" -gt 0 ] && pg_pf=$(proximity_floor "$PASSIVE_GPU_MAX" "$GPU_EMERGENCY" "$GPU_APPROACH_WINDOW")
        ag_pf=0
        [ "$ACTIVE_GPU_MAX" -gt 0 ] && ag_pf=$(proximity_floor "$ACTIVE_GPU_MAX" "$ACTIVE_GPU_EMERGENCY" "$ACTIVE_GPU_APPROACH_WINDOW")
        hdd_pf=0
        [ "$HDD_MAX" -gt 0 ] && hdd_pf=$(proximity_floor "$HDD_MAX" "$HDD_EMERGENCY" "$HDD_APPROACH_WINDOW")
        [ "$cpu_pf" -gt "$exit_speed" ] && exit_speed=$cpu_pf
        [ "$pg_pf"  -gt "$exit_speed" ] && exit_speed=$pg_pf
        [ "$ag_pf"  -gt "$exit_speed" ] && exit_speed=$ag_pf
        [ "$hdd_pf" -gt "$exit_speed" ] && exit_speed=$hdd_pf
        current_speed=$exit_speed
        set_fan "$current_speed"
        log "Emergency cleared — fan=${current_speed}% (base=${int_base} cpu_pf=${cpu_pf} pg_pf=${pg_pf} ag_pf=${ag_pf} hdd_pf=${hdd_pf})"
        in_emergency=0
    fi

    # Three independent PIDs. Each candidate is computed from
    # `current_speed` plus that class's step (or drift). max() across all
    # candidates and floors below is what the fans actually do — the
    # worst-offender class wins without coupling its state to the others.
    cpu_cand=$(run_pid "$CPU_MAX"         "$CPU_TARGET" "$CPU_DEADBAND" "$last_cpu_temp")
    pg_cand=$(run_pid  "$PASSIVE_GPU_MAX" "$GPU_TARGET" "$GPU_DEADBAND" "$last_pg_temp")
    hdd_cand=$(run_pid "$HDD_MAX"         "$HDD_TARGET" "$HDD_DEADBAND" "$last_hdd_temp")

    # Per-class proximity floors. Each class has its own APPROACH_WINDOW
    # so a class operating closer to its limits (HDDs near 50°C) doesn't
    # need to share the 10°C ramp distance that suits a CPU near 80°C.
    cpu_pf=$(proximity_floor "$CPU_MAX" "$CPU_EMERGENCY" "$CPU_APPROACH_WINDOW")
    pg_pf=0
    [ "$PASSIVE_GPU_MAX" -gt 0 ] && pg_pf=$(proximity_floor "$PASSIVE_GPU_MAX" "$GPU_EMERGENCY" "$GPU_APPROACH_WINDOW")
    ag_pf=0
    [ "$ACTIVE_GPU_MAX" -gt 0 ] && ag_pf=$(proximity_floor "$ACTIVE_GPU_MAX" "$ACTIVE_GPU_EMERGENCY" "$ACTIVE_GPU_APPROACH_WINDOW")
    hdd_pf=0
    [ "$HDD_MAX" -gt 0 ] && hdd_pf=$(proximity_floor "$HDD_MAX" "$HDD_EMERGENCY" "$HDD_APPROACH_WINDOW")

    # Active-GPU assist: chassis fans can't cool an active GPU's die,
    # but they can lower the air the GPU's own fan pulls in. When the
    # active GPU exceeds ACTIVE_GPU_TARGET, lift the chassis floor
    # proportionally to give that intake-air assist.
    ag_assist=0
    if [ "$ACTIVE_GPU_MAX" -gt "$ACTIVE_GPU_TARGET" ]; then
        overshoot=$(( ACTIVE_GPU_MAX - ACTIVE_GPU_TARGET ))
        assist_lift=$(awk -v o="$overshoot" -v g="$ASSIST_GAIN" \
            'BEGIN { s = o * g; printf "%d", s + 0.5 }')
        ag_assist=$(( MIN_FAN + assist_lift ))
        ag_assist=$(clamp "$ag_assist" "$MIN_FAN" "$MAX_FAN")
    fi

    # max() across all candidates + floors. Track which source binds so
    # the log line tells us why fans are at the speed they're at.
    new_speed=$cpu_cand
    src="cpu"
    for pair in "pg:$pg_cand" "hdd:$hdd_cand" "cpu_pf:$cpu_pf" "pg_pf:$pg_pf" "ag_pf:$ag_pf" "hdd_pf:$hdd_pf" "ag_assist:$ag_assist"; do
        s="${pair##*:}"
        n="${pair%:*}"
        if [ "$s" -gt "$new_speed" ]; then
            new_speed=$s
            src=$n
        fi
    done
    new_speed=$(clamp "$new_speed" "$MIN_FAN" "$MAX_FAN")

    if [ "$new_speed" -ne "$current_speed" ]; then
        current_speed=$new_speed
        set_fan "$current_speed"
    fi

    log "${DETAILS}cpu:${CPU_MAX} p_gpu:${PASSIVE_GPU_MAX} a_gpu:${ACTIVE_GPU_MAX} hdd:${HDD_MAX} | pid c${cpu_cand}/p${pg_cand}/h${hdd_cand} pf c${cpu_pf}/p${pg_pf}/a${ag_pf}/h${hdd_pf} ag_assist:${ag_assist} → ${current_speed}%(${src}) base:${base_speed}"

    # EWMA baseline update — every cycle, slow.
    base_speed=$(ewma "$base_speed" "$current_speed" "$ADAPT_ALPHA")
    samples=$(( samples + 1 ))

    # Store last-cycle temps for next cycle's D-term. Only update classes
    # that returned a real reading (max stays 0 if class missing).
    last_cpu_temp=$CPU_MAX
    [ "$PASSIVE_GPU_MAX" -gt 0 ] && last_pg_temp=$PASSIVE_GPU_MAX
    [ "$HDD_MAX"         -gt 0 ] && last_hdd_temp=$HDD_MAX

    now=$(date +%s)
    if [ $(( now - last_persist )) -ge "$PERSIST_INTERVAL" ]; then
        write_state
    fi

    emit_metrics || true

    sleep "$INTERVAL"
done
