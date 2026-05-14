# Migrating `dell-fans` → `host-agent`

The dell-fans stack absorbed the per-host Prometheus exporters and was
renamed to `host-agent`. This is a one-time per-host migration; after
this, watchtower handles image updates as usual.

## What changes

- Stack directory: `/srv/dell-fans/` → `/srv/host-agent/`
- Compose project name: `dell-fans` → `host-agent`
- Containers added: `node-exporter`, `cadvisor`, `ipmi-exporter`,
  `smartctl-exporter` (and `nvidia-gpu-exporter` on GPU hosts).
- Containers using host network ports: 9100 (node), 8080 (cadvisor),
  9290 (ipmi), 9633 (smartctl), 9835 (nvidia). These must NOT
  simultaneously run from the old `monitoring/` stack — the matching
  Stage 3 commit removes them from there.
- `/var/lib/dell-fans/state/` — same path. Keeps the EWMA baseline so
  the controller doesn't have to relearn. Now also holds `metrics.prom`
  written each cycle.
- Image name unchanged: `registry.docker.pq.io/dell-fans:latest`. The
  image is still about controlling Dell fans; the stack is the unit
  that gained scope. Watchtower picks up image rebuilds normally.

## Per-host migration (one-time)

Run on classe AND docker-2 (any host that had `dell-fans` deployed):

```sh
ssh <host>

# 1. Pull the latest repo state (in-repo symlinks update automatically).
cd /srv/docker-server
sudo git pull --ff-only

# 2. dr/restore.sh re-creates the /srv/host-agent symlink from the
#    updated manifest. Safe to run; idempotent on all other stacks.
sudo bash /srv/docker-server/dr/restore.sh "$(hostname -s)"

# 3. Deploy. The new deploy.sh detects + tears down any still-running
#    `dell-fans` project before bringing host-agent up, so /dev/ipmi0
#    isn't double-claimed.
sudo bash /srv/host-agent/infra/deploy.sh
```

## Optional cleanup

- `pq/secrets`: rename `docker-server/dell-fans.env` →
  `docker-server/host-agent.env` to match the manifest. dr/restore.sh
  will WARN if the old name lingers (harmless — controller has its own
  defaults; profile files cover everything).
- `/srv/dell-fans` host symlink will be left dangling after the rename.
  Remove with `sudo rm /srv/dell-fans`. Not urgent.

## Rollback

If anything breaks:

```sh
sudo docker compose -p host-agent down --remove-orphans
sudo git -C /srv/docker-server checkout <pre-rename-commit> -- host-agent dell-fans hosts/ template/ CLAUDE.md
sudo bash /srv/dell-fans/infra/deploy.sh   # back to the old stack
```

(The `dell-fans/` directory exists at the prior commit; `git checkout` of
that path restores it.)
