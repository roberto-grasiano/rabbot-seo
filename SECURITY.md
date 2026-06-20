# Security Policy

Thanks for helping keep Rabbot-SEO and its users safe.

## Supported versions

Rabbot-SEO is pre-1.0. Only the **latest release** receives security fixes —
there are no backported patch branches yet. If you're running an older build,
upgrade before reporting, in case the issue is already fixed.

## Reporting a vulnerability

**Please report security issues privately, never in a public issue.**

Use GitHub's private vulnerability reporting:
**[Report a vulnerability](https://github.com/roberto-grasiano/rabbot-seo/security/advisories/new)**
(repository → **Security** tab → **Report a vulnerability**). This opens a
private advisory thread visible only to you and the maintainer.

Please don't open a public GitHub issue, discussion, or pull request for a
vulnerability — that discloses it to everyone before there's a fix.

What to include, if you can:

- the affected version (`rabbot version`) and OS/arch,
- a description of the issue and its impact,
- steps to reproduce, ideally a minimal proof of concept,
- any logs or output — with **Slack webhook URLs, control tokens, and other
  secrets redacted**.

**Acknowledgment within 7 days.** From there we'll keep you posted on the fix
and coordinate disclosure timing through the same private advisory thread.

## Security-sensitive surfaces

A few parts of Rabbot-SEO are deliberately security-relevant. If you're
auditing, these are the places worth the most attention:

- **Control plane.** The loopback IPC server binds to localhost only. Requests
  authenticate with a token compared in constant time, and the token file on
  disk is `0600`.
- **Outbound crawler.** The crawler honors `robots.txt`, guards against SSRF
  and unsafe redirects, and puts a timeout on every outbound HTTP request.
- **Secrets.** Secrets — Slack webhook URLs, the control token — are never
  written to logs and never embedded in generated config. They arrive through
  `RABBOT_`-prefixed environment variables and are read from their files at
  runtime.

If you find a way around any of these, that's exactly the kind of report we
want.
