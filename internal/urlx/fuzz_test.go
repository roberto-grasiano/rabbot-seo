package urlx

import (
	"strings"
	"testing"
)

// reref turns a Host() result back into a parseable authority so the round-trip
// invariant can re-feed Host's own output. Host returns a bare host
// ("example.com", "::1"), a host:port ("example.com:8080"), or an already
// bracketed IPv6 host:port ("[::1]:8080"). To re-reference it we prefix "//" so
// net/url reads it as scheme-relative; a bare IPv6 literal (multiple ":" and no
// "[") must be re-bracketed so its colons aren't mistaken for a port separator.
func reref(h string) string {
	if strings.Count(h, ":") > 1 && !strings.Contains(h, "[") {
		// Bare IPv6 literal with no port (Host strips brackets when no port).
		h = "[" + h + "]"
	}
	return "//" + h
}

// FuzzNormalizeURL fuzzes the host-scoped normalization seam (Host + SameHost),
// per b1-parser-robustness.md T4. The card name is kept ("FuzzNormalizeURL")
// though the symbols under test are urlx.Host / urlx.SameHost.
func FuzzNormalizeURL(f *testing.F) {
	// >=8 seeds drawn from the urlx_test.go fixture table (IPv6, default ports,
	// userinfo, scheme-relative, scheme-less) plus %-encoded hosts (the
	// documented net/url authority-decode falsifier).
	seeds := []string{
		"http://Example.COM/path",        // lowercasing
		"http://user:pass@example.com/x", // userinfo strip
		"http://example.com:80/",         // default http port
		"https://example.com:443/",       // default https port
		"http://example.com:8080/",       // kept non-default port
		"//example.com/x",                // scheme-relative
		"//example.com:80/x",             // scheme-less keeps :80
		"example.com",                    // bare host
		"example.com/path",               // bare host + path
		"example.com:8080/x",             // documented "" limitation (host:port, no scheme)
		"http://[::1]/",                  // IPv6 no port
		"https://[::1]:443/",             // IPv6 default port stripped
		"http://[::1]:8080/",             // IPv6 kept port re-bracketed
		"http://[2001:DB8::1]/",          // IPv6 uppercase
		"//ex%2541mple.com",              // verified falsifier: %-escape decode in authority
		"https://ex%41mple.com/",         // once-decoded escape in host
		"",                               // empty
		"/just/a/path",                   // relative path, no authority
		"mailto:foo@bar.com",             // scheme, opaque, no host
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, s string) {
		h := Host(s) // invariant 1: no panic on arbitrary input.

		if h != "" {
			// Invariant 3: output shape — already lowercase, no userinfo "@".
			if h != strings.ToLower(h) {
				t.Fatalf("Host(%q) = %q is not lowercase", s, h)
			}
			if strings.Contains(h, "@") {
				t.Fatalf("Host(%q) = %q contains userinfo '@'", s, h)
			}

			// Invariant 2: precise reref round-trip idempotency.
			if got := Host(reref(h)); got != h {
				t.Fatalf("round-trip: Host(reref(Host(%q)))=Host(%q)=%q, want %q", s, reref(h), got, h)
			}

			// Invariant 4: reflexivity when Host != "".
			if !SameHost(s, s) {
				t.Fatalf("SameHost(%q, %q) = false, want true (Host=%q != \"\")", s, s, h)
			}
		}
	})
}

// FuzzSameHostSymmetry fuzzes the SameHost symmetry invariant over two strings.
func FuzzSameHostSymmetry(f *testing.F) {
	pairs := [][2]string{
		{"http://Example.com/a", "http://EXAMPLE.COM/b"},
		{"http://example.com:80/a", "http://example.com/b"},
		{"http://example.com/a", "https://example.com/b"},
		{"https://alice@example.com/a", "https://bob:pw@example.com/b"},
		{"http://example.com:8080/a", "http://example.com/b"},
		{"//example.com/x", "http://example.com/y"},
		{"http://[::1]/a", "http://[::1]:80/b"},
		{"http://[2001:DB8::1]/", "http://[2001:db8::1]/"},
		{"/path/only", ""},
		{"//ex%2541mple.com", "example.com"},
	}
	for _, p := range pairs {
		f.Add(p[0], p[1])
	}

	f.Fuzz(func(t *testing.T, a, b string) {
		if SameHost(a, b) != SameHost(b, a) {
			t.Fatalf("SameHost asymmetry: SameHost(%q,%q)=%v but SameHost(%q,%q)=%v",
				a, b, SameHost(a, b), b, a, SameHost(b, a))
		}
	})
}

// FuzzNormalizeIdempotent fuzzes Normalize for two invariants: it never panics
// on arbitrary input, and on any input it accepts it is idempotent
// (Normalize(Normalize(x)) == Normalize(x)). Idempotency is the property most
// sensitive to the percent-encoding / dot-segment ordering, so it is the most
// valuable thing to fuzz.
func FuzzNormalizeIdempotent(f *testing.F) {
	seeds := []string{
		"HTTP://Example.COM:80/a/../b/%7Ec?Q=%2f#top",
		"http://h/a/%2E%2E/b",
		"http://h/%2D%2e%5F",
		"http://h/a//b/./c/..",
		"http://user:pass@h:443/x?a=1&a=2",
		"https://[::1]:443/p?x=%41",
		"http://h/foo%20bar#frag",
		"//example.com/x",
		"example.com/path",
		"",
		"/just/a/path",
		"mailto:foo@bar.com",
		"http://h/%ZZ", // malformed escape: must not panic, must be idempotent
		"http://h/a/b/../../../c",
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, s string) {
		got, err := Normalize(s) // invariant 1: no panic on arbitrary input.
		if err != nil {
			return // unparseable input has no normal form to check.
		}
		got2, err := Normalize(got)
		if err != nil {
			t.Fatalf("Normalize(%q)=%q re-normalized to an error: %v", s, got, err)
		}
		if got2 != got {
			t.Fatalf("Normalize not idempotent: Normalize(%q)=%q, Normalize(that)=%q", s, got, got2)
		}
	})
}
