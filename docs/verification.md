# Verification & crawler identity

Proving you control a domain lifts Rabbot-SEO's unverified throttle and lets it crawl at
the speed you configured. This page covers the three ways to prove ownership — and,
separately, how the owner of a site you crawl sees you in their server logs.

## Prove you control a domain — `rabbot verify`

`rabbot verify <url>` proves you control a monitored domain and **lifts the
unverified throttle** so the site runs at full speed. Place a public, unguessable
token via any one of three methods and verify it:

- a **well-known file** at `https://<host>/.well-known/rabbot-verify.txt`,
- a **DNS TXT** record `rabbot-verify=<token>`, or
- a homepage **`<meta name="rabbot-verify" content="<token>">`** tag.

```sh
./rabbot verify https://example.com                   # default method: well-known file
./rabbot verify https://example.com --method dns      # or dns / meta
./rabbot verify https://example.com --skip            # record an attestation, stay throttled
```

The token is **public** — its placement on a surface only the domain owner controls
*is* the proof — so it carries no secrecy requirement, only unguessability. Tokens are
unique to your install (derived via HMAC from a per-instance key in your data dir) and
re-checked continuously; **back up your data dir.** A token fetch follows **no
redirects**: the token must sit at the exact path on the literal host, so a redirect to
an attacker host can never satisfy a proof. `--skip` records an *attestation* (it
requires the authorization attestation from setup) and **keeps the site throttled**
until a real verify succeeds. Verification state is a living record: the daemon
re-verifies it on a schedule, and the throttle resolver moves a site's effective
crawl rate with its tier automatically — a successful verify lifts the unverified
floor and lets your configured `speed` apply (see
[Crawl speed & coverage](crawl-speed.md)).

Because each token is bound to your install's key, editing `config.yaml` cannot fake a
verified state and a token placed for one install cannot verify the same domain in
another. See [upgrading-verification.md](upgrading-verification.md) if you are
upgrading from an earlier version.

## How a crawled site owner sees you

`--contact-email` is **mandatory** and is published in the crawler's `User-Agent` so
whoever reads their server logs can reach you. The User-Agent also carries a **per-site
trust signal** built from two inputs, **verification first** then email-domain match:

- a site you have **proven control of** (`rabbot verify <site>`) reads as
  `Rabbot-SEO/<v> (+mailto:you@you.example; verified for you.example)` — trusted
  regardless of your email domain (so an agency crawling a client's verified site
  still reads as trusted);
- an **unverified** site whose registrable domain **matches** your email reads as
  `… (+mailto:you@you.example; you.example contact, unverified)`;
- anything else reads as `… (+mailto:you@you.example; unverified — confirm or block)`,
  inviting the owner to confirm or block you.

Setting `crawler.user_agent` explicitly overrides all of this with your verbatim
string. A mismatch is **never blocked** — you may be authorized on a site you don't
own — the User-Agent simply flags it honestly.
