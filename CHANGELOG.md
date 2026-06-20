# Changelog

All notable, user-facing changes to Rabbot-SEO. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/); releases are cut from git
tags (see [docs/RELEASING.md](docs/RELEASING.md)). Entries under **Unreleased** are
folded into the next tag's GitHub release notes.

## [Unreleased]

### Added

- **Verifiable releases — Sigstore signatures + SBOMs.** Every release artifact is
  keyless-signed with cosign (GitHub OIDC → Fulcio → Rekor; no stored keys) and ships a
  per-archive SPDX SBOM. The signature on `checksums.txt` covers every archive
  transitively, so a download verifies in two commands (`cosign verify-blob` +
  `sha256sum -c`) — see [docs/RELEASING.md → *Verify a download*](docs/RELEASING.md#verify-a-download).
  The Docker image is signed the same way.
- **Segments — focus on the pages that matter.** Name slices of a site (`/blog`,
  `/product`, a template family) with an anchored path regexp under `sites[].segments`,
  and every other surface learns to speak in those terms. Alerts **route** by segment
  (`segment` is the fourth route-match key alongside `severity` / `site` /
  `change_type`, so `/blog/**` alerts go to `#content` while `/product/**` go to
  `#growth` — two webhooks, four config lines). `rabbot issues --segment <name>` and
  `rabbot report --segment <name>` **filter** by it (an unknown name returns an empty
  result plus a hint listing the known names, never an error), and a new
  `rabbot segments [--site URL] [--json]` lists each segment's pattern and live
  member count. Over MCP, `list_issues` and `summarize_changes` gain a `segment` input
  and `get_site` exposes the configured segments so an agent can discover the
  filterable names. Membership is M:N (a URL may belong to several segments), matched
  against the URL **path only** (query-string matching is a fast-follow), kept current
  at reconcile and as discovery admits new URLs — no daemon restart and no per-fetch
  database read. Segments **annotate and route**; they never re-group incidents
  (fingerprints are byte-identical). See [docs/segments.md](docs/segments.md) and
  [docs/configuration.md → Routing](docs/configuration.md#routing).
- **Health-score model (site + segment, over time).** Rabbot now rolls up the impact
  points it already scores per issue into a per-site **and** per-segment 0–100 health
  score, recomputed on every recheck and recorded over time when it moves. The score is
  importance-weighted and page-capped (a dead page can't sink the whole site), excludes
  `ignored` and `closed` issues (it reflects acknowledged reality), and is **honest
  under cold start**: it stays `—` (never a fake 100 or 0) until at least half a scope's
  known URLs have been crawled, with each scope's coverage floor applied independently.
  History is **persist-on-change** with **no backfill** — the trend starts at the first
  defined score, and every point is explainable from its own integer
  `impact_mass`/`max_mass`. No new rules, signals, weights, or dependencies; pure SQL on
  the existing tables. The model, its properties, and its frozen stances are recorded in
  [ADR 0002](docs/adr/0002-health-score-model.md).
- **Structured-data (rich-result) eligibility rules.** Four new default rules validate
  the JSON-LD a page already ships against a versioned subset of Google rich-result
  requirements (profile `grr-2026.06`): `rich_result_product`, `rich_result_article`,
  `rich_result_breadcrumb`, and `structured_data_invalid_json`. They are presence-driven —
  a type the page does not implement is never flagged. An eligibility *loss* (a deploy
  drops `offers` from Product markup while the `@type` set is unchanged, so no
  `schema_types` diff fires) pages **critical** within one recheck interval; steady-state
  or partial invalidity opens a **warning** issue. The rules emit nothing when the fetched
  body was truncated (a severed `<script>` is not a real defect) and bridge to Slack under
  their own `change_type`, so a marquee critical is never deduped behind a `schema_types`
  warning.
- **Email alerts (`email-smtp`).** A new alert channel that sends a plain-text message
  per change over SMTP. Port `465` dials implicit TLS; every other port **requires
  STARTTLS** and fails closed rather than sending credentials in the clear
  (`allow_plaintext` opts out only for a localhost relay). The SMTP password is a secret:
  `${ENV}`-interpolated, masked in the wizard, and never logged or echoed into an error.
  See [docs/configuration.md](docs/configuration.md#email-smtp) for Gmail-app-password,
  hosting-provider, and transactional-tier recipes. Ships at Slack parity (one message
  per alert on each hourly digest flush).
- **Generic webhook alerts (`generic-webhook`).** POSTs a **stable, versioned**
  (`payload_version: 1`) snake_case JSON payload to one URL you control — the channel
  that connects PagerDuty, ntfy, automation platforms (n8n / Zapier / Make), or your own
  glue. Optional static request headers cover `Authorization: Bearer …` auth; delivery
  retries on `429`/`5xx` with bounded backoff. The URL and header values are secrets —
  `${ENV}`-interpolated and scrubbed from every error. Microsoft Teams and Discord
  connect via this webhook plus a small adapter; native channels are a planned
  good-first-issue (see [docs/extending-notifiers.md](docs/extending-notifiers.md), the
  "add a channel in ~100 lines" walkthrough).
- **Zero-channel honesty.** A monitor with no alert channel records every change but tells
  no one, so Rabbot now says so without ever hard-blocking: the `rabbot init` wizard
  requires an **explicit** choice (configure Slack / email / a webhook, or deliberately
  pick "no alerts — CLI/MCP only"), `rabbot run` logs a **prominent startup warning** when
  zero notifiers are configured, and `rabbot doctor` reports the zero-channel state.
  Pull-only (read changes via `rabbot report` / `history` / MCP) stays a fully supported
  way to run.
- **SERP-fit alerts.** New warning rules `title_pixel_overflow` and
  `meta_description_pixel_overflow` flag titles/descriptions that render wider than the
  desktop search-result container (measured in **pixels**, not characters, so a few wide
  glyphs clip where many thin ones fit). The alert carries the measured width and budget.
  A pre-existing over-budget title opens its issue silently on upgrade — only a title or
  description *edited* into overflow pages, so upgrading never stampedes your channel.
- **Activated dormant signals.** New rules over data the crawler already stored:
  `external_link_spike` (warning — external links jumped sharply, a classic injected-link
  tell), `image_alt_regression` (warning — more images lost their `alt` text than last
  crawl), `image_alt_missing` (info, issue-only — `alt` coverage below the floor on an
  image-heavy page), `redirect_chain_growth` (warning — the redirect chain gained a hop),
  and `redirect_loop` (critical — the chain revisits a URL, e.g. `A→B→A`, within the
  redirect cap). See [docs/configuration.md](docs/configuration.md#what-raises-an-alert).
- **Sitemap watching.** The scheduled sitemap pass is now a real watch, not just discovery:
  each site's sitemap is rechecked on its existing refresh cadence and diffed against the
  previous snapshot. A `sitemap_xml_status` **critical** fires when the sitemap breaks
  (a `200`→`4xx`/`5xx`/network-error regression, with the reverse reported as recovery), and
  a `sitemap_xml` **warning** fires when the URL set shifts — carrying before/after counts
  and added/dropped sample paths rather than bare hashes. An incomplete collection (a child
  sitemap that fails to fetch) is flagged and **never** raises a mass-drop alert. The first
  pass is a silent baseline, so upgrading never stampedes your channel.
- **Coverage reconciliation.** Rabbot now reconciles what a sitemap *declares* against what
  has actually been crawled, tracking sitemapped-but-uncrawled URLs (including
  declared-but-never-admitted ones, e.g. page-cap exhaustion) and crawled-but-unlisted URLs.
  A `sitemap_coverage_drift` **warning** fires when that drift grows. Read the live counts
  any time with the new **`rabbot sitemap coverage [--site <id|url>] [--json]`** command, the
  loopback `GET /v1/coverage` control endpoint, or the **`get_coverage`** MCP tool.
- **Parser-robustness fuzz suites.** Native Go fuzz targets now guard every parser that eats
  untrusted bytes — `FuzzExtract` (HTML), `FuzzRobots` (`robots.txt`), `FuzzSitemap` (sitemap
  XML, gzip included), and `FuzzNormalizeURL` (URL host comparison) — asserting no-panic plus
  invariants (closed error sets, extraction determinism, finite sitemap priorities, URL
  round-trip stability). A `make fuzz-smoke` target and a CI step run them on every push, and
  minimized crashers are committed under `testdata/fuzz/` so `make test` replays them forever.
  One find landed already: a non-finite (`NaN`) sitemap `<priority>` is now sanitized to the
  `0.5` default before it can reach the discovery priority sort.
- **Self-observability — a Prometheus `/metrics` endpoint + a provisioned Grafana dashboard.**
  Rabbot can now monitor itself. A bounded, cardinality-disciplined metric set (fetches by
  class, fetch duration, changes by cosmetic/substantive, alert dispatches by outcome, digest
  drops, crawls in flight, due backlog, DB size, build info, plus the stock Go/process
  collectors) is served on a **separate, read-only, GET-only, unauthenticated** HTTP listener
  that is **off by default** and binds the loopback default `127.0.0.1:9464` when enabled —
  never on the token-authed control plane. There are **no per-URL or per-site labels, ever**
  (closed enums and notifier config names only; webhook URLs never become labels), and the
  scrape path never touches the database. A new **`rabbot observability init`** generator (and
  the wizard's "See it on a dashboard" step, and `rabbot init --with-grafana`) writes a
  committed, provisioned bundle — `docker-compose.observability.yml`, `prometheus.yml`,
  Grafana datasource + dashboard provisioning, and the dashboard JSON — and sets `metrics.addr`
  (only when unset, so a custom address survives). **Rabbot never runs Docker**: it writes the
  files and prints the one command; you (or an agent) bring the stack up. `rabbot status` (and
  MCP `get_status`) report the metrics address when the listener is on. Grafana runs locally
  behind its stock `admin`/`admin` + forced first-login change (every setup path prints the
  warning). See [docs/observability.md](docs/observability.md) and the agent recipe
  [docs/observability-with-claude.md](docs/observability-with-claude.md).
- **A run-it-24/7 deployment story (and a service identity fix).** New guides say where to run
  Rabbot for real-time monitoring: a [README "Where to run Rabbot-SEO"](README.md) section
  (laptop = trying/staging; an always-on box for live watching — and the honest "the wizard
  starts monitoring now; the service keeps it monitoring after reboot" line), plus
  [docs/raspberry-pi.md](docs/raspberry-pi.md) (64-bit/arm64, USB-SSD for SQLite endurance,
  retention + `rabbot db compact`, systemd) and [docs/mac-mini.md](docs/mac-mini.md) (Homebrew,
  a per-user launchd LaunchAgent, auto-login, `pmset`). The fix: `rabbot service install` now
  installs a unit that runs as **the installing user** — on Linux it sets systemd `User=` from
  `SUDO_USER` (an unprivileged `init` + `sudo service install` no longer yields a service
  reading root's empty config; a genuine root login stays root), and on macOS it installs a
  **per-user LaunchAgent without sudo** instead of a system LaunchDaemon. The wizard adds a
  best-effort sleep-nudge on a laptop ("real-time monitoring wants a box that stays awake"),
  and [docs/vps.md](docs/vps.md) is aligned to the identity fix (unprivileged `init`/`status`/
  `verify`; `sudo` only for the service manager).

### Changed

- **JSON-LD extraction is now malformed-block-resilient and array-aware.** A single
  unparseable `<script type="application/ld+json">` block no longer voids the whole stored
  `jsonld` column: only blocks that parse are kept, and the count of rejected blocks is
  recorded per snapshot (`jsonld_invalid_count`, new column). Legal top-level-array blocks
  (`[{…},{…}]`) now contribute each member's `@type` to `schema_types` instead of reading
  as type-less. **One-time re-baseline:** a page whose JSON-LD is a top-level array may emit
  a single `schema_types` change event on its first crawl after upgrade as the previously
  missed member types are recorded — expected, warning-tier, and it cannot recur.
- **`alt=""` no longer counts as a missing alt** (the SEO convention for a decorative
  image is an explicit empty `alt`). `MissingAltCount` now counts only images with **no
  `alt` attribute present** at all (a present-but-whitespace-only `alt` also counts as
  declared). **One-time re-baseline:** each monitored page's stored `missing_alt_count`
  may **drop** the next time it is crawled, after the fix lands — this is expected, not a
  site-wide alt improvement, and you may see those `missing_alt_count` rows in
  `rabbot report` / `summarize_changes`. Because `image_alt_regression` fires only on an
  **increase**, the re-baseline drop **cannot** trip a false alert.
- **The opaque per-hop `redirect_chain` alert was retired.** Redirect-chain churn that
  neither grows nor loops no longer pages (it is still recorded as history). The new
  parsed `redirect_chain_growth` and `redirect_loop` rules own redirect alerting,
  surfacing the two states that actually matter (a chain that is *growing* and one that
  *loops*). If you routed on the raw `redirect_chain` change type for alerts, switch to
  those two rule ids.
