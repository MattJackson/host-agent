# Dell PowerEdge fan controller.
#
# - ipmitool: drives fans via /dev/ipmi0 (host must mount it in)
# - smartmontools: reads HDD temps via smartctl (host must mount /dev in)
# - tini: PID 1 forwards SIGTERM to the script so the iDRAC handback runs
#
# Debian (not Alpine) for two reasons:
#   1) GNU grep — script uses `grep -oP`, BusyBox grep doesn't support -P.
#   2) glibc — NVIDIA Container Runtime injects nvidia-smi from the host,
#      which is glibc-linked. On musl/Alpine it's "found but not runnable."
#
# nvidia-smi is intentionally NOT installed by apt. The NVIDIA Container
# Runtime injects it (plus libnvidia-ml.so) at start on GPU hosts. On
# non-GPU hosts the script's GPU_AWARE=auto detects the absence and skips
# the GPU branch. Same pattern for HDDs: HDD_AWARE=auto skips silently
# if smartctl --scan finds nothing readable.
FROM debian:stable-slim

RUN apt-get update \
    && apt-get install -y --no-install-recommends ipmitool smartmontools tini ca-certificates mawk \
    && rm -rf /var/lib/apt/lists/*

COPY dell-fan-controller.sh /usr/local/bin/dell-fan-controller.sh
COPY profiles/ /etc/dell-fans/profiles/
RUN chmod +x /usr/local/bin/dell-fan-controller.sh

ENTRYPOINT ["/usr/bin/tini", "--"]
CMD ["/usr/local/bin/dell-fan-controller.sh"]
