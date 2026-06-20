# Rabbot-SEO — Claude Code guide

Open-source, self-hosted, real-time SEO monitoring gateway. A **single static Go
binary** that runs 24/7 as a service, rechecks sites for SEO-relevant changes,
and alerts via Slack. Public OSS — treat code as portfolio-grade.

- **Module:** `github.com/roberto-grasiano/rabbot-seo` · **GitHub:** `roberto-grasiano/rabbot-seo`
- **Binary / CLI:** `rabbot` · **License:** AGPL-3.0 · **Status:** M2 shipped (crawl/extract/store + diff/rules/Slack + discovery); "make it real" validated end-to-end (Stream 3); M3 next

## Non-negotiable conventions

- **CGO_ENABLED=0, always.** The whole point is a single static cross-platform
  binary. SQLite is `modernc.org/sqlite` (pure Go). **Never add a cgo dependency**
  (e.g. `mattn/go-sqlite3`) — it would break static builds for macOS/Windows/Linux.
- **TDD.** Write the failing test first, then implement. See `docs/superpowers/plans/`.
- **Race-clean.** `make test` runs `go test -race ./...`. It must be green before
  any commit. If a test is timing-sensitive under `-race`, fix the synchronization,
  don't loosen the test.
- **Lint clean.** `make lint` runs golangci-lint **v2** (`.golangci.yaml`:
  govet, staticcheck, errcheck, ineffassign, unused).
- **Format on save.** A PostToolUse hook runs `gofmt -w` on every edited `.go`
  file (`.claude/hooks/gofmt.sh`) — commits stay gofmt-clean automatically.
- **No secrets in the repo.** Secrets arrive via `RABBOT_`-prefixed env vars
  and koanf interpolation. `control.token`, `*.db` are gitignored.
- **No competitor names in public artifacts.** Anything that ships in the repo
  (docs, specs, README, code comments, release notes) or gets published never
  names other commercial SEO products/companies — describe capabilities
  generically ("commercial real-time monitors", "enterprise crawl-analytics
  platforms"). Naming platforms we integrate with or run on (Slack, Grafana,
  Google Search Console, VPS providers) is fine. Competitive strategy lives in
  private, untracked artifacts only (`rabbot-*.html` is gitignored for this).

## Common commands

```sh
make build     # CGO_ENABLED=0 static build with version/commit/date ldflags
make test      # go test -race ./...   (the gate)
make vet       # go vet ./...
make lint      # golangci-lint v2
make tidy      # go mod tidy
make snapshot  # goreleaser local snapshot (no publish)
make all       # tidy vet lint test build
```

Build metadata (`version`, `commit`, `date`) is injected via `-ldflags -X main.*`
— see `Makefile` and `cmd/rabbot/main.go`. Don't hardcode versions.

## Layout

```
cmd/rabbot/        entrypoint; builds the cobra command tree
internal/cli/         cobra commands: run, init, config, service, version, doctor, observability
internal/precheck/    pure-Go honest preflight: reuses fetcher.Doctor + a JS-dependency HINT (no renderer)
internal/config/      koanf loader; precedence flags > env > file > defaults; per-OS dirs (adrg/xdg)
internal/control/     loopback HTTP IPC + token auth (server/client/token/types)
internal/store/       modernc sqlite, embedded forward-only migrations
internal/supervisor/  long-running daemon orchestration
internal/model/        core domain enums/types
internal/obs/          structured logging (lumberjack rotation) + Prometheus metrics (read-only /metrics listener, off by default)
internal/mcp/         read + safe-actions MCP server (package mcpsrv): stdio-only Bridge over the loopback control client
```

## MCP slice — read + safe-actions, stdio-only, expandable seam

A **Model Context Protocol** server lives in `internal/mcp` (package `mcpsrv`),
exposed via `rabbot mcp`. It is **stdio-only** (no HTTP/network endpoint —
`mcp.StdioTransport` only; stdout is the JSON-RPC channel so nothing may write there)
and exposes a **read + safe-actions** catalog of tools plus three read-only resources
(`rabbot://health|status|sites`) for `@`-mention. Reads and writes flow through the
daemon's loopback control endpoints (the child opens no DB); destructive ops
(purge/reload/shutdown) are excluded; `set_config` is allow-listed. The **control token
is never logged or embedded** in any generated config — the server reads the token file
at runtime. Full recipe (incl. VPS-over-SSH): `docs/mcp-connect-guide.md`.

Handlers depend on the injectable `Bridge` interface (Health/Status/Sites), unit-
tested against a mock with no live daemon. `init --connect-claude` (and the wizard's
step 9) generate/merge-write the Claude config that launches `rabbot mcp`. This is
the official **`github.com/modelcontextprotocol/go-sdk` v1.6.1** (pure Go — keeps the
static binary). It is a **deliberate expandable seam**: Spec 2 grows it into a full
read + safe-actions catalog, and the **same launch snippet keeps working** — only
`controlBridge`'s production impl changes (move sites onto a new control read
endpoint), not the `Bridge` contract. Look up the SDK via context7 + `go doc`.

## Migrations

- Canonical, **embedded** dir: `internal/store/migrations/NNNN_name.sql`
  (`//go:embed migrations/*.sql` in `internal/store/migrations.go`).
- Zero-padded version prefix (`0001_`, `0002_`, …); applied in lexical order in a
  single transaction; tracked in `schema_migrations`. **Forward-only** — never edit
  an applied migration; add a new one.
- Use the `/new-migration` skill to scaffold the next file at the right path.

## Security-sensitive surfaces (review carefully)

- **Control server** (`internal/control`): must bind loopback only; token auth
  should use constant-time comparison; `control.token` file must be 0600.
- **Outbound crawler** (M1+): robots.txt compliance, redirect/SSRF safety, rate
  limiting, timeouts on every HTTP client.
- **Slack alerts** (M2+): never log webhook URLs/tokens.

Use the `security-reviewer` subagent for changes touching these areas, and
`go-oss-reviewer` for general Go idiom / goroutine-lifecycle review.

## Agent tooling & process gates

- **Look up library APIs via the context7 MCP** instead of relying on memory for
  any third-party dependency — `slack-go/slack`, `gocron/v2`, `knadh/koanf`,
  `spf13/cobra`, `modernc.org/sqlite`. APIs drift; the live docs are
  authoritative. The repo ships a keyless `context7` server in `.mcp.json` (set
  `CONTEXT7_API_KEY` for higher rate limits). The review subagents have `WebFetch`
  + context7 access — use it when a finding hinges on a library's contract.
- **Run the review subagents before opening a PR**, not just in CI:
  `security-reviewer` for control-server / crawler / secret / SQL changes,
  `go-oss-reviewer` otherwise. CI runs the Claude PR-review Action on top, but
  local review catches issues a turn earlier.
- **The automated PR-review Action** (`.github/workflows/claude-review.yaml`) is
  **advisory**: it reviews only the PR diff (not the whole repo) and posts inline
  comments plus one sticky summary. A guard step fails the `review` check if no
  real review ran (model error or permission denial), so a green check genuinely
  means reviewed. It cannot submit a formal approve/request-changes review and does
  not gate merge — read its comments; don't merge on green alone. A PR that
  *edits* `claude-review.yaml` itself shows a red `review` **by design** (the
  Action refuses to run a workflow version not yet on `main`); validate workflow
  changes on a follow-up PR after they merge.
- **Lint is a gate, not a suggestion.** `make lint` must be green before commit.
  golangci-lint v2 runs `errorlint` (sentinel comparisons), `gosec` (insecure file
  modes / SSRF), `bodyclose`, and `sqlclosecheck`/`rowserrcheck` — these catch the
  exact bug classes that passing tests sail over.
- **Migrations are forward-only** — a PreToolUse hook blocks edits to existing
  `internal/store/migrations/*.sql`. Add a new file via `/new-migration`.

## JS rendering — out of binary by design

JS rendering stays **out of the binary**. `internal/precheck` (and `rabbot
doctor <url>`) emit a pure-Go, **calibrated HINT** about JS-dependency — never a
verdict — and parse hydration payloads (`__NEXT_DATA__`/`__NUXT_DATA__`); a present
payload or server-rendered head means content is **recoverable without JS** (no
needs-JS flag). External-Chrome rendering (Tier 1) is explicitly **dropped as
overengineering** — no headless browser, no build tags, no new dep; the
`internal/renderer` no-op stays. Rationale + options matrix:
`docs/superpowers/research/2026-06-05-js-rendering-options.md`.

## Roadmap

M0 walking skeleton (current) → M1 crawl/extract/store → M2 diff/rules/Slack →
M3 polish/distribution. Plans live in `docs/superpowers/plans/`.
