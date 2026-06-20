package wizard

import (
	"github.com/roberto-grasiano/rabbot-seo/internal/precheck"
	"github.com/roberto-grasiano/rabbot-seo/internal/verify"
)

// RecommendMethod picks the easiest proof-of-control method for a detected
// platform plus a one-line plain hint to pre-highlight it on the method screen
// (§V V2). An editable CMS / site builder ⇒ the homepage meta tag (no file
// upload, no DNS record) with a hint that names the platform. An unknown
// platform ⇒ no recommendation: the method returned is still the sensible
// default surface (meta), but the hint is EMPTY, which the screen reads as
// "make no claim" and shows the three plain choices with nothing pre-selected.
//
// The empty-hint signal — not the method value — is the contract the screen
// keys on for "is there a recommendation?", so an unknown platform never pushes
// a guess at the user.
func RecommendMethod(p precheck.Platform) (verify.Method, string) {
	switch p {
	case precheck.PlatformWordPress, precheck.PlatformSquarespace,
		precheck.PlatformWix, precheck.PlatformShopify, precheck.PlatformGhost:
		return verify.MethodMeta, "looks like " + p.Label() + " — adding a tag to your homepage is easiest"
	default:
		return verify.MethodMeta, "" // sensible default surface, but no claim/recommendation
	}
}
