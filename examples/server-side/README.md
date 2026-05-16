# server-side example

The companion Prometheus + Grafana for host-agent. Brings you from "I
just installed the agent on three boxes" to "I can see them on a
dashboard" in two commands.

## Run

```sh
cd examples/server-side
docker compose up -d
```

That's it. You now have:

- **Prometheus** at `http://localhost:9090`, accepting `remote_write`
  pushes on `/api/v1/write`, with the `hosts_active` recording rule
  loaded.
- **Grafana** at `http://localhost:3000` (admin / admin), with the
  Prometheus datasource pre-wired (UID `prometheus`) and the
  **Server Overview** dashboard already provisioned.

Both bind to `127.0.0.1` only — put TLS in front of them if you expose
them. Override the Grafana admin password via env:

```sh
GRAFANA_ADMIN_PASSWORD=$(openssl rand -hex 16) docker compose up -d
```

## Point your host-agents at this Prometheus

On each host that's running host-agent, set the remote-write target to
this box's address:

```sh
PROMETHEUS_REMOTE_WRITE_URL=http://<your-prometheus-host>:9090/api/v1/write
```

The dashboard's host dropdown populates automatically as each agent
sends its first push — no central config edits needed.

## Customize

- **Retention**: change `--storage.tsdb.retention.time=30d` in
  `docker-compose.yml`.
- **Auth on the receiver**: bind Prometheus behind nginx / Caddy with
  basic or bearer auth; configure each host-agent with the matching
  `PROMETHEUS_REMOTE_WRITE_USERNAME` / `_PASSWORD` or
  `PROMETHEUS_REMOTE_WRITE_BEARER_TOKEN`.
- **External storage**: swap the `prometheus` image for VictoriaMetrics
  or Mimir if you outgrow single-node TSDB. The dashboard works
  unchanged — it only depends on Prometheus query syntax.
- **Extra dashboards**: drop more JSON into `grafana/dashboards/` and
  Grafana picks them up on its next provisioning cycle (60s).
