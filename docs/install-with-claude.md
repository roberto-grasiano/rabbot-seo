# Install Rabbot-SEO — a guide for Claude (or any coding agent)

**You are an AI assistant helping a human install and run Rabbot-SEO.** Read this
whole file first, then follow it. Work *with* the user — ask for what you need, confirm
before anything irreversible, and explain what you're doing in plain language.

---

## What Rabbot-SEO is (so you can explain it)

Rabbot-SEO is a self-hosted, open-source **SEO monitoring gateway**: a single static
Go binary (`rabbot`) that runs 24/7, discovers a website's pages, rechecks them on an
adaptive cadence, diffs each crawl against the last, and **alerts the user in Slack** when
something SEO-relevant regresses (a page going `noindex`, a canonical disappearing, a
title/`<h1>` change, a 4xx/5xx, an indexability flip). State lives in a local SQLite file;
there are no external services to run. It exposes a **Model Context Protocol (MCP) server
over stdio**, so once it's running you (Claude) can check status and take safe, confirmed
actions from inside the conversation.

- Repo: `https://github.com/roberto-grasiano/rabbot-seo` · Module:
  `github.com/roberto-grasiano/rabbot-seo` · License: AGPL-3.0.
- The control plane is **loopback-only** (`127.0.0.1`) with a file-based bearer token —
  there is no network port to expose. Remote daemons are reached over **SSH**, never an
  open port.

---

## Step 0 — Understand the environment before you install

Run these (read-only) and reason about the answers:

```sh
uname -s; uname -m              # OS + architecture (Linux/Darwin; x86_64/arm64)
command -v brew scoop docker go 2>/dev/null   # which installers are available
echo "$SSH_CONNECTION"          # non-empty ⇒ you're likely on a remote box / VPS
```

Then ask the user (don't assume):

1. **Where should the monitor run** — on *this* machine, or on a remote server/VPS?
   Set expectations honestly: real-time monitoring wants an **always-on** box. If this is a
   **laptop**, say so — it's great for trying Rabbot-SEO or watching a staging site, but a
   laptop **sleeps** when the lid closes and monitoring pauses with it (a gap). For 24/7
   watching, offer the always-on options (a small VPS, a Raspberry Pi, a Mac mini, or
   Docker — see the README's "Where to run Rabbot-SEO" section and `docs/vps.md`,
   `docs/raspberry-pi.md`, `docs/mac-mini.md`).
2. **What site** do they want to monitor (a URL), and **are they authorized** to monitor
   it? (Rabbot-SEO requires an explicit authorization attestation; do not add a site the
   user is not authorized to monitor.)
3. **A contact email** to identify the crawler (a valid address the site owner could
   reach them at). This is mandatory and shown in the crawler's User-Agent, which also
   carries a per-site trust signal: a site they have proven control of (via `rabbot
   verify`) reads as "verified for <site>"; an unverified site whose domain matches the
   email reads as "<site> contact, unverified"; anything else reads as "unverified —
   confirm or block".
4. (Optional) a **Slack incoming-webhook URL** for alerts.
5. **Do they want you (Claude) connected** to it afterward (MCP)?

---

## Step 1 — Install the binary (pick the best channel for their system)

Use the first that fits. Prefer a package manager (gives them updates); fall back to the
installer script, then source. **All commands below are real once a release is published;**
if the user is on a brand-new repo with no release yet, use *Go / source*.

| Situation | Command |
|---|---|
| **macOS** (or Linux) with Homebrew | `brew install roberto-grasiano/rabbot-seo/rabbot` |
| **Windows** with Scoop | `scoop bucket add rabbot-seo https://github.com/roberto-grasiano/scoop-rabbot-seo`<br>`scoop install rabbot` |
| **Linux/macOS**, no package manager | `curl -fsSL https://raw.githubusercontent.com/roberto-grasiano/rabbot-seo/main/install.sh \| sh` |
| **Docker** (run as a container) | see Step 3 (Docker) below |
| **From source** (Go ≥ 1.26.3 toolchain) | `go install github.com/roberto-grasiano/rabbot-seo/cmd/rabbot@latest` |

After installing, confirm: `rabbot version`. If `rabbot` isn't on `PATH`, tell the
user the install dir (e.g. `~/.local/bin`) and how to add it.

> Release artifacts are **Sigstore-signed** with a per-archive SBOM — for a raw-archive
> download you can verify provenance before trusting it (see
> [docs/RELEASING.md → *Verify a download*](RELEASING.md#verify-a-download): cosign +
> sha256). The binaries aren't OS-notarized, so Homebrew/Scoop clear
> Gatekeeper/SmartScreen for you; a raw archive may need a one-time macOS
> *right-click → Open* / Windows *More info → Run anyway*.

---

## Step 2 — Set up the first site

Prefer the **headless** path (you can run it directly with the answers from Step 0):

```sh
rabbot init \
  --contact-email "THEIR-CONTACT-EMAIL" \
  --site "https://THEIR-SITE.com" \
  --i-am-authorized \
  --slack-webhook '${RABBOT_SLACK_WEBHOOK}'   # optional; keep the secret in the env
```

- `--i-am-authorized` records the authorization attestation — only pass it if the user
  confirmed (Step 0, Q2).
- If they gave a Slack webhook, set it in the environment as `RABBOT_SLACK_WEBHOOK`
  (so it stays out of the config file) and pass the `${...}` token as shown.
- If the user prefers a guided walkthrough, run `rabbot init` with **no flags in a
  real terminal** for the interactive wizard instead (it needs an interactive TTY — it
  won't run over a non-interactive pipe/SSH-without-`-t`).

`init` writes `config.yaml` (it prints the path) and can auto-start the daemon. Confirm
with `rabbot status`.

---

## Step 3 — Keep it running 24/7 (open it once)

**As a service (recommended)** — auto-starts on boot, survives logout:

```sh
rabbot service install   # states when elevation is needed; never silently escalates
rabbot service start
rabbot service status
```

(Uses systemd on Linux, launchd on macOS, the Service Manager on Windows. It may prompt
for sudo/Admin — tell the user; don't try to escalate silently.)

**Or in Docker** (containerized daemon, via Compose — it mounts both `./config` and
`./data` and sets the XDG env, so the config *and* the instance key persist):

```sh
mkdir -p data config && sudo chown -R 65532:65532 data config   # nonroot image (uid 65532)
curl -fsSLO https://raw.githubusercontent.com/roberto-grasiano/rabbot-seo/main/docker-compose.yml
docker compose run --rm rabbot \
  init --contact-email "THEIR-CONTACT-EMAIL" --site "https://THEIR-SITE.com" --i-am-authorized
docker compose up -d
docker compose logs -f          # watch it
```

**If you used the env-token Slack webhook** (`--slack-webhook '${RABBOT_SLACK_WEBHOOK}'`
in Step 2), the daemon interpolates `${RABBOT_SLACK_WEBHOOK}` at runtime, so it needs that
variable in **its** environment — a shell `export` does not carry into a service. Wire it
to the runtime so the secret stays out of `config.yaml`:

- **systemd service:** the generated unit reads `EnvironmentFile=-/etc/sysconfig/rabbot`
  (optional, so it starts without it — which is why the gap is easy to miss). That path is
  the RedHat default and is **absent on Debian/Ubuntu**, so create it, then reload + start:

  ```sh
  sudo mkdir -p /etc/sysconfig
  printf 'RABBOT_SLACK_WEBHOOK=%s\n' "$SLACK_WEBHOOK_URL" | sudo tee /etc/sysconfig/rabbot >/dev/null
  sudo chmod 600 /etc/sysconfig/rabbot       # it holds a secret
  sudo systemctl daemon-reload && rabbot service start
  ```

  (More detail, with provider context, in [docs/vps.md](vps.md#slack-alerts-as-a-service-secret).)
- **Docker (Compose):** the shipped `docker-compose.yml` has a commented
  `- RABBOT_SLACK_WEBHOOK=${RABBOT_SLACK_WEBHOOK}` line in the service's `environment:`
  block — uncomment it, put `RABBOT_SLACK_WEBHOOK=…` in a `0600` `.env` beside the compose
  file (Compose substitutes it into that block), then recreate with `docker compose up -d`.

Never echo the resolved webhook back to the user; only ever reference the `${RABBOT_SLACK_WEBHOOK}`
token form.

---

## Step 4 — Connect yourself (MCP), so the user can drive it through you

The daemon must be **running** (Step 3). Generate the MCP client config:

- **This machine + Claude Code (project):**
  `rabbot init --connect-claude project` → writes `./.mcp.json`. Restart Claude Code;
  run `/mcp` to confirm `rabbot` is `connected`.
- **This machine + Claude Desktop:**
  `rabbot init --connect-claude claude-desktop` → merges the per-OS Desktop config.
- **Daemon on a remote VPS, Claude on the user's laptop (over SSH):** generate the snippet
  on the user's *laptop* (not the VPS):
  `rabbot init --connect-claude project --connect-remote user@vps`
  → emits `{ "command": "ssh", "args": ["user@vps", "rabbot", "mcp"] }`. This needs
  passwordless SSH key auth to the VPS and the VPS host key already in `known_hosts` (have
  them `ssh user@vps` once first). The daemon stays loopback-only; the token never leaves
  the VPS. Use `--connect-remote-bin /path/to/rabbot` if it isn't on the VPS `PATH`.
- **Daemon in Docker:** use `docker exec` as the MCP command —
  `{ "command": "docker", "args": ["exec", "-i", "rabbot", "rabbot", "mcp"] }`.

The generated snippet **never contains the token** (the launched subprocess reads it from
disk at runtime). After wiring it up, confirm the full chain by asking to run the
`get_status` tool — a healthy response proves Claude → subprocess → daemon → DB works.

**Once connected, you can:** read status, sites, per-site SEO detail, open issues, and a
URL's change history; and take *confirmed* actions — add a site, recheck now, pause/resume,
ignore an issue, send a test alert, set an allow-listed config key, and run ownership
verification. Every action shows the user a permission prompt first. Destructive ops
(remove-with-purge, daemon shutdown) are **not** available over MCP by design — use the CLI
and ask the user first.

---

## Step 5 — Verify ownership (lifts the speed throttle) — recommended

Until the user proves they control the domain, crawling is **throttled** (a safety floor).
Verifying lifts it. Offer to walk them through it:

```sh
rabbot verify https://THEIR-SITE.com               # well-known file (default)
rabbot verify https://THEIR-SITE.com --method dns  # or dns / meta
```

`rabbot verify` prints an unguessable token and exactly where to place it (a
`/.well-known/rabbot-verify.txt` file, a DNS `TXT`, or a homepage `<meta>` tag). Help the
user place it for their host/DNS/CMS, then re-run. The token is bound to this install and
re-checked continuously — editing config can't fake it. If they can't verify right now,
`--skip` records an attestation but keeps the throttle on. (From inside Claude you can do
the same with the `verify_begin` then `verify_check` tools.)

---

## Step 6 — Confirm and hand off

- `rabbot status` (or the `get_status` tool) → live counts + uptime.
- `rabbot doctor https://THEIR-SITE.com` → an honest preflight (reachability, robots,
  whether the SEO content is visible without JavaScript).
- Tell the user the **config + data dir paths** (printed by `init`), that the **data dir
  holds the instance key — back it up**, and that they can manage it with
  `rabbot status | stop | pause | resume | sites | issues | history | inspect`.

## Step 7 — Updating later

When a new version ships, you (or the user) update safely — the data and config are never
touched by an upgrade. The playbook, in order:

1. **Back up the DB** (schema changes are forward-only): with the daemon stopped,
   `cp <data-dir>/rabbot.db <data-dir>/rabbot.db.bak`.
2. **Get the new binary** via the channel it was installed with (`brew upgrade rabbot` /
   `scoop update rabbot` / re-run `install.sh` / `docker pull …:vX.Y.Z` /
   `go install …/cmd/rabbot@latest` / replace the file).
3. **Restart** so it picks up the binary and applies any migrations:
   `rabbot service stop && rabbot service start` (or `systemctl restart rabbot`); foreground is
   `rabbot stop` then `rabbot run`.
4. **Verify:** `rabbot version` shows the new build; `rabbot status` shows the same sites and
   URL counts.

Full per-channel detail, versioning, and rollback: **[Updating Rabbot](updating.md)**.

## Guardrails for you (the agent)

- **Never** add a site the user isn't authorized to monitor; the attestation is real.
- **Never** expose the control port to the network or copy the token off-box; use SSH for
  remote.
- **Never** claim a site is verified without a real proof check; don't bypass the throttle.
- **Confirm before** anything destructive (removing a site + purging history, stopping the
  daemon) and before installing a system service (it touches the OS).
- Keep the Slack webhook in the environment, never echo it back.
- **Before upgrading**, back up the DB and skim the CHANGELOG for a *minor* 0.x bump (it may
  change behavior) — see [Updating Rabbot](updating.md).
