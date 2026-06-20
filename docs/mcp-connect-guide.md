# Connecting Claude to your Rabbot daemon (MCP)

`rabbot mcp` is a stdio Model Context Protocol server. An MCP host (Claude Code
or Claude Desktop) **launches it as a subprocess** — there is no network port, no
token in any config, and nothing for a stranger to connect to. The subprocess talks
to your running daemon over the loopback control API, authenticated by the
`control.token` file (0600) that the daemon and the subprocess both read from the
same **config dir** (`config.ResolveConfigDir()`).

Once connected, Claude can **read** (status, sites, per-site detail, issues, change
history) and take **safe, confirmed actions** (add a site, recheck now, pause/resume
monitoring globally, ignore an issue, send a test alert, set an allow-listed config
key, run ownership verification). Every action is annotated so the host shows you a
permission prompt before it runs. Destructive operations (remove-with-purge, config
reload, daemon shutdown) are deliberately **not** exposed — they stay CLI-only.

## Two hops, one token

```
Claude  ──launches subprocess──▶  rabbot mcp  ──loopback HTTP + Bearer token──▶  daemon
        (trust = OS process spawn)                (trust = control.token, 0600, constant-time)
```

- **Claude ↔ subprocess** is a launched process, not a network connection. No port,
  no token. This is why "a random MCP client connecting to your instance" cannot
  happen: nothing is listening.
- **subprocess ↔ daemon** is the real auth boundary: loopback HTTP, bearer token from
  `control.token`. The token is never embedded in any generated config and never
  logged; the subprocess reads the token file at runtime.

## 1. Local, default dirs (the common case)

Write the launch snippet into your host config. It carries no secrets:

```sh
rabbot init --connect-claude project          # ./.mcp.json (Claude Code project)
rabbot init --connect-claude claude-desktop    # Claude Desktop per-OS config
rabbot init --connect-claude print             # just print it to copy
```

The snippet is just:

```json
{ "mcpServers": { "rabbot": { "command": "/path/to/rabbot", "args": ["mcp"] } } }
```

## 2. Local, custom dirs

What actually decides Hop-2 reachability is whether the subprocess resolves the
**same `control.token` and control port** as the daemon. That token lives in the
**config dir** (`run.go` writes it under `config.ResolveConfigDir()`); the mcp child
reads it from the config dir keyed off `--config` and **no longer opens the DB at
all** (the bridge reads everything over the control client). So:

- **Custom `--data-dir`:** running connect-claude under it bakes `--data-dir` into
  the snippet args, but this is **forward-compat only** — it touches **neither** the
  token **nor** the control port, so it does **not** affect reachability. It is
  harmless, just not load-bearing.

  ```sh
  rabbot --data-dir /srv/rabbot init --connect-claude project
  # -> args: ["mcp", "--data-dir", "/srv/rabbot"]   # baked, but a no-op for Hop 2
  ```

- **Custom config dir (the one that matters):** config-path baking is not yet wired
  into `init --connect-claude`, so the snippet won't carry `--config`. To keep the
  subprocess coherent with a daemon under a non-default config dir, make sure the
  MCP host launches the child with the **same environment** the daemon uses — a
  Claude-spawned child inherits the parent's env, so exporting the same
  `XDG_CONFIG_HOME` (and `XDG_DATA_HOME` / `RABBOT_DATA_DIR` where you set them)
  before launching the host is what lines up the token and control port. The token
  is never embedded; the child reads it from that resolved config dir at runtime.

## 3. VPS over SSH (remote daemon, never an open port)

A daemon on a VPS stays loopback-only — you never expose the control port. Instead,
Claude launches the subprocess **on the VPS** over SSH, so the loopback token never
leaves the box and JSON-RPC rides the encrypted SSH pipe:

```sh
rabbot init --connect-claude project --connect-remote you@vps           # remote bin defaults to "rabbot" on PATH
rabbot init --connect-claude project --connect-remote you@vps --connect-remote-bin /opt/rabbot/rabbot
```

emits:

```json
{ "mcpServers": { "rabbot": { "command": "ssh", "args": ["you@vps", "rabbot", "mcp"] } } }
```

**Prerequisites and caveats:**
- **SSH key auth**, non-interactive: the host must reach `you@vps` without a password
  prompt (use an `ssh-agent` key or a configured `~/.ssh/config` host). A password
  prompt will hang the stdio handshake.
- **Host-key trust-on-first-use:** SSH to the VPS manually once first so the host key
  is pinned in `known_hosts`; otherwise the first launch stalls on the unknown-host
  prompt.
- **`--connect-remote-bin`** is the escape hatch when `rabbot` is not on the VPS's default
  `PATH` (e.g. a release dropped under `/opt`). `os.Executable()` only knows the
  *local* path, so the remote command defaults to the bare name `rabbot`.
- The token, the DB, and the loopback bind all stay on the VPS. The identity chain is:
  **your SSH key** (only you can launch it) → **the VPS host key** (the right machine)
  → **the rabbot token on the VPS** (the right daemon).

## Confirming it actually works — three layers

Because MCP returns errors-as-data, the stdio handshake (and so `/mcp` "connected")
succeeds **even when the daemon is down**. "Connected" only proves the subprocess
launched, not that it can reach the daemon. Confirm all three layers:

1. **`claude mcp list` / `/mcp`** → the `rabbot` server shows `connected`. This
   proves **Hop 1 only** (the subprocess launched).
2. **`rabbot doctor`** (control-plane readiness) → checks the binary path, that
   `control.token` is present/readable/0600, and that the **daemon is reachable and
   the token authenticates** (`GET /v1/health` → healthy). Prints ✓/✗ with remediation.
   This is what actually proves **Hop 2**.
3. **Ask Claude to run `get_status`** → a healthy response proves the full
   `Claude → subprocess → daemon → DB` chain end to end.

## "Open once" — keep the daemon up

MCP never manages the daemon's lifecycle; Claude never starts or stops it. Install the
daemon as an OS service so it auto-starts on boot/login and stays up:

```sh
rabbot service install      # states when elevation is needed; never silently escalates
rabbot service start
```

After that you open the daemon **once**, and from then on you live in Claude. If the
daemon is down, every read tool returns a friendly payload — the literal message
(`mapBridgeError` in `internal/mcp/tools.go`) is: *"daemon not running — it is installed
as a service; try restarting it (e.g. `rabbot service start`)"* — and every action
returns the same remediation as a clean tool error — never a partial write, never a panic.

## Troubleshooting

- **`/mcp` says connected but tools return "daemon not running":** Hop 1 is fine, Hop 2
  is not. Run `rabbot doctor` — either the daemon is down (`rabbot service start`)
  or the subprocess resolved a different **config dir** than the daemon (so it read a
  different `control.token`/control port). Ensure the MCP host launches the child with
  the same `XDG_CONFIG_HOME`/env the daemon uses (see §2).
- **Tools return "token mismatch":** the subprocess and daemon disagree on the config
  dir, so they loaded different `control.token` files. The token lives in the config
  dir (not the data dir); make both resolve the same config dir — pass the same
  `--config` to the daemon and export the same `XDG_CONFIG_HOME` for the MCP host that
  launches the child (config-path baking into the snippet is not yet wired).
- **VPS launch hangs:** a password or unknown-host prompt is blocking stdio. Fix SSH
  key auth and pin the host key (`ssh you@vps` once manually), then retry.
