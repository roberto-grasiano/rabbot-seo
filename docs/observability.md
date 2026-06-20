# Self-observability — metrics, Prometheus, and Grafana

A monitor should be able to monitor *itself*. Rabbot-SEO can expose a Prometheus
`/metrics` endpoint and ships a committed, provisioned **Grafana** dashboard so you can see
fetch throughput, error classes, the cosmetic-vs-substantive change ratio, alert-delivery
outcomes, the crawl backlog, and database growth at a glance — the questions you'd want
answered at 3 a.m.

Prefer to let an agent wire it up? See
[Set up observability with Claude](observability-with-claude.md) — same generator, the agent
runs the Docker stack in its own shell.

## What it exposes

Metrics live on a **separate, read-only, GET-only, unauthenticated** HTTP listener — distinct
from the token-authed loopback control plane. It is **off by default**. When a setup path
enables it, it binds the loopback default `127.0.0.1:9464` (the Prometheus exporter
convention). Mutations never touch this listener; it serves only `GET /metrics` (other paths
404, non-GET 405).

The metric set is deliberately bounded:

| Metric | Type | Labels |
|---|---|---|
| `rabbot_fetches_total` | counter | `class` ∈ ok, soft_block, hard_block, unreachable |
| `rabbot_fetch_duration_seconds` | histogram | — |
| `rabbot_changes_total` | counter | `class` ∈ cosmetic, substantive |
| `rabbot_alerts_dispatched_total` | counter | `notifier` (config names), `outcome` ∈ ok, error |
| `rabbot_digest_dropped_total` | counter | — |
| `rabbot_crawls_in_flight` | gauge | — |
| `rabbot_due_urls` | gauge | — |
| `rabbot_db_size_bytes` | gauge | — |
| `rabbot_build_info` | gauge (=1) | `version` |
| Go + process collectors | stock | stock |

**Cardinality discipline:** there are **no per-URL or per-site labels, ever.** Label values
are closed enums or operator-config names only (the notifier *name*, never a webhook URL).
Per-URL detail stays in the store — query it with `rabbot history` / `rabbot report` or the
MCP tools. The scrape path never touches the database, so a scrape can never stall a writer.

## Enable it (the one command)

`rabbot observability init` is the deterministic generator. It:

1. sets `metrics.addr` to `127.0.0.1:9464` — **only when it's unset**, so a custom address
   survives a re-run;
2. writes the provisioned bundle to `<config-dir>/observability/`
   (`docker-compose.observability.yml`, `prometheus.yml`, the Grafana datasource + dashboard
   provisioning, and the dashboard JSON);
3. prints the next steps.

```sh
rabbot observability init
# Wrote the observability bundle to ~/.config/rabbot/observability
# Set metrics.addr to 127.0.0.1:9464 (loopback).
#
# Next steps:
#   1. docker compose -f ~/.config/rabbot/observability/docker-compose.observability.yml up -d
#   2. Open the dashboard at http://localhost:3000
#   3. Restart the daemon so it starts serving /metrics.
```

Re-running is **byte-identical**, so it's safe to retry. **Rabbot-SEO never runs Docker** —
it only writes files and config; you (or an agent) bring the stack up.

> The wizard offers this too — "See it on a dashboard" in the post-go-live menu — and
> `rabbot init --with-grafana` is the same generator for non-interactive setups.

## Bring up Prometheus + Grafana

```sh
docker compose -f <config-dir>/observability/docker-compose.observability.yml up -d
```

Then restart the daemon (`rabbot stop` then `rabbot run`, or restart the service) so it
starts serving `/metrics`. Open the dashboard at **http://localhost:3000**.

On a Linux VPS the compose file runs Prometheus and Grafana with `network_mode: host`
(a bridge network can't reach a host-`127.0.0.1` bind); Prometheus is pinned to
`127.0.0.1:9090` and only Grafana answers off-loopback, at `:3000` behind its own login.

> **WARNING — Grafana credentials:** Grafana starts with the stock **admin/admin** login and
> forces a password change on first sign-in. Change it immediately, and don't expose Grafana
> (`:3000`) to the public internet without a proxy/firewall in front. (It's localhost by
> default — keep it that way unless you've put auth in front.)

## Without Docker

Docker is only the *convenience packaging*. The integration point is a plain Prometheus
`/metrics` endpoint, so **any** Prometheus + Grafana works — Rabbot-SEO has no Docker
dependency:

- **Native install.** Run Prometheus and Grafana from your package manager or their release
  binaries. Point a Prometheus scrape job at `127.0.0.1:9464` (the bundle's `prometheus.yml`
  is a copy-pasteable starting point) and import the dashboard JSON
  (`<config-dir>/observability/grafana/dashboards/rabbot.json`) into Grafana; its datasource
  is named `Prometheus`.
- **An existing stack / Grafana Cloud.** Add `/metrics` as a scrape target and import the same
  dashboard JSON. Nothing here is Docker-specific.
- **No Docker on the box at all.** Leave the metrics listener on its loopback default and run
  Prometheus + Grafana *somewhere else* (your laptop, another host), scraping the daemon over
  an **SSH local-forward**:

  ```sh
  # on your machine: forward the daemon's loopback /metrics to your localhost
  ssh -N -L 9464:127.0.0.1:9464 you@your-box
  # a local Prometheus scraping 127.0.0.1:9464 now reads the remote daemon.
  ```

  This keeps `/metrics` loopback-only on the server, so nothing new is exposed publicly.

  > **Platform note:** the shipped compose uses `network_mode: host`, so on **Linux Docker**
  > it scrapes the host-loopback tunnel as-is. On **Docker Desktop (macOS/Windows)** containers
  > run in a VM that can't reach a host `127.0.0.1` forward — there, bind the tunnel to a
  > host-reachable address (`ssh -L 0.0.0.0:9464:127.0.0.1:9464 …`) and point Prometheus at
  > `host.docker.internal:9464`, or just run the stack on a Linux host.

## Where to see it on the daemon

`rabbot status` shows the metrics address when the listener is on:

```sh
rabbot status
#   Metrics:      http://127.0.0.1:9464/metrics
```

The MCP `get_status` tool inherits the same field, so an agent can verify observability is
live without leaving the conversation.

## What's intentionally out

- **No per-URL/per-site labels** (cardinality discipline; see above).
- **No Alertmanager and no shipped alert rules** — Rabbot-SEO's own alert path (Slack, email,
  generic webhook) is the product's push surface; meta-alerting on Rabbot's health belongs to
  *your* Prometheus.
- **No TLS/auth on the listener** — it's read-only and loopback by default. Binding it wider
  is an explicit operator action that logs a startup warning.
- **No OTel / push gateways / StatsD** — Prometheus pull only.
- **Grafana is a provisioned sidecar**, never embedded in Rabbot-SEO.
