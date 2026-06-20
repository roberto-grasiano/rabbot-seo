# Run on a Raspberry Pi

A Raspberry Pi is a great always-on home for Rabbot-SEO: it sips power, runs 24/7 without a
cloud bill, and the single static binary needs nothing but the OS. The control plane is
loopback-only, so — exactly like the [VPS recipe](vps.md) — **no inbound ports**, no
firewall holes. You drive it over SSH and connect Claude over SSH too.

## Before you start — use a 64-bit OS

Rabbot-SEO's prebuilt binaries are **arm64-only** for the Pi (`linux/arm64`). You need a
**64-bit** Raspberry Pi OS (Bullseye 64-bit or newer, on a Pi 3 / 4 / 5 / Zero 2 W). The
`install.sh` script maps `aarch64 → arm64`; a **32-bit** OS fails honestly with
"unsupported architecture" rather than installing a binary that can't run. Check with:

```sh
uname -m        # want: aarch64   (32-bit shows armv7l/armv6l)
```

If you're on a 32-bit OS, reflash with the 64-bit image (Raspberry Pi Imager → "Raspberry Pi
OS (64-bit)"), or run Rabbot-SEO in Docker — the image manifest includes `linux/arm64`.

## Put the database on a USB SSD, not the SD card

Rabbot-SEO stores everything in one SQLite file, and SQLite means steady small writes.
SD cards wear out under that workload; a cheap **USB SSD** lasts far longer and is faster.
Point Rabbot-SEO's **data directory** at the SSD with the `data_dir` config key (or the
`RABBOT_DATA_DIR` environment variable) so the database — and the instance key used for
verification — live on the SSD:

```sh
# Mount your SSD (example: /mnt/ssd), then:
export RABBOT_DATA_DIR=/mnt/ssd/rabbot
mkdir -p "$RABBOT_DATA_DIR"
```

…or set it in `config.yaml` (`rabbot init` prints the config path):

```yaml
data_dir: /mnt/ssd/rabbot
```

A service unit inherits the environment of the user it runs as, so if you use the env var,
set it where the service can see it (a drop-in `Environment=` line, or just use the config
key — simpler and survives reboots cleanly).

## Keep the database small

The Pi's storage is precious, so lean on Rabbot-SEO's built-in **retention** sweep: it
trims old snapshots automatically so the SQLite file doesn't grow forever (see
[Database retention](configuration.md#database-retention)). To reclaim the freed space on
disk after a big trim, run:

```sh
rabbot db compact     # VACUUMs the SQLite file; reclaims space after retention trims
```

Run it occasionally (or after lowering a retention window). Between the retention sweep and
the SSD, a handful of small sites stays comfortably within a few hundred MB.

## Install and run as a service

The recipe is the VPS recipe — `install.sh` works unmodified on a 64-bit Pi:

```sh
# 1. Install (no package manager required):
curl -fsSL https://raw.githubusercontent.com/roberto-grasiano/rabbot-seo/main/install.sh | sh

# 2. Set up your site (run as yourself — NOT root):
rabbot init --contact-email you@you.example \
  --site https://example.com --i-am-authorized

# 3. Keep it running across reboots, via systemd:
sudo rabbot service install      # installs a systemd unit that runs as YOU
sudo rabbot service start
rabbot status                    # confirm it's live (run as yourself)
```

`rabbot service install` registers a **systemd** unit that runs `rabbot run`. Because you
run `rabbot init` as yourself and the service is installed to run **as the installing user**
(`User=` set from `SUDO_USER`), the daemon reads *your* config and data — not root's empty
config. Use `sudo` only for the `service install`/`start` steps the manager requires; every
other command runs as yourself.

## Size it first with `rabbot doctor`

A Pi has modest RAM and a slower disk, so check a site's footprint before you commit:

```sh
rabbot doctor https://example.com
#   Verdict: GREEN — the SEO content is in the server's HTML, fully monitorable.
#   Coverage: ~1,200 pages · full pass ≈ 11m · ~14 MB on disk
```

The estimate (pages, full-pass time, disk) tells you whether a site fits comfortably. A Pi
4/5 handles several small-to-medium sites; on a Zero 2 W, keep to one or two and lean on the
page cap and adaptive cadence ([crawl speed & coverage](crawl-speed.md)).

> **Security on a Pi:** same posture as the VPS — your locked-down SSH *is* the access
> control. Rabbot-SEO exposes nothing to the network (loopback-only control plane, token
> file `0600`). Back up the data dir on the SSD; it holds the instance key used for
> verification.

Other always-on options: a [VPS](vps.md), a [Mac mini](mac-mini.md), or Docker
([`docker-compose.yml`](../docker-compose.yml), with `restart: unless-stopped`).
