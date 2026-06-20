# Crawling politely: doctor, speed & coverage

Rabbot-SEO tries to be a polite guest on every site it watches. This page covers how to
size up a site before you point Rabbot-SEO at it (`rabbot doctor`), how *hard* Rabbot-SEO
crawls (request rate), how *much* it covers (the page cap), and the estimator that
tells you what your settings cost in time and disk.

## Check a URL first — `rabbot doctor`

`rabbot doctor <url>` runs a one-shot, read-only preflight: reachability, robots,
blocking, the exact User-Agent the site sees — plus an **honest JS-rendering hint** so
you know up front whether Rabbot-SEO can fully monitor a page. Rabbot-SEO reads the **server's
HTML only**; for client-rendered sites it tells you that plainly rather than silently
missing content. The JS read is a **calibrated hint, not a verdict** — it shows every
signal that drove the call and how to confirm in a browser.

```sh
./rabbot doctor https://example.com
#  Doctor report for https://example.com
#  Verdict: GREEN
#  Green: this URL looks monitorable — its SEO content is reachable in the server's HTML.
#
#  Preflight:
#    homepage status: 200
#    fetch class:     ok
#    robots:          allowed (status 200)
#
#  Rendering check:
#    render mode:     server_rendered (confidence: high)
#    visible words:   812
#    script bytes:    1043
#  ...
#  Looks good: this page's SEO content (title, meta, headings) appears to be present
#  in the server's HTML, so Rabbot-SEO can fully monitor this URL without JavaScript.
```

For a client-rendered shell the verdict turns yellow/red and the report states the
honest limit — *Rabbot-SEO reads the server's HTML only; it may not see content, links,
or some meta tags that JavaScript adds in a browser.* If a hydration payload
(`__NEXT_DATA__`/`__NUXT_DATA__`) is present, those fields are reported as recoverable
without rendering. `doctor` makes no third-party calls by default; pass `--check-egress`
to also probe your outbound IP. (No headless browser is bundled — JS *rendering* stays
out of the binary by design.)

## How hard — per-host request rate

Rabbot-SEO spaces requests to a single host. The base spacing is global:

```yaml
defaults:
  per_host_rate: 2s          # one request per host every 2s (the default)
  per_host_concurrency: 2    # at most 2 in-flight requests per host
```

A per-site **`speed`** percent dials that base up or down without touching the
global default — `100` = base, `200` = twice as fast (half the spacing), `50` =
half speed (double the spacing):

```yaml
sites:
  - url: https://example.com
    speed: 200               # ~1s spacing — only applies once verified (below)
```

Three rules keep this from ever becoming a hammer, in this order:

1. **A hard sanity floor.** No setting — not even `speed: 100000` — can space a
   host faster than **250ms**. It is an internal constant, not a config knob.
2. **robots.txt always wins when stricter.** A `Crawl-delay` in the site's
   `robots.txt` is composed on top of your rate and is never lowered by config;
   if the site asks for 30s, Rabbot-SEO waits 30s.
3. **Speed-ups require proof of ownership.** An **unverified** site is capped by
   the unverified throttle floor (≥ 60s between requests) regardless of `speed`
   — a `speed: 200` on an unverified site stays throttled. Run
   [`rabbot verify`](verification.md) to lift the floor; only then does your
   configured `speed` apply, down to the 250ms sanity floor. Slowing a site
   (`speed < 100`, a larger `per_host_rate`) works whether or not it is verified.

Error backoff still self-limits on top of all of this: a `429`/`503`/timeout
multiplies the interval and honors `Retry-After`. Your rate sets the *base*;
Rabbot-SEO only ever goes *gentler* from there.

## How much — the page cap, now visible

`defaults.discovery.max_pages_per_site` (default **`2000`**; `0` = unlimited)
bounds how many pages of a large site Rabbot-SEO monitors, sitemap-priority first.
When a site hits the cap it is no longer silent — `rabbot status` notes any
capped sites, and per-site detail spells out the exact knob to raise or remove it:

```sh
./rabbot sites show https://example.com
#  ...
#  pages: monitoring 2000 of 2000 cap (capped — raise/remove with
#         'rabbot config set defaults.discovery.max_pages_per_site <N|0>'; 0 = all)
```

`rabbot status` carries the same aggregate when any site is capped:

```sh
./rabbot status
#  ...
#  Capped sites: 1 (raise/remove with 'rabbot config set defaults.discovery.max_pages_per_site <N|0>'; 0 = all)
```

Raise the cap (or set `0` to monitor everything) and the next discovery pass
re-admits the extra pages:

```sh
./rabbot config set defaults.discovery.max_pages_per_site=0   # monitor all pages
```

> Note: more pages means a longer full crawl pass and a larger database — use the
> estimator below before you uncap a very large site.

## How long & how big — the coverage estimator

`rabbot doctor <url>` counts the pages in the site's sitemap and estimates a
full crawl pass and the rough on-disk footprint at your current rate, so "my site
has ~10k pages — how long, how big?" has an honest answer up front:

```sh
./rabbot doctor https://example.com
#  ...
#  Coverage: ~10000 pages · ~0.50 req/s · full pass ≈ 5h 33m · ~117.2 MB on disk (go faster by verifying ownership + raising speed). Rechecks every ~10m 0s.
```

The `0.50 req/s` is **one request every `per_host_rate`** (the 2s default) — the
real per-host admission ceiling, set by a single token bucket per host. Per-host
`concurrency` only overlaps fetch *latency*; it never raises that rate. Raise the
speed by lowering the spacing on a verified site (`speed: 200` → ~1s → 1 req/s),
not by adding workers.

> The per-host rate is what bounds a full pass — *not* how fast Rabbot-SEO can
> process a page once it has the bytes. The per-page pipeline CPU cost (measured, and
> far below this interval) is a separate number; see
> [Pipeline cost is not crawl speed](PERFORMANCE.md#pipeline-cost-is-not-crawl-speed)
> in the performance & capacity page. Rabbot-SEO never crawls real sites at pipeline
> speed.

When there's no usable sitemap, pass the count yourself with `--pages N`; without
a count the estimator says so plainly (*"Coverage: page count unknown — pass
`--pages N` to estimate a full crawl pass."*) rather than guessing. The same
estimate is printed in the `rabbot init` summary for a freshly added site. The
footprint is a calibrated approximation (a typical snapshot row plus change
overhead, ~12 KiB/page). The daemon's global crawl-parallelism cap (at most 8
concurrent fetches) only bounds the **sum** across all hosts, not any single
host's rate — so the per-host number above is honest as a planning estimate, not a
guarantee.

## Right-size the cap while adding a site

When you add a large site, `rabbot init` turns the page cap from a silent
`2000` default into a deliberate choice — at setup time, before go-live.

**Interactive wizard.** On the site you enter, Rabbot-SEO counts the pages in
`sitemap.xml` in the background (the same bounded, SSRF-guarded count as
`rabbot doctor`). If the estimate is at or under the `2000` cap, nothing
changes and the wizard stays as fast as before. If it's larger, one extra
question appears, each option showing its time-and-disk consequence so you can:

- **keep the default** (Enter) — monitor the top `2000` pages,
- **monitor all pages** — write `0` (unlimited),
- **set a number** — cap at exactly `N`.

When the sitemap can't be read (missing, broken, or slow — common in the wild),
the wizard asks you directly with pre-filled ranges (`Under 1,000`, `1,000 – 5,000`,
…, `50,000+`, `Not sure`) defaulting to `Under 1,000`, so a small site is one
keystroke and a large one still gets an honest range estimate.

**Headless / CI.** Pass the cap on the command line — no prompt:

```sh
# monitor everything (0 = unlimited)
./rabbot init --i-am-authorized --contact-email me@me.example \
  --site https://example.com --max-pages 0

# cap a large site at 5000 pages
./rabbot init --i-am-authorized --contact-email me@me.example \
  --site https://example.com --max-pages 5000
```

Either way the setup summary states the decision back per site:

```sh
#  https://example.com — throttled
#      monitoring up to 5000 pages — raise/remove with 'rabbot config set defaults.discovery.max_pages_per_site <N|0>' (0 = all)
```

The `up to 5000` is the **configured ceiling**, not the live crawl rate: an
unverified (`throttled`) site is fetched more slowly until you run
`rabbot verify https://example.com` to prove ownership and lift the throttle.
A site set to **monitor all** is affirmed with `monitoring all pages (no cap)` — the
choice is stated back, never left silent.
