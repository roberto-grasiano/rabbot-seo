package benchcorpus

import (
	"bytes"
	"strconv"
)

// hreflangs are the alternate-language codes every page declares (the extractor
// collects link[rel=alternate][hreflang]). Fixed and ordered → part of the
// golden bytes.
var hreflangs = []string{"en", "en-gb", "fr", "de", "es", "x-default"}

// writeHead emits a realistic <head> exercising every head selector the
// extractor reads: title, meta description, meta robots, canonical, hreflang
// alternates, Open Graph, Twitter card, and a JSON-LD block. All values are
// derived from (class, index) so the head is deterministic.
func writeHead(b *bytes.Buffer, class Class, index int) {
	idx := strconv.Itoa(index)
	cls := class.String()
	canonical := URL(class, index)

	// Title: a deterministic short phrase + class + index, so titles differ per
	// page (the diff bench's "title changed" arm and the extract title read both
	// want a non-empty, page-specific title).
	title := pick(headingWords, index, 0) + " " + pick(nouns, index, 1) + " — " + cls + " " + idx

	b.WriteString("<!DOCTYPE html>\n<html lang=\"en\">\n<head>\n")
	b.WriteString("<meta charset=\"utf-8\">\n")
	b.WriteString("<meta name=\"viewport\" content=\"width=device-width, initial-scale=1\">\n")

	b.WriteString("<title>")
	writeEscaped(b, title)
	b.WriteString("</title>\n")

	// Meta description: one deterministic sentence.
	b.WriteString("<meta name=\"description\" content=\"")
	writeEscaped(b, sentence(index, 7, 14))
	b.WriteString("\">\n")

	// Robots: indexable by default (the indexability verdict should read as
	// indexable so the diff/rules benches see a normal page).
	b.WriteString("<meta name=\"robots\" content=\"index,follow\">\n")

	// Canonical (absolute, self-referential).
	b.WriteString("<link rel=\"canonical\" href=\"")
	b.WriteString(canonical)
	b.WriteString("\">\n")

	// hreflang alternates: each alternate points at the same path on a
	// per-language subpath so the hrefs are distinct and absolute.
	for _, lang := range hreflangs {
		b.WriteString("<link rel=\"alternate\" hreflang=\"")
		b.WriteString(lang)
		b.WriteString("\" href=\"")
		b.WriteString(site)
		b.WriteString(lang)
		b.WriteString("/")
		b.WriteString(cls)
		b.WriteString("/")
		b.WriteString(idx)
		b.WriteString("\">\n")
	}

	// Open Graph.
	b.WriteString("<meta property=\"og:type\" content=\"website\">\n")
	b.WriteString("<meta property=\"og:title\" content=\"")
	writeEscaped(b, title)
	b.WriteString("\">\n")
	b.WriteString("<meta property=\"og:url\" content=\"")
	b.WriteString(canonical)
	b.WriteString("\">\n")
	b.WriteString("<meta property=\"og:description\" content=\"")
	writeEscaped(b, sentence(index, 8, 12))
	b.WriteString("\">\n")

	// Twitter card.
	b.WriteString("<meta name=\"twitter:card\" content=\"summary_large_image\">\n")
	b.WriteString("<meta name=\"twitter:title\" content=\"")
	writeEscaped(b, title)
	b.WriteString("\">\n")

	// JSON-LD: a single valid block whose @type cycles by index (the extractor
	// parses it, collects schema_types, and A4 reads snap.JSONLD).
	writeJSONLD(b, class, index, title, canonical)

	b.WriteString("</head>\n")
}

// writeJSONLD emits one valid application/ld+json block. The JSON is assembled
// by hand (not encoding/json) so the byte layout is fixed and deterministic —
// encoding/json's output is stable for these inputs, but hand-assembly removes
// any doubt and keeps the golden SHA insensitive to stdlib formatting changes.
// All interpolated strings are JSON-escaped via writeJSONString.
func writeJSONLD(b *bytes.Buffer, class Class, index int, title, canonical string) {
	typ := schemaTypes[abs(index)%len(schemaTypes)]
	b.WriteString("<script type=\"application/ld+json\">\n")
	b.WriteString("{\"@context\":\"https://schema.org\",\"@type\":")
	writeJSONString(b, typ)
	b.WriteString(",\"name\":")
	writeJSONString(b, title)
	b.WriteString(",\"url\":")
	writeJSONString(b, canonical)
	b.WriteString(",\"headline\":")
	writeJSONString(b, sentence(index, 6, 10))
	b.WriteString(",\"description\":")
	writeJSONString(b, sentence(index, 9, 13))
	b.WriteString("}\n</script>\n")
}

// writeBody emits the page body: an H1, then a sequence of sections each with a
// subheading and prose paragraphs sized to the class's word budget, an image
// set (mixed alt / missing-alt), and an internal+external link block sized to
// the class's link budget. The body is what readability extracts as main text.
func writeBody(b *bytes.Buffer, class Class, index int) {
	b.WriteString("<body>\n<main>\n")

	// H1 — single, page-specific.
	b.WriteString("<h1>")
	writeEscaped(b, pick(headingWords, index, 2)+" "+pick(nouns, index, 3))
	b.WriteString("</h1>\n")

	// Lead paragraph.
	b.WriteString("<p>")
	writeEscaped(b, paragraph(index, 0, 4))
	b.WriteString("</p>\n")

	// Body sections. Distribute the word budget across a fixed number of
	// sections so each class has a believable outline (multiple H2/H3 the
	// heading extractor collects).
	words := wordCountFor(class)
	sections := 8
	if class == Listing || class == Landing {
		// A light page (landing) and a link-dominated page (listing) carry a
		// shorter outline than a long-form article.
		sections = 4
	}
	perSection := words / sections
	if perSection < 20 {
		perSection = 20
	}
	for s := 0; s < sections; s++ {
		b.WriteString("<section>\n<h2>")
		writeEscaped(b, pick(headingWords, index+s, 5)+" "+pick(adjectives, index+s, 6))
		b.WriteString("</h2>\n")
		// One H3 per section for a deeper outline.
		b.WriteString("<h3>")
		writeEscaped(b, pick(headingWords, index+s, 7))
		b.WriteString("</h3>\n")
		// Prose paragraphs until the per-section budget is spent. Each paragraph
		// is a fixed number of sentences; the sentence word counts come from the
		// page+section+paragraph indices, so the prose is deterministic.
		emitted := 0
		p := 0
		for emitted < perSection {
			para := paragraph(index+s, p, 5)
			b.WriteString("<p>")
			writeEscaped(b, para)
			b.WriteString("</p>\n")
			emitted += countWords(para)
			p++
		}
		b.WriteString("</section>\n")
	}

	writeImages(b, class, index)
	writeLinks(b, class, index)

	b.WriteString("</main>\n</body>\n</html>\n")
}

// writeImages emits a small fixed image set. Roughly half carry an alt attribute
// and half omit it, so the extractor's MissingAltCount does real work. The count
// is fixed per class (not size-dominant).
func writeImages(b *bytes.Buffer, class Class, index int) {
	n := 6
	if class == Article {
		n = 10
	}
	b.WriteString("<figure>\n")
	for i := 0; i < n; i++ {
		b.WriteString("<img src=\"")
		b.WriteString(site)
		b.WriteString("img/")
		b.WriteString(strconv.Itoa(index))
		b.WriteString("-")
		b.WriteString(strconv.Itoa(i))
		b.WriteString(".jpg\"")
		// Even images get an alt; odd images omit it (deterministic split).
		if i%2 == 0 {
			b.WriteString(" alt=\"")
			writeEscaped(b, pick(adjectives, index+i, 8)+" "+pick(nouns, index+i, 9))
			b.WriteString("\"")
		}
		b.WriteString(">\n")
	}
	b.WriteString("</figure>\n")
}

// writeLinks emits the internal + external link block. Internal links point at
// OTHER in-corpus pages by their stable Path (so a corpus-site's listing pages
// genuinely cross-reference one another and extract's internal-link discovery
// resolves real same-host hrefs). A fixed minority are absolute external links
// to reserved documentation domains so the external classifier also fires. The
// internal link count dominates the listing class's byte size.
func writeLinks(b *bytes.Buffer, class Class, index int) {
	total := linkCountFor(class)
	b.WriteString("<nav class=\"links\">\n<ul>\n")
	for i := 0; i < total; i++ {
		b.WriteString("<li>")
		// Every 7th link is external; the rest are internal cross-references.
		if i%7 == 6 {
			host := externalHosts[(index+i)%len(externalHosts)]
			b.WriteString("<a href=\"")
			b.WriteString(host)
			b.WriteString("/ref/")
			b.WriteString(strconv.Itoa(i))
			b.WriteString("\" rel=\"nofollow\">")
			writeEscaped(b, pick(verbs, index+i, 10)+" "+pick(nouns, index+i, 11))
			b.WriteString("</a>")
		} else {
			// Internal: cycle target class and derive a target index from the
			// position so links spread across the corpus (and are stable).
			targetClass := ClassForIndex(index + i)
			targetIndex := (index*13 + i*7) % 10000
			b.WriteString("<a href=\"")
			b.WriteString(Path(targetClass, targetIndex))
			b.WriteString("\">")
			writeEscaped(b, pick(headingWords, index+i, 12)+" "+pick(nouns, index+i, 13))
			b.WriteString("</a>")
		}
		b.WriteString("</li>\n")
	}
	b.WriteString("</ul>\n</nav>\n")
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
