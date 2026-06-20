package precheck

// Honest, calibrated message text. These are written in one place so the CLI (and
// the future TUI wizard) just print them. The wording is deliberately tentative —
// "appears/likely", never "definitely/guaranteed/certain" — because JS-need detection
// from raw HTML is a hint, not proof (adversarial verification refuted "reliably
// detectable"). The ClientShell advice carries the user's #1 requirement: the honest
// warning that Rabbot reads the server's HTML only and may not see JS-loaded content.
const (
	summaryServerRendered = "Looks good: this page's SEO content (title, meta, headings) appears to be " +
		"present in the server's HTML, so Rabbot can fully monitor this URL without JavaScript."

	summaryHydrated = "This page looks client-rendered, but a hydration payload " +
		"(e.g. __NEXT_DATA__/__NUXT_DATA__, or a decoded React Server Components " +
		"__next_f stream) was found and decoded — so its SEO fields are recoverable " +
		"without rendering. Fields present in the head or payload are monitorable; any field " +
		"missing from both may still require JavaScript."

	summaryHeadOnlyShell = "Partial: the SEO head (title, meta, headings) appears present in the " +
		"server's HTML, but the page body looks client-rendered — an empty app shell with little " +
		"server-side text and no hydration payload to recover it."

	summaryClientShell = "Heads up: this page appears to be client-rendered — its SEO content does not " +
		"seem to be in the server's HTML and likely loads via JavaScript in a browser."

	summaryUnknown = "Couldn't form a confident read of how this page renders (it may be blocked, " +
		"unreachable, non-HTML, or returned an empty body)."

	// adviceConfirm is the shared "this is a hint, confirm it" line.
	adviceConfirm = "This is a hint based on the raw HTML — confirm by comparing the page's " +
		"View Source with its rendered DOM in a browser."

	// adviceClientShell carries the mandatory honest warning (the user's #1 requirement).
	adviceClientShell = "IMPORTANT: Rabbot reads the server's HTML only. It may not see content, " +
		"links, or some meta tags that JavaScript adds in a browser, so it cannot fully verify " +
		"this domain's on-page SEO. This is a calibrated hint, not a definitive verdict — the " +
		"signals that drove this call are listed above. " + adviceConfirm

	// adviceHeadOnlyShell carries the honest body-not-visible caveat for the partial case:
	// the head is monitorable, but the body content is likely JS-rendered.
	adviceHeadOnlyShell = "Rabbot can monitor the head fields, but body content, internal links, " +
		"and headings that JavaScript adds in the browser may not be visible — so it cannot fully " +
		"verify this page's on-page SEO. " + adviceConfirm

	// adviceUnknown nudges the user to the preflight reasons plus the same honest caveat.
	adviceUnknown = "Rabbot reads server HTML only; if the site relies on JavaScript it may not " +
		"see all content. Check the preflight reasons above, then " + adviceConfirm
)

// applyMessages sets the honest per-kind Summary and Advice on a JSDependency.
// Kept separate from Detect so wording can evolve without touching detection logic.
func applyMessages(js *JSDependency) {
	switch js.Kind {
	case ServerRendered:
		js.Summary = summaryServerRendered
		js.Advice = adviceConfirm
	case Hydrated:
		js.Summary = summaryHydrated
		js.Advice = adviceConfirm
	case HeadOnlyShell:
		js.Summary = summaryHeadOnlyShell
		js.Advice = adviceHeadOnlyShell
	case ClientShell:
		js.Summary = summaryClientShell
		js.Advice = adviceClientShell
	default: // Unknown
		js.Summary = summaryUnknown
		js.Advice = adviceUnknown
	}
}
