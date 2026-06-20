# Contributing to Rabbot-SEO

Thanks for wanting to help. The most valuable thing you can send is a **good issue** —
this project takes **issues, not pull requests**.

## Issues, not pull requests

Rabbot-SEO is maintained by a **single author**, and it keeps its copyright in one place
**by design** — so it does **not** accept code contributions (pull requests). A PR opened
here will be **politely closed** with a pointer back to this page; that's automated and is
**not** a judgment on your idea.

What that means in practice:

- **Found a bug?** [Open an issue](../../issues/new/choose) — what you did, what you expected,
  what happened, plus `rabbot version` and your OS. A clear repro is gold.
- **Want a feature or a change?** Open an issue describing the *SEO problem* you're trying to
  solve (not just the implementation) — the "why" is what makes a request actionable.
- **Found a security issue?** Please **don't** open a public issue — follow
  [SECURITY.md](SECURITY.md) to report it privately.

Well-scoped bug reports and feature requests are genuinely welcome and are exactly how
changes get in. Thank you for taking the time.

## The shape of the project (so requests land well)

A few constraints are load-bearing — knowing them helps a feature request be realistic:

- **One static binary, `CGO_ENABLED=0`.** Everything must be **pure Go** so `rabbot`
  cross-compiles to macOS, Linux, and Windows with no runtime dependencies (e.g. SQLite is
  `modernc.org/sqlite`, never a cgo driver). Requests that would need a cgo dependency are
  out of scope.
- **Two surfaces for humans: Slack and an agent.** Rabbot **pushes** alerts to Slack and is
  **driven by an LLM** (Claude over MCP); the CLI is the bedrock and a Grafana dashboard / TUI
  are optional. Feature ideas that fit those surfaces fit the project.
- **Self-hosted, no SaaS.** State lives in a local SQLite file; nothing phones home. Features
  that assume a hosted backend aren't a fit.
- **Honest by default.** Rabbot would rather say "I can't tell" than guess (the JS-rendering
  hint, the coverage estimator, the health score's "undefined" state). Requests that ask it to
  fabricate certainty won't land.

## Licensing

Rabbot-SEO is **AGPL-3.0** (see [LICENSE](LICENSE)). You're free to use, self-host, study, and
modify it under those terms. Because the project doesn't accept pull requests, there's no CLA
and nothing to sign — there's simply no inbound code to license.
