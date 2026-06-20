# Google Search Console — Google's ground truth, on your box

Rabbot-SEO is crawl-based: it sees **what changed on a page**. Connecting a site to its
**Google Search Console** (GSC) property adds the other half — **what Google thinks**: is
the URL indexed, which canonical did Google actually pick, and how is the page doing in
Search. It turns a change-monitor into a search-intelligence monitor.

It is **read-only** and **self-hosted**. Rabbot pulls your Search Console data straight
into your own SQLite file; **the data never leaves your box** — a real difference from
cloud SEO platforms that ingest your Search Console into their servers.

- **Connect it** (per-site `gsc:` block, both auth paths, the credential rules): the
  **[configuration reference → Google Search Console](configuration.md#google-search-console)**.
- **Read the data**: `rabbot gsc status <url>` and `rabbot gsc performance --url <url>`
  (below), or the `get_index_status` / `get_search_performance` MCP tools — see
  **[Claude / MCP](claude-mcp.md)**.

## The three things it surfaces

Rabbot is deliberately conservative about what GSC data is allowed to **page** you. The
bar is *valid **and** worth the interruption* — the worst outcome is crying wolf. Three
signals clear it; raw traffic numbers do not (see [Non-goals](#what-it-deliberately-does-not-do)).

### 1. Index-status discrepancy

Rabbot's own indexability verdict for a URL **disagrees with Google's**. The two
high-signal shapes:

- **Rabbot says indexable, Google isn't indexing it** — e.g. Google reports
  *"Crawled — currently not indexed"* or *"Discovered — currently not indexed"*. Rabbot
  thinks the page is fine; Google is choosing not to index it. That gap is worth knowing.
- **Rabbot says not-indexable, Google indexed it anyway** — e.g. the page ships
  `noindex` but Google has it indexed. Either the directive is new (Google hasn't
  recrawled) or something is inconsistent.

This validates Rabbot's engine against Google's reality, per URL. When Rabbot and Google
**agree**, nothing fires.

### 2. Google-canonical mismatch

Google chose a **different canonical** than the one the page declares — GSC's
`googleCanonical` differs from `userCanonical`. Google is effectively ignoring your
declared canonical and consolidating signals onto a URL you didn't pick. Rare, real, and
**actionable** — exactly what should surface.

### 3. Search-performance shift — an *enrichment*, not an alert

This one is **not** a standalone alert. When a page **changes** (a title, meta, content,
or canonical edit Rabbot already recorded), Rabbot can correlate that change date against
the page's search performance and add a line to the **history/report** — *"title changed
on the 3rd; the primary query lost N impressions / dropped M positions over the following
week."* It is a correlation Rabbot can make uniquely because it owns the change history.

It rides the **history** surfaces (`rabbot report`, the `summarize_changes` tool), **not**
a live alert — by design: at the instant a change alert fires there is no finalized
post-change data yet (see [latency](#latency--quota), below), so the correlation only
becomes meaningful days later, where history is reviewed after the fact. It appears only
when there is **enough finalized post-change data** to be meaningful.

## What it deliberately does NOT do

Anti-noise is a feature. Rabbot **never** fires:

- **Standalone traffic / impression / ranking-drop alerts.** A clicks or impressions or
  average-position delta on its own is dominated by **seasonality, SERP volatility, and
  data lag** — it is noise, not signal. Rabbot exposes that data **read-only** (the
  `performance` verb / `get_search_performance` tool) for you or an agent to inspect, but
  it does not threshold-alert on it.
- **Alerts on unfinalized data.** Only `dataState=final` rows are stored/evaluated; the
  most recent **~3 days are partial** and excluded.
- **Alerts on absent data.** The URL-inspection quota is bounded, so a URL may simply
  have **no inspection on record yet**. That absence is reported honestly and is **never**
  read as a discrepancy — missing data is not a problem.
- **GA4 / analytics.** Out of scope (noisier, lower SEO-signal). GSC only.

## Latency & quota

- **Search performance is finalized-only.** Rabbot pulls with `dataState=final`, so the
  freshest ~**3 days are partial and held back**. Expect search data to "lag" reality by a
  few days — that lag is why the performance shift is an after-the-fact enrichment, not a
  live alert.
- **URL inspection is quota-bounded (~2000/day/property).** Rabbot prioritizes changed +
  high-importance URLs and spreads inspections out; it **never blocks the crawl** on GSC.
  A URL you just added may not have an index-status row for a little while. Both the read
  surfaces report that as *"no inspection on record"* (or `has_status=false` in JSON),
  never an error.
- **The signals run on the daily GSC pull cadence**, not on every recheck — index status
  only changes when Google re-inspects, so a daily evaluation is the right (and only)
  cadence.

## Reading the data

Both read verbs hit the local database **directly**, so they work even with the daemon
down. Pipe `--json` into Claude or `jq`.

```sh
# Latest Google index status for one URL: verdict, coverage/indexing state,
# the canonical Google chose vs the one you declared, last crawl time.
rabbot gsc status https://www.example.com/product/widget

# Stored search performance for one URL — clicks / impressions / CTR / position
# per (query, day), newest first. Finalized days only; default window 7 days.
rabbot gsc performance --url https://www.example.com/product/widget --since 720h

# Pipe either into an agent:
rabbot gsc status https://www.example.com/product/widget --json \
  | claude -p "Does Google agree this page is indexable? If not, why might that be?"
```

Inside Claude, the same data is one MCP tool call away — `get_index_status` and
`get_search_performance` — see **[Claude / MCP](claude-mcp.md)**. Both tools report absent
GSC data as `has_status=false` / `has_data=false`, **never a guess and never an error**.

## Auth, in one paragraph

Credentials are always **bring-your-own** — no public OAuth client ships in Rabbot. Two
paths, both writing a **`0600`** credential file Rabbot reads at runtime:

- **Service account (recommended, headless).** No browser: make a GCP project, enable the
  Search Console API, create a service account + JSON key, and grant that service
  account's email **read** access to the property in Search Console. Point
  `service_account_key_file` at the saved key. The fit for a daemon on a server.
- **OAuth2 (browser consent).** Run `rabbot gsc auth` with your own OAuth client to
  complete a one-time consent → a refresh-token file. On a **headless** box, run it on
  your laptop and `scp` the token file over, then point `oauth_token_file` at it.

The credential keys hold a **path, never a key body**; the content is never logged or
returned by `rabbot config get`, and the path keys are denied from the control plane.
Full step-by-step (both paths) and the field reference:
**[configuration → Google Search Console](configuration.md#google-search-console)**.
Confirm a connection any time with **`rabbot doctor <url>`** — it checks the key is
present + `0600`, that the credential authenticates, and that your property is in its
verified-site list.
