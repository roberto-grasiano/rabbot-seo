# Run on a Mac mini

A Mac mini is an excellent always-on home for Rabbot-SEO: quiet, low-power, and it never
sleeps if you tell it not to. The single static binary needs nothing but macOS, and the
control plane is loopback-only — **no inbound ports**, no firewall holes.

## Install with Homebrew

```sh
brew install roberto-grasiano/rabbot-seo/rabbot
```

The Homebrew cask also strips the macOS quarantine bit, so the binary launches without a
Gatekeeper prompt. Confirm with `rabbot version`.

## Set up your first site

Run `rabbot init` **as yourself** (not via `sudo`) — the service you install next runs as
this same user and reads this user's config and data:

```sh
rabbot init --contact-email you@you.example \
  --site https://example.com --i-am-authorized
rabbot status
```

## Keep it running with a per-user LaunchAgent (no sudo)

On macOS, `rabbot service install` installs a **launchd** **LaunchAgent** in your
`~/Library/LaunchAgents` — a *per-user* agent that runs as you, **without sudo**:

```sh
rabbot service install     # per-user LaunchAgent — NO sudo needed on macOS
rabbot service start
rabbot service status
```

Because it's a per-user LaunchAgent (not a system LaunchDaemon), it runs as the installing
user and resolves config/data in **your** home directory — exactly where `rabbot init` wrote
them. There is nothing to elevate.

## Enable auto-login (honest caveat)

A LaunchAgent is **login-scoped**: it runs while you're logged in and resumes when you log
in. So after a reboot or a power blip, monitoring only resumes once a session is active. On a
headless always-on Mac mini, turn on **auto-login** so the box logs in by itself after a
restart:

> System Settings → Users & Groups → Automatically log in as → *your user*.

Honest line: **no login, no monitoring.** With auto-login on, a reboot or power-loss event
brings the session — and the LaunchAgent — back automatically.

## Keep the Mac awake

A Mac mini will sleep on its own and pause monitoring with it. Disable sleep so it stays a
true 24/7 box:

```sh
sudo pmset -a sleep 0       # never sleep (display can still sleep)
sudo pmset -a disksleep 0   # keep the disk spun up for SQLite
```

(`pmset` is the only command here that needs `sudo` — it changes a system-wide power
setting, not anything Rabbot-SEO owns.) Verify with `pmset -g` (look for `sleep  0`).

## Size it first with `rabbot doctor`

```sh
rabbot doctor https://example.com
#   Coverage: ~1,200 pages · full pass ≈ 11m · ~14 MB on disk
```

A Mac mini comfortably handles several sites; the estimate tells you the disk and time
footprint before you add one.

> **Security:** the control plane stays on `127.0.0.1` with a `0600` token file — nothing is
> exposed to the network. Back up the data dir; it holds the instance key used for
> verification. Reach a remote Mac mini over SSH, never an open port.

Other always-on options: a [VPS](vps.md), a [Raspberry Pi](raspberry-pi.md), or Docker
([`docker-compose.yml`](../docker-compose.yml), with `restart: unless-stopped`).
