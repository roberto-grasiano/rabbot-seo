package extract

import "testing"

func TestIndexability(t *testing.T) {
	tests := []struct {
		name       string
		in         IndexabilityInput
		wantIdx    bool
		wantReason string
	}{
		{
			name:    "clean 200 page indexable",
			in:      IndexabilityInput{HTTPStatus: 200, FinalURL: "https://x.com/p"},
			wantIdx: true, wantReason: "indexable",
		},
		{
			name:       "404 not indexable",
			in:         IndexabilityInput{HTTPStatus: 404, FinalURL: "https://x.com/p"},
			wantIdx:    false,
			wantReason: "non_2xx_status",
		},
		{
			name:       "meta robots noindex",
			in:         IndexabilityInput{HTTPStatus: 200, MetaRobots: "noindex, follow", FinalURL: "https://x.com/p"},
			wantIdx:    false,
			wantReason: "meta_robots_noindex",
		},
		{
			name:       "x-robots-tag noindex",
			in:         IndexabilityInput{HTTPStatus: 200, XRobotsTag: "noindex", FinalURL: "https://x.com/p"},
			wantIdx:    false,
			wantReason: "x_robots_tag_noindex",
		},
		{
			name:       "robots.txt disallowed",
			in:         IndexabilityInput{HTTPStatus: 200, RobotsDisallowed: true, FinalURL: "https://x.com/p"},
			wantIdx:    false,
			wantReason: "robots_txt_disallowed",
		},
		{
			name:       "canonical points off-page",
			in:         IndexabilityInput{HTTPStatus: 200, Canonical: "https://x.com/other", FinalURL: "https://x.com/p"},
			wantIdx:    false,
			wantReason: "canonicalized_away",
		},
		{
			name:    "self canonical is fine",
			in:      IndexabilityInput{HTTPStatus: 200, Canonical: "https://x.com/p", FinalURL: "https://x.com/p"},
			wantIdx: true, wantReason: "indexable",
		},
		{
			// F8: http->https self-canonical (migration) must not be canonicalized_away.
			name:    "scheme-only self canonical is fine",
			in:      IndexabilityInput{HTTPStatus: 200, Canonical: "http://x.com/p", FinalURL: "https://x.com/p"},
			wantIdx: true, wantReason: "indexable",
		},
		{
			// F8 / F52: mixed-case host self-canonical must not be canonicalized_away.
			name:    "case-only host self canonical is fine",
			in:      IndexabilityInput{HTTPStatus: 200, Canonical: "https://X.com/p", FinalURL: "https://x.com/p"},
			wantIdx: true, wantReason: "indexable",
		},
		{
			// F8: default-port self-canonical must not be canonicalized_away.
			name:    "default-port self canonical is fine",
			in:      IndexabilityInput{HTTPStatus: 200, Canonical: "https://x.com:443/p", FinalURL: "https://x.com/p"},
			wantIdx: true, wantReason: "indexable",
		},
		{
			// F9: robots directive "none" == "noindex, nofollow".
			name:       "meta robots none is noindex",
			in:         IndexabilityInput{HTTPStatus: 200, MetaRobots: "none", FinalURL: "https://x.com/p"},
			wantIdx:    false,
			wantReason: "meta_robots_noindex",
		},
		{
			// F9: X-Robots-Tag "none" == "noindex, nofollow".
			name:       "x-robots-tag none is noindex",
			in:         IndexabilityInput{HTTPStatus: 200, XRobotsTag: "none", FinalURL: "https://x.com/p"},
			wantIdx:    false,
			wantReason: "x_robots_tag_noindex",
		},
		{
			// F9: "noindexible" substring must NOT trigger noindex (token-exact).
			name:    "noindexible is not noindex",
			in:      IndexabilityInput{HTTPStatus: 200, MetaRobots: "noindexible", FinalURL: "https://x.com/p"},
			wantIdx: true, wantReason: "indexable",
		},
		{
			// #3: a googlebot/X-Robots "googlebot: noindex" UA-prefixed value
			// deindexes via the shared robotsmeta parser.
			name:       "x-robots-tag user-agent-prefixed noindex",
			in:         IndexabilityInput{HTTPStatus: 200, XRobotsTag: "googlebot: noindex", FinalURL: "https://x.com/p"},
			wantIdx:    false,
			wantReason: "x_robots_tag_noindex",
		},
		{
			// #15: a canonical that differs only in the QUERY string is NOT
			// self-referential — /list?page=2 -> /list?page=1 is canonicalized away.
			name:       "query-differing canonical is canonicalized away",
			in:         IndexabilityInput{HTTPStatus: 200, Canonical: "https://x.com/list?page=1", FinalURL: "https://x.com/list?page=2"},
			wantIdx:    false,
			wantReason: "canonicalized_away",
		},
		{
			// #15: same params in a different order are the same page (self).
			name:    "reordered-query self canonical is fine",
			in:      IndexabilityInput{HTTPStatus: 200, Canonical: "https://x.com/list?b=2&a=1", FinalURL: "https://x.com/list?a=1&b=2"},
			wantIdx: true, wantReason: "indexable",
		},
		{
			// #15: query equality must keep slash- and scheme-insensitivity.
			name:    "query self canonical with trailing slash and scheme change is fine",
			in:      IndexabilityInput{HTTPStatus: 200, Canonical: "http://x.com/list/?a=1", FinalURL: "https://x.com/list?a=1"},
			wantIdx: true, wantReason: "indexable",
		},
		{
			// #15: a self canonical with no query on either side stays fine
			// (regression guard for the query addition).
			name:    "no-query self canonical still fine",
			in:      IndexabilityInput{HTTPStatus: 200, Canonical: "https://x.com/p", FinalURL: "https://x.com/p?utm=ad"},
			wantIdx: false, wantReason: "canonicalized_away",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			idx, reason := Indexability(tc.in)
			if idx != tc.wantIdx {
				t.Errorf("Indexability() indexable = %v, want %v", idx, tc.wantIdx)
			}
			if reason != tc.wantReason {
				t.Errorf("Indexability() reason = %q, want %q", reason, tc.wantReason)
			}
		})
	}
}
