package extract

import (
	"net/url"
	"sort"
	"strings"

	"github.com/roberto-grasiano/rabbot-seo/internal/robotsmeta"
)

// IndexabilityInput carries the signals needed to derive an indexability verdict.
type IndexabilityInput struct {
	HTTPStatus       int
	MetaRobots       string
	XRobotsTag       string
	RobotsDisallowed bool
	Canonical        string
	FinalURL         string
}

// Indexability derives the indexability verdict and a machine reason from
// status + robots.txt + meta-robots + X-Robots-Tag + canonical.
func Indexability(in IndexabilityInput) (bool, string) {
	if in.HTTPStatus < 200 || in.HTTPStatus >= 300 {
		return false, "non_2xx_status"
	}
	if in.RobotsDisallowed {
		return false, "robots_txt_disallowed"
	}
	if robotsmeta.IsNoindex(in.MetaRobots) {
		return false, "meta_robots_noindex"
	}
	if robotsmeta.IsNoindex(in.XRobotsTag) {
		return false, "x_robots_tag_noindex"
	}
	if in.Canonical != "" && !sameURL(in.Canonical, in.FinalURL) {
		return false, "canonicalized_away"
	}
	return true, "indexable"
}

// sameURL reports whether the canonical and the final URL are the same page for
// the purposes of suppressing the canonicalized_away verdict. It parses both and
// compares host (case-insensitive, port stripped via Hostname), path
// (trailing-slash-insensitive), AND the query (param-sorted so order is
// irrelevant), deliberately IGNORING the scheme so an http->https self-canonical
// (a common migration) still matches. The query MUST participate: a paginated
// page /list?page=2 whose canonical is /list?page=1 is NOT self-referential — it
// is canonicalized away — so a host+path-only match would wrongly judge it self
// (#15). It stays conservative: only a full host+path+query match suppresses
// canonicalized_away, so a genuine off-page canonical is preserved. On any parse
// failure it falls back to a raw trailing-slash-insensitive compare.
func sameURL(a, b string) bool {
	ua, errA := url.Parse(a)
	ub, errB := url.Parse(b)
	if errA != nil || errB != nil {
		return strings.TrimRight(a, "/") == strings.TrimRight(b, "/")
	}
	return strings.EqualFold(ua.Hostname(), ub.Hostname()) &&
		strings.TrimRight(ua.EscapedPath(), "/") == strings.TrimRight(ub.EscapedPath(), "/") &&
		canonicalQuery(ua.RawQuery) == canonicalQuery(ub.RawQuery)
}

// canonicalQuery returns an order-independent canonical form of a raw query
// string so two URLs that differ only in parameter ORDER (?a=1&b=2 vs ?b=2&a=1)
// compare equal, while a genuine difference in params or values does not. It
// re-encodes the parsed values with url.Values.Encode, which sorts by key; on a
// parse failure it falls back to the raw string so a malformed query is still
// compared verbatim rather than silently treated as empty.
func canonicalQuery(raw string) string {
	if raw == "" {
		return ""
	}
	vals, err := url.ParseQuery(raw)
	if err != nil {
		return raw
	}
	// url.Values.Encode sorts keys; values per key keep their slice order, so
	// also sort them to make repeated keys (?a=2&a=1) order-independent.
	for k := range vals {
		sort.Strings(vals[k])
	}
	return vals.Encode()
}
