package extract

import (
	"bytes"
	"net/url"
	"strings"
	"unicode/utf8"

	readability "codeberg.org/readeck/go-readability/v2"
	"github.com/PuerkitoBio/goquery"
	htmlparse "golang.org/x/net/html"
)

// invalidUTF8Replacement is the byte sequence ToValidUTF8 substitutes for each
// run of invalid bytes: the UTF-8 encoding of U+FFFD (the replacement char).
var invalidUTF8Replacement = []byte("�")

// MainText extracts the normalized main-content text used for SimHash. When
// contentSelector is non-empty it overrides readability with that CSS selector.
func MainText(pageURL string, html []byte, contentSelector string) (string, error) {
	// Normalize invalid UTF-8 deterministically before parsing. The same bytes
	// must always yield the same text, or ContentSHA256/SimHash flap and
	// diff.Compare emits false change alerts (B1 fuzz find). Two independent
	// charset-guess sources have to be neutralized:
	//
	//  1. ToValidUTF8 coerces the bytes so the UTF-8-assuming x/net/html parse
	//     below is deterministic (a no-op for the real-world valid-UTF-8 case).
	//  2. readability.FromReader routes through go-shiori/dom.Parse, which runs
	//     gogs/chardet.DetectBest — and that re-guesses an encoding for the
	//     coerced bytes (even the U+FFFD replacement run is ambiguous to it),
	//     re-introducing the flap after step 1. So we parse the document
	//     ourselves with x/net/html (UTF-8, deterministic) and hand the node to
	//     readability.FromDocument, which clones the node and never calls chardet.
	//
	// This is not a charset converter: a genuinely non-UTF-8-encoded page is
	// already mis-handled upstream; this only removes the nondeterminism.
	if !utf8.Valid(html) {
		html = bytes.ToValidUTF8(html, invalidUTF8Replacement)
	}

	if contentSelector != "" {
		doc, err := goquery.NewDocumentFromReader(bytes.NewReader(html))
		if err != nil {
			return "", err
		}
		return normalizeWhitespace(doc.Find(contentSelector).Text()), nil
	}

	u, err := url.Parse(pageURL)
	if err != nil {
		u = &url.URL{}
	}
	// Deterministic UTF-8 parse, bypassing dom.Parse's nondeterministic chardet.
	node, perr := htmlparse.Parse(bytes.NewReader(html))
	if perr != nil {
		return "", perr
	}
	// Strip script/style/noscript/template from the parsed tree BEFORE readability
	// runs. Their text is executable bundle code or embedded framework state (e.g. a
	// __NEXT_DATA__ JSON payload or a <template> buildId), never visible page
	// content. readability's RenderText (the PRIMARY path) renders such elements
	// when they fall inside the chosen article subtree, so a deploy-only buildId
	// flip would churn ContentSHA256 and spam false `content` change alerts (#12).
	// Stripping up front fixes BOTH the readability path and the fallback below from
	// a single source of truth; goquery.Remove detaches the nodes from `node` in
	// place, so readability (which clones `node` internally) never sees them.
	goquery.NewDocumentFromNode(node).Find("script, style, noscript, template").Remove()

	article, aerr := readability.FromDocument(node, u)
	if aerr == nil && article.Node != nil {
		var buf bytes.Buffer
		if rerr := article.RenderText(&buf); rerr == nil {
			return normalizeWhitespace(buf.String()), nil
		}
	}
	// Fall back to full-document text so SimHash still has signal. The
	// script/style/noscript/template strip already ran above (it removes the same
	// noise — executable bundle code / embedded state — that would (a) churn the
	// content hash on a deploy-only buildId flip and (b) miscount a JS bundle as
	// "visible words", the same exclusion precheck.visibleBodyText makes for its
	// word-count proxy), so the fallback body text is clean too.
	doc := goquery.NewDocumentFromNode(node)
	return normalizeWhitespace(doc.Find("body").Text()), nil
}

func normalizeWhitespace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
