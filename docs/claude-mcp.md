# Drive Rabbot-SEO from Claude (MCP)

`rabbot mcp` lets an MCP host (Claude Code, Claude Desktop) drive your monitor from
inside Claude — reading status and taking safe, confirmed actions. It runs over
[Model Context Protocol](https://modelcontextprotocol.io) on **stdio with no network
endpoint**: the host launches it as a subprocess, so there is nothing for a stranger to
connect to. The subprocess talks to your running daemon over the loopback control API,
authenticated by the `control.token` file (0600) — never embedded in any generated
config, never logged.

Once connected, Claude can **read and take safe, confirmed actions**:

- **Read:** full status, your sites, per-site SEO detail, open issues, a URL's
  change history, a windowed cross-site activity digest, sitemap-coverage drift,
  a URL's rich-result eligibility, and a site's health score over time
  (tools `get_status`, `list_sites`, `get_site`, `list_issues`, `get_history`,
  `summarize_changes`, `get_coverage`, `get_rich_results`, `get_health_score`; the
  three resources `rabbot://health|status|sites` remain for `@`-mention).
  `summarize_changes` mirrors `rabbot report` over the daemon: it returns
  **structured facts** (change volume with a substantive/cosmetic split, top changed
  URLs, an issue rollup, and a per-site breakdown) for Claude to summarise. It defaults
  to the last **7 days**; pass `since` (a Go duration like `"24h"`) for any ad-hoc
  window, `site_id` to scope to one site, and `segment` to scope to a named slice of a
  site. **Segments** are named URL slices (e.g. `content` for `/blog/**`, `product` for
  `/product/**`) configured per site; `list_issues` and `summarize_changes` both accept
  an optional `segment` filter, and `get_site` lists each site's configured segments
  (name, match pattern, member count) so you can discover the filterable names. An
  unknown segment name is reported as an empty result, never an error. `get_coverage` mirrors
  `rabbot sitemap coverage`: it reconciles a site's declared sitemap against the crawled
  inventory and returns the four counts (sitemapped-but-uncrawled, declared-but-unadmitted,
  crawled-but-unlisted, and total) with sample URLs per bucket; pass `site_id` to scope.
  `get_rich_results` mirrors the `rabbot inspect` rich-results section over the daemon:
  it validates a URL's latest-crawl JSON-LD against the in-binary Rabbot rich-result
  profile and returns per-type eligibility (the type, whether it is eligible, and the
  missing required/any-of properties when it is not) plus the profile version. The profile
  mostly mirrors Google's documented requirements but encodes a few stricter Rabbot policy
  choices (e.g. it flags an `Article` with no `headline`, which Google lists as recommended,
  not required), so a missing property is a Rabbot policy verdict, not always a Google one.
  Validation is **presence-driven** — only the structured-data types the site already
  ships and the profile encodes get a verdict; other types are reported only as a
  neutral `unprofiled` count, never as a recommendation to add markup.
  `get_health_score` returns a site's (or a named segment's) **LIVE 0-100 health score**
  plus its recorded trend: the current score with a `defined` flag, the canonical
  impact/max masses, the crawl-coverage counts (`known_urls`/`processed_urls`), open-issue
  counts by severity, an uncapped per-rule breakdown of what hurts the score most, and the
  time-series (defaults to the last **7 days**; pass `since` as a Go duration). The score
  stays **undefined** (`defined=false`, rendered `—` — never a fake 100 or 0) until at
  least half the scope's known URLs have been crawled; pass `segment` to scope to a named
  slice. An unknown site id or segment name is reported as `not_found`, never an error.
  `blast_radius` and `what_links_to` answer the internal-link questions over a URL: how
  many pages link to it and how many are high-importance (*"how bad is it if this URL goes
  dark?"*), and which pages point at it ranked by source importance. For these two, a URL
  with no inbound links is reported as `not_found`, never an error. `get_link_graph`
  hands you a **bounded** internal-link graph as JSON — a focus URL's in+out neighborhood
  (≤ 2 hops; `hops` > 2 is rejected) or a whole-site overview grouped by segment (or by
  top-level folder when no segments are configured). The export is hard-capped server-side
  (tens of KB, never megabytes); when it is clipped, `truncated=true` and
  `total_nodes`/`total_edges` report **at least this many** (bounded by the server
  ceiling) so you can say the graph is larger than shown. An unknown site id is reported
  as `not_found`, never an error. (There is
  deliberately **no orphans tool**: orphan pages surface in the `page_orphaned` issue
  stream via `list_issues`, and `rabbot links --orphans` covers the pull side.)

  **Node identity is exact-string** (fragment-stripped only): `https://site/a`,
  `https://site/a/`, and `https://site/a?utm=x` are **three distinct nodes**. This is a
  deliberate LITE limitation — pass the exact monitored URL, and do not assume a
  trailing-slash or query-string variant is the same page.

  **Ask Claude to draw your site.** Rabbot never renders a picture — it emits JSON and
  *the agent draws*. Call `get_link_graph` (with a `focus` URL for a neighborhood, or no
  focus for the overview) and then ask Claude to render it: *"draw this 404's blast
  radius as a Mermaid diagram"* or *"render the site overview as an HTML graph"*. Claude
  turns the bounded node/edge JSON into a Mermaid `graph TD`, an inline SVG/HTML diagram,
  or whatever the host can show — directly in the chat.

  Tools added by this surface: `blast_radius`, `what_links_to`, `get_link_graph`.

  **Google Search Console (read-only ground truth).** When a site is connected to its
  Search Console property, two tools expose Google's own data over the daemon:
  `get_index_status` returns the latest URL-inspection verdict for one URL — Google's
  `verdict`, `coverage_state` (e.g. *"Submitted and indexed"*, *"Crawled - currently not
  indexed"*, *"Discovered - currently not indexed"*), `indexing_state`, `robots_txt_state`,
  `page_fetch_state`, the canonical **Google chose** (`google_canonical`) vs the one you
  **declared** (`user_canonical`), `crawled_as`, when Rabbot last inspected it, and Google's
  last crawl time — so you can compare Google's ground truth against Rabbot's own indexability
  verdict. `get_search_performance` returns the stored search rows for one URL — clicks,
  impressions, CTR, and average position per `(query, day)`, newest first, bounded by `since`
  (a Go duration, default **7 days**). Only **finalized** data is stored (`dataState=final`),
  so the most recent ~3 days are excluded by design; it is the read view behind change-vs-search
  correlation (*"did this title change cost impressions on its primary query?"*). Both tools
  report **absent GSC data honestly**: the URL-inspection quota is bounded (~2000/day/property),
  so a monitored URL may have no inspection on record yet — that is reported as
  `has_status=false` / `has_data=false`, **never a guess and never an error**. Rabbot
  deliberately does **not** fire standalone traffic/impression/ranking-drop alerts (seasonality
  and SERP volatility make them noise) — these tools expose the raw data read-only.

  Tools added by this surface: `get_index_status`, `get_search_performance`.
- **Act:** add a site, recheck now, pause/resume monitoring (global), ignore an issue,
  send a test alert, set an **allow-listed** config key, and run ownership verification
  (`add_site`, `recheck_site`, `pause_monitoring`, `resume_monitoring`, `ignore_issue`,
  `send_test_alert`, `set_config`, `verify_begin`, `verify_check`).

Every action is annotated so the host shows you a **permission prompt** before it runs.
`set_config` accepts only a strict allow-list of safe keys and **echoes key names, not
values** — the unverified throttle floor, notifier secrets, and the database path can
never be set or read back over MCP. **Destructive/lifecycle operations
(remove-with-purge, config `reload`, daemon `shutdown`) are deliberately not exposed**
and stay CLI-only.

Point Claude at it with the generated config (the snippet carries no secrets):

```sh
./rabbot init --connect-claude print            # print the snippet to copy
./rabbot init --connect-claude project          # write ./.mcp.json (Claude Code project)
./rabbot init --connect-claude claude-desktop   # write the Claude Desktop config (per-OS)
```

The local snippet just launches `rabbot mcp` over stdio:

```json
{ "mcpServers": { "rabbot": { "command": "/path/to/rabbot", "args": ["mcp"] } } }
```

## Remote daemon on a VPS — over SSH, never an open port

A daemon on a VPS stays **loopback-only**; you never expose the control port. Claude
launches the subprocess **on the VPS over SSH**, so the token never leaves the box and
JSON-RPC rides the encrypted SSH pipe:

```sh
./rabbot init --connect-claude project --connect-remote you@vps
./rabbot init --connect-claude project --connect-remote you@vps --connect-remote-bin /opt/rabbot
```

emits:

```json
{ "mcpServers": { "rabbot": { "command": "ssh", "args": ["you@vps", "rabbot", "mcp"] } } }
```

This needs **non-interactive SSH key auth** and the VPS **host key already pinned** in
`known_hosts` (SSH to the box once manually first). `--connect-remote-bin` is the escape hatch
when `rabbot` is not on the VPS's default `PATH`. See the
[connect guide](mcp-connect-guide.md) for the full recipe.

**Dedicated SSH key? Pin it in `~/.ssh/config`.** The emitted snippet runs a bare
`ssh you@vps` with **no `-i <key>`** by design — it relies on your SSH client config rather
than baking a key path into the tracked `.mcp.json`. If the VPS was created with a
*dedicated* key (the usual case — you paste a key at instance creation), that bare `ssh`
tries your *default* key and fails, so the non-interactive subprocess can't authenticate
and `/mcp` shows `rabbot` not connected. Add a `Host` block so `ssh you@vps` resolves to
the right key unattended:

```sshconfig
# ~/.ssh/config
Host vps
  HostName <ip-or-hostname>
  User you
  IdentityFile ~/.ssh/<dedicated-key>
  IdentitiesOnly yes      # use ONLY this key — don't offer every loaded key
  BatchMode yes           # fail fast instead of prompting (the subprocess can't answer)
```

Then generate the snippet against that alias (`--connect-remote vps`) and Claude's
`ssh vps rabbot mcp` authenticates without a prompt. Confirm with `claude mcp list` →
`rabbot ✔ Connected`.

**Scope a personal remote daemon to `local`.** `--connect-claude project` writes
`./.mcp.json` — a **tracked, shared** file. For your own remote daemon you usually don't
want it committed into a teammate's checkout; register it at user/local scope instead, with
the same launch command:

```sh
claude mcp add rabbot --scope local -- ssh vps rabbot mcp
```

`--scope local` keeps the entry in your personal Claude config rather than the project's
`.mcp.json`, so a personal VPS connection never lands in version control.

## Confirming it works — three layers

Because MCP returns errors-as-data, `/mcp` can show **connected even when the daemon is
down**. Confirm all three:

1. **`claude mcp list` / `/mcp`** → `rabbot` shows `connected` (proves the subprocess
   launched — Hop 1 only).
2. **`rabbot doctor`** → control-plane readiness: binary path valid, `control.token`
   present/0600, and the **daemon reachable and the token authenticates**. Prints ✓/✗
   with remediation. This is what proves Hop 2.
3. Ask Claude to run **`get_status`** → a healthy response proves the full
   Claude → subprocess → daemon → DB chain.

## Open once — keep the daemon up

MCP never manages the daemon's lifecycle. Install it as an OS service so it auto-starts
and stays up; then you **open the daemon once** and live in Claude:

```sh
./rabbot service install      # states when elevation is needed; never silently escalates
./rabbot service start
```

If the daemon is down, every read tool returns a friendly payload (paraphrasing the
literal message in `internal/mcp/tools.go`: *"daemon not running — it is installed as a
service; try restarting it (e.g. `rabbot service start`)"*) and every action returns
the same remediation as a clean tool error — never a partial write.
