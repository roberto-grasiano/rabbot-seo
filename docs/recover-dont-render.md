# My crawler refuses to render JavaScript (and recovers your content anyway)

- **Status:** Accepted
- **Component:** `internal/precheck` (the JS-dependency hint), `internal/hydration`
  (the payload decoders), `internal/extract` (DOM-first field merge), the
  `needs_rendering` rule, and the deliberately empty `internal/renderer`

This is a decision record. It explains a feature Rabbot-SEO does **not** have, why
that is on purpose, and what the binary does instead. If you have ever asked an SEO
tool "does it render JavaScript?", this is the long answer.

## The short version

**Rabbot does not render JavaScript. It never will — not behind a flag, not as a
sidecar, not on a roadmap.** Instead it *recovers* the SEO content that modern
JavaScript frameworks already ship in the initial HTTP response, classifies what is
genuinely left behind a browser, and reports that gap honestly rather than papering
over it.

Two jobs that the industry checklist treats as one:

- **Rendering a page** means executing a site's client bundle to a hydrated DOM. That
  needs a real browser — a DOM, a CSSOM, layout, timers, `fetch`. It cannot be done
  inside Rabbot's binary, and it is expensive outside it.
- **Recovering the SEO content** is a different job. Modern frameworks embed their
  state as *data* in the HTML they send: as JSON, as an encoded array, as a streamed
  flight of component rows. On those pages the title, description, canonical, and
  structured data are already in the response — as a payload, not yet as DOM. You can
  read them without running a line of the page's JavaScript.

Rabbot does the second job and is honest about the first.

## The promise this protects

Rabbot's whole shape is its deployment story: one static Go binary, built with
`CGO_ENABLED=0` and pure-Go SQLite, cross-compiled for every platform. You copy a
single file onto a small always-on box — a $5/month VPS, a Raspberry Pi, a Mac mini —
run it as a service, and it rechecks your sites around the clock. No runtime to
install, no container requirement, no dependency tree to babysit.

Headless rendering breaks every clause of that sentence. It means a separate browser
install (hundreds of megabytes) that the binary has to locate, launch, and supervise;
on the order of a hundred-plus megabytes of RAM *per rendered page*; and constant
version skew between the browser and the protocol that drives it. On the 1–2 GB box
this tool is built for, the browser is not a feature — it is the workload. And once
rendering exists, it inevitably creeps into the 24/7 recheck loop, multiplying the
cost of every poll, forever.

So the question was never "is headless rendering nice?" It is. The question is
whether it is the right trade for *this* tool, whose headline feature is that you can
run it anywhere with a single file. The answer is no.

## What the research established

Before deciding, I researched one narrow question: can you do lightweight JavaScript
crawling without bundling a browser, while keeping a single static binary? Every
load-bearing claim was then handed to independent reviewers whose only job was to try
to refute it. The findings, in plain terms:

**The wall is real.** You cannot render the modern JavaScript web inside a CGO-free
static Go binary. The pure-Go JavaScript engines are genuinely capable as *languages*
now — async/await, most of modern ES — but they ship no DOM, no `window`, no `fetch`,
no timers; a production framework bundle throws on its first tick. Compiling a JS
engine to WebAssembly and running it in a pure-Go runtime hits the same wall: even
bolting a JavaScript-land DOM onto it yields a *static parser* — no script execution,
no hydration, no layout. And the engines that genuinely have a DOM (Chromium and its
relatives) cannot be statically linked into a CGO-free Go binary at all.

The engines that fit the binary lack a DOM; the engine that has a DOM cannot fit the
binary. That is why **no** open-source Go crawler ships "single static CGO-free binary
*and* renders the modern JS web" — that artifact is not currently buildable by anyone.
Every established Go crawler that does render JavaScript does it the same way: it
drives an *external*, separately installed browser over a socket.

**One belief got refuted — and it changed the design.** I went in assuming you could
*reliably detect*, from raw HTML alone, whether a page needs JavaScript for its
content. The reviewers rejected the word "reliably," and they were right. Framework
fingerprints — an embedded data script, a `data-reactroot` attribute, an empty `#root`
div — show up on perfectly server-rendered pages too, because server rendering is the
*default* in modern meta-frameworks. Islands, streaming, and JavaScript-injected meta
tags split the signals further. Every production tool that reports JS-dependence
settles it by *rendering and diffing* — fetch raw, render, compare field by field —
and search engines do the same with a second indexing wave. From raw HTML you get a
**hint, never a verdict.** That distinction became the spine of the design.

**The recovery math favors recovery.** Public measurements of the web put the median
word-count difference between rendered and raw HTML in the mid-teens of a percent;
canonical-tag mismatches are well under one percent; almost every page ships a
`<title>` in its initial HTML, and most structured data lives there too. The major
meta-frameworks server-render by default, and the most popular one goes further: it
detects HTML-limited bots by user-agent and serves them the SEO head directly. The
genuinely empty client-rendered shells cluster in app dashboards and logged-in UIs —
not in the content pages that compete for rankings. For the field set an SEO monitor
cares about, the render-required slice of the web is small and shrinking.

## The decision: recover, don't render

So Rabbot's policy is two honest halves:

1. **Recover everything recoverable without a browser** — the server-rendered head,
   the visible DOM, and the hydration payloads riding along in the same HTML.
2. **Classify what is left, and say so.** A page whose content genuinely needs a
   browser is reported as exactly that, so the user sees a finding instead of a silent
   gap. The worst outcome for a monitoring tool is not a page it cannot read; it is a
   page it *pretends* to read.

The research left a door open — an opt-in renderer behind a build flag, driving a
browser the user supplies, with the default binary untouched. I closed that door too.
It would mean a second test surface, render-versus-raw diff plumbing, and a steady
trickle of browser-version support tickets — all to serve a slice the data says is
small, and that the tool can already identify and disclose. The renderer interface in
the codebase stays a no-op; at this point its comment reads less like a stub than a
policy.

## What gets recovered

Modern frameworks hand you their state in the HTML, you just have to read the right
shape:

- **`__NEXT_DATA__`** — a plain JSON script block. Trivial to read. Its head keys
  (title/canonical) are app-specific and often empty, so the bigger win is the **body
  prose** carried in its props — recovered as body-text candidates that keep a content
  page monitorable when the head alone would not.
- **`__NUXT_DATA__`** — *not* plain JSON. It is a compact array with integer
  back-references and tagged values (dates, maps, sets), so it needs a small,
  hand-written decoder rather than a JSON parse. As with `__NEXT_DATA__`, the payoff is
  less the occasional head key than the **body prose** it carries — that recovered text
  is what keeps the page's content under watch.
- **React Server Components flight** (`self.__next_f.push(...)`) — a streamed sequence
  of component rows. A best-effort parser recovers the streamed head elements (title,
  meta, canonical links, structured data) **and the body prose** in the element children;
  the prose is usually the larger recovery here too.

Recovered values merge **DOM-first**: a value found in the real DOM always wins, and a
payload only *fills a field the DOM left empty*. Every recovered field carries its
provenance, so you can always tell whether a title came from the head or from a
payload. The decoders are hand-rolled over the standard library, size-capped against
hostile input, and fuzzed. No new dependencies. No rendering.

## The honest classification

Every page Rabbot crawls gets graded into one of five render modes, persisted on the
snapshot so the verdict is queryable and its changes are tracked over time:

| Render mode | Meaning | Monitoring posture |
|---|---|---|
| `server_rendered` | Content is in the initial HTML, no framework hydration markers. | Fully monitored. |
| `hydrated` | Content is present *and* a real hydration payload rides along. | Fully monitored; recoverable without JS. |
| `head_only_shell` | A complete SEO head sits above an empty app-shell body. | Head monitored; body is not. |
| `client_shell` | Empty shell, no payload, no recoverable content. | Only the fetch status is monitored. |
| `unknown` | Signals are mixed or a payload could not be decoded. | Treated conservatively — never claimed as needs-JS. |

Some rules of this classifier are load-bearing, because they are where honesty is
enforced in code rather than in prose:

- **A real hydration payload outranks everything.** If a non-empty payload parses, the
  content is recoverable without JavaScript and the page can *never* be called
  needs-JS — even when its root div looks empty. A parsed, non-empty `__NEXT_DATA__`
  earns high confidence; a present-but-undecoded payload earns medium, because the
  binary will not over-claim what it has not actually read.
- **The only "needs JS" verdict is `client_shell`, and it is hard-capped to low
  confidence.** It fires only when *everything* lines up: empty framework root, almost
  no visible text, heavy inline script, no head fields, no payload, no recoverable
  stream. No code path can produce a *confident* "needs JavaScript" call. That is the
  refuted assumption, enforced.
- **No credit for content we did not read.** A degenerate payload (an empty object, a
  bare scalar) is not counted as recoverable. A streamed flight we cannot decode
  *blocks* the needs-JS call but routes to `unknown`, not to "recovered."
- **The counting is careful.** Visible words exclude script, style, and `noscript`
  text, so a heavy inline bundle cannot masquerade as prose; script bytes exclude data
  payloads, because recoverable data is not bundle weight.
- **The thresholds confess.** The word-count floor and the script-size ceiling are
  documented in code as empirically un-tuned starting points. Nobody — including me —
  has published benchmarked precision and recall for raw-HTML JS detection, and the
  copy never says "definitely": its worst case reads "appears to be client-rendered,"
  followed by a standing instruction to confirm by comparing View Source with the
  rendered DOM.

The `head_only_shell` mode exists because the honesty itself got audited. Live testing
turned up pages with a complete SEO head above an empty body, and early drafts graded
them too reassuringly. They are now a named partial: the head is monitorable, the body
is not, and the verdict says so.

## The `needs_rendering` finding

Classification is only useful if it reaches you. When a page's content stops being
visible without JavaScript — it slides into `client_shell` or `head_only_shell` —
Rabbot opens a `needs_rendering` finding (a **warning**) that describes exactly what
is no longer monitored: "head monitored; body not" for a head-only shell, "not
monitored beyond fetch status" for a client shell. When the page recovers to a
server-rendered or hydrated state, the finding closes on its own.

This is the payoff of recover-don't-render. A renderer that silently struggled on a
starved box would give you a green check and a blind spot. Reporting the gap turns "I
can't see this" from a hidden failure into a tracked, alertable signal — which is the
entire job of a monitor.

## Try it

The classifier ships in the binary today. Point it at any URL:

```sh
rabbot doctor https://example.com
```

It reports reachability, robots, a crawl-size estimate, and the render-mode hint —
with every signal it weighed, fired or not, so the verdict is an auditable trail
rather than a black box.

## The takeaway

The impressive feature was never the hard part. Headless rendering would have shipped
in a week and cost something every week after — in RAM, in crash handling, in version
churn, in a deployment story quietly reduced to "well, mostly one binary." The work
that mattered was the other half: research until the trade-off was legible, watch a
favorite assumption get refuted, and build the calibration into the code — confidence
grades, auditable signals, and a needs-JS verdict that is never allowed to be
confident.

A monitor that says "here is what I can see, here is what I can't, and here is the
evidence" is worth more than one that renders sometimes-correctly on a small server.
The feature you decline to build is still a design decision — and here it was the
load-bearing one.
