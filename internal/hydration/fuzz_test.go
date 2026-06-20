package hydration

import (
	"testing"
	"unicode/utf8"
)

// fuzzCap is the byte cap the fuzz targets pass. It is small enough to exercise
// the truncation path on larger inputs but large enough that the seed corpora
// decode, so both the recover and the skip-with-Truncated branches are fuzzed.
const fuzzCap = 1 << 16 // 64 KiB

// FuzzFromNuxtData throws arbitrary bytes at the devalue decoder and asserts the
// safety contract the B1 fuzz-smoke gate relies on: it never panics, always
// terminates (the test process itself is the termination oracle — a hang is a
// fuzz failure), and produces bounded output. Seeds include the hostile shapes
// named in the spec: deeply nested, cyclic indexes, non-finite numbers, invalid
// UTF-8, and truncated frames.
func FuzzFromNuxtData(f *testing.F) {
	f.Add([]byte(`[{"seo":1,"body":2},{"title":3},"prose text here and there for a while"]`))
	f.Add([]byte(`[]`))
	f.Add([]byte(`[{"a":1},{"b":0}]`))                                                   // 2-cycle
	f.Add([]byte(`[{"self":0}]`))                                                        // self-cycle
	f.Add([]byte(`[{"x":99}]`))                                                          // out-of-range index
	f.Add([]byte(`[1e999,-1e999,1.5,2,3]`))                                              // non-finite-ish / large numbers
	f.Add([]byte(`[{"deep":1},{"deep":2},{"deep":3},{"deep":4},{"deep":5},{"deep":0}]`)) // long ref chain back into a cycle
	f.Add([]byte(`[[1,1],[2,2],[3,3],[4,4],[5,5],[6,6],[7,7],[8,8],"leaf"]`))            // forward-branching DAG (exponential without memo/budget)
	f.Add([]byte("[\"\xff\xfe invalid utf8\"]"))                                         // invalid UTF-8 string
	f.Add([]byte(`[{"a":1`))                                                             // truncated frame
	f.Add([]byte(`5`))                                                                   // scalar root
	f.Add([]byte(`null`))
	f.Add([]byte(``))

	f.Fuzz(func(t *testing.T, raw []byte) {
		got, err := FromNuxtData(raw, fuzzCap)
		// Contract: never panic (the harness catches a panic as a failure), always
		// return. On error the result must claim nothing.
		if err != nil {
			if got.Decoded {
				t.Fatalf("Decoded=true alongside a non-nil error: %v", err)
			}
			return
		}
		assertBoundedFields(t, got)
	})
}

// FuzzFromFlight throws arbitrary row slices at the RSC flight parser with the
// same safety contract. Seeds cover truncated/garbage frames, deeply nested
// element tuples, invalid UTF-8, and the element kinds.
func FuzzFromFlight(f *testing.F) {
	f.Add(`1:["$","title",null,{"children":"t"}]`)
	f.Add(`2:["$","meta",null,{"name":"description","content":"d"}]`)
	f.Add(`3:["$","link",null,{"rel":"canonical","href":"https://e.com/c"}]`)
	f.Add(`4:["$","script",null,{"type":"application/ld+json","dangerouslySetInnerHTML":{"__html":"{}"}}]`)
	f.Add(`5:["$","unknown",null,{}]`)
	f.Add(`not-a-frame`)
	f.Add(`:`)
	f.Add(`7:`)
	f.Add(`8:not,json[`)
	f.Add(`9:["$",["$",["$",["$","title",null,{"children":"deep"}]]]]`) // nested tuples
	f.Add("a:[\"\xff invalid utf8\"]")
	f.Add(``)

	f.Fuzz(func(t *testing.T, row string) {
		// The production caller harvests many rows; fuzz a single-row slice (the
		// per-row parser is where the parsing risk lives).
		got, err := FromFlight([]string{row}, fuzzCap)
		if err != nil {
			if got.Decoded {
				t.Fatalf("Decoded=true alongside a non-nil error: %v", err)
			}
			return
		}
		assertBoundedFields(t, got)
	})
}

// assertBoundedFields enforces the bounded-output invariant shared by both fuzz
// targets: recovered strings must be valid UTF-8 (we never emit raw garbage
// bytes downstream into a SQLite TEXT column) and the candidate count must be
// bounded (a decoder that emitted one candidate per array entry on a huge input
// would be an unbounded-output bug).
func assertBoundedFields(t *testing.T, got Fields) {
	t.Helper()
	if !utf8.ValidString(got.Title) {
		t.Fatalf("recovered Title is not valid UTF-8: %q", got.Title)
	}
	if !utf8.ValidString(got.MetaDescription) {
		t.Fatalf("recovered MetaDescription is not valid UTF-8: %q", got.MetaDescription)
	}
	if !utf8.ValidString(got.Canonical) {
		t.Fatalf("recovered Canonical is not valid UTF-8: %q", got.Canonical)
	}
	if len(got.BodyTextCandidates) > maxProseCandidates {
		t.Fatalf("BodyTextCandidates count %d exceeds the maxProseCandidates bound %d",
			len(got.BodyTextCandidates), maxProseCandidates)
	}
	for _, c := range got.BodyTextCandidates {
		if !utf8.ValidString(c) {
			t.Fatalf("recovered prose candidate is not valid UTF-8: %q", c)
		}
	}
	if len(got.JSONLD) > maxJSONLDBlocks {
		t.Fatalf("JSONLD block count %d exceeds the maxJSONLDBlocks bound %d",
			len(got.JSONLD), maxJSONLDBlocks)
	}
}
