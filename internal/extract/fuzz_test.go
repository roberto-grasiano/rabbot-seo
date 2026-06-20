package extract

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/roberto-grasiano/rabbot-seo/internal/fetcher"
	"github.com/roberto-grasiano/rabbot-seo/internal/model"
	"github.com/roberto-grasiano/rabbot-seo/internal/urlx"
)

// fuzzFinalURL is the pinned response URL for FuzzExtract. Pinning it (rather
// than fuzzing it) keeps the URL-dependent invariants — link host equality and
// canonical resolution — decidable: a fuzzed FinalURL would make "every link is
// SameHost(FinalURL)" untestable, because the host the extractor compares
// against would itself be garbage.
const fuzzFinalURL = "https://example.com/a/b?q=1"

// hexLower reports whether s is exactly n lowercase hex characters.
func isLowerHex(s string) bool {
	if _, err := hex.DecodeString(s); err != nil {
		return false
	}
	return s == strings.ToLower(s)
}

// FuzzExtract throws arbitrary body bytes, Content-Type and content-selector
// values at NewExtractor().Extract with a pinned FinalURL/HTTPStatus, and
// asserts the snapshot invariants the downstream pipeline relies on (closed
// error set, JSON-valid snapshot fields, count/hash shape, link host/fragment,
// and extraction determinism). It also locks goquery's invalid-selector
// behavior (match-nothing, no panic) on the user-configurable content_selector.
func FuzzExtract(f *testing.F) {
	// Seed >= 8 inputs drawn from the inline extract_test.go fixtures plus the
	// hostile shapes named in the B1 spec.
	f.Add([]byte(fullHTML), "text/html", "")
	f.Add([]byte(`<html><head><title>t</title>`+
		`<script type="application/ld+json">{"@type":"Article","headline":"x"}</script>`+
		`</head><body><p>hi</p></body></html>`), "text/html", "")
	f.Add([]byte(`<html><head><title>t</title>`+
		`<script type="application/ld+json">{"@context":"https://schema.org","@graph":[{"@type":"WebSite","name":"s"},{"@type":["Organization","LocalBusiness"],"name":"o"}]}</script>`+
		`</head><body><p>hi</p></body></html>`), "text/html", "main")
	// 600-deep DOM — exercises the ErrDOMTooDeep open-stack sentinel path.
	f.Add([]byte(deepDOM(600)), "text/html", "")
	// Empty body.
	f.Add([]byte(``), "text/html", "")
	// NUL bytes interleaved in markup.
	f.Add([]byte("<html><head><title>\x00\x00null\x00</title></head><body>\x00<p>\x00x</p></body></html>"), "text/html", "")
	// PDF magic carried under a text/html Content-Type (mislabeled binary).
	f.Add([]byte("%PDF-1.7\n%\xe2\xe3\xcf\xd3\n1 0 obj<</Type/Catalog>>"), "text/html", "")
	// HTML body served as application/json (non-HTML gate must fire => ErrNonHTML).
	f.Add([]byte(`<html><head><title>json</title></head><body><a href="/x">x</a></body></html>`), "application/json", "")
	// A fuzzed-shaped invalid CSS selector (locks goquery match-nothing behavior).
	f.Add([]byte(fullHTML), "text/html", ":::not-a-selector[[")
	// Body with assorted links + images for the count/host/fragment invariants.
	f.Add([]byte(`<html><head><title>t</title></head><body>`+
		`<a href="/a">a</a><a href="/a#frag">dup</a><a href="https://example.com/b#z">b</a>`+
		`<a href="https://other.com/c">ext</a><a href="mailto:x@y.z">m</a>`+
		`<img src="1.jpg" alt=""><img src="2.jpg"><img src="3.jpg" alt="ok">`+
		`</body></html>`), "text/html", "")

	f.Fuzz(func(t *testing.T, body []byte, contentType, selector string) {
		res := fetcher.Result{
			FinalURL:   fuzzFinalURL,
			HTTPStatus: 200,
			FetchClass: model.FetchOK,
			Header:     http.Header{"Content-Type": {contentType}},
			Body:       body,
		}

		snap, links, err := NewExtractor().Extract(res, selector)

		// Invariant 1: closed error set.
		if err != nil {
			if !errors.Is(err, ErrNonHTML) && !errors.Is(err, ErrDOMTooDeep) {
				t.Fatalf("Extract returned an unexpected error outside the closed set "+
					"{nil, ErrNonHTML, ErrDOMTooDeep}: %v (contentType=%q)", err, contentType)
			}
			// On a non-nil error the snapshot is not a success result; nothing
			// more to assert. (links is nil on every error path.)
			return
		}

		// --- success-path invariants ---

		// Invariant 2: every JSON-typed snapshot field is valid JSON; JSONLD is
		// "" OR valid.
		jsonFields := map[string]string{
			"Headings":      snap.Headings,
			"Hreflang":      snap.Hreflang,
			"OG":            snap.OG,
			"Twitter":       snap.Twitter,
			"SchemaTypes":   snap.SchemaTypes,
			"RedirectChain": snap.RedirectChain,
		}
		for name, v := range jsonFields {
			if v == "" {
				t.Fatalf("snapshot JSON field %s is empty; want a JSON document (even null/[])", name)
			}
			if !json.Valid([]byte(v)) {
				t.Fatalf("snapshot JSON field %s is not valid JSON: %q", name, v)
			}
		}
		if snap.JSONLD != "" && !json.Valid([]byte(snap.JSONLD)) {
			t.Fatalf("snapshot JSONLD is non-empty but not valid JSON: %q", snap.JSONLD)
		}

		// Invariant 3: count sanity.
		if snap.ImageCount < 0 || snap.MissingAltCount < 0 ||
			snap.InternalLinkCount < 0 || snap.ExternalLinkCount < 0 || snap.WordCount < 0 {
			t.Fatalf("negative count in snapshot: img=%d missingAlt=%d int=%d ext=%d words=%d",
				snap.ImageCount, snap.MissingAltCount, snap.InternalLinkCount,
				snap.ExternalLinkCount, snap.WordCount)
		}
		if snap.MissingAltCount > snap.ImageCount {
			t.Fatalf("MissingAltCount (%d) > ImageCount (%d)", snap.MissingAltCount, snap.ImageCount)
		}

		// Invariant 4: every returned link parses, has an empty fragment, and is
		// same-host as the (pinned) FinalURL.
		for _, link := range links {
			u, perr := url.Parse(link)
			if perr != nil {
				t.Fatalf("returned link does not parse: %q: %v", link, perr)
			}
			if u.Fragment != "" {
				t.Fatalf("returned link retains a fragment: %q", link)
			}
			if !urlx.SameHost(link, fuzzFinalURL) {
				t.Fatalf("returned link is not same-host as FinalURL: link=%q final=%q", link, fuzzFinalURL)
			}
		}

		// Invariant 5: WordCount==0 => ContentSimhash==0; ContentSHA256 is 64
		// lowercase hex chars.
		if snap.WordCount == 0 && snap.ContentSimhash != 0 {
			t.Fatalf("WordCount==0 but ContentSimhash=%d (want 0)", snap.ContentSimhash)
		}
		if len(snap.ContentSHA256) != 64 || !isLowerHex(snap.ContentSHA256) {
			t.Fatalf("ContentSHA256 is not 64 lowercase hex chars: %q (len=%d)",
				snap.ContentSHA256, len(snap.ContentSHA256))
		}

		// Invariant 6: determinism. A second Extract over identical input must
		// produce an identical snapshot and link slice — nondeterminism here
		// makes diff.Compare emit false change alerts.
		snap2, links2, err2 := NewExtractor().Extract(res, selector)
		if err2 != nil {
			t.Fatalf("second Extract on identical input errored where the first did not: %v", err2)
		}
		if !reflect.DeepEqual(snap, snap2) {
			t.Fatalf("Extract is non-deterministic: snapshots differ across identical calls")
		}
		if !reflect.DeepEqual(links, links2) {
			t.Fatalf("Extract is non-deterministic: link slices differ: %v vs %v", links, links2)
		}
	})
}

// deepDOM builds a fully-closed DOM nested `depth` <div> elements deep, matching
// the fixture in TestExtractDeepDOMReturnsSentinel.
func deepDOM(depth int) string {
	var b strings.Builder
	b.WriteString("<!DOCTYPE html><html><head><title>t</title></head><body>")
	for i := 0; i < depth; i++ {
		b.WriteString("<div>")
	}
	b.WriteString("deep content")
	for i := 0; i < depth; i++ {
		b.WriteString("</div>")
	}
	b.WriteString("</body></html>")
	return b.String()
}
