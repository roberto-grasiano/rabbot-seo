package diff_test

import (
	"crypto/sha256"
	"encoding/hex"
	"hash/fnv"
	"strings"
	"testing"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/benchcorpus"
	"github.com/roberto-grasiano/rabbot-seo/internal/diff"
	"github.com/roberto-grasiano/rabbot-seo/internal/model"
)

// benchCorpusClass / benchCorpusIndex select one deterministic benchcorpus page
// as the shared substrate for the diff fixtures. Article is the typical page;
// the index is arbitrary but fixed so the derived field values (and therefore
// the bench inputs) are byte-stable across runs and machines.
const (
	benchCorpusClass = benchcorpus.Article
	benchCorpusIndex = 42
)

// contentSHA256 mirrors extract.ContentSHA256 (hex sha256 of the main text). It
// is recomputed here rather than imported so the diff bench has no dependency on
// the extract package — only on benchcorpus (the shared corpus) and diff itself.
func contentSHA256(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// simHash is a small deterministic 64-bit SimHash over the lowercased
// whitespace tokens of text, matching extract.SimHash's shape closely enough to
// produce a realistic NON-ZERO fingerprint for the bench fixtures. The exact
// algorithm need not match extract bit-for-bit: the diff bench only needs two
// distinct non-zero hashes so Compare exercises ClassifyContentChange (the
// SimHash Hamming-distance path) instead of the "either side is zero ->
// substantive" shortcut at diff.go:106. A zero hash on either side would skip
// that classification, so non-zero on both sides is the load-bearing property.
func simHash(text string) uint64 {
	var v [64]int
	for _, tok := range strings.Fields(strings.ToLower(text)) {
		h := fnv.New64a()
		_, _ = h.Write([]byte(tok))
		sum := h.Sum64()
		for i := 0; i < 64; i++ {
			if sum&(1<<uint(i)) != 0 {
				v[i]++
			} else {
				v[i]--
			}
		}
	}
	var out uint64
	for i := 0; i < 64; i++ {
		if v[i] > 0 {
			out |= 1 << uint(i)
		}
	}
	return out
}

// fullSnapshot builds a fully-populated model.Snapshot for the diff bench from
// benchcorpus pages. The salt parameter distinguishes the two arms: the empty
// salt is the "new"/base snapshot, and a non-empty salt produces an "old"
// snapshot that differs in EVERY field Compare reads (so the all_fields_changed
// arm emits one Change per compared field). The content body is drawn from a
// DIFFERENT corpus page per salt (a one-character suffix on a 60 KiB body barely
// perturbs the SimHash; a whole different page differs in many bits -> the
// content change classifies substantive, exercising ClassifyContentChange
// rather than the zero shortcut). id is set non-zero on the "old" side so
// Compare does NOT short-circuit the zero-baseline guard at diff.go:15 — that
// guard is the single easiest tautology trap in this bench (a zero-ID old
// returns nil and the bench would measure an early return, not a diff).
func fullSnapshot(id int64, salt string) model.Snapshot {
	// Distinct corpus page per arm so the content hash AND SimHash differ
	// substantially between new and old (many differing bits -> substantive).
	bodyIndex := benchCorpusIndex
	if salt != "" {
		bodyIndex = benchCorpusIndex + 1000
	}
	body := string(benchcorpus.Page(benchCorpusClass, bodyIndex))
	return model.Snapshot{
		ID:                 id,
		URLID:              1,
		Title:              "Reference Guide — article " + salt,
		MetaDescription:    "A deterministic meta description for the diff bench " + salt,
		MetaRobots:         "index,follow," + salt, // must differ per arm to emit a change
		XRobotsTag:         "",
		Canonical:          benchcorpus.URL(benchCorpusClass, benchCorpusIndex) + salt,
		Hreflang:           "en,en-gb,fr,de,es,x-default" + salt,
		Headings:           "h1:Guide|h2:Section " + salt,
		SchemaTypes:        "Article" + salt,
		IndexabilityReason: "indexable" + salt,
		RedirectChain:      salt,
		RenderMode:         model.RenderMode("server" + salt),
		Indexable:          salt == "", // base is indexable; salted arm flips it
		HTTPStatus:         200 + len(salt),
		InternalLinkCount:  24 + len(salt),
		WordCount:          6400 + len(salt),
		ImageCount:         10 + len(salt),
		MissingAltCount:    5 + len(salt),
		ExternalLinkCount:  3 + len(salt),
		ContentSHA256:      contentSHA256(body),
		ContentSimhash:     simHash(body),
	}
}

// BenchmarkCompare measures the pure-CPU cost of diff.Compare over two
// fully-populated snapshots, in two arms:
//
//   - no_change: two identical snapshots (old.ID non-zero so the zero-baseline
//     guard does not fire) -> Compare walks every field, finds none changed,
//     returns an empty slice. This is the common steady-state recheck cost.
//   - all_fields_changed: every compared field differs (including a substantive
//     content-hash + SimHash delta) -> Compare emits a Change per field.
//
// EXPECTATION (the bench proves it): diffing is negligible next to HTML
// parsing. Compare does only string/int comparisons and a 64-bit popcount; it
// performs ZERO I/O and no HTML parse. Against BenchmarkExtract (two full HTML
// parses per page, internal/extract) these numbers are expected to be orders of
// magnitude smaller — the recheck pipeline's cost lives in parsing, not
// comparison. b.ReportAllocs() records the (tiny) per-op allocation so
// docs/PERFORMANCE.md can state it honestly.
func BenchmarkCompare(b *testing.B) {
	now := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)

	b.Run("no_change", func(b *testing.B) {
		// Identical snapshots; old.ID == 1 (non-zero) so Compare does the full
		// field walk instead of returning nil at the zero-baseline guard.
		newSnap := fullSnapshot(1, "")
		oldSnap := fullSnapshot(1, "")
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			changes := diff.Compare(newSnap, oldSnap, diff.DefaultSimhashThreshold, now)
			if len(changes) != 0 {
				b.Fatalf("no_change arm produced %d changes, want 0", len(changes))
			}
		}
	})

	b.Run("all_fields_changed", func(b *testing.B) {
		// Every compared field differs between new and old.
		newSnap := fullSnapshot(2, "")
		oldSnap := fullSnapshot(1, "x")
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			changes := diff.Compare(newSnap, oldSnap, diff.DefaultSimhashThreshold, now)
			if len(changes) == 0 {
				b.Fatal("all_fields_changed arm produced 0 changes, want > 0")
			}
		}
	})
}

// TestCompareBenchArms is the falsifiability guard for the bench fixtures: it
// asserts the two arms behave as the bench claims, so a future fixture edit that
// silently turned an arm into a tautology (e.g. an all-identical "changed" pair,
// or a zero-ID "no_change" old that short-circuits the guard) fails here rather
// than publishing a meaningless number.
//
// Run: CGO_ENABLED=1 go test -race ./internal/diff/...
func TestCompareBenchArms(t *testing.T) {
	now := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)

	// no_change: identical, non-zero old ID -> exactly zero changes, and the
	// guard must NOT have fired (a fired guard also returns zero, so prove the
	// old side is past the baseline by checking ID != 0).
	newSame := fullSnapshot(1, "")
	oldSame := fullSnapshot(1, "")
	if oldSame.ID == 0 && oldSame.ContentSHA256 == "" {
		t.Fatal("no_change old snapshot would trip the zero-baseline guard at diff.go:15 -> bench measures an early return, not a diff")
	}
	if got := diff.Compare(newSame, oldSame, diff.DefaultSimhashThreshold, now); len(got) != 0 {
		t.Fatalf("no_change arm: got %d changes, want 0: %+v", len(got), got)
	}

	// all_fields_changed: every compared field differs -> a Change per compared
	// field. Assert each compared field name appears exactly once and the
	// content change is classified substantive (non-zero SimHash on both sides,
	// distance > threshold), proving the SimHash path ran, not the zero shortcut.
	newDiff := fullSnapshot(2, "")
	oldDiff := fullSnapshot(1, "x")
	if newDiff.ContentSimhash == 0 || oldDiff.ContentSimhash == 0 {
		t.Fatal("a zero SimHash on either side skips ClassifyContentChange (diff.go:106); the bench would not exercise the Hamming-distance path")
	}
	changes := diff.Compare(newDiff, oldDiff, diff.DefaultSimhashThreshold, now)

	wantFields := []string{
		"title", "meta_description", "meta_robots", "canonical", "hreflang",
		"headings", "schema_types", "indexability_reason", "render_mode",
		"indexable", "http_status", "internal_link_count", "word_count",
		"image_count", "missing_alt_count", "external_link_count", "content",
	}
	seen := map[string]int{}
	for _, c := range changes {
		seen[c.Field]++
	}
	for _, f := range wantFields {
		if seen[f] != 1 {
			t.Errorf("all_fields_changed: field %q changed %d times, want exactly 1", f, seen[f])
		}
	}
	// Content must be substantive (the salted body differs in many bits).
	var contentClass model.ChangeClass
	var foundContent bool
	for _, c := range changes {
		if c.Field == "content" {
			contentClass = c.ChangeClass
			foundContent = true
		}
	}
	if !foundContent {
		t.Fatal("all_fields_changed: no content change emitted")
	}
	if contentClass != model.ChangeSubstantive {
		t.Errorf("content change classified %q, want substantive (the salted bodies differ well beyond the cosmetic threshold)", contentClass)
	}
}
