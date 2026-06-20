# Updating Rabbot-SEO

Rabbot-SEO is a single static binary with its state in a separate SQLite database, so an
update is just **swap the binary, restart the daemon** — your data and config are never
touched by the upgrade, and schema changes apply themselves on startup. There is **no
auto-update**: a monitor that runs 24/7 shouldn't change under you, so you update on purpose.

## Why it's safe by design

- **Your data persists.** The binary lives on `PATH` (e.g. `/usr/local/bin/rabbot`); the
  database (`<data-dir>/rabbot.db`) and config (`<config-dir>/config.yaml`) live elsewhere.
  Replacing the binary leaves them untouched. (`rabbot status` prints both locations.)
- **Migrations are forward-only and automatic.** On startup the daemon applies any new schema
  migrations in order — each in its own transaction, tracked in `schema_migrations`. A newer
  binary against an older database brings the schema forward, then runs. You never run a
  migration by hand.
- **Config is additive.** New releases add keys with safe defaults; your existing config keeps
  working. (Skim the [CHANGELOG](../CHANGELOG.md) before a *minor* 0.x bump — see
  [Versioning](#versioning) below.)

## The update, by install channel

Get the new binary, then **restart the daemon** so it picks it up (and applies any pending
migrations):

| Installed via | Update the binary | Then restart |
|---|---|---|
| **Homebrew** | `brew upgrade rabbot` | restart the daemon (below) |
| **Scoop** | `scoop update rabbot` | restart the daemon |
| **install.sh** | re-run the one-liner — it fetches the latest release and overwrites the binary | restart the daemon |
| **Docker** | `docker pull ghcr.io/roberto-grasiano/rabbot-seo:vX.Y.Z` | recreate the container, **keeping the same data volume** so `rabbot.db` persists |
| **Go / source** | `go install github.com/roberto-grasiano/rabbot-seo/cmd/rabbot@latest` | restart the daemon |
| **Manual binary** | download the new archive, replace the binary in place | restart the daemon |

**Restarting the daemon** — if it runs as an OS service (the usual case):

```sh
rabbot service stop && rabbot service start
# or use your init system directly: systemctl restart rabbot  (launchctl / Windows SCM)
```

Running it in the foreground instead? `rabbot stop`, then `rabbot run`.

> **Pin a version** wherever you want determinism: `RABBOT_VERSION=vX.Y.Z` for `install.sh`,
> an explicit `:vX.Y.Z` tag for Docker, `@vX.Y.Z` for `go install`.

## Recommended: back up the database first

Cheap insurance against a one-way schema change. With the daemon **stopped** (so the
write-ahead log is checkpointed back into the file), copy the DB:

```sh
rabbot service stop                                   # or: rabbot stop
cp "<data-dir>/rabbot.db" "<data-dir>/rabbot.db.bak"  # e.g. ~/.local/share/rabbot/rabbot.db
rabbot service start                                  # restart — applies any new migrations
```

(`rabbot db compact` reclaims disk space with `VACUUM`, but it is **not** a backup — copy the
file.)

## Verify the upgrade

```sh
rabbot version    # the new version + commit
rabbot status     # RUNNING, with your sites and URL counts unchanged
```

## Versioning

Rabbot-SEO follows SemVer, with the usual **0.x** rule:

- **patch** (`0.1.0 → 0.1.1`) — fixes only; always safe.
- **minor** (`0.1.x → 0.2.0`) — **may change behavior while pre-1.0**. Skim the
  [CHANGELOG](../CHANGELOG.md) first.
- After **1.0**, minor releases stay backward-compatible and breaking changes wait for a major.

## Rolling back

Downgrades aren't officially supported — migrations are forward-only, so an older binary may
not understand a newer schema. To roll back: stop the daemon, restore your pre-upgrade
`rabbot.db` backup, and reinstall the previous version (pin the tag, as above). This is
exactly why the one-line DB backup is worth it.

## Updating through Claude

If an agent drives your box (see [install with Claude](install-with-claude.md)), the same
playbook applies, in order: **back up the DB → swap the binary → restart → verify
`rabbot version` and `rabbot status`.** The agent works over your host shell / SSH; the daemon
stays loopback-only and the control token never leaves the box.

---

Upgrading specifically across the instance-bound verification change? See
[upgrading-verification.md](upgrading-verification.md).
