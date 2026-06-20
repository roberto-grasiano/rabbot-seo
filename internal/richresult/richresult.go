// Package richresult validates a page's stored JSON-LD against a versioned
// Rabbot profile of rich-result eligibility checks. The profile mostly mirrors
// Google's documented rich-result requirements but encodes a few deliberate
// Rabbot policy choices that are stricter than Google's literal wording (e.g.
// Article.headline; see GRR202606 and ADR 0001) — so a check is a Rabbot policy,
// not always a restatement of a Google requirement. It is pure, stdlib-only
// (encoding/json), performs no network I/O, and never renders.
//
// Validation is presence-driven: it judges only the structured-data types the
// site has implemented and that the active Profile encodes. It never infers a
// vertical and never recommends adding markup the site does not ship. The
// question answered is "is this markup still eligible?", never "what markup
// should you ship?".
//
// The encoded requirements live in a versioned Profile (GRR202606, "grr-2026.06").
// A golden test pins the version string and the exact type/property table; a
// requirement change must ship as a NEW version constant, never a silent edit.
package richresult

import (
	"encoding/json"
	"strings"
)

// TypeProfile encodes one rich-result type family's requirements.
//
//   - Aliases: alternate @type names that resolve to this same canonical family
//     (e.g. Article ← NewsArticle, BlogPosting). The canonical key in
//     Profile.Types plus the aliases form the full match set.
//   - Required: properties Rabbot treats as required for this family. Most mirror
//     Google's documented requirement; some are a stricter Rabbot policy choice
//     (Article.headline) — surfaces must word those as Rabbot policy, not as a
//     Google requirement. See GRR202606 and ADR 0001.
//   - AnyOf: groups of which at least one member must be present (e.g. a Product
//     needs at least one of offers|review|aggregateRating).
type TypeProfile struct {
	Aliases  []string
	Required []string
	AnyOf    [][]string
}

// Profile is a versioned encoding of rich-result requirements. Types is keyed by
// the canonical @type name.
type Profile struct {
	Version string
	Types   map[string]TypeProfile
}

// GRR202606 is the v1 profile: a Rabbot-curated rich-result check set, encoded
// June 2026, mostly mirroring Google's documented requirements with documented
// policy deltas (Article.headline is a Rabbot policy choice — Google lists it as
// recommended, not required). Deliberately three type families (Product, Article
// family, BreadcrumbList). See docs/adr for the scoping rationale and the
// live-docs reconciliation.
//
// NOTE: any edit to this table must bump the Version and add a NEW package-level
// constant — the golden test enforces this.
var GRR202606 = Profile{
	Version: "grr-2026.06",
	Types: map[string]TypeProfile{
		"Product": {
			Required: []string{"name"},
			AnyOf:    [][]string{{"offers", "review", "aggregateRating"}},
		},
		"Article": {
			Aliases: []string{"NewsArticle", "BlogPosting"},
			// headline is a Rabbot policy choice, NOT a Google requirement.
			// Google lists Article properties (headline included) as RECOMMENDED,
			// not required, for eligibility — an Article rich result can render with
			// no required structured-data property. Rabbot still flags an Article
			// that ships no headline, because a deploy that strips it leaves no
			// usable enhanced presentation and is a regression worth paging. Surfaces
			// must word this as Rabbot policy ("Rabbot flags Article without a
			// headline; Google lists headline as recommended, not required"), never
			// as a Google fact. See ADR 0001, "Required-vs-recommended reconciliation".
			Required: []string{"headline"},
		},
		"BreadcrumbList": {
			Required: []string{"itemListElement"},
		},
	},
}

// EntityResult is the per-entity verdict for one profiled JSON-LD entity.
//
//   - Type is the canonical profile key the entity matched (e.g. "Article").
//   - RawType is the markup's literal @type (e.g. "BlogPosting"); for a
//     multi-type @type array it is the first member that matched the profile.
//   - Missing lists Required properties that are absent.
//   - MissingAnyOf lists the AnyOf groups that have no present member.
//   - Eligible is true iff Missing and MissingAnyOf are both empty.
type EntityResult struct {
	Type         string
	RawType      string
	Eligible     bool
	Missing      []string
	MissingAnyOf [][]string
}

// Report is the result of validating a JSON-LD column.
//
//   - Entities holds one EntityResult per discovered entity whose type the
//     Profile encodes, in discovery order.
//   - Unprofiled counts discovered typed entities with no profile entry. It is a
//     neutral detail figure only — never an eligibility verdict.
type Report struct {
	Profile    string
	Entities   []EntityResult
	Unprofiled int
}

// Validate parses jsonld and validates each discovered entity against p.
//
// It tolerates the empty string, "null", "[]", "{}", whitespace, and malformed
// JSON — any of these yields an empty Report with no panic (extract marshals a
// nil block slice to "null"). Entity discovery covers: a top-level object, a
// top-level array of objects, and members of a top-level object's @graph
// (an array, a single object, or a nested @graph container — recursed up to a
// fixed depth bound). @type is read as a string or an array of strings, and a
// fully-qualified schema.org IRI or schema: CURIE @type resolves the same as
// its bare term (see normalizeType).
//
// Entities whose @type matches no profile entry are not validated; they only
// increment Report.Unprofiled.
func Validate(jsonld string, p Profile) Report {
	report := Report{Profile: p.Version}

	trimmed := strings.TrimSpace(jsonld)
	if trimmed == "" {
		return report
	}

	var raw any
	if err := json.Unmarshal([]byte(trimmed), &raw); err != nil {
		return report
	}

	for _, obj := range discoverEntities(raw) {
		validateEntity(obj, p, &report)
	}
	return report
}

// discoverEntities flattens the supported JSON-LD shapes into a slice of entity
// objects: a top-level object (and its @graph members), or each object in a
// top-level array. Array members that are themselves arrays are unwrapped one
// level: the stored snapshots.jsonld column is an array of raw <script> blocks,
// so a block whose own content is a top-level array (legal JSON-LD) arrives
// nested — [[{...},{...}]]. Other non-object members are ignored.
func discoverEntities(raw any) []map[string]any {
	var out []map[string]any
	switch v := raw.(type) {
	case map[string]any:
		out = append(out, expandObject(v)...)
	case []any:
		for _, member := range v {
			switch m := member.(type) {
			case map[string]any:
				out = append(out, expandObject(m)...)
			case []any:
				for _, inner := range m {
					if obj, ok := inner.(map[string]any); ok {
						out = append(out, expandObject(obj)...)
					}
				}
			}
		}
	}
	return out
}

// maxGraphDepth bounds @graph recursion so a cyclic or pathologically nested
// document (a @graph member that is itself a @graph, repeated) cannot drive
// unbounded recursion. JSON-LD never legitimately nests this deep; 32 is far
// past any real markup.
const maxGraphDepth = 32

// expandObject returns obj plus any @graph member objects. An object with a
// @graph is treated as a container: its members are entities. The container
// itself is still included so a container that also carries its own @type is not
// dropped. A member that is itself a @graph container is recursed into (depth-
// bounded by maxGraphDepth) so nested @graph entities stay discoverable, and a
// @graph whose value is a single object (not an array) is treated as a one-
// member graph.
func expandObject(obj map[string]any) []map[string]any {
	return expandObjectDepth(obj, 0)
}

func expandObjectDepth(obj map[string]any, depth int) []map[string]any {
	out := []map[string]any{obj}
	if depth >= maxGraphDepth {
		return out
	}
	switch graph := obj["@graph"].(type) {
	case []any:
		for _, member := range graph {
			if m, ok := member.(map[string]any); ok {
				out = append(out, expandObjectDepth(m, depth+1)...)
			}
		}
	case map[string]any:
		// Single-object @graph: a lone node, not an array.
		out = append(out, expandObjectDepth(graph, depth+1)...)
	}
	return out
}

// validateEntity resolves obj's @type to a profile entry and, if matched,
// appends an EntityResult to report. A typed-but-unprofiled entity increments
// Unprofiled. An untyped object (no @type, e.g. a bare @graph container)
// contributes nothing.
func validateEntity(obj map[string]any, p Profile, report *Report) {
	rawTypes := typeValues(obj["@type"])
	if len(rawTypes) == 0 {
		return
	}

	// First @type member that the profile encodes wins. The RawType recorded is
	// that matching member, so aliases (BlogPosting) stay visible to surfaces.
	for _, rt := range rawTypes {
		canonical, tp, ok := resolve(p, rt)
		if !ok {
			continue
		}
		report.Entities = append(report.Entities, evaluate(canonical, rt, tp, obj))
		return
	}

	// Typed but no @type member is profiled → neutral unprofiled count.
	report.Unprofiled++
}

// evaluate checks tp's Required and AnyOf requirements against obj.
func evaluate(canonical, rawType string, tp TypeProfile, obj map[string]any) EntityResult {
	res := EntityResult{Type: canonical, RawType: rawType}

	for _, req := range tp.Required {
		if !present(obj[req], hasKey(obj, req)) {
			res.Missing = append(res.Missing, req)
		}
	}
	for _, group := range tp.AnyOf {
		satisfied := false
		for _, member := range group {
			if present(obj[member], hasKey(obj, member)) {
				satisfied = true
				break
			}
		}
		if !satisfied {
			res.MissingAnyOf = append(res.MissingAnyOf, group)
		}
	}

	res.Eligible = len(res.Missing) == 0 && len(res.MissingAnyOf) == 0
	return res
}

// resolve maps a raw @type to its canonical profile key and TypeProfile. It
// matches the canonical key first, then any alias. The @type is first normalized
// to its bare schema.org term (see normalizeType) so a fully-qualified
// schema.org IRI ("https://schema.org/Product") or the schema: CURIE
// ("schema:Product") resolves the same as the bare term.
func resolve(p Profile, rawType string) (string, TypeProfile, bool) {
	t := normalizeType(rawType)
	if tp, ok := p.Types[t]; ok {
		return t, tp, true
	}
	for name, tp := range p.Types {
		for _, alias := range tp.Aliases {
			if alias == t {
				return name, tp, true
			}
		}
	}
	return "", TypeProfile{}, false
}

// normalizeType collapses a fully-qualified schema.org @type IRI or the schema:
// CURIE prefix to its bare term, leaving every other @type untouched. It is
// deliberately conservative: it only strips a leading http(s)://schema.org/
// (with an optional single trailing slash) or a leading "schema:" prefix, so a
// lookalike host (https://schema.org.evil.com/Product) or an unrelated CURIE
// prefix is NOT coerced to a profile key. Already-bare terms (and IRIs from
// other vocabularies) pass through unchanged.
func normalizeType(rawType string) string {
	for _, prefix := range []string{"https://schema.org/", "http://schema.org/", "schema.org/"} {
		if rest, ok := strings.CutPrefix(rawType, prefix); ok {
			// Bare schema.org term; tolerate one trailing slash
			// (".../Product/"). A residual slash means a deeper path
			// (".../Product/extra") — not a bare term, leave it.
			rest = strings.TrimSuffix(rest, "/")
			if rest != "" && !strings.Contains(rest, "/") {
				return rest
			}
			return rawType
		}
	}
	// CURIE forms naming the schema.org vocabulary (kept consistent with
	// extract.bareSchemaType); an arbitrary "foo:Bar" is a different vocabulary, left intact.
	for _, prefix := range []string{"schema:", "schemaorg:"} {
		if rest, ok := strings.CutPrefix(rawType, prefix); ok {
			if rest != "" && !strings.Contains(rest, ":") {
				return rest
			}
		}
	}
	return rawType
}

// typeValues extracts @type as a string or []string. Non-empty string members
// are collected in order; whitespace-only/empty members are skipped.
func typeValues(v any) []string {
	var out []string
	switch t := v.(type) {
	case string:
		if s := strings.TrimSpace(t); s != "" {
			out = append(out, s)
		}
	case []any:
		for _, e := range t {
			if s, ok := e.(string); ok {
				if s = strings.TrimSpace(s); s != "" {
					out = append(out, s)
				}
			}
		}
	}
	return out
}

// hasKey reports whether obj contains key (distinguishing an explicit JSON null
// from a missing key — both read as absent, but kept explicit for clarity).
func hasKey(obj map[string]any, key string) bool {
	_, ok := obj[key]
	return ok
}

// present reports whether a JSON-LD property value counts as present. A property
// is present iff the key exists AND the value is not null, an empty/whitespace
// string, an empty array, or an empty object. Numbers (incl. 0) and booleans
// (incl. false) count as present.
func present(v any, keyExists bool) bool {
	if !keyExists {
		return false
	}
	switch val := v.(type) {
	case nil:
		return false
	case string:
		return strings.TrimSpace(val) != ""
	case []any:
		return len(val) > 0
	case map[string]any:
		return len(val) > 0
	default:
		// numbers, bools — present.
		return true
	}
}
