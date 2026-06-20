# Run on a VPS — Hetzner, DigitalOcean, Hostinger

Rabbot-SEO is ideal for a small always-on box: one static binary, light footprint, SQLite
on local disk, and — crucially — **no inbound ports**. The control plane is loopback-only,
so you never open a firewall hole; you drive it over SSH, and connect Claude over SSH too.

**The recipe is identical on any provider** (examples use Ubuntu 22.04+):

```sh
# 1. SSH in and install (no package manager required):
ssh user@your-vps
curl -fsSL https://raw.githubusercontent.com/roberto-grasiano/rabbot-seo/main/install.sh | sh

# 2. Set up your site and keep it running as a service:
rabbot init --contact-email you@you.example \
  --site https://example.com --i-am-authorized
sudo rabbot service install      # systemd unit; runs as YOU, auto-starts on boot
sudo rabbot service start
rabbot status                    # confirm it's live (run as yourself)

# 3. Verify ownership to lift the throttle (full-speed monitoring):
rabbot verify https://example.com
```

It now runs 24/7. **No `ufw` / security-group changes are needed** — nothing listens on a
public interface.

**Run `init` as yourself, `sudo` only for the service.** `rabbot init`, `rabbot status`, and
`rabbot verify` all run as your normal login user — no elevation. Only `service install` and
`service start` need `sudo`, because the OS service manager does. The installed systemd unit
is set to run **as the installing user** (`User=` is taken from `SUDO_USER`), so the daemon
reads *your* config and data — not root's empty config. A genuine root-login VPS keeps
working unchanged (installing as root resolves the identity to root). Always run `init` as
your normal login user and let only the service steps elevate — splitting config across two
users (by switching to a root shell before `init`) is the one shape to avoid. The plain flow
above is the only shape you need.

**Connect Claude from your laptop** (the daemon stays loopback-only on the VPS; Claude
launches the MCP subprocess over SSH). On your *laptop*, with `rabbot` installed locally:

```sh
ssh user@your-vps true                                          # pin the host key once
rabbot init --connect-claude project --connect-remote user@your-vps
```

This writes a `./.mcp.json` that runs `ssh user@your-vps rabbot mcp`; open Claude Code
there and `/mcp` shows `rabbot` connected. (Details + `--connect-remote-bin` are in
[Drive Rabbot-SEO from Claude](claude-mcp.md) and the
[connect guide](mcp-connect-guide.md).)

**If you created the VPS with a dedicated SSH key** (the common case — you paste a key at
instance creation), a bare `ssh user@your-vps` uses your *default* key, so the
non-interactive MCP subprocess can't authenticate and `/mcp` won't connect. Add a
`~/.ssh/config` `Host` block on your laptop that pins the right key, then the snippet's
`ssh user@your-vps` works unattended:

```sshconfig
# ~/.ssh/config — on your laptop
Host your-vps
  HostName <ip-or-hostname>
  User user
  IdentityFile ~/.ssh/<your-vps-key>
  IdentitiesOnly yes
  BatchMode yes
```

The full reasoning (and the `claude mcp add --scope local` note, so a personal remote
daemon isn't written into a project's tracked `.mcp.json`) is in
[Drive Rabbot-SEO from Claude](claude-mcp.md#remote-daemon-on-a-vps--over-ssh-never-an-open-port).

## Slack alerts as a service secret

The recommended hygiene is to keep your Slack webhook **out of `config.yaml`**: pass the
env token form at setup —

```sh
rabbot init --contact-email you@you.example \
  --site https://example.com --i-am-authorized \
  --slack-webhook '${RABBOT_SLACK_WEBHOOK}'
```

— and supply the real value through the environment. The daemon interpolates
`${RABBOT_SLACK_WEBHOOK}` at runtime, so the service needs that variable in **its** environment
(your interactive shell's export does *not* carry into a systemd service). The generated
systemd unit already reads an environment file:

```ini
EnvironmentFile=-/etc/sysconfig/rabbot
```

The leading `-` makes it optional, so the unit starts fine when the file is absent — which
is exactly the gap: nothing tells you it exists. Put the secret there (note the path is
`/etc/sysconfig/rabbot`, which is **not present by default on Debian/Ubuntu** — create the
directory first):

```sh
sudo mkdir -p /etc/sysconfig
printf 'RABBOT_SLACK_WEBHOOK=%s\n' 'https://hooks.slack.com/services/XXX/YYY/ZZZ' \
  | sudo tee /etc/sysconfig/rabbot >/dev/null
sudo chmod 600 /etc/sysconfig/rabbot       # it holds a secret
sudo systemctl daemon-reload
rabbot service start                        # (or `rabbot service restart` if already up)
```

The unit now loads `RABBOT_SLACK_WEBHOOK` before launching the daemon, the `${...}` token
in `config.yaml` interpolates to your real webhook, and alerts fire — with the secret never
written into the tracked config. (The webhook never appears in logs.) Editing the file later
needs a `sudo systemctl daemon-reload` + a service restart to take effect.

**Provider notes** — the create-an-instance step differs, the rest is the same:

- **Hetzner Cloud** — a `CX22` (2 vCPU / 4 GB, ~€4/mo) is ample. Create the server with
  your SSH key + Ubuntu in the console, then SSH in and run the recipe. Only SSH (22)
  inbound; no other rule needed.
- **DigitalOcean** — a Basic Droplet ($4–6/mo) handles a few small sites (more RAM for
  many pages). Add your SSH key + Ubuntu at creation, SSH in, run the recipe. Leave the
  Cloud Firewall at SSH-only.
- **Hostinger VPS** — pick a KVM plan, set Ubuntu and add your SSH key in hPanel, SSH in,
  run the recipe. Same story: only SSH (22) inbound.
- **Any other VPS / your own server** — identical. Prefer containers? Use
  [`docker-compose.yml`](../docker-compose.yml) instead of the service.

**Other always-on boxes:** a VPS isn't the only home for a 24/7 monitor. The same static
binary runs on a Raspberry Pi ([docs/raspberry-pi.md](raspberry-pi.md)) — a low-power home
server — or a Mac mini ([docs/mac-mini.md](mac-mini.md)) — a quiet desktop — each with its
own short recipe.

> **Security on a VPS:** your locked-down SSH (key auth, no passwords) *is* the access
> control — Rabbot-SEO exposes nothing to the network. The control token stays on the box
> (`0600`); back up the data dir (it holds the instance key used for verification).

---

*Validated: harness-validated on a clean Ubuntu 22.04 systemd container, 2026-06-12 —
non-root `init` → `sudo rabbot service install` (unit installed with `User=` set to the
installing user) → `service start` → `rabbot status` as that user reaching the live
daemon, plus `rabbot observability init` → service restart → `/metrics` scrape (GET 200,
POST 405, other paths 404). The `install.sh` download step was side-loaded with a locally
built static binary (no published release yet — it gets verbatim validation at the first
release), and the laptop-side `--connect-remote` snippet was previously live-validated in
the MCP connect work.*
