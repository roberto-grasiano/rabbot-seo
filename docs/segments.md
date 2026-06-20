# Segments — focus on the pages that matter

A **segment** is a named slice of a site — `/blog`, `/product`, a template family —
defined by a single pattern. Once a segment exists, it scopes everything else:
alerts route by it, `rabbot issues` and `rabbot report` filter by it, and an agent
can pull "just the blog" through MCP. You name the slices that matter once, in
config, and every surface learns to speak in those terms.

Segments are **zero-magic**: Rabbot does not guess them for you. You declare a name
and an anchored pattern; Rabbot compiles it, classifies every URL it knows, and keeps
memberships current as discovery admits new pages — no daemon restart needed (a
config reload re-syncs and re-classifies).

## Define a segment

Add a `segments:` list under a site. Each entry is a `name` and a `match` pattern:

```yaml
sites:
  - url: https://example.com
    segments:
      - name: content          # the route key — lowercase letters, digits, '_' / '-'
        match: "^/blog/"        # a Go regexp, matched against the URL PATH ONLY
      - name: product
        match: "^/product/"
```

- **`name`** must be unique within the site and use the route-key charset:
  lowercase ASCII letters, digits, `_`, and `-` (so `content` and `product-pages`
  are fine; `Blog` and `blog pages` are rejected at load with an error naming the
  site and segment). The constraint exists because a segment name doubles as an
  alert-route key (below).
- **`match`** is a [Go regular expression](https://pkg.go.dev/regexp/syntax)
  matched against the URL **path only** — query strings are excluded in v1. Anchor
  it: `^/blog/` matches `/blog/post-1` but **not** `/blogger`. An invalid regexp is
  a load-time error naming the site and segment.
- A URL may belong to **multiple** segments (the membership is M:N). A page that
  matches both `^/blog/` and `^/2026/` joins both.

Patterns are matched in config order; `rabbot segments` and the read surfaces
report a URL's memberships in that order.

## The segment-routing demo — `/blog` → content channel, `/product` → growth channel

This is the headline use: route alerts for different parts of the site to different
people. Two webhooks, four config lines of segments, two routes:

```yaml
sites:
  - url: https://example.com
    segments:
      - name: content
        match: "^/blog/"
      - name: product
        match: "^/product/"

notifiers:
  - name: slack-content
    type: slack-webhook
    url: ${RABBOT_SLACK_CONTENT}      # your #content channel webhook
  - name: slack-growth
    type: slack-webhook
    url: ${RABBOT_SLACK_GROWTH}       # your #growth channel webhook

routes:
  - match: { segment: content }       # any /blog/** alert → #content
    notifier: slack-content
  - match: { segment: product }       # any /product/** alert → #growth
    notifier: slack-growth
  - notifier: slack-content           # catch-all: everything else → #content
```

What happens at alert time:

- An alert on `/blog/how-to-rank` carries `segment: content`, so the **first** route
  matches and it lands in **#content**.
- An alert on `/product/widgets` carries `segment: product` and lands in **#growth**.
- A page that is in **both** segments matches **either** route — the route match is
  **any-of**: a route fires if *any* of the alert's segments equals the configured
  value. Routes are walked top-to-bottom, first match wins, so the page above lands
  in #content (the earlier route).
- A **site-level** event (a `robots.txt` or sitemap regression — no single URL)
  carries **no** segment, so it never matches a `segment` route. It falls through to
  the catch-all and lands in #content.

`segment` is the fourth route-match key, alongside `severity`, `site`, and
`change_type` — see [configuration.md → Routing](configuration.md#routing). Mix them
freely: `{ segment: product, severity: critical }` routes only critical product
alerts.

> **Routing changes need a restart.** Like all notifier/route edits, the alerting
> stack is built once at daemon startup. After editing `notifiers:`/`routes:`,
> restart the daemon. A *config reload* re-syncs **sites and segments** (so new
> segment definitions and memberships apply live), but not the route table.

## Pull: `rabbot segments`, `--segment` filters

`rabbot segments` lists every configured segment with its pattern and live
member-URL count:

```console
$ rabbot segments
SITE_ID  NAME     PATTERN      MEMBERS
1        content  ^/blog/      128
1        product  ^/product/   43

$ rabbot segments --site https://example.com --json
[
  {
    "site_id": 1,
    "name": "content",
    "match": "^/blog/",
    "member_count": 128
  },
  ...
]
```

`--site <base-url>` scopes to one site; `--json` emits the snake_case payload (the
same field names a `get_site` MCP detail uses), ready to pipe into `jq` or Claude.

Filter the issue and change surfaces with `--segment`:

```console
# Only issues on URLs that are members of the content segment
$ rabbot issues --segment content

# A change/issue digest counting only member-URL activity (totals are ≤ the
# unfiltered report — a segment is a subset of the site)
$ rabbot report --segment content
```

Segment names are scoped per site. On an all-sites command, `--segment content`
matches a segment named `content` in **any** site. A typo (an **unknown** segment
name) yields an **empty** result plus a hint on stderr listing the known names — it
never errors, and the hint never pollutes a `--json` stdout stream:

```console
$ rabbot issues --segment blogg
unknown segment "blogg"; known segments: content, product
```

Both `rabbot issues` and `rabbot report` read the store directly (the settled CLI
idiom), so the filters work even with the daemon down.

## Agent: MCP filter walkthrough

The same filters ride the MCP tools, so an agent can scope its questions to a slice
of the site. First, **discover** the filterable names — `get_site` exposes the
configured segments:

```jsonc
// get_site → SiteDetail (excerpt)
{
  "id": 1,
  "base_url": "https://example.com",
  "segments": [
    { "name": "content", "match": "^/blog/", "member_count": 128 },
    { "name": "product", "match": "^/product/", "member_count": 43 }
  ]
}
```

Then pass a name as the optional `segment` field on the read tools:

- **`list_issues`** — `{ "site_id": 1, "segment": "content" }` returns only issues
  whose URL is a member of `content`.
- **`summarize_changes`** — `{ "site_id": 1, "segment": "content", "since": "24h" }`
  scopes the digest to member-URL changes over the last day.

Under the hood these flow through the loopback control plane as a `segment` query
param on `GET /v1/issues` and `GET /v1/report`; the DTOs are JSON-identical across
store → control → MCP. An **unknown** segment value degrades to empty data (an empty
issue list / empty digest), never a transport error — the same errors-as-data
contract the rest of the read surface follows.

A walkthrough prompt that ties it together:

> "Discover the segments on site 1, then summarize the last 7 days of changes for the
> `content` segment and tell me which URLs changed most."

Claude calls `get_site` to learn the names, then `summarize_changes` with
`segment: content` — and reports on just the blog.

## What segments do **not** do (the v1 line)

- **No query-string matching.** Patterns see the URL path only; a faceted-nav
  segment like `\?page=` is a backlogged fast-follow.
- **No auto-suggested segments.** Rabbot never derives patterns from your URL
  inventory; you declare them.
- **No segment-scoped crawl scheduling or page caps** (per-segment intervals or
  budgets) — fast-follow territory.
- **No wizard step.** `rabbot init` does not grow a segments screen; segments are
  config-file + docs only.
- Segments **annotate and route**; they never **re-group** incidents. An alert's
  fingerprint and group key are byte-identical whether or not it carries segments,
  so adding segments to a running monitor cannot split or merge existing incidents.

## How it works (one paragraph)

At reconcile time (startup, config reload, and after a re-verify demotion) Rabbot
syncs the segment definitions to the database, compiles each site's patterns into an
in-memory matcher, and reclassifies every known URL in one write. As discovery
admits a **new** URL, it is classified at insert from the same in-memory registry —
no per-fetch database read on the hot path, and membership is in place by the time
the page's first snapshot is readable. Alert routing looks up a URL's segments from
that same in-memory registry, so routing never touches the database either.

See also: [configuration.md → Routing](configuration.md#routing) for the route-match
keys, and [ADR 0002 — health-score model](adr/0002-health-score-model.md) for how
per-segment health scores consume the `url_segments` membership this page describes.
