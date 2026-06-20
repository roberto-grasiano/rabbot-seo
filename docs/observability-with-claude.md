# Set up observability with Claude — a guide for the agent

**You are an AI assistant helping a human stand up Rabbot-SEO's self-observability**
(a Prometheus `/metrics` endpoint plus a provisioned Grafana dashboard). Read this whole
file first, then follow it. Work *with* the user — explain each step, and confirm before you
bring anything up. The human picked "Let Claude set it up" because they'd rather not juggle
SSH tabs and Docker commands — so do the hoops for them.

The split of responsibility is firm: **Rabbot-SEO only writes files and config; it never runs
Docker.** *You* (the agent) bring the stack up yourself with `docker compose up -d` in your
own shell. Rabbot-SEO's MCP surface stays read-only for observability — you drive setup
through the host shell and verify through the read surface.

---

## What you're setting up

- A read-only, GET-only `/metrics` endpoint on the daemon (loopback `127.0.0.1:9464`,
  off until enabled).
- A committed, provisioned bundle (a `docker-compose.observability.yml`, a `prometheus.yml`,
  Grafana datasource + dashboard provisioning, and the dashboard JSON) under the config dir's
  `observability/` folder.
- Prometheus scraping the daemon and Grafana showing the dashboard at
  `http://localhost:3000`.

---

## Step 1 — Generate the bundle and enable metrics

Run the deterministic generator on the host where the daemon runs (locally, or over SSH on a
VPS):

```sh
rabbot observability init
```

This sets `metrics.addr` to the loopback default (only if it's unset — a custom value
survives), writes the bundle under `<config-dir>/observability/`, and prints the next steps,
including the exact `docker compose` command and the bundle path. Re-running is
byte-identical, so it's safe to retry. It **never runs Docker** — that's your job in Step 3.

Note the bundle path it prints (e.g. `~/.config/rabbot/observability`); you'll point
`docker compose -f` at the `docker-compose.observability.yml` inside it.

---

## Step 2 — Restart the daemon so it serves `/metrics`

The metrics listener binds at startup, so the daemon must be **restarted** to pick up the
newly-set `metrics.addr`:

```sh
rabbot stop && rabbot run        # foreground; or restart the service:
# rabbot service stop && rabbot service start   (or: systemctl restart rabbot / launchd / SCM)
```

Verify the endpoint is live:

```sh
curl -s http://127.0.0.1:9464/metrics | head
# expect rabbot_build_info and the rabbot_* families
```

---

## Step 3 — Bring up Prometheus + Grafana (you run Docker)

Run the command the generator printed — this is the step Rabbot-SEO deliberately leaves to
you:

```sh
docker compose -f <config-dir>/observability/docker-compose.observability.yml up -d
docker compose -f <config-dir>/observability/docker-compose.observability.yml ps
```

Give Prometheus a few seconds, then open the dashboard at **http://localhost:3000**.

> **Tell the user about Grafana's credentials:** Grafana starts with the stock
> **admin/admin** login and forces a password change on first sign-in. Have them change it
> immediately, and do **not** expose Grafana (`:3000`) to the public internet without a
> proxy/firewall in front. It's localhost by default — keep it that way unless they've added
> auth.

### No Docker on the box? Run the stack elsewhere and tunnel

If the server has no Docker — and the user would rather not install it just for a dashboard —
keep the metrics listener on its loopback default and bring the **stack up where Docker *is*
available** (your shell, the user's laptop, any host), scraping the daemon over an SSH
local-forward:

This works as-is with **Linux Docker** (where `network_mode: host` shares the host loopback):

```sh
# 1. forward the remote daemon's loopback /metrics to localhost (run in the background)
ssh -N -L 9464:127.0.0.1:9464 user@their-box &
# 2. run the bundle locally — on Linux Docker, network_mode: host lets Prometheus reach
#    127.0.0.1:9464 (the tunnel, hence the remote daemon)
docker compose -f <config-dir>/observability/docker-compose.observability.yml up -d
```

On **Docker Desktop (macOS/Windows)** the containers run in a VM that can't see a host
`127.0.0.1` forward — bind the tunnel wider (`ssh -L 0.0.0.0:9464:127.0.0.1:9464 …`) and
scrape `host.docker.internal:9464`, or run the stack on a Linux host.

Open Grafana at `http://localhost:3000` as usual; `/metrics` stays loopback-only on the
server, so the tunnel exposes nothing new. The non-Docker, native-Prometheus, and
Grafana-Cloud variants are in
[Self-observability → Without Docker](observability.md#without-docker).

---

## Step 4 — Verify the whole chain

Confirm each link end-to-end:

1. **The endpoint:** `curl -s http://127.0.0.1:9464/metrics | grep rabbot_build_info` → a
   line is returned.
2. **Prometheus target is up:** open `http://localhost:9090/targets` (on the box, or tunnel
   it over SSH) and confirm the `rabbot` target shows **UP**. (On a VPS, Prometheus binds
   `127.0.0.1:9090`, so reach it via an SSH local-forward.)
3. **The read surface agrees:** run the **`get_status`** MCP tool and confirm it reports a
   non-empty **`MetricsAddr`** (the daemon now knows the listener is on). This is the
   read-only proof that observability is live — no mutating MCP call is involved.
4. **The dashboard:** Grafana at `http://localhost:3000` renders the Rabbot-SEO dashboard
   (fetch rate, error-class rate, cosmetic/substantive ratio, dispatch success, in-flight +
   due, DB size).

---

## Guardrails for you (the agent)

- **Never** run `rabbot` itself to start Docker — Rabbot-SEO never runs Docker; *you* run it
  in your own shell (Step 3). Rabbot-SEO's MCP stays read-only for observability.
- **Never** set `metrics.addr` over MCP `set_config` — it isn't allow-listed (it changes
  network exposure and only binds at startup). Use `rabbot observability init` on the host.
- **Never** bind the metrics listener to a public interface without telling the user it's
  unauthenticated and read-only; the default is loopback for a reason.
- **Tell the user** the Grafana credentials caveat (Step 3) before you hand it off.
- On a remote box, drive everything over **SSH**; the daemon stays loopback-only and the
  control token never leaves the box.
