package hydration

import (
	"encoding/json"
	"strings"
)

// FromFlight parses React Server Components __next_f flight rows and recovers SEO
// signals from the streamed head elements. Each row is framed as "id:payload" or
// "id:tag,payload" where the payload, when it is a JSON array, may be a React
// element tuple shaped ["$", "tagName", key, props]. This bounded decoder
// recovers four element kinds — title, meta (name=description), link
// (rel=canonical), and script (type=application/ld+json) — plus prose-like text
// found in element children. Unknown tags and unparseable rows are SKIPPED, never
// fatal. Over-cap input (summed row length) is skipped with Truncated=true.
//
// The rows are harvested by the caller from every <script> containing
// self.__next_f.push(...); this function takes the already-extracted row strings.
func FromFlight(rows []string, maxBytes int) (Fields, error) {
	var f Fields

	total := 0
	for _, r := range rows {
		total += len(r)
	}
	if overCap(total, maxBytes) {
		f.Truncated = true
		return f, nil
	}
	if len(rows) == 0 {
		return f, nil
	}

	sink := newProseSink()
	decodedAny := false
	for _, row := range rows {
		payload, ok := flightPayload(row)
		if !ok {
			continue
		}
		var v any
		if err := json.Unmarshal([]byte(payload), &v); err != nil {
			// Unparseable row — skip, never fatal.
			continue
		}
		if walkFlight(v, 0, &nodeBudget{n: 0}, &f, sink) {
			decodedAny = true
		}
	}
	f.BodyTextCandidates = sink.sorted()

	// Decoded is true only if at least one element/value was recovered — an
	// all-garbage or all-unknown stream recovers nothing and claims nothing.
	if decodedAny || f.Title != "" || f.MetaDescription != "" ||
		f.Canonical != "" || len(f.JSONLD) > 0 || len(f.BodyTextCandidates) > 0 {
		f.Decoded = true
	}
	return f, nil
}

// flightPayload extracts the JSON payload portion of a flight row. A row is
// "id:rest"; the id is the segment before the first ':'. The rest may itself be
// prefixed by a short tag and comma (e.g. "HL[...]" or "I[...]"), but the
// recoverable element tuples we care about begin at the first '[' or '{'. We
// return the substring from the first JSON-opening bracket onward; a row with no
// such bracket (or no ':') is not a decodable payload.
func flightPayload(row string) (string, bool) {
	colon := strings.IndexByte(row, ':')
	if colon < 0 {
		return "", false
	}
	rest := row[colon+1:]
	// Find the first JSON container opener; everything before it is framing.
	br := strings.IndexAny(rest, "[{")
	if br < 0 {
		return "", false
	}
	return rest[br:], true
}

// walkFlight recursively inspects a decoded flight payload value, recovering
// head elements from React element tuples and prose from element children. It
// reports whether it recovered anything. Bounded by maxWalkDepth and the node
// budget so deeply nested tuples terminate.
func walkFlight(v any, depth int, budget *nodeBudget, f *Fields, sink *proseSink) bool {
	if depth > maxWalkDepth {
		return false
	}
	budget.n++
	if budget.n > maxWalkNodes {
		return false
	}
	switch t := v.(type) {
	case []any:
		recovered := false
		if isElementTuple(t) {
			if recoverElement(t, f, sink) {
				recovered = true
			}
		}
		// Recurse into every element regardless: children may nest more tuples.
		for _, el := range t {
			if walkFlight(el, depth+1, budget, f, sink) {
				recovered = true
			}
		}
		return recovered
	case map[string]any:
		recovered := false
		for _, child := range t {
			if walkFlight(child, depth+1, budget, f, sink) {
				recovered = true
			}
		}
		return recovered
	case string:
		// A prose-like string leaf (e.g. an element's children text) is body
		// content worth recovering. Non-prose leaves (ids, urls, slugs) are
		// rejected by the sink's looksLikeProse gate. The sink's count bound is
		// the only stop condition here; recovery of prose does not by itself
		// set the element-recovered flag — the final Decoded determination in
		// FromFlight already counts a non-empty BodyTextCandidates as decoded.
		sink.add(t)
		return false
	default:
		return false
	}
}

// isElementTuple reports whether arr is a React element tuple: a JSON array
// whose first element is the sentinel string "$". Element tuples are shaped
// ["$", type, key, props].
func isElementTuple(arr []any) bool {
	if len(arr) < 2 {
		return false
	}
	s, ok := arr[0].(string)
	return ok && s == "$"
}

// recoverElement extracts SEO fields from a single element tuple
// ["$", tag, key, props]. Unknown tags are skipped (return false). It reports
// whether it recovered a field.
func recoverElement(arr []any, f *Fields, sink *proseSink) bool {
	tag, ok := arr[1].(string)
	if !ok {
		return false
	}
	var props map[string]any
	if len(arr) >= 4 {
		props, _ = arr[3].(map[string]any)
	}
	switch tag {
	case "title":
		if f.Title == "" {
			if s := childrenText(props); s != "" {
				f.Title = s
				return true
			}
		}
	case "meta":
		if f.MetaDescription == "" && props != nil {
			if name, _ := stringVal(props["name"]); strings.EqualFold(name, "description") {
				if content, _ := stringVal(props["content"]); strings.TrimSpace(content) != "" {
					f.MetaDescription = strings.TrimSpace(content)
					return true
				}
			}
		}
	case "link":
		if f.Canonical == "" && props != nil {
			if rel, _ := stringVal(props["rel"]); strings.EqualFold(rel, "canonical") {
				if href, _ := stringVal(props["href"]); strings.TrimSpace(href) != "" {
					f.Canonical = strings.TrimSpace(href)
					return true
				}
			}
		}
	case "script":
		if props != nil {
			if typ, _ := stringVal(props["type"]); strings.EqualFold(strings.TrimSpace(typ), "application/ld+json") {
				if blk := ldJSONFromProps(props); blk != nil {
					if len(f.JSONLD) < maxJSONLDBlocks {
						f.JSONLD = append(f.JSONLD, blk)
						return true
					}
				}
			}
		}
	}
	return false
}

// childrenText returns the text of an element's "children" prop when it is a
// plain string (the common title/text case). Non-string children (nested
// elements) are left to the recursive walk's prose harvest.
func childrenText(props map[string]any) string {
	if props == nil {
		return ""
	}
	if s, ok := stringVal(props["children"]); ok {
		return strings.TrimSpace(s)
	}
	return ""
}

// ldJSONFromProps extracts the raw ld+json document from a script element's
// props. React renders inline JSON-LD via dangerouslySetInnerHTML.__html; the
// inner string is itself a JSON document, which we validate before keeping.
func ldJSONFromProps(props map[string]any) json.RawMessage {
	dsi, ok := props["dangerouslySetInnerHTML"].(map[string]any)
	if !ok {
		return nil
	}
	html, ok := stringVal(dsi["__html"])
	if !ok {
		return nil
	}
	html = strings.TrimSpace(html)
	if html == "" || !json.Valid([]byte(html)) {
		return nil
	}
	return json.RawMessage(html)
}
