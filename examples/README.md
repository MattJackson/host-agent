# examples

Drop-in helpers around host-agent that you can use unmodified or copy
and adapt.

| dir | what |
|---|---|
| [`server-side/`](server-side/) | Minimal Prometheus + Grafana receiver with the Server Overview dashboard pre-provisioned. Run with `docker compose up -d`. |
| [`server-side/grafana/dashboards/server-overview.json`](server-side/grafana/dashboards/server-overview.json) | The Grafana dashboard. Import into an existing Grafana via Dashboards → New → Import → paste JSON. Requires the `prometheus` datasource UID. |

The server-side compose is opinionated and minimal by design — it gets
you to a working dashboard in two commands. For production
deployments (TLS, auth, external storage, alerting), use it as a
starting point and adapt.
