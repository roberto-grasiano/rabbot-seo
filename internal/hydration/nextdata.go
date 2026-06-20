package hydration

import (
	"encoding/json"
	"strings"
)

// headKeyAllowlist names the object keys, under props.pageProps, that carry SEO
// head fields in the wild. __NEXT_DATA__ has no standardized title/meta/canonical
// keys (Next Pages Router renders next/head into the real HTML head at SSR time,
// so payload SEO keys are app-specific), so head-field recovery rides this small
// heuristic allowlist. It is deliberately narrow — payload recovery is strongest
// for body text; head fields back-fill only what the DOM left empty.
var (
	titleKeys = []string{"metaTitle", "seoTitle", "title", "pageTitle", "ogTitle"}
	descKeys  = []string{"metaDescription", "seoDescription", "description", "metaDesc", "ogDescription"}
	canonKeys = []string{"canonicalUrl", "canonical", "canonicalURL", "canonicalLink"}
	// seoContainerKeys name nested objects that group the head fields above
	// (e.g. props.pageProps.seo.title). Recovery walks these one level deep.
	seoContainerKeys = []string{"seo", "meta", "metadata", "head", "openGraph", "og"}
)

// FromNextData decodes a Next.js __NEXT_DATA__ JSON payload and recovers SEO
// signals from it. Degenerate payloads (scalars, empty object/array) yield zero
// Fields with Decoded=false — no recovery is claimed, mirroring the precheck
// honesty rule. A payload larger than maxBytes is skipped with Truncated=true
// and zero recovered fields. Malformed JSON returns an error and never panics.
func FromNextData(raw []byte, maxBytes int) (Fields, error) {
	var f Fields
	if overCap(len(raw), maxBytes) {
		f.Truncated = true
		return f, nil
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return f, nil
	}

	var root any
	if err := json.Unmarshal(raw, &root); err != nil {
		return f, err
	}

	// Honesty: a scalar or empty container carries no recoverable content.
	if !nonDegenerate(root) {
		return f, nil
	}
	f.Decoded = true

	// Locate props.pageProps for the head-field allowlist; harvest prose from
	// the whole tree (volatile-key filtered) so body content is recovered even
	// when it lives outside pageProps.
	if obj, ok := root.(map[string]any); ok {
		if props, ok := obj["props"].(map[string]any); ok {
			if pageProps, ok := props["pageProps"].(map[string]any); ok {
				harvestHeadFields(pageProps, &f)
			}
		}
	}

	sink := newProseSink()
	walkProse(root, "", 0, &nodeBudget{n: 0}, sink)
	f.BodyTextCandidates = sink.sorted()
	return f, nil
}

// nonDegenerate reports whether v is a non-empty object or array. A scalar
// (null/bool/number/string) or an empty container is degenerate: no recoverable
// hydration content (mirrors precheck's detect.go honesty rule).
func nonDegenerate(v any) bool {
	switch t := v.(type) {
	case map[string]any:
		return len(t) > 0
	case []any:
		return len(t) > 0
	default:
		return false
	}
}

// harvestHeadFields fills f's empty head fields from the allowlisted keys in obj
// and one level of nested seo-container objects. The first non-empty value for
// each field wins; later occurrences do not overwrite (allowlist order is the
// preference order).
func harvestHeadFields(obj map[string]any, f *Fields) {
	pick := func(keys []string, dst *string) {
		if *dst != "" {
			return
		}
		for _, k := range keys {
			if s, ok := stringVal(obj[k]); ok && strings.TrimSpace(s) != "" {
				*dst = strings.TrimSpace(s)
				return
			}
		}
	}
	pick(titleKeys, &f.Title)
	pick(descKeys, &f.MetaDescription)
	pick(canonKeys, &f.Canonical)

	// One level of nested seo-container objects (props.pageProps.seo.title, …).
	for _, ck := range seoContainerKeys {
		nested, ok := obj[ck].(map[string]any)
		if !ok {
			continue
		}
		pick2 := func(keys []string, dst *string) {
			if *dst != "" {
				return
			}
			for _, k := range keys {
				if s, ok := stringVal(nested[k]); ok && strings.TrimSpace(s) != "" {
					*dst = strings.TrimSpace(s)
					return
				}
			}
		}
		pick2(titleKeys, &f.Title)
		pick2(descKeys, &f.MetaDescription)
		pick2(canonKeys, &f.Canonical)
	}
}

// stringVal returns v as a string if it is one.
func stringVal(v any) (string, bool) {
	s, ok := v.(string)
	return s, ok
}

// nodeBudget caps the total nodes a prose walk visits, independent of depth, as
// a second guard against pathological fan-out.
type nodeBudget struct{ n int }

// walkProse recursively collects prose-like string leaves from a decoded JSON
// value, skipping values held under volatile keys. It is bounded by maxWalkDepth,
// the node budget, and the sink's count bound, so it always terminates.
func walkProse(v any, key string, depth int, budget *nodeBudget, sink *proseSink) {
	if depth > maxWalkDepth {
		return
	}
	budget.n++
	if budget.n > maxWalkNodes {
		return
	}
	if len(sink.out) >= maxProseCandidates {
		return
	}
	// A value held directly under a volatile key is never prose (the buildId /
	// *Hash churn-defence): skip the whole subtree.
	if key != "" && volatileKey(key) {
		return
	}
	switch t := v.(type) {
	case map[string]any:
		for k, child := range t {
			walkProse(child, k, depth+1, budget, sink)
		}
	case []any:
		for _, child := range t {
			walkProse(child, key, depth+1, budget, sink)
		}
	case string:
		sink.add(t)
	}
}
