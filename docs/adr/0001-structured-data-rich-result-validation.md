# ADR 0001 — Structured-data (rich-result) validation scope

- **Status:** Accepted
- **Date:** 2026-06-12
- **Deciders:** Rabbot-SEO maintainers
- **Component:** `internal/richresult`, `internal/rules` (the `rich_result_*` rules)
- **Supersedes:** —

This is the first ADR in the repository; it establishes `docs/adr/` as the home
for architecture decision records (settled in the B6 architecture-doc work). ADRs
are numbered, append-only prose — a decision is changed by writing a new ADR that
supersedes it, never by editing an accepted one. The numbering follows the same
zero-padded convention as `internal/store/migrations/`.

## Context

Rabbot extracts the JSON-LD blocks on a page and diffs the set of `@type` values
(`internal/extract/extract.go`; `internal/diff/diff.go`, the `schema_types`
field). That catches a whole typed block appearing or disappearing, but it is
blind to the regression that actually costs a site its rich result: a deploy that
keeps the `@type` set identical while dropping a **required property** inside a
block. The canonical example — a `Product` block loses its `offers` — leaves
`schema_types` unchanged, so no diff fires and no alert is raised. Search Console
will eventually report the lost eligibility, but on its own multi-day cadence,
long after the bad deploy is live.

We want Rabbot to page the operator within one recheck interval — hours, not days
— when markup the site already ships stops being eligible for the rich result it
was earning. The question to answer is **"is the markup you ship still
eligible?"**, never "what markup should you ship?".

## Decision

Encode a **versioned subset of Google rich-result requirements** as an in-binary
constant and validate the stored JSON-LD column against it inside the existing
rule engine. No new dependency (`encoding/json` only), no network call, no
rendering.

### Versioned-profile design

The encoded requirements live in a single package-level value,
`richresult.GRR202606` (`internal/richresult/richresult.go`), whose `Version`
string is `"grr-2026.06"` — *Google rich results, encoded June 2026*. A profile
is `{Version, Types: map[canonicalType]TypeProfile}`, and each `TypeProfile`
carries `Aliases`, `Required` properties, and `AnyOf` groups (at least one member
of each group must be present).

The profile is **pinned by a golden test**. Any edit to the version string or the
type/property table fails the test until a *new* version constant is added. We
never silently change what "eligible" means under a fixed version: a requirement
change ships as a new profile version plus an ADR note plus a new golden, so an
operator can always tie a behaviour change to a named profile version. This is the
**requirement-change policy**, and it is why the profile is versioned at all.

### Presence-driven stance

Validation judges only the structured-data types the site **has implemented** and
that the active profile encodes. Rabbot does **not** infer the site's vertical,
and it **never** recommends adding markup the site does not ship — that is simply
not the job of a regression monitor (decision #1, `decisions-2026-06-10.md`). An
entity whose `@type` matches no profile entry is **not validated**: it contributes
at most a neutral "unprofiled type" count to detail/inspect output, and never an
eligibility verdict, an issue, or a recommendation.

A property counts as **present** iff its key exists and its value is not `null`,
an empty/whitespace string, an empty array, or an empty object. Numbers (including
`0`) and booleans (including `false`) count as present.

### The v1 table (and why)

`grr-2026.06` deliberately encodes three type families:

| Canonical type | Aliases | Required | Any-of (≥1) |
|---|---|---|---|
| `Product` | — | `name` | `offers` \| `review` \| `aggregateRating` |
| `Article` | `NewsArticle`, `BlogPosting` | `headline` | — |
| `BreadcrumbList` | — | `itemListElement` (non-empty) | — |

Why these three:

- **Product** is the marquee case. The `offers` / `review` / `aggregateRating`
  any-of mirrors Google's "a product needs at least one of price, review, or
  rating to show an enhanced result," and `offers` dropping while `@type` stays
  `Product` is exactly the invisible regression that motivated A4.
- **Article** (with its `NewsArticle` / `BlogPosting` aliases resolving to the
  same family) is the most widely deployed content type; `headline` is the one
  property without which the article rich result cannot render.
- **BreadcrumbList** is near-universal, cheap to check (a non-empty
  `itemListElement`), and a common silent-breakage case when a navigation
  refactor empties the list.

These three give meaningful coverage of the structured data real sites ship while
keeping the v1 table small enough to golden-test by hand and reason about.

### Required-vs-recommended reconciliation against Google's live docs

Open question #1 in the A4 spec flagged that the roadmap card asserted "Article
needs `headline`" while Google's Article documentation has historically listed
most Article properties as **recommended**, not required, for eligibility. We
reconciled the v1 table against Google's live rich-result documentation at encode
time. The deltas, recorded here because the profile is versioned exactly for this:

- **`Article.headline` — encoded as Required (a Rabbot profile choice, stricter
  than Google's literal "recommended").** Google renders an Article rich result
  with essentially no required structured-data property, but an Article entity
  with no `headline` has no usable enhanced presentation, and the regression we
  care about — a deploy that strips `headline` — is precisely a loss worth
  paging. We encode `headline` as required for `grr-2026.06` and document the
  delta rather than encode an empty requirement set that could never fire. A
  future profile version may relax this if it proves noisy.
- **`Product.name` — Required, matching Google.** Google lists `name` as required
  for Product.
- **`Product` any-of (`offers`/`review`/`aggregateRating`) — matches Google's
  "at least one of" rule** for an eligible product result.
- **`BreadcrumbList.itemListElement` — Required and *non-empty*, matching
  Google.** A breadcrumb with an empty list earns no rich result.

The standing rule: when a future Google requirement change diverges from an
encoded profile, we do **not** edit `GRR202606`; we add a new version constant, a
new golden, and a note in a follow-up ADR.

### FAQ / HowTo exclusion

`FAQPage` and `HowTo` are **deliberately excluded** from v1. Google withdrew the
FAQ and HowTo rich results for most sites in 2023 (FAQ rich results were limited
to authoritative government/health sites; HowTo was deprecated entirely). Encoding
requirements for markup that no longer earns a rich result would page operators
about a "regression" with no SERP consequence — the opposite of the tool's job.
Sites that still ship `FAQPage`/`HowTo` markup see it counted as an unprofiled
type (neutral), never validated. If Google reinstates those results, a future
profile version can add them.

### No nested-entity recursion

Entities are discovered at three depths only: the top-level object, members of a
top-level array, and one level into a top-level object's `@graph` — matching the
depth `extract.jsonLDTypes` already establishes for `schema_types`. Requirements
check **property presence on the typed entity**; they do **not** recurse into
sub-entities. We do not validate `offers.price`, per-`ListItem` breadcrumb
positions, or any nested object's internals. v1 answers "does this Product entity
carry an `offers` property at all?", not "is that `offers` object itself
well-formed?". Deep sub-entity validation is a candidate for a later profile
version; the line keeps v1 honest about what it checks.

### Single profiled family per entity

An entity whose `@type` is an array naming **more than one profiled family** is
validated only against the **first matching member**; the remaining profiled
facets are neither validated nor counted as an unprofiled type. For example, an
entity declaring `@type: ["Product","Article"]` is validated as `Product` only —
a simultaneous `Article`-eligibility regression on that same entity is invisible
to v1, and the `Article` facet is not counted toward the unprofiled count either.
This mirrors how `RawType` records "the first member that matched the profile" and
how `extract` collects member `@type`s separately; it is a deliberate v1 scope
line, pinned by a regression test
(`TestValidate_MultiProfiledType_FirstMatchWins`) so a future switch to
validate-all-families trips the test rather than silently changing surfaces or
severity. Multi-family-per-entity validation is a candidate for a later profile
version.

### Removal vs. invalid

A profiled type that is **absent** from the new snapshot is a **pass**, not a
defect. Deleting a whole typed block already fires the generic `schema_types`
change alert (warning tier), and a legitimately retired Product page must not hold
a rich-result issue open forever. The new rules therefore fire only when markup of
the type is **present but ineligible**:

- **Critical** on a *lost-eligibility flip*: a real prior baseline existed
  (`Old.ID != 0`), the old snapshot had at least one eligible entity of the type,
  and the new snapshot has none eligible. This is the marquee "you just broke a
  working rich result" page.
- **Warning** in every other failing case: steady-state invalid (old was also
  ineligible), first-crawl invalid (`Old.ID == 0`, no baseline to flip from),
  old-never-eligible, or partial (some entities of the type still eligible).

Both `Old.ID != 0` (has-baseline) and the `Old.ID == 0` / first-crawl
(no-baseline) arms are exercised by the rule tests, so the critical path and the
warning fallback are each pinned.

A truncated fetch body emits **no** rich-result finding at all: a cut
`<script>` can sever a JSON-LD block mid-stream and read as malformed JSON or a
vanished type, and guessing on unextractable input is worse than staying silent
(the `h1_issue` precedent). A dedicated `structured_data_invalid_json` warning
fires while the latest snapshot carries one or more JSON-LD blocks that failed to
parse (`Snapshot.JSONLDInvalidCount > 0`), also self-suppressing under truncation.

## Consequences

- A deploy that drops a required property — the missing-`offers` regression — now
  pages within one recheck interval, hours before Search Console notices. See the
  walkthrough: [`docs/rich-results-demo.md`](../rich-results-demo.md).
- Two extraction defects had to be fixed as in-scope prerequisites (the card
  assumed extraction was sound): one malformed JSON-LD block no longer voids the
  whole stored `jsonld` column (only parsing blocks are kept; rejects are counted
  into the new `jsonld_invalid_count` column), and legal top-level-array blocks
  now contribute their member `@type`s to `schema_types`. The array fix causes a
  **one-time** `schema_types` re-baseline event on the first recrawl of affected
  pages — accepted, warning-tier, and it cannot recur (recorded in the CHANGELOG).
- The four rule IDs (`rich_result_product`, `rich_result_article`,
  `rich_result_breadcrumb`, `structured_data_invalid_json`) bridge to Slack under
  their **own** `change_type`, so a marquee critical is never deduped behind a
  same-crawl `schema_types` warning.
- Surfaces ship across all three planes: **push** (Slack via the existing rule
  bridge), **pull** (`rabbot inspect <url>` gains a `Rich results:` section;
  `rabbot issues` lists the rule IDs generically), and **agent** (the
  `get_rich_results` MCP tool over a loopback control endpoint).
- Per-page silencing reuses the existing issue `ignored` state (the `ignore_issue`
  MCP tool); there is **no** rule-config DSL in v1.

## Out of scope (the line)

- No rendering, no remote fetches, **no call to Google's Rich Results Test API** —
  validation runs over the stored `snapshots.jsonld` column only.
- No full schema.org validation — only the v1 profile types, no vocabulary or
  spelling checks, JSON-LD only (no microdata/RDFa — the only syntax `extract`
  collects today).
- No vertical inference and no schema recommendations (presence-driven).
- No nested-entity recursion (see above).
- No rule-config DSL (the profile is compiled in and versioned in code).
- Full-block removal earning a *dedicated* critical (beyond the existing generic
  `schema_types` warning) is deferred — absence lifecycle semantics (auto-close
  vs. linger) are ambiguous and tracked as a fast-follow.
