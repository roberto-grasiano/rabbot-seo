# Extending Rabbot — add an alert channel in ~100 lines

Rabbot ships three alert channels — Slack (`slack-webhook`), email (`email-smtp`),
and a generic JSON webhook (`generic-webhook`). Adding a fourth is a small,
self-contained change: **one new file in `internal/notify`, one new `case` in a
construction switch, and a docs entry.** The routing, deduplication, the
per-recipient hourly cap, the incident state machine, the hourly digest, and the
`rabbot notify test <name>` / MCP `send_test_alert` surfaces are all
channel-generic — a new channel inherits every one of them for free.

This is a deliberate **good-first-issue on-ramp**. This page walks the exact
touch-points using the existing `generic-webhook` backend
(`internal/notify/webhook.go`, ≈180 lines of code — most of which is the versioned
DTO, the bounded retry/backoff policy, and the redirect-refusal + secret-scrubbing
hardening that a simpler channel inherits for free) as the worked example, then
ends with a template you can copy for a native `discord-webhook` or
`teams-webhook` channel. The per-channel delta you actually write — a constructor,
`Name()`, and `Notify()` — stays well under ~100 lines; the rest is shared.

> **Hard rule before you start — secrets are sacred.** SMTP passwords, webhook
> URLs, and `Authorization` header values must **never** appear in a log line, an
> error string, an alert body, or anything `rabbot config get` can echo. Every
> backend scrubs its own errors; your new one must too, and a test must assert it.
> This is non-negotiable (see [the never-log rule](#5-the-never-log-rule) below).

## The shape of a notifier

A channel is anything that satisfies the `Notifier` interface
(`internal/notify/notify.go`):

```go
type Notifier interface {
    Name() string
    Notify(ctx context.Context, a Alert) error
}
```

`notify.Alert` is the **backend-agnostic** payload the alerts pipeline produces.
It carries no Slack types — Block Kit rendering lives entirely in
`BuildBlocks` (`internal/notify/blockkit.go`), called *only* by the Slack
backend. Each notifier owns its own rendering: email builds a plain-text body,
the webhook marshals a versioned JSON DTO, and yours renders whatever your target
expects.

```go
type Alert struct {
    Site         string
    URL          string // empty for site-level (robots/sitemap/operational) alerts
    ChangeType   string // e.g. "title", "indexability", "redirect_loop"
    Severity     model.Severity // critical | warning | info
    Before       string
    After        string
    DetectedAt   time.Time
    GroupKey     string // site+change_type
    RelatedCount int    // count of rolled-up affected pages beyond Items
    DeepLink     string // affected URL, or a `rabbot history <url>` CLI hint
    Operational  bool   // true for access/monitoring incidents (unreachable/blocked)
    Items        []AlertItem // rolled-up affected pages
}
```

That is the whole contract. Implement those two methods and the rest of the
pipeline already knows how to reach you.

## The five touch-points

Adding a channel touches exactly these five places. Nothing in
`internal/alerts/*`, the registry, the dispatcher, or the pipeline changes.

### 1. Config schema — `internal/config/config.go`

Channels are configured under `notifiers:` in `config.yaml`. The fields are flat,
`omitempty`, and shared across types on a single `NotifierConfig` struct — each
type uses the subset it needs:

```go
type NotifierConfig struct {
    Name string `koanf:"name" yaml:"name"`
    Type string `koanf:"type" yaml:"type"`
    URL  string `koanf:"url"  yaml:"url,omitempty"`

    // email-smtp fields.
    SMTPHost       string   `koanf:"smtp_host"       yaml:"smtp_host,omitempty"`
    SMTPPort       int      `koanf:"smtp_port"       yaml:"smtp_port,omitempty"`
    Username       string   `koanf:"username"        yaml:"username,omitempty"`
    Password       string   `koanf:"password"        yaml:"password,omitempty"` // secret
    From           string   `koanf:"from"            yaml:"from,omitempty"`
    To             []string `koanf:"to"              yaml:"to,omitempty"`
    AllowPlaintext bool     `koanf:"allow_plaintext" yaml:"allow_plaintext,omitempty"`

    // generic-webhook fields. Headers values are secrets (e.g. Authorization).
    Headers map[string]string `koanf:"headers" yaml:"headers,omitempty"`
}
```

If your channel needs a field no existing type has, add it here with both a
`koanf:` and a `yaml:"...,omitempty"` tag. Reuse `URL` / `Headers` when you can
— the webhook fields already cover "POST to a URL with optional auth", which is
most targets.

**The type string is a public contract.** Add a named constant in
`internal/config/notifiers.go` and append it to `validNotifierTypes`:

```go
const (
    NotifierTypeSlack   = "slack-webhook"
    NotifierTypeEmail   = "email-smtp"
    NotifierTypeWebhook = "generic-webhook"
    // NotifierTypeDiscord = "discord-webhook" // ← your new type
)

var validNotifierTypes = []string{
    NotifierTypeSlack, NotifierTypeEmail, NotifierTypeWebhook,
    // NotifierTypeDiscord,
}
```

Follow the `service-transport` naming convention (`slack-webhook`,
`email-smtp`, `generic-webhook`) so the catalog stays consistent. Pick the name
carefully — config files, docs, and operators depend on it, so it is **never
renamed**; a new transport gets a new constant.

Finally, teach `validateNotifier` (same file) which fields your type requires.
The error names the offending notifier and the missing field — and **never a
secret value**:

```go
case NotifierTypeWebhook:
    return requireFields(n, requiredField{"url", n.URL == ""})
```

`ValidateNotifiers` is the early, friendly gate (`rabbot config validate` and
`rabbot doctor` use it); the construction switch in step 3 enforces the same
contract again, hard, at daemon startup.

### 2. The notifier — a new file in `internal/notify`

Create `internal/notify/<channel>.go` with a constructor and the two interface
methods. Use `webhook.go` as your template. The constructor:

- takes the fields it needs plus the shared `*http.Client` (never
  `http.DefaultClient`, which has no timeout — default a nil client to a
  30s-timeout one, exactly as the webhook and Slack backends do);
- **validates its required config and fails at construction**, returning
  `(Notifier, error)`, so a misconfigured channel fails *daemon startup* with a
  named error, never silently at first send;
- **never echoes a secret** in that error.

```go
func NewWebhookNotifier(name, webhookURL string, headers map[string]string, client *http.Client) (Notifier, error) {
    if webhookURL == "" {
        return nil, fmt.Errorf("generic-webhook %q: incomplete config, missing url", name)
    }
    if client == nil {
        client = &http.Client{Timeout: 30 * time.Second}
    }
    // copy headers so a later mutation of the caller's map can't change what we send …
    return &webhookNotifier{name: name, url: webhookURL, headers: hdr, client: client}, nil
}
```

`Notify` renders the alert and delivers it. **Honor `ctx`** on every blocking
step (the dial, the HTTP round-trip, any backoff sleep) so a daemon shutdown or
the `rabbot notify test` timeout aborts a stuck send promptly — the webhook
backoff selects on `ctx.Done()`:

```go
timer := time.NewTimer(wait)
select {
case <-timer.C:
case <-ctx.Done():
    timer.Stop()
    return ctx.Err()
}
```

**Decouple your wire shape from `notify.Alert`.** The webhook POSTs a *distinct*
`webhookPayload` DTO with explicit `json:` tags and a `payload_version`, not the
internal struct — so the public wire format is versioned and stable even as
`Alert` evolves. If your target has its own schema (Discord's `content`, a Teams
Adaptive Card), build that shape in your own file; don't leak internal field
names onto the wire.

**Match the retry policy shape.** For HTTP targets, reuse the bounded
Slack-policy retry: up to 3 retries on `429` (honoring an integer-seconds
`Retry-After`, capped at `maxRetryAfter`) and on `5xx`; other `4xx` are
terminal; transport errors are terminal (don't hammer a dead endpoint). The
constants `maxRetryAfter` / `defaultRetryAfter` live in `slack.go` and are
shared. Always drain-and-close the response body (`drainClose` in `helpers.go`)
so the keep-alive connection is reusable — `golangci-lint`'s `bodyclose` flags
the unclosed case.

### 3. Wiring — one `case` in `BuildAlertingStack`

`supervisor.BuildAlertingStack` (`internal/supervisor/wiring.go`) is the **single
point of concrete-type knowledge** in the whole codebase. Add one `case` that
maps your config fields onto your constructor:

```go
switch nc.Type {
case config.NotifierTypeSlack:
    byName[nc.Name] = notify.NewSlackNotifier(nc.Name, nc.URL, client)
case config.NotifierTypeEmail:
    n, err := notify.NewEmailNotifier(notify.EmailConfig{ /* … */ })
    if err != nil {
        return nil, err
    }
    byName[nc.Name] = n
case config.NotifierTypeWebhook:
    n, err := notify.NewWebhookNotifier(nc.Name, nc.URL, nc.Headers, client)
    if err != nil {
        return nil, err
    }
    byName[nc.Name] = n
// case config.NotifierTypeDiscord: ← your new case
default:
    return nil, fmt.Errorf("rabbot: unknown notifier type %q for %q", nc.Type, nc.Name)
}
```

Because your constructor returns an error, a bad config fails here — at startup,
naming the notifier, echoing no secret — exactly like every existing type. The
`default` arm already rejects an unknown type; you are just adding a known one.

That's the only production file outside `internal/notify` and `internal/config`
you touch. The registry, dispatcher, and pipeline are built from `byName` and
the routes immediately after, unchanged.

### 4. Routing & config — what the operator writes

Routing is already generic. The operator names your channel in a route and the
dispatcher resolves it by name:

```yaml
notifiers:
  - name: my-channel
    type: discord-webhook
    url: ${RABBOT_DISCORD_WEBHOOK}

routes:
  - notifier: my-channel        # a notifier with NO route is unreachable
```

A notifier with no route never fires — the dispatcher iterates `routes` and
there is no implicit fallback — so the wizard always writes a catch-all route
alongside a new channel. First match wins, top to bottom; an empty `match` is the
catch-all. Your channel keys the per-recipient hourly cap by its own name
(`RouteTarget`), so its throttle is independent of the others. None of this is
code you write — it falls out of registering under a name.

### 5. The never-log rule

Any error your `Notify` returns can surface in a log line, in the
`rabbot notify test` output, and in terminal scrollback. So **scrub every secret
out of the error before it leaves the package.** Both shipped backends do this
with a small helper; copy the pattern:

- **email** strips the password from the error and returns a fresh `fmt.Errorf`
  with `%s` (not `%w`) so the chain is severed and no `Unwrap` can recover the
  original:

  ```go
  func (e *emailNotifier) scrub(err error) error {
      if err == nil {
          return nil
      }
      msg := replaceAllNonEmpty(err.Error(), e.cfg.Password, "<redacted>")
      return fmt.Errorf("email-smtp %q: %s", e.cfg.Name, msg)
  }
  ```

- **webhook** redacts the URL's path-and-query (which may carry a token) and
  every header value, keeping only `scheme://host:port` as operator-facing
  context, and likewise severs the `*url.Error` chain.

Use `replaceAllNonEmpty` (`helpers.go`) — plain `strings.ReplaceAll` with an
empty `old` would splice the replacement between every rune, a footgun when a
secret field happens to be empty.

Two more rules for free:

- **`${ENV}` interpolation.** If your channel adds a *new* secret-bearing field,
  expand `${ENV}` references for it in `interpolateSecrets`
  (`internal/config/load.go`) so the secret can live in the environment instead
  of `config.yaml`. The webhook header values and the email password are already
  wired; `URL` is too. Reusing those fields means no change here.
- **The control plane already protects you.** Every `notifiers.*` key is denied
  by the `config set` allow-list (`internal/config/allowlist.go`), so a new field
  cannot be read back or written over the control endpoint — it is covered the
  moment it lives under `notifiers`.

## The test pattern

TDD — write the failing test first. Drive the notifier against an in-process
fake server (`net/http/httptest` for HTTP targets; a `net`/`textproto` fake for a
line protocol like SMTP) so the suite needs no live network and stays
`go test -race` clean. The webhook tests (`internal/notify/webhook_test.go`) are
the model. Cover:

- **the happy path** — assert the method, `Content-Type`, the exact wire fields
  (including `payload_version`), and that configured static headers are present
  (`TestWebhookNotifierPostsVersionedJSON`);
- **the retry policy** — `429`-with-`Retry-After` then `200` ⇒ exactly two
  attempts; `5xx` retried to the cap; `400`/`404` terminal after one attempt;
  `ctx` cancellation aborts a pending backoff promptly
  (`TestWebhookNotifierRetryPolicy`);
- **construction validation** — missing required config is a startup error
  naming the notifier, with no secret in the message;
- **secret scrubbing — the one you must not skip.** Force a transport/HTTP error
  and assert the secret is absent from `err.Error()`
  (`TestWebhookErrorScrubsURL`; the email equivalent is
  `TestEmailErrorNeverContainsPassword`):

  ```go
  // connection refused → error must not leak the URL path/query
  if strings.Contains(err.Error(), secretPath) {
      t.Fatalf("error leaked the webhook URL: %v", err)
  }
  ```

Round it out with a wiring test that `supervisor.BuildAlertingStack` constructs
your type and rejects an incomplete config, and you have parity with the shipped
channels.

The gate for any change here:

```sh
CGO_ENABLED=0 go test -race ./internal/notify/ ./internal/config/ ./internal/supervisor/
make lint   # golangci-lint v2 — gosec forbids InsecureSkipVerify; bodyclose; errorlint
```

## Good-first-issue template: a native `discord-webhook`

The generic webhook already reaches Discord *via glue or an automation platform*
(Discord rejects arbitrary JSON — it wants a `content`/embeds shape), but a
**native `discord-webhook`** that renders Discord's payload directly is an ideal
first contribution. The scope, copy-pasteable into an issue:

> **Add a native `discord-webhook` alert channel.**
>
> - **Type string:** `discord-webhook` (`internal/config/notifiers.go`).
> - **Config:** reuse `URL` (the Discord webhook URL) — no new field needed.
> - **Notifier:** `internal/notify/discord.go` — POST a `{ "content": "…" }` (or
>   an `embeds` array) shaped to Discord's webhook API; reuse the
>   `generic-webhook` retry/scrub helpers. Keep it ≤ ~150 non-test lines.
> - **Wiring:** one `case config.NotifierTypeDiscord` in
>   `supervisor.BuildAlertingStack`.
> - **Tests:** `httptest` happy-path asserting the Discord shape + the
>   `Retry-After` retry path + a URL-scrub test.
> - **Docs:** a config block in [docs/configuration.md](configuration.md) and a
>   `### Added` line in [CHANGELOG.md](../CHANGELOG.md).
>
> A `teams-webhook` (Microsoft Teams Adaptive Card) is the same shape with a
> different payload.

If the registry, dispatcher, and pipeline stay untouched while the catalog grows
— and they will — the seam has done its job.
