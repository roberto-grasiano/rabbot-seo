# Demo — catching the missing-`offers` regression hours before Search Console

This is the regression Rabbot's structured-data validation was built to catch: a
deploy that keeps a page's `@type` set identical while silently dropping a
**required property** from its markup. The page still says "I'm a `Product`," so
nothing in a plain `@type` diff fires — but the product has quietly lost its rich
result, and Search Console will only report it days later, on its own crawl
cadence. Rabbot pages the operator within **one recheck interval**.

For why these specific types and properties are validated — and what is
deliberately left out — see the scoping decision record:
[`docs/adr/0001-structured-data-rich-result-validation.md`](adr/0001-structured-data-rich-result-validation.md).

## The scenario

A storefront ships a product page with valid `Product` JSON-LD. It is eligible for
the product rich result because it carries `name` **and** at least one of
`offers` / `review` / `aggregateRating`:

```json
{
  "@context": "https://schema.org",
  "@type": "Product",
  "name": "Widget",
  "offers": { "@type": "Offer", "price": "9.99" }
}
```

Rabbot crawls it on its adaptive cadence and stores the JSON-LD. The
`rich_result_product` check passes — the markup is eligible. No issue, no alert.

A later deploy refactors the template and drops the `offers` block (a stale
partial, a renamed field, a pricing-service migration — it does not matter how).
The markup that ships now is:

```json
{
  "@context": "https://schema.org",
  "@type": "Product",
  "name": "Widget"
}
```

The `@type` is still `Product`. The `schema_types` field Rabbot diffs is
**unchanged**, so the generic schema-change alert never fires. But the product is
no longer eligible for its rich result: it has `name`, yet **none** of
`offers` / `review` / `aggregateRating`.

On the next recheck, `richresult.Validate` runs over the stored JSON-LD, finds the
`Product` entity ineligible, and — because the **prior** snapshot was eligible and
the **new** one is not (a *lost-eligibility flip*, `Old.ID != 0`) — the
`rich_result_product` rule opens a **critical** issue. The new finding bridges
straight into the alert pipeline and reaches Slack.

## The Slack alert payload shape

The finding bridges to Slack under its **own** `change_type` (`rich_result_product`)
— deliberately **not** mapped onto `schema_types`, so a marquee critical can never
be deduped behind an unrelated `schema_types` warning that happened the same crawl.
The bridged alert carries:

| Field | Value |
|---|---|
| `change_type` | `rich_result_product` |
| `severity` | `critical` |
| `url` / deep link | the affected page URL |
| `before` | *(empty — rule findings carry no "before" string)* |
| `after` | the finding's Detail JSON (below) |

The Detail JSON rides through to the alert body verbatim, naming exactly what
regressed:

```json
{
  "profile": "grr-2026.06",
  "type": "Product",
  "entities": 1,
  "ineligible": 1,
  "missing": ["offers", "review", "aggregateRating"]
}
```

`missing` lists every candidate that would have satisfied the requirement, so the
operator sees that restoring **any one** of `offers` / `review` /
`aggregateRating` fixes it. `profile` names the exact versioned requirement set
(`grr-2026.06`) the verdict came from.

### What the operator does next

- **Read it on the CLI.** `rabbot inspect <url>` prints a `Rich results:` section
  under the latest snapshot:

  ```
  Rich results:  (profile grr-2026.06)
    - Product                      ineligible — one-of: offers|review|aggregateRating
  ```

- **Ask the agent.** The `get_rich_results` MCP tool returns the same per-type
  eligibility report and profile version from the latest stored snapshot, so
  Claude can answer "did we just break a rich result on the product pages?"

### After the fix

The next crawl after the flip, the *old* snapshot is now also ineligible, so the
finding is no longer a flip: the engine **refreshes** the open issue to **warning**
while preserving its `OpenedAt`, and emits **no** new alert — only newly opened
findings bridge to Slack. When the deploy that restores `offers` lands, the
`rich_result_product` check passes again and the issue **closes** with no further
alert.

## The e2e test as the receipt

The behaviour above is pinned by tests, written before the implementation:

- **`internal/rules/rich_result_test.go`**
  - `TestRichResultLostEligibilityCritical` — the marquee path: an eligible
    `Product` (`name` + `offers`) flips to ineligible (`offers` removed) and the
    rule fails **critical**, with a Detail JSON naming `profile = grr-2026.06`,
    `type = Product`, `entities = 1`, `ineligible = 1`, and a non-empty `missing`.
  - `TestRichResultSteadyStateInvalidWarning` / `TestRichResultFirstCrawlInvalidWarning`
    — the no-flip arms (old also ineligible; or no baseline at all, `Old.ID == 0`)
    fail **warning**, not critical.
  - `TestRichResultEngineRefreshOnFlip` — through the real engine: crawl 1 opens
    the critical; crawl 2 (old now also ineligible) refreshes to warning with
    `OpenedAt` **preserved** and the issue **not** returned as newly opened (no
    re-alert).
- **`internal/scheduler/rich_result_bridge_test.go`**
  - `TestRichResultCriticalNotDedupedBehindSchemaWarning` — the alert-path proof:
    even when a `schema_types` warning is ingested the *same crawl*, the
    `rich_result_product` **critical** still reaches Slack under its own
    `change_type`, because the rule is intentionally unmapped in the bridge.
  - `TestProcessFetchPassesTruncatedToApplyRules` — guards that a truncated body is
    threaded into the rules so a severed `<script>` self-suppresses rather than
    paging a false "lost eligibility."

Run them:

```sh
make test                        # the full race-clean gate
# or, scoped:
CGO_ENABLED=1 go test -race ./internal/rules/ ./internal/scheduler/ -run RichResult
```
