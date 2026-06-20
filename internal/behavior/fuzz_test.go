package behavior

import (
	"testing"

	"github.com/roberto-grasiano/rabbot-seo/internal/diff"
	"github.com/roberto-grasiano/rabbot-seo/internal/model"
	"github.com/roberto-grasiano/rabbot-seo/internal/rules"
)

// transitionRules are the rule IDs that MUST NOT fire when Old.ID == 0 (no real
// prior baseline). This mirrors the engine contract's "TRANSITION GUARDS"
// (rule-level Old.ID != 0): they quietly pass on a first crawl. The 4xx arm of
// status_regression is guarded too, but its 5xx arm fires with no baseline, so
// status_regression is NOT in this set (a first-crawl 5xx legitimately fires).
// rich_result_*'s CRITICAL severity is the transition; the rule can still fire at
// WARNING on a first crawl, so they are not pure "must-not-fire" rules either.
var transitionRules = map[string]bool{
	"indexability_flip":     true,
	"broken_links_spike":    true,
	"external_link_spike":   true,
	"image_alt_regression":  true,
	"redirect_chain_growth": true,
}

// decodeSnapshot deterministically maps a byte budget to a model.Snapshot,
// drawing each field from the fuzz input so the fuzzer can explore the field
// space (hostile strings into JSON-LD/Headings/RedirectChain/robots, arbitrary
// status/counts/simhash). It never errors: a short input just yields zero-ish
// fields. This keeps the corpus interpretation total and the harness panic-free
// by construction (any panic is then the ENGINE's).
func decodeSnapshot(d *byteDrainer) model.Snapshot {
	return model.Snapshot{
		ID:                 d.int64(),
		URLID:              d.int64(),
		HTTPStatus:         d.intRange(0, 600),
		RedirectChain:      d.str(),
		Title:              d.str(),
		MetaDescription:    d.str(),
		MetaRobots:         d.str(),
		XRobotsTag:         d.str(),
		Canonical:          d.str(),
		Hreflang:           d.str(),
		Headings:           d.str(),
		WordCount:          d.intRange(0, 100000),
		ContentSHA256:      d.str(),
		ContentSimhash:     d.uint64(),
		JSONLD:             d.str(),
		JSONLDInvalidCount: d.intRange(0, 50),
		SchemaTypes:        d.str(),
		InternalLinkCount:  d.intRange(0, 100000),
		ExternalLinkCount:  d.intRange(0, 100000),
		ImageCount:         d.intRange(0, 100000),
		MissingAltCount:    d.intRange(0, 100000),
		Indexable:          d.boolean(),
		IndexabilityReason: d.str(),
		RenderMode:         d.renderMode(),
	}
}

// FuzzSnapshotPairToFindings fuzzes an (old, new) Snapshot pair through the full
// diff.Compare -> rules.Eval pipeline and asserts the load-bearing invariants:
//
//  1. NO PANIC on any input (hostile JSON-LD/Headings/RedirectChain, arbitrary
//     counts, garbage robots tokens, zero/huge simhash).
//  2. Transition rules never fire when Old.ID == 0 (no baseline).
//  3. Every emitted Change carries the fuzzed New.URLID (diff.Compare copies it).
//  4. Every failing Finding has a non-empty RuleID and a severity in the known set.
func FuzzSnapshotPairToFindings(f *testing.F) {
	// Seed corpus: a few shaped inputs so the fuzzer starts from interesting states.
	f.Add([]byte{})                                   // empty -> two zero snapshots
	f.Add([]byte("\x01\x01\x01\x01"))                 // tiny
	f.Add([]byte(`[{"@type":"Product","name":"x"}]`)) // JSON-LD-ish bytes
	f.Add([]byte(`["a","b","a"]noindex,follow`))      // redirect-loop + robots-ish

	f.Fuzz(func(t *testing.T, data []byte) {
		// Split the byte budget into two halves: one drives old, one drives new.
		half := len(data) / 2
		oldD := &byteDrainer{b: data[:half]}
		newD := &byteDrainer{b: data[half:]}
		old := decodeSnapshot(oldD)
		nw := decodeSnapshot(newD)

		// (1) must not panic.
		changes := diff.Compare(nw, old, diff.DefaultSimhashThreshold, FixedNow)
		ec := rules.EvalContext{
			URLID:      nw.URLID,
			Importance: 0.5,
			New:        nw,
			Old:        old,
			Changes:    changes,
			Truncated:  newD.boolean(),
		}
		var findings []rules.Finding
		for _, r := range rules.DefaultRuleSet() {
			f := r.Eval(ec) // must not panic on any field content
			if f.Failed {
				findings = append(findings, f)
			}
		}

		// (3) every Change carries new.URLID.
		for _, c := range changes {
			if c.URLID != nw.URLID {
				t.Fatalf("Change.URLID=%d, want new.URLID=%d (field %q)", c.URLID, nw.URLID, c.Field)
			}
		}

		// (2) + (4): invariants on findings.
		for _, fd := range findings {
			if fd.RuleID == "" {
				t.Fatalf("failing finding has empty RuleID: %+v", fd)
			}
			switch fd.Severity {
			case model.SeverityCritical, model.SeverityWarning, model.SeverityInfo:
			default:
				t.Fatalf("finding %q has unknown severity %q", fd.RuleID, fd.Severity)
			}
			if old.ID == 0 && transitionRules[fd.RuleID] {
				t.Fatalf("transition rule %q fired with Old.ID==0 (no baseline): %+v", fd.RuleID, fd)
			}
		}
	})
}

// FuzzNoopYieldsNoChange asserts the two no-op invariants over fuzzed snapshots:
//
//   - A true baseline (Old.ID==0 AND Old.ContentSHA256=="") yields ZERO changes
//     for ANY new snapshot (the diff.Compare baseline sentinel).
//   - Comparing a real snapshot against ITSELF (same struct, Old.ID!=0) yields
//     ZERO changes (a no-op recheck must produce no diff churn).
//
// Both must hold for arbitrary field content and must never panic.
func FuzzNoopYieldsNoChange(f *testing.F) {
	f.Add([]byte("seed"))
	f.Add([]byte(`{"h1":["x"]}` + "\x00" + `["a","b"]`))

	f.Fuzz(func(t *testing.T, data []byte) {
		d := &byteDrainer{b: data}
		s := decodeSnapshot(d)

		// Baseline sentinel: any new vs a true-baseline old => no changes.
		baseline := model.Snapshot{ID: 0, ContentSHA256: ""}
		if got := diff.Compare(s, baseline, diff.DefaultSimhashThreshold, FixedNow); len(got) != 0 {
			t.Fatalf("baseline sentinel produced %d changes, want 0: %+v", len(got), got)
		}

		// Self-compare: a snapshot vs itself with a real ID => no changes. Force a
		// real prior ID and a non-empty content hash so it is NOT the baseline path.
		self := s
		if self.ID == 0 {
			self.ID = 1
		}
		if self.ContentSHA256 == "" {
			self.ContentSHA256 = "nonempty"
		}
		if got := diff.Compare(self, self, diff.DefaultSimhashThreshold, FixedNow); len(got) != 0 {
			t.Fatalf("self-compare produced %d changes, want 0 (no-op recheck): %+v", len(got), got)
		}
	})
}

// byteDrainer hands out deterministic field values from a byte slice, never
// running out (it returns zero-ish values once drained). It is the fuzz-input
// decoder; total by construction so the harness itself cannot panic.
type byteDrainer struct {
	b   []byte
	pos int
}

func (d *byteDrainer) next() byte {
	if d.pos >= len(d.b) {
		return 0
	}
	v := d.b[d.pos]
	d.pos++
	return v
}

func (d *byteDrainer) uint64() uint64 {
	var v uint64
	for i := 0; i < 8; i++ {
		v = v<<8 | uint64(d.next())
	}
	return v
}

func (d *byteDrainer) int64() int64 { return int64(d.uint64()) }

func (d *byteDrainer) intRange(lo, hi int) int {
	if hi <= lo {
		return lo
	}
	span := uint64(hi - lo + 1)
	return lo + int(d.uint64()%span)
}

func (d *byteDrainer) boolean() bool { return d.next()&1 == 1 }

// str reads a length-prefixed run of bytes as a string. The length is one byte
// (0..255) so a single fuzz byte controls field length; the content is the next
// run. This lets the fuzzer build hostile JSON-LD / robots / redirect-chain
// strings of varied length without a structured encoder.
func (d *byteDrainer) str() string {
	n := int(d.next())
	if n == 0 || d.pos >= len(d.b) {
		return ""
	}
	if d.pos+n > len(d.b) {
		n = len(d.b) - d.pos
	}
	s := string(d.b[d.pos : d.pos+n])
	d.pos += n
	return s
}

// renderMode maps a fuzz byte to one of the model.RenderMode values (including
// the empty/unknown zero value), so the fuzzer exercises every needs_rendering arm.
func (d *byteDrainer) renderMode() model.RenderMode {
	modes := []model.RenderMode{
		model.RenderServerRendered, model.RenderHydrated, model.RenderHeadOnlyShell,
		model.RenderClientShell, model.RenderUnknown, model.RenderMode(""),
		model.RenderMode("garbage-mode"), // a value outside the enum: must pass needs_rendering
	}
	return modes[int(d.next())%len(modes)]
}
