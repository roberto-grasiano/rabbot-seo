package urlx

import "testing"

func TestHost(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   string
		want string
	}{
		// Lowercasing: net/url does not lowercase the host; we do.
		{"lowercases host", "http://Example.COM/path", "example.com"},
		{"lowercases scheme-relative host", "//Example.COM/x", "example.com"},

		// userinfo is stripped.
		{"strips userinfo", "http://user:pass@example.com/x", "example.com"},
		{"strips userinfo no password", "https://user@Example.com/", "example.com"},

		// Default ports are stripped per-scheme; the bare host is canonical.
		{"strips default http port", "http://example.com:80/", "example.com"},
		{"strips default https port", "https://example.com:443/", "example.com"},
		{"implicit http", "http://example.com/", "example.com"},
		{"implicit https", "https://example.com/", "example.com"},

		// Non-default ports are kept (and distinguish hosts).
		{"keeps non-default port", "http://example.com:8080/", "example.com:8080"},
		{"keeps https on http-default port", "http://example.com:443/", "example.com:443"},
		{"keeps http on https-default port", "https://example.com:80/", "example.com:80"},

		// Scheme-agnostic / scheme-relative / missing scheme.
		{"scheme-relative", "//example.com/x", "example.com"},
		{"scheme-relative host+port", "//example.com:8080/x", "example.com:8080"},
		{"missing scheme bare host", "example.com", "example.com"},
		{"missing scheme host+path", "example.com/path", "example.com"},
		// A scheme-less host WITH a port is ambiguous with "scheme:opaque" in
		// net/url, so it (documented) yields "".
		{"missing scheme host+port is ambiguous", "example.com:8080/x", ""},

		// No scheme => cannot judge a "default" port, so it is kept verbatim
		// (we must not strip :80/:443 without knowing the scheme).
		{"no scheme keeps :80", "//example.com:80/x", "example.com:80"},
		{"no scheme keeps :443", "//example.com:443/x", "example.com:443"},

		// IPv6: bracketless when no port, re-bracketed when a port is kept.
		{"ipv6 no port", "http://[::1]/", "::1"},
		{"ipv6 default https port stripped", "https://[::1]:443/", "::1"},
		{"ipv6 with kept port re-bracketed", "http://[::1]:8080/", "[::1]:8080"},
		{"ipv6 uppercase lowercased", "http://[2001:DB8::1]/", "2001:db8::1"},

		// Fallbacks: no host -> "".
		{"empty", "", ""},
		{"relative path", "/just/a/path", ""},
		{"fragment only", "#frag", ""},
		{"scheme no host", "mailto:foo@bar.com", ""},
		// net/url decodes one level of %-escapes in the authority, leaving a
		// literal "%" in the host that is not a valid label nor re-parseable;
		// Host yields "" rather than a value that fails its own round-trip.
		{"percent-escaped host yields empty", "//ex%2541mple.com", ""},
		{"percent host with scheme yields empty", "https://ex%2541mple.com/", ""},
		// Unbracketed authority that net/url mis-parses into a stray-colon host
		// is not a valid host; only bracketed IPv6 may carry a colon.
		{"unbracketed double-colon yields empty", "//::", ""},
		{"unbracketed triple-colon yields empty", "//:::", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := Host(tc.in); got != tc.want {
				t.Errorf("Host(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestNormalize(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   string
		want string
	}{
		// Scheme + host lowercasing (RFC 3986 6.2.2.1).
		{"lowercases scheme and host", "HTTP://Example.COM/Path", "http://example.com/Path"},
		{"path case is preserved", "http://h/A/B/C", "http://h/A/B/C"},

		// Default-port stripping (RFC 3986 6.2.3): :80 http, :443 https.
		{"strips default http port", "http://example.com:80/x", "http://example.com/x"},
		{"strips default https port", "https://example.com:443/x", "https://example.com/x"},
		{"keeps non-default port", "http://example.com:8080/x", "http://example.com:8080/x"},
		{"keeps https on http-default port", "http://example.com:443/x", "http://example.com:443/x"},

		// Percent-encoding case normalization (RFC 3986 6.2.2.1): hex -> uppercase.
		{"uppercases percent hex in path", "http://h/a%2fb", "http://h/a%2Fb"},
		{"uppercases percent hex in query", "http://h/p?x=a%2fb", "http://h/p?x=a%2Fb"},
		{"already-uppercase hex unchanged", "http://h/a%2Fb", "http://h/a%2Fb"},

		// Unreserved-octet decoding (RFC 3986 6.2.2.2): ALPHA/DIGIT/-/./_/~.
		{"decodes encoded tilde in path", "http://h/%7Euser", "http://h/~user"},
		{"decodes lowercase encoded tilde", "http://h/%7euser", "http://h/~user"},
		{"decodes encoded unreserved letter", "http://h/%41%42", "http://h/AB"},
		// Hyphen and underscore decode; the dot stays ENCODED (only its hex is
		// uppercased) so an encoded "%2E"/"%2E%2E" can never become a dot-segment.
		{"decodes hyphen+underscore but keeps encoded dot", "http://h/%2D%2e%5F", "http://h/-%2E_"},
		{"decodes unreserved in query value", "http://h/p?a=%7E", "http://h/p?a=~"},
		{"does not decode reserved (slash)", "http://h/a%2Fb", "http://h/a%2Fb"},
		{"does not decode reserved (space)", "http://h/a%20b", "http://h/a%20b"},

		// Dot-segment removal (RFC 3986 5.2.4 / 6.2.2.3).
		{"removes single parent segment", "http://h/a/../b", "http://h/b"},
		{"removes current-dir segment", "http://h/a/./b", "http://h/a/b"},
		{"removes mixed dot segments", "http://h/a/../b/./c", "http://h/b/c"},
		{"clamps excess parent segments at root", "http://h/a/b/../../../c", "http://h/c"},
		{"trailing parent yields dir slash", "http://h/a/b/..", "http://h/a/"},
		{"trailing dot yields dir slash", "http://h/a/b/.", "http://h/a/b/"},
		{"root parent stays root", "http://h/..", "http://h/"},
		{"preserves empty segments (double slash)", "http://h/a//b", "http://h/a//b"},
		// An ENCODED %2F is not a separator, so dot segments around it are not collapsed.
		{"encoded slash is not a dot-segment separator", "http://h/a/%2E%2E/b", "http://h/a/%2E%2E/b"},

		// Fragment is DROPPED (crawl/link identity).
		{"drops fragment", "http://h/x#frag", "http://h/x"},
		{"drops fragment keeps query", "http://h/x?a=1#frag", "http://h/x?a=1"},
		{"drops empty fragment", "http://h/x#", "http://h/x"},

		// Query is IDENTITY-significant: order and presence preserved, NOT reordered/dropped.
		{"preserves query param order", "http://h/x?b=2&a=1", "http://h/x?b=2&a=1"},
		{"preserves duplicate params", "http://h/x?a=1&a=2", "http://h/x?a=1&a=2"},
		{"preserves empty query marker", "http://h/x?", "http://h/x?"},

		// userinfo is preserved (Normalize is identity-preserving; only SameHost strips creds).
		{"preserves userinfo", "http://user:pass@h/x", "http://user:pass@h/x"},

		// A "%" in the query that is not a valid escape is re-encoded as %25 so it
		// can never combine with later bytes into a spurious escape on re-normalize
		// (net/url is lenient about malformed escapes in the query). Idempotency for
		// these is also exercised by the loop below and by FuzzNormalizeIdempotent.
		{"bare percent in query re-encoded", "http://h/p?a=100%done", "http://h/p?a=100%25done"},
		{"truncated percent escape in query re-encoded", "http://h/p?x=%zz", "http://h/p?x=%25zz"},
		{"malformed escape does not form spurious escape", "http://h/p?x=%2%44", "http://h/p?x=%252D"},

		// Combined: a realistic messy URL normalized in one shot.
		{"combined normalization", "HTTP://User@Example.COM:80/a/../b/%7Ec?Q=%2f#top", "http://User@example.com/b/~c?Q=%2F"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := Normalize(tc.in)
			if err != nil {
				t.Fatalf("Normalize(%q) returned error: %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("Normalize(%q) = %q, want %q", tc.in, got, tc.want)
			}
			// Idempotency: normalizing the result again must be a fixed point.
			got2, err := Normalize(got)
			if err != nil {
				t.Fatalf("Normalize(%q) (idempotency) returned error: %v", got, err)
			}
			if got2 != got {
				t.Errorf("Normalize not idempotent: Normalize(%q) = %q, want %q", got, got2, got)
			}
		})
	}
}

func TestNormalizeError(t *testing.T) {
	t.Parallel()
	// A control byte in the URL is a parse error; Normalize surfaces it.
	if _, err := Normalize("http://h/\x7f\x00"); err == nil {
		t.Errorf("Normalize of malformed URL: want error, got nil")
	}
}

func TestSameSite(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		a, b string
		want bool
	}{
		// Exact-host equivalence still holds (superset of SameHost's host rule).
		{"identical hosts", "http://example.com/a", "http://example.com/b", true},
		{"case-insensitive", "http://Example.com/a", "http://EXAMPLE.COM/b", true},
		{"default port equivalence", "http://example.com:80/a", "https://example.com:443/b", true},

		// apex <-> www are the SAME site (the headline rule).
		{"apex equals www", "http://example.com/a", "http://www.example.com/b", true},
		{"www equals apex (reversed)", "http://www.example.com/a", "http://example.com/b", true},
		{"www equals www", "https://www.example.com/", "http://www.example.com/x", true},
		{"apex equals www case-insensitive", "http://Example.com/", "http://WWW.Example.com/", true},
		{"apex equals www with userinfo+port", "http://u@example.com:80/", "https://www.example.com:443/", true},

		// NOT eTLD+1: a deeper subdomain is a DIFFERENT site.
		{"sub is not apex", "http://blog.example.com/", "http://example.com/", false},
		{"sub is not www", "http://blog.example.com/", "http://www.example.com/", false},
		{"sub.example vs example false", "http://sub.example.com/", "http://example.com/", false},
		{"www-prefixed deeper sub is distinct", "http://www.blog.example.com/", "http://example.com/", false},

		// Unrelated hosts.
		{"unrelated hosts", "http://example.com/", "http://example.org/", false},
		{"different non-default ports distinct", "http://example.com:8080/", "http://example.com:9090/", false},

		// "www" as a bare apex-less host is not stripped to "" (no false-merge of www.com == com).
		{"bare www host not stripped to empty", "http://www.com/", "http://com/", false},

		// Two unparseable inputs are NOT the same site.
		{"both unparseable not same", "/path/only", "", false},
		{"one unparseable not same", "http://example.com/", "/path/only", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := SameSite(tc.a, tc.b); got != tc.want {
				t.Errorf("SameSite(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
			// Symmetry invariant.
			if got := SameSite(tc.b, tc.a); got != tc.want {
				t.Errorf("SameSite(%q, %q) [symmetry] = %v, want %v", tc.b, tc.a, got, tc.want)
			}
		})
	}
}

func TestSameHost(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		a, b string
		want bool
	}{
		// Case-insensitivity across the comparison.
		{"case-insensitive", "http://Example.com/a", "http://EXAMPLE.COM/b", true},

		// Explicit default port == implicit (the headline normalization).
		{"explicit :80 == implicit http", "http://example.com:80/a", "http://example.com/b", true},
		{"explicit :443 == implicit https", "https://example.com:443/a", "https://example.com/b", true},

		// Cross-scheme but same host (scheme-agnostic): default ports of each
		// scheme normalize away to the same bare host.
		{"http vs https same host", "http://example.com/a", "https://example.com/b", true},

		// userinfo does not affect host identity.
		{"userinfo ignored", "https://alice@example.com/a", "https://bob:pw@example.com/b", true},

		// A non-default port makes a DISTINCT host.
		{"non-default port distinct", "http://example.com:8080/a", "http://example.com/b", false},
		{"different non-default ports distinct", "http://example.com:8080/", "http://example.com:9090/", false},

		// Scheme-relative and missing-scheme resolve to the same host as a
		// fully-qualified URL.
		{"scheme-relative vs absolute", "//example.com/x", "http://example.com/y", true},
		{"missing scheme vs absolute", "example.com/x", "https://example.com/y", true},

		// IPv6 identity regardless of bracket/port/case presentation.
		{"ipv6 same", "http://[::1]/a", "http://[::1]:80/b", true},
		{"ipv6 case-insensitive", "http://[2001:DB8::1]/", "http://[2001:db8::1]/", true},

		// Genuinely different hosts.
		{"different hosts", "http://a.example.com/", "http://b.example.com/", false},

		// Two unparseable inputs are NOT the same host (must not admit garbage
		// as same-scope just because both yield "").
		{"both unparseable not same", "/path/only", "", false},
		{"one unparseable not same", "http://example.com/", "/path/only", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := SameHost(tc.a, tc.b); got != tc.want {
				t.Errorf("SameHost(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}
