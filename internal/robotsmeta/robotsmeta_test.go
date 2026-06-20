package robotsmeta

import "testing"

func TestParseTokens(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  []string
	}{
		{"empty", "", nil},
		{"single", "noindex", []string{"noindex"}},
		{"comma separated", "noindex, nofollow", []string{"noindex", "nofollow"}},
		{"comma no space", "noindex,nofollow", []string{"noindex", "nofollow"}},
		{"whitespace separated", "noindex nofollow", []string{"noindex", "nofollow"}},
		{"tabs and newlines", "noindex\tnofollow\nnoarchive", []string{"noindex", "nofollow", "noarchive"}},
		{"lowercased", "NoIndex, NOFOLLOW", []string{"noindex", "nofollow"}},
		// A leading "user-agent:" token is stripped: Google documents
		// "googlebot: noindex" as a valid X-Robots-Tag value where the first
		// colon-delimited token names the bot, not a directive.
		{"strips leading user-agent", "googlebot: noindex", []string{"noindex"}},
		{"strips leading user-agent comma form", "googlebot: noindex, nofollow", []string{"noindex", "nofollow"}},
		{"strips leading user-agent case-insensitive", "Googlebot: NoIndex", []string{"noindex"}},
		// A leading token that is itself a known directive is NOT stripped as a
		// user-agent: "noindex: nofollow" keeps noindex (defensive — colon used
		// as a separator, not a UA prefix).
		{"known leading directive not stripped", "noindex: nofollow", []string{"noindex", "nofollow"}},
		{"max-snippet directive value kept", "max-snippet:-1", []string{"max-snippet", "-1"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Parse(tc.value)
			if len(got) != len(tc.want) {
				t.Fatalf("Parse(%q) = %v, want %v", tc.value, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("Parse(%q) = %v, want %v", tc.value, got, tc.want)
				}
			}
		})
	}
}

func TestIsNoindex(t *testing.T) {
	tests := []struct {
		value string
		want  bool
	}{
		{"", false},
		{"index, follow", false},
		{"noindex", true},
		{"noindex, follow", true},
		{"index, nofollow", false},
		{"NOINDEX", true},
		// none == noindex + nofollow per Google's robots-meta spec.
		{"none", true},
		{"None", true},
		{"all, none", true},
		// user-agent prefix forms.
		{"googlebot: noindex", true},
		{"googlebot: none", true},
		{"googlebot: index, follow", false},
		// substring must NOT false-positive (token-exact).
		{"noindexible", false},
		{"unnoindex", false},
		// a non-noindex directive that merely starts with "no".
		{"nofollow", false},
		{"noarchive, nosnippet", false},
	}
	for _, tc := range tests {
		t.Run(tc.value, func(t *testing.T) {
			if got := IsNoindex(tc.value); got != tc.want {
				t.Errorf("IsNoindex(%q) = %v, want %v", tc.value, got, tc.want)
			}
		})
	}
}

func TestHasToken(t *testing.T) {
	if !HasToken("noindex, nofollow", "nofollow") {
		t.Errorf("HasToken should find nofollow")
	}
	if HasToken("noindex, follow", "nofollow") {
		t.Errorf("HasToken should not find nofollow in index,follow")
	}
	// none expands to noindex+nofollow for HasToken too.
	if !HasToken("none", "nofollow") {
		t.Errorf("HasToken(none, nofollow) should be true (none == noindex+nofollow)")
	}
	if !HasToken("none", "noindex") {
		t.Errorf("HasToken(none, noindex) should be true (none == noindex+nofollow)")
	}
	// case-insensitive match on the wanted token.
	if !HasToken("NoArchive", "noarchive") {
		t.Errorf("HasToken should be case-insensitive")
	}
	// user-agent prefix stripped.
	if !HasToken("googlebot: noarchive", "noarchive") {
		t.Errorf("HasToken should strip the leading user-agent token")
	}
}
