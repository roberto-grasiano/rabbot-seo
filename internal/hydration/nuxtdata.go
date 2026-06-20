package hydration

import (
	"encoding/json"
	"strings"
)

// FromNuxtData decodes a Nuxt 3 __NUXT_DATA__ "devalue" payload and recovers SEO
// signals. The devalue wire format is a flat JSON array: the value at index 0 is
// the root, and any integer found inside an object or array is an INDEX
// REFERENCE into the same array (so the structure is reconstructed by following
// indexes). Certain entries are tagged wrappers — a 2-element array whose first
// element is a string tag like "Date"/"Set"/"Map"/"NaN" — which this bounded
// decoder treats conservatively (it recovers the inner value for Set/Map and
// otherwise ignores the tag, never fabricating a typed value).
//
// The resolver carries a visited-set and a depth cap so cyclic references (index
// 0 -> 1 -> 0) terminate instead of looping forever, and never panics on an
// out-of-range index. A non-array root, malformed JSON, scalar, or empty array
// is degenerate/erroneous per the contract: a malformed payload returns an
// error; an empty array returns Decoded=false with no error. Over-cap input is
// skipped with Truncated=true.
func FromNuxtData(raw []byte, maxBytes int) (Fields, error) {
	var f Fields
	if overCap(len(raw), maxBytes) {
		f.Truncated = true
		return f, nil
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return f, jsonSyntaxError("empty devalue payload")
	}

	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil {
		// A non-array root (object/scalar) is not a devalue payload — error per
		// the contract (callers distinguish "couldn't decode" from "nothing to
		// recover"). json.Unmarshal already errors on truncated/invalid JSON.
		return f, err
	}
	if len(arr) == 0 {
		// Empty array: structurally valid but carries nothing — degenerate, not
		// an error.
		return f, nil
	}

	r := &nuxtResolver{
		arr:    arr,
		memo:   make(map[int]any, len(arr)),
		budget: &nodeBudget{n: 0},
	}
	root := r.resolve(0, 0, map[int]struct{}{})
	if !nonDegenerate(root) {
		return f, nil
	}
	f.Decoded = true

	if obj, ok := root.(map[string]any); ok {
		harvestHeadFields(obj, &f)
	}
	sink := newProseSink()
	walkProse(root, "", 0, &nodeBudget{n: 0}, sink)
	f.BodyTextCandidates = sink.sorted()
	return f, nil
}

// nuxtResolver dereferences devalue index references into a plain Go value tree.
// It carries a memo cache and a node budget so resolution is bounded regardless
// of the payload's reference fan-out (see resolve).
type nuxtResolver struct {
	arr    []json.RawMessage
	memo   map[int]any // index -> materialized value, so each index resolves once
	budget *nodeBudget // total work guard, shared across the whole resolution
}

// resolve materializes the value at array index idx, following nested index
// references.
//
// Devalue is a DAG by design: a value (e.g. a shared object) is emitted once and
// referenced from many places. The visited-set holds only the indexes currently
// on the resolution stack so a true cycle (an index that references itself up the
// stack) returns nil instead of recursing forever — but it is deleted on return,
// so it does NOT stop a forward-branching DAG where the same index is reachable
// via many edges. Without further bounding, a payload shaped [[1,1],[2,2],…]
// re-materializes each shared child once per incoming edge: 2^N resolutions on a
// sub-300-byte input — an exponential hot-path DoS that the depth cap alone does
// not stop (depth stays ~N while work is 2^N).
//
// Two guards bound the work:
//   - memo caches each index's materialized value, so a shared index is resolved
//     at most once (correct for a DAG and the primary fix for the blowup); and
//   - budget caps total resolve/expand calls at maxWalkNodes, matching the
//     walkProse/walkFlight node-budget pattern, as defense-in-depth against any
//     pathological fan-out within a single node.
//
// depth remains as a belt-and-braces nesting bound.
func (r *nuxtResolver) resolve(idx, depth int, visited map[int]struct{}) any {
	if depth > maxWalkDepth {
		return nil
	}
	r.budget.n++
	if r.budget.n > maxWalkNodes {
		return nil
	}
	if idx < 0 || idx >= len(r.arr) {
		// Out-of-range reference: the payload is internally inconsistent. Return
		// nil rather than panicking or fabricating a value.
		return nil
	}
	if v, done := r.memo[idx]; done {
		// Already materialized via another edge — a DAG share, not a cycle.
		return v
	}
	if _, on := visited[idx]; on {
		// Cycle: this index is already being resolved up the stack. Do NOT memoize
		// nil here — the index is genuinely resolved (to a real value) when reached
		// off the stack; only this back-edge is nil.
		return nil
	}
	visited[idx] = struct{}{}
	defer delete(visited, idx)

	var node any
	if err := json.Unmarshal(r.arr[idx], &node); err != nil {
		return nil
	}
	out := r.expand(node, depth, visited)
	r.memo[idx] = out
	return out
}

// expand walks a decoded node, replacing integer index references with their
// resolved values. Strings/bools/null pass through; floats that are whole
// numbers within array bounds are treated as references (devalue encodes refs as
// JSON integers, which json.Unmarshal yields as float64). Tagged wrappers
// (["Set", idx] / ["Map", ...]) are unwrapped conservatively.
func (r *nuxtResolver) expand(node any, depth int, visited map[int]struct{}) any {
	if depth > maxWalkDepth {
		return nil
	}
	r.budget.n++
	if r.budget.n > maxWalkNodes {
		return nil
	}
	switch t := node.(type) {
	case float64:
		// An integer is an index reference; a non-integer float is a literal
		// number (devalue does not use fractional refs).
		if ref, ok := asIndex(t, len(r.arr)); ok {
			return r.resolve(ref, depth+1, visited)
		}
		return t
	case []any:
		// Tagged wrapper? ["Date", idx], ["Set", a, b], ["Map", k, v, ...],
		// ["NaN"], etc. The first element being a known string tag marks it.
		if tag, rest, ok := taggedWrapper(t); ok {
			return r.expandTagged(tag, rest, depth, visited)
		}
		out := make([]any, 0, len(t))
		for _, el := range t {
			out = append(out, r.expand(el, depth+1, visited))
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, v := range t {
			out[k] = r.expand(v, depth+1, visited)
		}
		return out
	default:
		// string, bool, nil — literal.
		return t
	}
}

// expandTagged handles devalue's tagged-wrapper entries. Only the value-bearing
// tags (Set/Map) contribute recoverable content; everything else (Date, NaN,
// Infinity, BigInt, RegExp, …) is intentionally dropped — we never fabricate a
// typed value, and these carry no SEO prose.
func (r *nuxtResolver) expandTagged(tag string, rest []any, depth int, visited map[int]struct{}) any {
	switch tag {
	case "Set":
		out := make([]any, 0, len(rest))
		for _, el := range rest {
			out = append(out, r.expand(el, depth+1, visited))
		}
		return out
	case "Map":
		// rest is a flat key,value,key,value list of references; recover values
		// into a slice (keys are rarely prose; values may be).
		out := make([]any, 0, len(rest))
		for _, el := range rest {
			out = append(out, r.expand(el, depth+1, visited))
		}
		return out
	default:
		return nil
	}
}

// taggedWrapper reports whether arr is a devalue tagged wrapper ["Tag", ...] and
// returns the tag plus the remaining elements. Only a known string tag in the
// first position qualifies, so an ordinary array whose first element happens to
// be a string (common!) is NOT misread as a wrapper.
func taggedWrapper(arr []any) (tag string, rest []any, ok bool) {
	if len(arr) == 0 {
		return "", nil, false
	}
	s, isStr := arr[0].(string)
	if !isStr {
		return "", nil, false
	}
	switch s {
	case "Date", "Set", "Map", "NaN", "Infinity", "-Infinity", "BigInt", "RegExp", "undefined", "null":
		return s, arr[1:], true
	default:
		return "", nil, false
	}
}

// asIndex reports whether f is a whole number usable as an array index in
// [0,length). Devalue index references are JSON integers; json.Unmarshal yields
// them as float64, so we check integrality and range here.
func asIndex(f float64, length int) (int, bool) {
	i := int(f)
	if float64(i) != f {
		return 0, false // fractional — a literal number, not a ref
	}
	if i < 0 || i >= length {
		return 0, false
	}
	return i, true
}

// jsonSyntaxError builds a small error value for the malformed-input contract
// without dragging in fmt; it mirrors the closed-error-set discipline elsewhere
// in the codebase (callers only check err != nil for these decoders).
func jsonSyntaxError(msg string) error {
	return &decodeError{msg: msg}
}

type decodeError struct{ msg string }

func (e *decodeError) Error() string { return "hydration: " + e.msg }
