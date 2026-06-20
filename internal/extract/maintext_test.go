package extract

import (
	"strings"
	"testing"
)

const articleHTML = `<!DOCTYPE html><html><head><title>News</title></head><body>
<nav>Home About Contact</nav>
<article><h1>Big Story</h1>
<p>This is the main article body containing several meaningful sentences about the topic at hand.</p>
<p>Here is a second paragraph that adds substantive content to the page for readability extraction.</p>
</article>
<footer>Copyright 2026</footer></body></html>`

func TestMainTextReadability(t *testing.T) {
	text, err := MainText("https://example.com/news", []byte(articleHTML), "")
	if err != nil {
		t.Fatalf("MainText() error = %v", err)
	}
	if !strings.Contains(text, "main article body") {
		t.Errorf("readability dropped main content: %q", text)
	}
	if !strings.Contains(text, "second paragraph") {
		t.Errorf("readability dropped second paragraph: %q", text)
	}
}

// TestMainTextDeterministicOnInvalidUTF8 pins the B1 fuzz find: readability's
// charset detector (gogs/chardet, reached via go-shiori/dom.Parse) returns a
// non-deterministic encoding guess for invalid-UTF-8 bodies, so MainText would
// decode the same bytes to different text across calls — making
// ContentSHA256/SimHash flap and diff.Compare emit false change alerts. MainText
// must yield identical text for identical bytes. The cases below are minimized
// crasher inputs from FuzzExtract; the second exercises the residual ambiguity
// that survives a plain ToValidUTF8 coercion (chardet re-guesses an encoding for
// the U+FFFD replacement bytes themselves), which is why the fix routes through a
// single deterministic x/net/html parse instead of dom.Parse's chardet path.
func TestMainTextDeterministicOnInvalidUTF8(t *testing.T) {
	bodies := [][]byte{
		[]byte("\x81\xf4 \"B"),
		[]byte("0<A>0<A>0<A>0\xbe\x16\x16>0<A>"),
	}
	for _, body := range bodies {
		first, _ := MainText("https://example.com/a/b?q=1", body, "")
		for i := 0; i < 2000; i++ {
			got, _ := MainText("https://example.com/a/b?q=1", body, "")
			if got != first {
				t.Fatalf("MainText non-deterministic on invalid UTF-8 %q: iter %d got %q, first %q",
					body, i, got, first)
			}
		}
	}
}

// TestMainTextStripsTemplateOnReadabilityPath (#12): the
// script/style/noscript/template strip must run on the PRIMARY readability path,
// not only the fallback. A <template> carrying embedded framework state (e.g. a
// buildId) is never visible page content; if its text leaks into the hashed main
// text, a deploy-only buildId flip churns ContentSHA256 and spams false `content`
// change alerts. Two documents that differ ONLY by the buildId inside a
// <template> must yield identical MainText.
func TestMainTextStripsTemplateOnReadabilityPath(t *testing.T) {
	// The <template> sits INSIDE the article so readability picks it up on the
	// primary RenderText path (a sibling-of-article template is excluded by
	// readability's own scoring and would not reproduce the leak).
	tmpl := func(buildID string) []byte {
		return []byte(`<!DOCTYPE html><html><head><title>News</title></head><body>
<article><h1>Big Story Headline Here</h1>
<p>This is the main article body containing several meaningful sentences about the topic at hand for readers.</p>
<p>Here is a second paragraph that adds substantive content to the page for readability extraction now.</p>
<template id="__state">{"buildId":"` + buildID + `","props":{}}</template>
</article>
</body></html>`)
	}
	a, err := MainText("https://example.com/news", tmpl("BUILD_AAAA"), "")
	if err != nil {
		t.Fatalf("MainText() error = %v", err)
	}
	b, err := MainText("https://example.com/news", tmpl("BUILD_BBBB"), "")
	if err != nil {
		t.Fatalf("MainText() error = %v", err)
	}
	if a != b {
		t.Fatalf("MainText churns on a <template> buildId flip:\n a=%q\n b=%q", a, b)
	}
	// The real article content must survive; only the template payload is stripped.
	if !strings.Contains(a, "main article body") {
		t.Errorf("readability dropped main content: %q", a)
	}
	if strings.Contains(a, "buildId") || strings.Contains(a, "BUILD_AAAA") {
		t.Errorf("template payload leaked into main text: %q", a)
	}
}

func TestMainTextSelectorOverride(t *testing.T) {
	html := `<html><body>
<div class="boiler">junk junk junk</div>
<div id="content"><p>selected main content only here</p></div>
</body></html>`
	text, err := MainText("https://example.com/x", []byte(html), "#content")
	if err != nil {
		t.Fatalf("MainText() error = %v", err)
	}
	if !strings.Contains(text, "selected main content only here") {
		t.Errorf("selector content missing: %q", text)
	}
	if strings.Contains(text, "junk") {
		t.Errorf("selector should exclude boilerplate: %q", text)
	}
}
