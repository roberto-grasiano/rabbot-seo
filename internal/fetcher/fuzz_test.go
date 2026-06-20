package fetcher

import (
	"net/url"
	"strings"
	"testing"
)

// FuzzRobots fuzzes the unexported robotsVerdict gate — the exact parse +
// group-lookup + path-test sequence the production crawl gate runs in
// frontier.RobotsCache (robots.go) — without coupling the fuzzer to the cache's
// HTTP client. It transitively fuzzes github.com/temoto/robotstxt (FromBytes +
// FindGroup + Group.Test) against hostile robots.txt bytes, user-agent tokens,
// and target paths.
//
// Invariants:
//  1. No panic on arbitrary input.
//  2. Closed verdict set: the result is always "allowed" or "disallowed".
//  3. Determinism: a second call on identical input yields the identical
//     verdict (a nondeterministic gate would crawl-or-block the same URL
//     differently between runs).
func FuzzRobots(f *testing.F) {
	// Seeds drawn from the inline fixtures in doctor_test.go and
	// internal/frontier/robots_test.go, plus the hostile-byte cases named in the
	// B1 spec: BOM-prefixed, CRLF-only, one mega-line, %-encoded Disallow paths,
	// unicode UA tokens, and NULs.
	type seed struct {
		body      string
		status    int
		ua        string
		rawTarget string
	}
	seeds := []seed{
		// doctor_test.go: blocked-site robots with a Disallow group.
		{"User-agent: *\nDisallow: /private/\n", 200, "Rabbot-SEO/test (+https://example.test)", "https://example.test/private/secret"},
		// frontier/robots_test.go: allow/disallow + crawl-delay fixture.
		{"User-agent: *\nDisallow: /private/\nCrawl-delay: 7\n", 200, "Rabbot-SEO/test", "https://example.test/public/page"},
		// frontier/robots_test.go: cache-reuse fixture (Disallow /x/).
		{"User-agent: *\nDisallow: /x/\n", 200, "Rabbot-SEO/test", "https://example.test/x/y"},
		// Allow-all.
		{"User-agent: *\nAllow: /\n", 200, "Rabbot-SEO/test", "https://example.test/p"},
		// Missing robots.txt (404 => allowed branch).
		{"", 404, "Rabbot-SEO/test", "https://example.test/anything"},
		// Empty body, 200 (len==0 => allowed branch).
		{"", 200, "Rabbot-SEO/test", "https://example.test/"},
		// BOM-prefixed robots.txt (UTF-8 BOM = 0xEF 0xBB 0xBF).
		{"\xef\xbb\xbfUser-agent: *\nDisallow: /admin/\n", 200, "Rabbot-SEO/test", "https://example.test/admin/x"},
		// CRLF-only line endings.
		{"User-agent: *\r\nDisallow: /crlf/\r\n", 200, "Rabbot-SEO/test", "https://example.test/crlf/page"},
		// One mega-line Disallow value.
		{"User-agent: *\nDisallow: /" + strings.Repeat("a", 50000) + "\n", 200, "Rabbot-SEO/test", "https://example.test/" + strings.Repeat("a", 50000)},
		// %-encoded Disallow path + %-encoded target path.
		{"User-agent: *\nDisallow: /a%2Fb/\n", 200, "Rabbot-SEO/test", "https://example.test/a%2Fb/c"},
		// Unicode UA token + unicode path.
		{"User-agent: Rabbøt-SEO/üñîçødé\nDisallow: /café/\n", 200, "Rabbøt-SEO/üñîçødé", "https://example.test/café/menu"},
		// NUL bytes scattered through body, UA, and target.
		{"User-agent: *\x00\nDisallow: /\x00/\n", 200, "Rab\x00bot", "https://example.test/%00/x"},
		// Garbage / non-grammar body.
		{"\xff\xfe not robots at all \x00\x01\x02", 200, "Rabbot-SEO/test", "https://example.test/g"},
	}
	for _, s := range seeds {
		f.Add([]byte(s.body), s.status, s.ua, s.rawTarget)
	}

	const (
		allowed    = "allowed"
		disallowed = "disallowed"
	)

	f.Fuzz(func(t *testing.T, body []byte, status int, ua, rawTarget string) {
		target, err := url.Parse(rawTarget)
		if err != nil {
			t.Skip()
		}

		rres := Result{HTTPStatus: status, Body: body}

		v1 := robotsVerdict(rres, ua, target)
		if v1 != allowed && v1 != disallowed {
			t.Fatalf("robotsVerdict returned %q, want one of %q/%q (body=%q status=%d ua=%q target=%q)",
				v1, allowed, disallowed, body, status, ua, rawTarget)
		}

		// Determinism: identical input must yield an identical verdict.
		v2 := robotsVerdict(Result{HTTPStatus: status, Body: body}, ua, target)
		if v1 != v2 {
			t.Fatalf("robotsVerdict nondeterministic: %q then %q (body=%q status=%d ua=%q target=%q)",
				v1, v2, body, status, ua, rawTarget)
		}
	})
}
