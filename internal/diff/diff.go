package diff

import (
	"strconv"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/model"
)

// Compare diffs a new snapshot against the latest stored snapshot (old) and
// returns the per-field model.Change records. A zero old snapshot (old.ID == 0
// AND empty content hash) is treated as a baseline first fetch and yields no
// changes. Content body changes are classified cosmetic/substantive via SimHash.
func Compare(new, old model.Snapshot, simhashThreshold int, now time.Time) []model.Change {
	if old.ID == 0 && old.ContentSHA256 == "" {
		return nil
	}

	var out []model.Change
	add := func(field, oldV, newV string, class model.ChangeClass) {
		out = append(out, model.Change{
			URLID:       new.URLID,
			SnapshotID:  new.ID,
			Field:       field,
			OldValue:    oldV,
			NewValue:    newV,
			ChangeClass: class,
			DetectedAt:  now,
		})
	}

	// Scalar string fields.
	type sf struct {
		field string
		oldV  string
		newV  string
	}
	strFields := []sf{
		{"title", old.Title, new.Title},
		{"meta_description", old.MetaDescription, new.MetaDescription},
		{"meta_robots", old.MetaRobots, new.MetaRobots},
		{"x_robots_tag", old.XRobotsTag, new.XRobotsTag},
		{"canonical", old.Canonical, new.Canonical},
		// canonical_type is intentionally NOT diffed: extract hard-codes
		// snap.CanonicalType = "link" with no other write path, so old==new always
		// holds in production. Diffing it would be a dead branch, and because
		// severityForField has no canonical_type case any emitted event would
		// mis-route to SeverityInfo. Re-add this field here only once extract
		// actually classifies canonical_type AND severityForField buckets it.
		{"hreflang", old.Hreflang, new.Hreflang},
		{"headings", old.Headings, new.Headings},
		{"schema_types", old.SchemaTypes, new.SchemaTypes},
		{"indexability_reason", old.IndexabilityReason, new.IndexabilityReason},
		{"redirect_chain", old.RedirectChain, new.RedirectChain},
		// render_mode (A8): a flip in how a page delivers its SEO content (e.g.
		// hydrated -> client_shell) is a substantive monitoring signal, recorded as
		// change history. RenderMode is a typed string, so both sides are stringified
		// into the generic Change values. It routes QUIET (severityForField has no
		// render_mode case -> info default): the needs_rendering rule carries the
		// alert; this field is history-only. extraction_source is provenance, NOT a
		// signal — like canonical_type it is deliberately NOT diffed (no
		// severityForField case, would be info-tier noise).
		{"render_mode", string(old.RenderMode), string(new.RenderMode)},
	}
	for _, f := range strFields {
		if f.oldV != f.newV {
			add(f.field, f.oldV, f.newV, model.ChangeSubstantive)
		}
	}

	// Boolean / numeric fields rendered as strings.
	if old.Indexable != new.Indexable {
		add("indexable", strconv.FormatBool(old.Indexable), strconv.FormatBool(new.Indexable), model.ChangeSubstantive)
	}
	if old.HTTPStatus != new.HTTPStatus {
		add("http_status", strconv.Itoa(old.HTTPStatus), strconv.Itoa(new.HTTPStatus), model.ChangeSubstantive)
	}
	if old.InternalLinkCount != new.InternalLinkCount {
		add("internal_link_count", strconv.Itoa(old.InternalLinkCount), strconv.Itoa(new.InternalLinkCount), model.ChangeSubstantive)
	}
	if old.WordCount != new.WordCount {
		add("word_count", strconv.Itoa(old.WordCount), strconv.Itoa(new.WordCount), model.ChangeCosmetic)
	}
	// Previously-dormant signals (A5): the rules engine owns their alerting
	// (external_link_spike / image_alt_regression / image_alt_missing), so here
	// they are recorded as cosmetic history only — like word_count, the scheduler's
	// alert-ingest loop skips cosmetic changes and they drive no adaptive-cadence
	// shrink. The numbers stay visible to `rabbot report` / `summarize_changes`.
	if old.ImageCount != new.ImageCount {
		add("image_count", strconv.Itoa(old.ImageCount), strconv.Itoa(new.ImageCount), model.ChangeCosmetic)
	}
	if old.MissingAltCount != new.MissingAltCount {
		add("missing_alt_count", strconv.Itoa(old.MissingAltCount), strconv.Itoa(new.MissingAltCount), model.ChangeCosmetic)
	}
	if old.ExternalLinkCount != new.ExternalLinkCount {
		add("external_link_count", strconv.Itoa(old.ExternalLinkCount), strconv.Itoa(new.ExternalLinkCount), model.ChangeCosmetic)
	}

	// Main-content body change via exact hash, classified by SimHash distance.
	if old.ContentSHA256 != new.ContentSHA256 {
		// A zero SimHash on either side means "unknown" (e.g. empty body or a
		// snapshot taken before SimHash was computed). HammingDistance against 0
		// would compare against an all-zero fingerprint and could misclassify a
		// real change as cosmetic churn, so treat unknown as substantive.
		var class model.ChangeClass
		if old.ContentSimhash == 0 || new.ContentSimhash == 0 {
			class = model.ChangeSubstantive
		} else {
			class = ClassifyContentChange(old.ContentSimhash, new.ContentSimhash, simhashThreshold)
		}
		add("content", old.ContentSHA256, new.ContentSHA256, class)
	}

	return out
}
