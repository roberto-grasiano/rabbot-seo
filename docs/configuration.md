# Configuration & operation

Everything about configuring and operating Rabbot-SEO once it's running: how settings are
resolved, the headless `init` flags, the Slack / email / webhook alert channels and how
they route, connecting Google Search Console, reading what changed, and database retention.

## Configuration precedence

Configuration precedence is **defaults → `config.yaml` → `RABBOT_`-prefixed env vars →
CLI flags** (koanf). Secrets belong in env vars, not the repo; `control.token` and the
`*.db` files are gitignored.

`rabbot init` with no flags and no terminal writes a commented config template you can
edit by hand. Other commands you'll use to drive a running daemon: `pause` / `resume`
(global crawl kill-switch), `sites add|show|remove`, `issue ignore <id>`,
`config get|set|path|validate`, `version`. Run `rabbot <command> --help` for flags.

## Headless setup flags

For CI or scripted installs, pass everything to `rabbot init` up front:

```sh
rabbot init \
  --contact-email you@yourdomain.example \
  --site https://yourdomain.example \
  --i-am-authorized \
  --slack-webhook '${RABBOT_SLACK_WEBHOOK}' \
  --install-service \
  --start
```

The `--slack-webhook` value is written to config **verbatim** — pass a
`${RABBOT_SLACK_WEBHOOK}` token to keep the secret in the environment (koanf
expands it at load) — and is **never echoed to the screen or logs**. `init` sends a
best-effort test alert so you can confirm delivery (a failure is advisory, never
fatal). `--install-service` always **states when elevation is needed and never
silently escalates** — the OS prompts you. `--start`/`--install-service` failures
surface the OS error but do **not** roll back the written config.

`--contact-email` is **mandatory** — it identifies your crawler to the sites you watch.
For how that contact and your verification state shape the crawler's `User-Agent`, see
[Verification & crawler identity](verification.md).

**Reconfigure / add a site.** Re-running `init` on a terminal with an existing
config offers Add a site / Reconfigure / Cancel; headlessly, use
`init --add-site --site https://another.example ...` to append (sites are deduped,
so it is safe to re-run) or `--force` to overwrite the scaffold.

## Alert channels

Rabbot-SEO ships three alert channels. A `notifiers:` entry declares one
destination; a `routes:` entry points alerts at it. **A notifier with no route never
fires** — the dispatcher walks `routes` top-to-bottom with no implicit fallback, so
every channel needs at least one route (the wizard writes a catch-all for you).

| `type` | What it does | Secret-bearing fields |
| --- | --- | --- |
| `slack-webhook` | Posts a [Block Kit](https://api.slack.com/block-kit) card to a Slack Incoming Webhook | `url` |
| `email-smtp` | Sends a plain-text email per alert over SMTP | `password` |
| `generic-webhook` | POSTs a stable, versioned JSON payload to a URL you control | `url`, `headers` values |

Every secret-bearing value may be a `${RABBOT_…}` token kept in the environment (koanf
expands it at load); it is written to config **verbatim** and is **never echoed to the
screen, logs, errors, or `rabbot config get`** — `notifiers.*` is denied from the
control plane. Test any channel with **`rabbot notify test <name>`**.

> **Restart to apply.** The alerting stack is built **once at daemon startup**. After
> you edit `notifiers:` or `routes:` in `config.yaml`, restart the daemon
> (`rabbot stop` then `rabbot run` / your service manager) for the change to take
> effect — a config reload re-syncs *sites* only, not notifiers. (Rebuilding the
> alerting stack on reload is a planned follow-up.)

### Slack

Rabbot-SEO posts a Block Kit alert to Slack the moment a page regresses. The fastest
path is at setup (see the README quickstart); to wire it on an already-running daemon,
add it to `config.yaml` by hand and restart:

```yaml
notifiers:
  - name: slack
    type: slack-webhook
    url: ${RABBOT_SLACK_WEBHOOK}   # or paste the URL directly

routes:
  - notifier: slack                   # catch-all: every alert → Slack
```

Want to cut the noise but still catch what matters? Route the **critical** *and*
**warning** tiers — warnings include content rewrites and title/description/heading
changes, so a `critical`-only route would silently skip them; only the cosmetic
`info` tier is dropped. Routes match top-to-bottom, first match wins:

```yaml
routes:
  - match: { severity: critical }     # noindex, canonical lost, 4xx/5xx, indexability flip
    notifier: slack                   # match keys: severity | site | change_type | segment
  - match: { severity: warning }      # content rewrite, title/meta/heading change
    notifier: slack
```

Critical changes (noindex shipped, canonical lost, a 4xx/5xx, an indexability flip)
alert **immediately**; warnings and info also deliver in **real time, up to a per-recipient hourly cap** — only alerts beyond that cap roll into an **hourly digest**. Alerts are
deduplicated and rate-capped, and the webhook URL is scrubbed from every log line. The
routing, dedup, hourly cap, and digest below are **shared by all three channels** — the
email and webhook channels inherit them with no extra config.

### Routing

A `routes:` entry points alerts at a notifier. The dispatcher walks `routes`
top-to-bottom, **first match wins**, with **no implicit fallback** — a notifier with
no route never fires, so every channel needs at least one route (a bare
`- notifier: name` with no `match` is a catch-all). Each `match` is a set of
**all-of** key/value conditions (every key in one `match` must hold for that route to
fire). There are **four** match keys:

| Key | Matches when | Example |
| --- | --- | --- |
| `severity` | the alert tier equals the value | `{ severity: critical }` |
| `site` | the alert's site base URL equals the value | `{ site: https://example.com }` |
| `change_type` | the change/rule type equals the value | `{ change_type: title_changed }` |
| `segment` | **any** of the alert's segments equals the value (any-of) | `{ segment: content }` |

An **unknown** match key never matches (the route is silently skipped) — a typo can't
accidentally catch every alert. Combine keys in one `match` to AND them:
`{ segment: product, severity: critical }` routes only critical alerts on `product`
pages.

The **`segment`** key routes by a named slice of the site (`/blog`, `/product`, …).
It is any-of: a route fires if *any* segment the alerting URL belongs to equals the
value. An alert with **no** segment — a site-level `robots.txt`/sitemap event, or a
URL that matches no segment — never matches a `segment` route and falls through to a
later one. Define the slices under `sites[].segments` and see the full
**`/blog → #content`, `/product → #growth`** demo in
[Segments](segments.md).

```yaml
routes:
  - match: { segment: content }       # any /blog/** alert → #content
    notifier: slack-content
  - match: { segment: product }       # any /product/** alert → #growth
    notifier: slack-growth
  - notifier: slack-content           # catch-all: everything else (incl. site-level events)
```

### Email (SMTP)

The `email-smtp` channel sends one **plain-text** message per alert. The subject carries
severity + site + change type so it is scannable and filterable; the body has the
labeled Before/After and the rolled-up affected pages.

```yaml
notifiers:
  - name: ops-mail
    type: email-smtp
    smtp_host: smtp.your-provider.com
    smtp_port: 465                  # 465 ⇒ TLS from the first byte; any other port ⇒ STARTTLS required
    username: alerts@example.com    # omit for an unauthenticated local relay
    password: ${RABBOT_SMTP_PASS}   # secret — interpolated from the env, never logged
    from: rabbot@example.com
    to: [seo-team@example.com]      # one or more recipients; one message is sent to all
    # allow_plaintext: true         # ONLY for a localhost relay with no TLS (see below)

routes:
  - match: { severity: critical }   # e.g. mail only the loud ones
    notifier: ops-mail
```

**Transport security is automatic and fails closed.** Port **465** dials implicit TLS
(SMTPS) — encrypted from the first byte. **Every other port** (587, 25, …) **requires
STARTTLS**: if the server will not upgrade, the send fails rather than leaking your
credentials in clear text. When a `username` is set, Rabbot uses `PLAIN` auth — and the
Go SMTP client refuses `PLAIN` over an unencrypted, non-localhost connection, so a
misconfigured relay can't silently send your password in the open. `allow_plaintext:
true` opts out of STARTTLS **only** for a localhost-style relay (e.g. a dev mail catcher
on `127.0.0.1`); never set it for a remote host.

> **Why Rabbot relays through your provider instead of delivering mail itself.** Most
> VPS hosts block outbound port 25, and even where they don't, mail you send straight
> from a fresh server IP lands in spam — deliverability (SPF/DKIM/DMARC, IP reputation,
> warm-up) is a problem your mail provider has already solved. Rabbot hands the message
> to an SMTP relay you already trust and lets it do the delivering.

Three setups that work out of the box:

**(a) A Gmail account with an app password.** With 2-Step Verification on, create a
16-character [app password](https://myaccount.google.com/apppasswords) (your normal
login password will not work for SMTP):

```yaml
  - name: gmail
    type: email-smtp
    smtp_host: smtp.gmail.com
    smtp_port: 465
    username: you@gmail.com
    password: ${RABBOT_GMAIL_APP_PASSWORD}   # the 16-char app password, spaces removed
    from: you@gmail.com
    to: [you@gmail.com]
```

**(b) Your hosting or domain provider's SMTP.** If you already have a mailbox with your
web host, domain registrar, or a workspace email service, use the SMTP host, port, and
mailbox credentials from their docs — the shape is identical, only the host changes:

```yaml
  - name: provider-mail
    type: email-smtp
    smtp_host: smtp.your-provider.com   # from your provider's "email / SMTP settings" page
    smtp_port: 587                      # 587 (STARTTLS) and 465 (TLS) are both common
    username: alerts@your-domain.com
    password: ${RABBOT_SMTP_PASS}
    from: alerts@your-domain.com
    to: [seo-team@your-domain.com]
```

**(c) A transactional-email provider's free tier.** Several transactional-email services
offer a small free monthly send quota with a ready-made SMTP endpoint — a good fit for
low-volume alerting. Create an API/SMTP credential in the provider's dashboard and plug
in the host and port they give you:

```yaml
  - name: transactional
    type: email-smtp
    smtp_host: smtp.your-transactional-provider.example
    smtp_port: 587
    username: ${RABBOT_SMTP_USER}       # often an API-key-style username
    password: ${RABBOT_SMTP_PASS}       # the provider's SMTP secret
    from: rabbot@your-verified-domain.com   # usually must be a verified sender/domain
    to: [seo-team@example.com]
```

Verify a channel anytime with **`rabbot notify test ops-mail`**. The email digest ships
at Slack parity — **one message per buffered alert** on each hourly flush (a single
roll-up summary email is a planned follow-up).

### Generic webhook

The `generic-webhook` channel POSTs a **stable, versioned JSON payload** to one URL you
control. It is the channel that unlocks everything else — PagerDuty, ntfy, automation
platforms (n8n / Zapier / Make), or your own service — without Rabbot learning each one:

```yaml
notifiers:
  - name: glue
    type: generic-webhook
    url: ${RABBOT_GLUE_WEBHOOK}
    headers:                          # optional static headers (sent on every POST)
      Authorization: ${RABBOT_GLUE_TOKEN}   # secret — interpolated, never logged

routes:
  - notifier: glue                    # catch-all
```

The optional `headers` are sent verbatim on every request — `Authorization: Bearer …`
covers the auth scheme every common target accepts. Header **values** may be `${ENV}`
tokens and are scrubbed from any error; only the value is a secret, not the header name.
Delivery retries on `429` (honoring an integer-seconds `Retry-After`) and `5xx` with
bounded backoff, matching the Slack policy; other `4xx` are terminal. The POST body is a
versioned, snake_case contract — build your glue against it and it won't break when
Rabbot's internals change:

```json
{
  "payload_version": 1,
  "site": "example.com",
  "url": "https://example.com/p",
  "change_type": "title",
  "severity": "warning",
  "before": "Old Title",
  "after": "New Title",
  "detected_at": "2026-06-10T12:00:00Z",
  "group_key": "example.com|title",
  "related_count": 3,
  "deep_link": "https://example.com/p",
  "operational": false,
  "items": [
    { "url": "https://example.com/a", "before": "a-old", "after": "a-new" }
  ]
}
```

`payload_version` is the first field so your glue can branch on it; it is bumped (never
reused) on any breaking change to the wire shape. `severity` is one of
`critical | warning | info`; `operational: true` marks an access/monitoring incident
(site unreachable or crawl blocked) rather than an SEO change.

#### Microsoft Teams & Discord — via glue or an automation platform

Teams and Discord **reject arbitrary JSON** — they require their own payload shapes
(Teams an Adaptive Card, Discord a `content`/embeds object). So you connect them by
pointing `generic-webhook` at a small adapter that re-shapes the payload: an automation
platform (n8n / Zapier / Make) or a few lines of your own glue that receives Rabbot's
JSON and forwards the Teams/Discord shape. Native `teams-webhook` / `discord-webhook`
channels that render those payloads directly are a planned good-first-issue — see
[docs/extending-notifiers.md](extending-notifiers.md), which walks adding a channel in
~100 lines using this webhook as the worked example.

### What raises an alert

Beyond the field-level change events above, Rabbot runs a set of named **rules** whose
findings route by the same tiers. Each opens an issue (always visible in `rabbot issues`)
and — except the info tier — pages through your routes. The `change_type` shown in an
alert is the rule's own id, so you can route a single rule with a
`match: { change_type: <rule_id> }`:

- **critical** — `status_regression`, `indexability_flip`, `meta_robots_noindex`,
  `canonical_changed`, and **`redirect_loop`** (a redirect chain that
  revisits a URL, e.g. `A→B→A`, burning crawl budget within the redirect cap); plus the
  structured-data rich-result rules **`rich_result_product`** (the marquee check),
  **`rich_result_article`**, and **`rich_result_breadcrumb`** — these page as **critical
  only on a *lost-eligibility flip*** (the page previously had ≥ 1 eligible entity of that
  type and now has none). Any other ineligibility — markup that was never eligible, or a
  partial loss where some entities still qualify — opens the same rule at **warning**, not
  critical.
- **warning** — `title_changed`, `meta_description_changed`, `h1_issue` (a *missing* or
  *changed* H1; a **multiple-H1** page is recorded at **info** instead, since multiple H1s
  are not an error under current Google guidance), `hreflang_invalid` (the page's hreflang
  set *changed* — a change detector only; Rabbot does not yet validate BCP-47 or
  reciprocity), `broken_links_spike`, and the new SERP-fit and dormant-signal rules:
  **`title_pixel_overflow`** / **`meta_description_pixel_overflow`** (the title or
  description renders wider than the desktop result container — measured in pixels, not
  characters — so it will truncate), **`external_link_spike`** (external links jumped
  sharply, a classic injected-link tell), **`image_alt_regression`** (more images lost
  their `alt` text than last crawl), and **`redirect_chain_growth`** (the chain gained a
  hop without looping); plus the internal-link-graph rules:
  **`page_orphaned`** (a previously-linked page lost its **last** internal inlink — fires
  only on the 1+→0 transition, never on a page that was never linked), **`inlink_loss`** (a
  page lost ≥ 50% of its inbound internal links from a base of ≥ 5), and
  **`click_depth_regression`** (a page's click-depth from the home page worsened by ≥ 2
  levels); plus two structured-data / rendering rules:
  **`structured_data_invalid_json`** (the latest crawl carries one or more JSON-LD blocks
  that failed to parse — self-suppressed when the body was truncated, since a cut `<script>`
  is the expected cause, not a real defect) and **`needs_rendering`** (the page's render
  mode shows its SEO content is no longer fully recoverable from the server HTML —
  `head_only_shell`, so only the head is monitored, or `client_shell`, so only the fetch
  status is — and it closes on recovery to a `server_rendered`/`hydrated` mode).
- **info** — `image_alt_missing` (a page with several images whose `alt` coverage is below
  the floor). Info findings open an issue but never page.

The pixel-overflow alerts carry the measured width and budget in their detail
(`{"measured_px":906,"budget_px":580,"chars":48}`); the count/depth rules carry
`{"old":N,"new":M}`. The internal-link-graph rules carry the URL plus their own
shape — `page_orphaned` → `{"url":…,"importance":…}`, `inlink_loss` →
`{"url":…,"before":N,"after":M}`, `click_depth_regression` →
`{"url":…,"old_depth":N,"new_depth":M}`. The rich-result rules carry the profile version,
the matched type, and the missing properties for the first ineligible entity —
`{"profile":"…","type":"Product","entities":N,"ineligible":M,"missing":[…]}`;
`structured_data_invalid_json` carries `{"invalid_blocks":N}`; and `needs_rendering` carries
the detected mode and what is still monitored — `{"render_mode":"head_only_shell","monitored":"head_only"}`
or `{"render_mode":"client_shell","monitored":"fetch_status_only"}`. A pre-existing long
title that was already over budget when you upgrade opens its issue **silently** (no page) —
only a title/description *edited* into overflow pages, so an upgrade never stampedes your
channel.

> **Note (behavior change):** the opaque per-hop `redirect_chain` alert was **retired**.
> Redirect-chain churn that neither grows nor loops no longer pages (it is still recorded
> as history for `rabbot report`); the parsed `redirect_chain_growth` and `redirect_loop`
> rules above own redirect alerting now. If you previously relied on the raw chain alert,
> these two replace it with the two states that actually matter.

## At least one channel — or deliberately none

A real-time monitor with **zero** alert channels still crawls and records every change,
but tells no one — so Rabbot is loud about that state without ever hard-blocking:

- **The setup wizard requires an explicit choice.** `rabbot init` asks how you want to
  be alerted and makes you pick: configure **Slack**, **email**, or a **webhook**, *or*
  deliberately choose **"no alerts — CLI/MCP only"**. There is no silent skip — backing
  out re-presents the choice, because pull-only is a legitimate option you select on
  purpose, not a default you fall into.
- **`rabbot run` warns at startup** when no notifier is configured — once, prominently —
  and points at `rabbot init` (or `rabbot report` to read changes). It does **not**
  block: the daemon starts and runs normally.
- **`rabbot doctor` reports the zero-channel state** as a warning line, not a failure.

**Pull-only is a legitimate trial mode.** If you just want to watch a site and read what
changed yourself — via `rabbot report`, `rabbot history <url>`, `rabbot inspect <url>`,
or your MCP host — choosing "no alerts" is a fully supported way to run. Add a channel
any time by re-running `rabbot init`.

## Google Search Console

Connect a monitored site to its **Google Search Console** property and Rabbot-SEO reads
Google's own ground truth alongside what it sees on the page — whether Google has
**indexed** a URL, which **canonical** Google actually chose, and the URL's **search
performance**. It is **read-only** and **self-hosted**: your Search Console data is pulled
straight into your own SQLite file and **never leaves your box** — unlike cloud SEO
platforms that ingest your Search Console into their servers. What it does (and
deliberately doesn't) alert on, plus the quota/latency notes, lives in the concise
**[Search Console guide](gsc.md)**; this section is the per-site **config + credential**
reference.

GSC is **opt-in per site** — a site with no `gsc:` block is simply not connected. Add the
block under the site in `config.yaml` (the `rabbot init` wizard's *"Connect Google Search
Console"* step writes it for you, then offers a connectivity check):

```yaml
sites:
  - url: https://www.example.com
    gsc:
      property: sc-domain:example.com          # OR a URL-prefix: https://www.example.com/
      auth: service_account                    # "service_account" | "oauth2"
      service_account_key_file: /etc/rabbot/gsc-sa.json   # 0600 path (service_account)
      # oauth_token_file: /etc/rabbot/gsc-oauth.json      # 0600 path (oauth2) — set ONE
```

| Key | Required | What it is |
| --- | --- | --- |
| `property` | yes | The property **exactly** as it appears in Search Console: a **Domain** property `sc-domain:example.com` (covers every scheme + subdomain) **or** a **URL-prefix** property `https://www.example.com/`. |
| `auth` | yes | The credential model: `service_account` (a GCP JSON key, headless — no browser) or `oauth2` (a one-time browser consent → refresh token). |
| `service_account_key_file` | with `service_account` | **Path** to the 0600 service-account JSON key. |
| `oauth_token_file` | with `oauth2` | **Path** to the 0600 OAuth refresh-token file written by `rabbot gsc auth`. |

The two credential keys are **mutually exclusive** — set exactly the one your `auth` mode
needs; setting both (or the wrong one) fails validation with a clear message.

### Credentials are a path, never a body — and never logged

The credential keys hold a **filesystem path to a `0600` file**, never the JSON key body
or a token (mirroring `control.token`, not the inline notifier secrets). Rabbot reads the
file at runtime; the **path** may itself be a `${RABBOT_…}` token that koanf expands at
load (handy for keeping it out of `config.yaml`). The credential **content is never
stored in config, logged, echoed into errors, or returned by `rabbot config get`**, and
both credential-path keys are **denied from the control plane** — an MCP agent cannot
repoint Rabbot at an arbitrary credential file. Keep the key file `0600` (`chmod 600`);
`rabbot doctor <url>` flags it if the permissions are too open.

### Service-account setup (recommended — headless)

The service-account path needs no browser, so it fits a daemon on a server. One-time:

1. In the **Google Cloud console**, create (or pick) a **project**.
2. **Enable the Search Console API** for that project (APIs & Services → Library →
   *Search Console API* → Enable).
3. Create a **service account**, then add a **key** of type **JSON** and download it.
4. Copy the JSON key onto the box running Rabbot and lock it down:
   `install -m 600 ~/Downloads/key.json /etc/rabbot/gsc-sa.json` (or `chmod 600`).
5. In **Search Console** → **Settings → Users and permissions**, add the **service
   account's email** (the `client_email` in the JSON, ending `…iam.gserviceaccount.com`)
   with **Restricted/Full read** access to the property you set in `property`.
6. Point `service_account_key_file` at the saved path and run `rabbot doctor <url>` — it
   confirms the key is present + `0600`, that the credential authenticates, and that your
   property is in its verified-site list.

### OAuth2 setup (browser consent)

For a property you own but **can't add a service account to**, use OAuth2. You bring your
**own** Google Cloud OAuth client (no public client ships in Rabbot — that would be an
extractable secret); run the one-time consent locally:

```sh
export RABBOT_GSC_OAUTH_CLIENT_ID='…apps.googleusercontent.com'
export RABBOT_GSC_OAUTH_CLIENT_SECRET='…'        # kept in the env, never on the cmdline
rabbot gsc auth --out /etc/rabbot/gsc-oauth.json # captures the consent on a loopback port
```

`gsc auth` opens a consent URL, captures the redirect on `127.0.0.1`, exchanges the code,
and writes a **`0600`** refresh-token file. On a **headless server**, run `rabbot gsc
auth` on your laptop (where you can open a browser) and **`scp`** the resulting file to the
box, then set `oauth_token_file` to its path. The client secret and tokens are **never
logged**.

> **Latency & quota.** Search-performance data is pulled with `dataState=final`, so the
> most recent **~3 days are partial and excluded** by design — the signals never alert on
> unfinalized data. URL-inspection is **quota-bounded (~2000/day/property)**, so a URL may
> not have an inspection on record yet; that absence is reported honestly (it is **not**
> treated as a problem). Details in the [Search Console guide](gsc.md).

## Reading what changed

`rabbot report` rolls up the preserved change/issue history into a **windowed,
cross-site activity digest**: how much changed (with a substantive/cosmetic split), the
top changed URLs, an issue rollup (open now by severity, opened in the window, resolved
in the window), and — across all sites — a per-site breakdown. Like `history` and
`issues`, it reads the database **directly**, so it works even when the daemon is down.

```sh
./rabbot report                       # default: the last 7 days, all sites
./rabbot report --since 24h           # any ad-hoc window as a Go duration (e.g. 24h, 168h)
./rabbot report --site https://example.com   # scope to one site by base URL
./rabbot report --limit 20            # widen the top-changed-URL list (default 10)
```

The binary emits **structured facts only** — it never writes the prose. Pass `--json`
to pipe the digest straight into Claude (or `jq`) and let the model write the summary:

```sh
./rabbot report --json | claude -p "Summarise this week's SEO activity for the team."
```

The default window is **168h (7 days)**; with no `--site` the digest covers all sites,
and quiet, healthy sites are omitted from the per-site breakdown to keep it readable.
Inside Claude, the same digest is one MCP tool call away — see
[the `summarize_changes` tool](claude-mcp.md). For a single URL, `rabbot inspect <url>`
shows its latest snapshot, open issues, and recent changes; `rabbot history <url>` is its
full change log; `rabbot issues --open` lists open SEO issues.

## Database retention

Rabbot-SEO stores a snapshot of every monitored page on each successful recheck. To
keep the database from growing without bound, a retention sweep runs in the
background (every `sweep_interval`):

- **raw HTML** is kept only on the newest `raw_html_keep` snapshots per URL (older
  HTML bodies are dropped — they have no reader and are not needed for change
  detection);
- **redundant snapshots** older than `snapshot_max_age` that recorded *no change*
  are deleted — every snapshot that recorded a change, and the latest snapshot per
  URL, is always kept, so your change history is preserved;
- **robots.txt / sitemap** snapshots are trimmed to the newest `file_snapshots_keep`
  per site.

Defaults (override any in `config.yaml`):

```yaml
retention:
  enabled: true
  sweep_interval: 6h
  raw_html_keep: 1
  snapshot_max_age: 720h   # 30 days; set to 0 to keep all snapshot rows
  file_snapshots_keep: 10
```

Pruning lets SQLite reuse freed space, so the file stops growing — but it does not
shrink on its own. To physically reclaim disk after a large cleanup, stop the daemon
and run:

```sh
rabbot stop
rabbot db compact
```
