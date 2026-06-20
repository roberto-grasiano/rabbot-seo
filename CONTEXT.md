# Rabbot-SEO — ubiquitous language

The shared vocabulary of the Rabbot-SEO domain. This is a **glossary only** — one
crisp sentence per term, no implementation detail. The architecture lives in
`ARCHITECTURE.md`; the locked decisions live in `docs/adr/`. Use these words, with
these meanings, in code, docs, commits, and conversation.

## Actors (the trust model)

- **Trusted actor (operator)** — the human who runs the daemon and owns the
  machine; they configure sites, secrets, and policy, and Rabbot does what they
  say.
- **Semi-trusted actor (LLM / MCP client)** — an AI assistant connected over the
  stdio MCP bridge that may read state and invoke allow-listed safe actions on the
  operator's behalf, but is never given destructive control or the control token.
- **Hostile actor (the monitored site)** — the third-party site under observation,
  assumed adversarial: it controls its own DNS, redirects, headers, and markup, so
  Rabbot treats everything it returns as untrusted input.

## Monitoring units

- **Asset** — a monitored page (URL) that Rabbot protects and prioritizes; its
  importance weights how much its regressions matter.
- **Indexability verdict** — the boolean "can a search engine index this page right
  now?" with a single machine reason, derived from HTTP status, robots.txt,
  meta-robots, X-Robots-Tag, and the canonical.
- **Canonical** — the page's declared preferred URL (`rel=canonical`), recognized
  from **both** the HTML `<link>` tag **and** the HTTP `Link` header; a page whose
  canonical points elsewhere is canonicalized away.
- **X-Robots-Tag** — the HTTP response header carrying robots directives (e.g.
  `noindex`), equivalent in force to the meta-robots tag and a first-class input to
  the indexability verdict.

## Rendering & recovery

- **Hydration payload / recover-don't-render** — a framework's embedded
  initial-state blob (`__NEXT_DATA__`, `__NUXT_DATA__`, RSC flight) that Rabbot
  parses to recover SEO content **without** running a browser, because the policy
  is to recover content, never to render it.
- **Render mode** — the persisted classification of how a page delivers its SEO
  content: server-rendered, hydrated, head-only shell, client shell, or unknown.

## Link graph

- **Blast radius** — how bad it is if a given page goes dark, measured from its
  inbound link picture (inlink count, high-importance sources, weighted mass).
- **Inlinks** — the monitored source pages that link **to** a given target URL.
- **Orphan** — a monitored page with zero inbound internal links (the site root is
  never an orphan).

## SERP & structured data

- **SERP pixel width** — the rendered width, in pixels, of a title or meta
  description in the desktop search-result reference font, used to flag truncation.
- **Rich-result eligibility profile (GRR version)** — a versioned, in-binary
  encoding of which structured-data types and properties make markup eligible for a
  rich result (current: `grr-2026.06`), answering "is the markup you ship still
  eligible?" and never "what markup should you ship?".
