package richresult

import (
	"reflect"
	"strings"
	"testing"
)

// FuzzValidate throws arbitrary strings at Validate and asserts the invariants
// the rule engine and surfaces rely on: no panic, determinism, and a
// well-formed Report (non-negative Unprofiled; every entity carries a non-empty
// canonical Type + RawType; Eligible iff nothing missing; any reported
// MissingAnyOf group is one the active profile actually encodes). Seeds (>= 8)
// replay under plain `go test`; they include deep nesting, huge arrays, NUL
// bytes, and hostile JSON-LD. The Makefile fuzz-smoke list is intentionally
// untouched.
func FuzzValidate(f *testing.F) {
	f.Add("")
	f.Add("null")
	f.Add("[]")
	f.Add("{}")
	f.Add(`{"@type":"Product","name":"x","offers":{"price":"1"}}`)
	// @graph with a multi-type member and an unprofiled member.
	f.Add(`{"@context":"https://schema.org","@graph":[{"@type":["NewsArticle","Thing"],"headline":"h"},{"@type":"WebSite","name":"s"}]}`)
	// Top-level array mixing eligible/ineligible/unprofiled entities.
	f.Add(`[{"@type":"Product","name":"a"},{"@type":"BreadcrumbList","itemListElement":[{"@type":"ListItem"}]},{"@type":"FAQPage"}]`)
	// Deep nesting — a Product whose property VALUES are nested objects/arrays
	// many levels deep (only @graph is recursed; property values never are, so
	// this exercises the parser, not @graph recursion).
	f.Add(`{"@type":"Product","name":"x","offers":` + strings.Repeat(`{"a":`, 400) + `1` + strings.Repeat(`}`, 400) + `}`)
	// Deeply nested @graph-of-@graph — exercises the depth-bounded recursion
	// guard against pathologically nested (cyclic-shaped) graph containers.
	f.Add(strings.Repeat(`{"@graph":[`, 200) + `{"@type":"Product","name":"x","offers":{"price":"1"}}` + strings.Repeat(`]}`, 200))
	// Single-object @graph (the @graph value is a lone node, not an array).
	f.Add(`{"@graph":{"@type":"Product","name":"x","offers":{"price":"1"}}}`)
	// Fully-qualified schema.org IRI and CURIE @type forms.
	f.Add(`{"@type":"https://schema.org/Product","name":"x","offers":{"price":"1"}}`)
	f.Add(`{"@type":"schema:Article","headline":"h"}`)
	// Huge array of typed entities.
	f.Add(`[` + strings.Repeat(`{"@type":"Article","headline":"h"},`, 2000) + `{"@type":"Article","headline":"h"}]`)
	// NUL bytes interleaved in keys/values.
	f.Add("{\"@type\":\"Product\",\"na\x00me\":\"\x00\",\"offers\":\x00}")
	// Hostile JSON-LD: @type as a number, @graph as a string, deeply weird shapes.
	f.Add(`{"@type":12345,"@graph":"not-an-array","name":["a","b"]}`)
	// Truncated mid-object (extract may store a severed block).
	f.Add(`{"@type":"Product","name":"x","offers":{`)
	// @type array with empty/whitespace members.
	f.Add(`{"@type":["","   ","BlogPosting"],"headline":"h"}`)
	f.Add(`[[{"@type":"Product","name":"n"},{"@type":"Article","headline":"h"}]]`) // stored array-form block (PR #75 finding)

	f.Fuzz(func(t *testing.T, jsonld string) {
		r := Validate(jsonld, GRR202606) // Invariant 1: never panics.

		// Invariant 2: Report shape.
		if r.Profile != GRR202606.Version {
			t.Fatalf("Report.Profile = %q, want %q", r.Profile, GRR202606.Version)
		}
		if r.Unprofiled < 0 {
			t.Fatalf("Unprofiled is negative: %d", r.Unprofiled)
		}
		for _, e := range r.Entities {
			if e.Type == "" {
				t.Fatalf("entity has empty canonical Type: %+v", e)
			}
			if e.RawType == "" {
				t.Fatalf("entity has empty RawType: %+v", e)
			}
			if _, ok := GRR202606.Types[e.Type]; !ok {
				t.Fatalf("entity Type %q is not a profile key", e.Type)
			}
			wantEligible := len(e.Missing) == 0 && len(e.MissingAnyOf) == 0
			if e.Eligible != wantEligible {
				t.Fatalf("Eligible=%v but Missing=%v MissingAnyOf=%v (inconsistent)", e.Eligible, e.Missing, e.MissingAnyOf)
			}
			// Every reported missing/any-of name must belong to the resolved
			// type's profile — the report never invents requirements.
			tp := GRR202606.Types[e.Type]
			for _, m := range e.Missing {
				if !contains(tp.Required, m) {
					t.Fatalf("Missing %q is not a Required property of %q", m, e.Type)
				}
			}
			for _, group := range e.MissingAnyOf {
				if !groupEncoded(tp.AnyOf, group) {
					t.Fatalf("MissingAnyOf group %v is not an encoded AnyOf group of %q", group, e.Type)
				}
			}
		}

		// Invariant 3: determinism — a second Validate over identical input
		// produces an identical Report.
		r2 := Validate(jsonld, GRR202606)
		if !reflect.DeepEqual(r, r2) {
			t.Fatalf("Validate is non-deterministic for input %q:\n a=%+v\n b=%+v", jsonld, r, r2)
		}
	})
}

func contains(s []string, v string) bool {
	for _, e := range s {
		if e == v {
			return true
		}
	}
	return false
}

func groupEncoded(groups [][]string, g []string) bool {
	for _, encoded := range groups {
		if reflect.DeepEqual(encoded, g) {
			return true
		}
	}
	return false
}
