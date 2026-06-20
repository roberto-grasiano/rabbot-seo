# ADR 0002 — Health-score model (site + segment, over time)

- **Status:** Accepted
- **Date:** 2026-06-12
- **Deciders:** Rabbot-SEO maintainers
- **Component:** `internal/store` (`health.go`: `ComputeHealthScore`,
  `RecordHealthScores`, `RecordSiteHealthScores`, `HealthScoreSeries`,
  `MinScoreCoverage`), migration `0008_health_scores.sql`, the per-recheck seam in
  `internal/scheduler` / `internal/supervisor` and the reconcile re-score in
  `internal/cli/reconcile.go`
- **Supersedes:** —

This is the second ADR in the repository; it records the design and the **frozen
stances** of the 0–100 health score. ADRs are numbered, append-only prose — a
decision is changed by writing a new ADR that supersedes it, never by editing an
accepted one.

## Context

Rabbot already scores every individual issue. `rules.ImpactPoints`
(`internal/rules/rules.go`) computes `importance × severityWeight × 1000` — it even
calls itself "the 0..1000 health-score contribution" — and that value is persisted on
`issues.impact_points`. But nothing ever rolled it **up**. The category's headline
number — the one a stakeholder watches between incidents — is the per-site 0–100
health score and its trend. We want that number to be:

- **Built only from data already persisted** — `urls.importance` and open
  `issues.impact_points`. No new signals, rules, severities, or weights (A3–A5 own
  check breadth; this model consumes whatever mass the engine writes).
- **Per-segment from day one**, via the `segments` / `url_segments` membership wired
  end-to-end by A7 (see [ADR-adjacent: docs/segments.md](../segments.md)).
- **Explainable per point** — any point on the trend is auditable from integers.
- **Honest** — it must never invent a confident-looking number out of thin data.

## Decision

### Formula

For a scope **U** — all monitored URLs of a site (whole-site scope), or the members
of one segment via `url_segments` (segment scope):

- `cap(u) = round(1000 × clamp(importance(u), 0, 1))` — the most health a page can
  lose, on the **same scale and rounding** as `rules.ImpactPoints` (and the same
  `severityWeight`: critical `1.0`, warning `0.5`, info `0.2`).
- `deficit(u) = min( Σ impact_points of OPEN issues on u, cap(u) )` — page-capped.
- `impact_mass = Σ deficit(u)` and `max_mass = Σ cap(u)` over U — both **integers**.
- `score = 100 × (1 − impact_mass / max_mass)` **when defined**.

The score is **undefined** (rendered `—`, never a fake `100` or `0`) when
`max_mass = 0` **or** the scope is below the cold-start coverage floor (below).

### Properties (each is pinned by a test)

- **Healthy = 100.** A scope with no open issues scores exactly `100.0`.
- **Monotone.** Opening an issue never *raises* the score; closing one never *lowers*
  it. The score moves in one direction with the facts.
- **Page-capped.** A page cannot lose more than its own weight. One critical at full
  importance fully impairs a page; piling a second and third critical on the *same*
  page does **not** sink the site further — `min(Σ impact, cap)` clamps it.
- **Importance-weighted.** A critical on the homepage (`importance 1.0`) moves the
  score more than the same critical at depth 5 — because `cap(u)` scales with
  `importance(u)`.
- **Explainable per point.** One score point ≡ `max_mass / 100` impact points, and
  each open issue's cost on the score is `100 × min-capped impact / max_mass`. The
  derived `score` always recomputes exactly from the row's own integer
  `impact_mass`/`max_mass`, and the sum of per-URL capped deficits equals
  `impact_mass`.

### Frozen stances

These are settled product decisions (`decisions-2026-06-10.md`), recorded here so
they are auditable and so a future change must supersede this ADR rather than drift.

1. **Ignored-issue exclusion (decision #5).** Issues with status `ignored` are
   excluded from the masses, exactly like `closed`. The user silenced them via
   `store.IgnoreIssue`; the engine (`internal/rules/engine.go`, `Apply`) clears the
   silence automatically when the defect heals; and ignored issues stay visible via
   `list_issues`. The score therefore reflects **acknowledged reality** — what the
   operator has *not* chosen to live with. The *current* score on every read is
   computed live by the same aggregate, so an `ignore` is reflected immediately
   without hooking the ignore path.

2. **Cold-start coverage floor (decision #6) — `store.MinScoreCoverage = 0.5`.**
   Uncrawled URLs are invisible to both masses (`urls.importance` defaults to `0`
   until the scheduler assigns it on first processing), so a freshly added site with
   3 of 500 known URLs crawled would otherwise show a confident-looking score
   computed from a sliver. The score therefore stays **undefined until the scope
   meets a crawl-coverage floor**: at least **half** of the scope's known URLs
   processed at least once. *Known* = the scope's `urls` rows (segment members via
   `url_segments` for a segment scope); *processed* = `last_checked IS NOT NULL`.
   Define ⇔ `max_mass > 0` **and** `processed ≥ ceil(MinScoreCoverage × known)` —
   integer math, no float traps (`ceil(known/2) = (known + 1) / 2`), inclusive
   boundary. **Each scope meets the floor independently** — a mostly-uncrawled
   segment renders `—` while the site score is live.

3. **Persist-on-change.** A history row is inserted only when the integer tuple
   `(impact_mass, max_mass, page_count)` differs from the latest persisted row for
   that `(site_id, segment_id)` — exact integer comparison, no float-equality traps.
   **Storage moves exactly when reality moves.** The persisted series is the trend (a
   step function — renderers extend the last point to "now"; there are **no heartbeat
   rows**).

4. **No backfill.** History starts when the feature ships. Earlier trend points are
   honestly **absent**, not synthesized. While a scope is below the coverage floor,
   **nothing is persisted** — the trend starts at the first *defined* score, the same
   honesty as no-backfill.

5. **Undefined, not fake.** When the score is undefined (`max_mass = 0` or below the
   floor) every surface renders `—`. It is never coerced to `100` (looks healthy) or
   `0` (looks broken). Coverage counts (`known_urls` / `processed_urls`) accompany the
   value so an undefined cold-start score is self-explaining.

6. **All timestamps UTC.** `computed_at` is always stored `time.Now().UTC()`. A
   local-time write would compare wrong against a UTC window cutoff later — a
   bug-class this repo has already paid for; `internal/store/report.go`'s
   `maxTimestampLayout` defense documents why.

### Capped vs. uncapped: the two masses

The score math uses the **capped** masses (`deficit(u) = min(Σ impact, cap(u))`) so a
dead page cannot drag the whole site below zero. But the per-row `breakdown` column
stores the **uncapped** per-rule mass — `{"rule_id": Σ raw impact_points}` — used
purely for **ranking attribution** ("which rule is costing the most?"). These are
deliberately different numbers: the capped masses answer *"what is the score?"*; the
uncapped breakdown answers *"what is dragging it down?"* without a page cap
flattening the comparison between rules. This distinction is the reason `breakdown`
exists as its own column and is called out explicitly so a future reader does not
"fix" the breakdown to use capped masses and silently change the ranking semantics.

### Persistence & trigger

- **Schema** (`0008_health_scores.sql`): one `health_scores` row per
  `(site, segment, change-in-score)`. `segment_id NULL` = whole site. The integer
  masses (`impact_mass`, `max_mass`, `page_count`) are canonical; `score REAL` is
  derived and recomputable from them. `breakdown TEXT` is the uncapped per-rule JSON.
  Indexed by `(site_id, segment_id, computed_at DESC)` for trend reads. Forward-only;
  `0001`–`0007` untouched.
- **Trigger** — compute per recheck, persist on change, read current live. The
  per-recheck seam (`ProcessorDeps.RecordHealthScore`, implemented by `procDeps` in
  `internal/supervisor/wiring.go`) calls `store.RecordHealthScores(ctx, siteID,
  urlID, now)` at the end of a successful `ProcessFetch` rules pass: it re-scores the
  whole-site scope **plus only the segments containing the rechecked URL** (a segment
  not containing it cannot have moved). A re-segmentation at reconcile time is its own
  scored event via `store.RecordSiteHealthScores` (whole site + every segment), the
  A7-coordination seam, because a membership rewrite can move any scope.

## Consequences

- Rabbot now has a per-site **and** per-segment 0–100 number recorded over time,
  computed from data it already stored — no new rule, signal, weight, or dependency,
  and `CGO_ENABLED=0` is preserved (pure SQL aggregates on `modernc.org/sqlite`).
- The number does not lie under cold start: a site with three crawled pages out of
  five hundred shows `—`, not a fake 100. The honesty is structural, not advisory.
- Re-segmentation, ignoring an issue, and importance drift all reflect on the next
  read because the *current* score is computed live; the persisted series captures
  only the moments the integer masses actually moved.
- The read surfaces (`rabbot report` HEALTH section, a `GET /v1/score` control
  endpoint, a `get_health_score` MCP tool) consume `ComputeHealthScore` /
  `HealthScoreSeries` over the established store → control → MCP seam; they are the
  presentation layer over this model and do not change the math.

## Rejected alternatives

- **Equal page weighting** (every URL contributes `1`, ignoring importance).
  Rejected: it would let a hundred low-value tag pages outvote the homepage, so a
  critical regression on the page that actually earns traffic would barely move the
  number. Importance-weighting is the whole point of having `urls.importance`.
- **Uncapped mass** (let `deficit(u) = Σ impact` with no page cap). Rejected: a
  single dead page accumulating many issues could drive `impact_mass` past `max_mass`
  and the score negative, and would let "pile more issues on one broken page" keep
  sinking the site — a perverse incentive and a meaningless ≤0 number. The page cap
  bounds each page's contribution to its own weight.
- **Heartbeat rows** (persist a row every recheck regardless of change). Rejected:
  it bloats the history with duplicate points and turns a clean step-function trend
  into noise. Persist-on-change stores exactly when reality moves; renderers extend
  the last point to "now".
- **Scoring a sliver** (compute and show a number from whatever few pages are
  crawled). Rejected: it manufactures false confidence during cold start. The
  coverage floor keeps the score `—` until at least half the scope has been seen,
  per scope.
- **A literal row-per-recheck time series / backfill of pre-launch history.**
  Rejected as dishonest and wasteful — see persist-on-change and no-backfill above.

## Out of scope (the line)

- **No push surface.** Issue-level alerting already pushes the underlying facts; a
  score-drop alert would double-alert. Fast-follow if demanded.
- **No new rules, severities, or weights** — the model reads whatever
  `impact_points` the engine writes.
- **No cross-site aggregate score** — per-site is the product unit; an all-sites view
  shows one score per site, never a blended number.
- **No score-based scheduling/throttling**, and **no Prometheus gauge** at launch
  (B2 owns daemon self-observability; exposing site scores there is a fast-follow).
- **No retention work** for `health_scores` at launch — rows are written only on
  change and are tiny; revisit via `store.ApplyRetention` only if real deployments
  prove it material.
