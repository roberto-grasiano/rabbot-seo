package precheck

import (
	"strings"

	"github.com/PuerkitoBio/goquery"
)

// Platform is a best-guess CMS / site-builder fingerprint, used only to
// RECOMMEND a verification method and pick the right "show me how" deep-link in
// the wizard. It NEVER gates: an unrecognized site is PlatformUnknown and the
// screen simply offers the plain choices with nothing pre-highlighted.
//
// The enum is the shared contract between the precheck sniff (which produces a
// Platform from the homepage HTML) and the wizard's recommendation/provider-hint
// tables (which consume it). It is deliberately small and easily extended.
type Platform int

const (
	// PlatformUnknown is the zero value: no confident CMS fingerprint.
	PlatformUnknown Platform = iota
	// PlatformWordPress is WordPress (self-hosted or .com).
	PlatformWordPress
	// PlatformSquarespace is the Squarespace site builder.
	PlatformSquarespace
	// PlatformWix is the Wix site builder.
	PlatformWix
	// PlatformShopify is the Shopify storefront platform.
	PlatformShopify
	// PlatformGhost is the Ghost publishing platform.
	PlatformGhost
)

// Label is the human-readable platform name woven into plain-language hints
// (e.g. "looks like WordPress — …"). PlatformUnknown degrades to a generic
// phrase so a hint that interpolates it never leaks the enum or reads oddly.
func (p Platform) Label() string {
	switch p {
	case PlatformWordPress:
		return "WordPress"
	case PlatformSquarespace:
		return "Squarespace"
	case PlatformWix:
		return "Wix"
	case PlatformShopify:
		return "Shopify"
	case PlatformGhost:
		return "Ghost"
	default:
		return "your site builder"
	}
}

// SniffPlatform fingerprints the CMS/site-builder from the homepage HTML —
// primarily the <meta name="generator"> tag (WordPress, Squarespace, Wix,
// Shopify, and Ghost all emit one), with a couple of body-level fallbacks for
// builders that fingerprint elsewhere (WordPress's /wp-content/ asset paths,
// Wix's runtime markers, Shopify's storefront markup).
//
// It is recommendation-only and best-effort: an unrecognized site returns
// PlatformUnknown and the wizard shows the plain method choices. Order matters —
// the most specific, highest-confidence signals are checked first.
func SniffPlatform(html string) Platform {
	gen := ""
	if doc, err := goquery.NewDocumentFromReader(strings.NewReader(html)); err == nil {
		gen, _ = doc.Find(`meta[name="generator"]`).Attr("content")
	}
	gen = strings.ToLower(gen)
	hay := strings.ToLower(html)
	switch {
	case strings.Contains(gen, "wordpress"), strings.Contains(hay, "/wp-content/"):
		return PlatformWordPress
	case strings.Contains(gen, "squarespace"), strings.Contains(hay, "squarespace"):
		return PlatformSquarespace
	case strings.Contains(gen, "wix"), strings.Contains(hay, "wix.com"),
		strings.Contains(hay, "_wixcssmodules"):
		return PlatformWix
	case strings.Contains(gen, "shopify"), strings.Contains(hay, "cdn.shopify.com"):
		return PlatformShopify
	case strings.Contains(gen, "ghost"):
		return PlatformGhost
	default:
		return PlatformUnknown
	}
}
