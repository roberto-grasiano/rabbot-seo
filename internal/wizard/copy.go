package wizard

import "github.com/roberto-grasiano/rabbot-seo/internal/config"

// WelcomeText is the warm, jargon-free intro shown on the first screen.
const WelcomeText = "Rabbot keeps an eye on your website and tells you when something " +
	"that matters for search (titles, descriptions, links, and more) changes. It's polite by " +
	"default — it identifies itself, respects robots rules, and never hammers a site. Let's set " +
	"up your first site; it takes about a minute."

// StagingNudge is the skippable hint shown beneath the "what site?" field. Pure
// copy — it never blocks; it just lowers the stakes for a first-timer.
const StagingNudge = "New to this? Pointing Rabbot at a staging or test site first is a " +
	"great way to see how it works — with no risk to a live site."

// ReadingSitemapNote is the unmistakable hold shown at the cap step WHILE the background
// sitemap count is in flight (Item C). It is the only thing on screen until the count
// resolves — neither the estimate nor the ranged ballpark question is presented yet — so
// the wizard never looks broken by showing the "couldn't read a sitemap" fallback while it
// is, in fact, still reading.
const ReadingSitemapNote = "⏳ Reading sitemap.xml… sizing up your site so we can right-size " +
	"coverage. This takes a few seconds."

// ContactExample renders how the operator will appear to site owners, WITHOUT
// ever using the term "User-Agent". When the named-site host is known it renders
// the realistic PRE-VERIFICATION per-site identity (config.UserAgentFor(host,
// version, verified=false)) — the exact string the daemon sends before the site is
// verified, including its trust suffix — so "what owners see" matches reality. When
// host is empty (not yet available at preview time) it softens to the host-agnostic
// base identity (config.ResolvedUserAgent) and frames it as the base form rather
// than fabricating a per-site trust suffix. Either way it reuses the config builders
// so the preview never drifts from the real identity string.
func ContactExample(version, contactEmail, host string) string {
	cfg := config.Config{}
	cfg.Crawler.ContactEmail = contactEmail
	if host == "" {
		ua := cfg.ResolvedUserAgent(version)
		return "Site owners will see your base identity: " + ua +
			" — that's how they know it's you, and how to reach you."
	}
	// verified=false: the daemon has not verified this site yet at setup time, so the
	// preview must show the pre-verification per-site form an owner would see first.
	ua := cfg.UserAgentFor(host, version, false)
	return "Site owners will see: " + ua + " — that's how they know it's you, and how to reach you."
}

// GoLiveLine is the "you're live" confirmation after the daemon starts.
func GoLiveLine(host string) string {
	return "✨ You're live — " + host + " is now being watched. We're checking it politely " +
		"and a little slowly for now."
}

// GrafanaSizingNote is the SETTLED sizing copy shown in the "See it on a dashboard"
// upgrade step's huh Note — BEFORE the operator picks how to set it up — so the real
// footprint is on screen before any commitment. Rabbot itself is tiny; the Prometheus
// + Grafana sidecar is what wants a slightly bigger box. Pure copy: it never gates.
const GrafanaSizingNote = "Rabbot alone runs on anything; Prometheus + Grafana add " +
	"roughly 512 MB — recommend a 2 GB box; 1 GB fits but snug."

// ── "Connect Search Console" step copy (cli drives the TTY; these are the single
// source of truth so the wizard and any future surface read identical text) ──────

// GSCConnectIntro frames the optional Search Console step in plain language: it adds
// Google's OWN view (is the page indexed? which canonical did Google pick? how is it
// doing in Search?) on top of what Rabbot sees on the page. Self-hosted, so the data
// never leaves the operator's box.
const GSCConnectIntro = "Connect Google Search Console to add Google's own ground truth — " +
	"whether a page is indexed, which canonical Google actually chose, and its search " +
	"performance — on top of what Rabbot reads from the page. It's read-only and self-hosted: " +
	"your Search Console data never leaves this box."

// GSCSkipAcknowledged is the lossless-skip acknowledgment (the alerts ChannelNone
// precedent): an EXPLICIT terminal state, not a silent skip. No block is written and
// the operator can connect any time later.
const GSCSkipAcknowledged = "Skipped Search Console for now — connect it any time by re-running " +
	"`rabbot init`, or by adding a `gsc` block to a site in config.yaml. See docs/gsc.md."

// GSCPropertyHelp explains the two property forms GSC accepts, with the exact shape so
// the operator can copy their identifier straight from Search Console.
const GSCPropertyHelp = "Your property is exactly how it appears in Search Console: a Domain " +
	"property \"sc-domain:whatthehellai.com\" (covers every scheme + subdomain), or a URL-prefix " +
	"property \"https://www.example.com/\"."

// GSCServiceAccountCredHelp walks the headless service-account path — the recommended
// default for a server. It names the steps but stays generic (no console UI labels that
// drift); the full walkthrough lives in docs/gsc.md and docs/configuration.md.
const GSCServiceAccountCredHelp = "Service account (no browser): in Google Cloud, create a project, " +
	"enable the Search Console API, make a service account + JSON key, then in Search Console grant " +
	"that service account's email read access to the property (Settings → Users and permissions). " +
	"Save the JSON key on this box as a 0600 file and give Rabbot its path below. Full walkthrough: docs/gsc.md."

// GSCOAuthCredHelp points the OAuth2 operator at `rabbot gsc auth` to mint the token
// file first (the consent needs a browser), then asks for the resulting path.
const GSCOAuthCredHelp = "OAuth2 (browser consent): run `rabbot gsc auth` with your own Google Cloud " +
	"OAuth client to complete a one-time consent — it writes a 0600 token file. On a headless server, " +
	"run it on your laptop and scp the file over. Then give Rabbot the token file's path below."

// SleepNudge is the best-effort, one-line advisory printed at go-live ONLY when the
// host looks like a machine that sleeps (a laptop). It never blocks or gates; it just
// sets honest expectations — real-time monitoring wants an always-on box, and a laptop
// that sleeps will pause watching while it's asleep. It points at the README "Where to
// run Rabbot" section and the menu's "Keep watching 24/7" row. The wording stays honest
// (a pause/gap, never an error): monitoring is not broken, it just pauses with the host.
const SleepNudge = "💡 This looks like a laptop. Real-time monitoring wants a box that stays " +
	"awake — when this machine sleeps, watching pauses too, leaving a gap. See \"Where to run " +
	"Rabbot\" in the README for always-on options (a small VPS, a Raspberry Pi, or a Mac mini), " +
	"or pick \"Keep watching 24/7\" below to install the service."
