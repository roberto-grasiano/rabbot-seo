package wizard

import (
	"github.com/roberto-grasiano/rabbot-seo/internal/precheck"
	"github.com/roberto-grasiano/rabbot-seo/internal/verify"
)

// providerLink is a curated "show me how" deep-link to a provider's OWN docs:
// a button label plus the URL. We link the provider's documentation rather than
// reproduce its UI (which goes stale the moment they redesign).
type providerLink struct {
	label string
	url   string
}

// providerHints maps (platform, method) to the provider's own documentation for
// that proof method. Keep this table SMALL and easy to update; a missing entry
// falls back to the generic how-to (ProviderHint returns "", ""), so the screen
// always has something to show. These links point at the provider's docs for
// adding custom meta tags / verification codes to a site's <head>.
var providerHints = map[precheck.Platform]map[verify.Method]providerLink{
	precheck.PlatformWordPress: {
		verify.MethodMeta: {"Show me how on WordPress", "https://wordpress.org/documentation/article/site-verification-services/"},
	},
	precheck.PlatformSquarespace: {
		verify.MethodMeta: {"Show me how on Squarespace", "https://support.squarespace.com/hc/en-us/articles/205815908"},
	},
	precheck.PlatformWix: {
		verify.MethodMeta: {"Show me how on Wix", "https://support.wix.com/en/article/adding-custom-meta-tags-to-your-site"},
	},
	precheck.PlatformShopify: {
		verify.MethodMeta: {"Show me how on Shopify", "https://help.shopify.com/en/manual/promoting-marketing/seo/adding-keywords"},
	},
}

// ProviderHint returns a labeled deep-link for (platform, method), or ("", "")
// when there is no curated entry. An empty return is the signal to the screen
// that it should show the generic, provider-agnostic how-to instead.
func ProviderHint(p precheck.Platform, m verify.Method) (label, url string) {
	if byMethod, ok := providerHints[p]; ok {
		if link, ok := byMethod[m]; ok {
			return link.label, link.url
		}
	}
	return "", ""
}
