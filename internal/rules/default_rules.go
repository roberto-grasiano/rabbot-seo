package rules

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/roberto-grasiano/rabbot-seo/internal/model"
	"github.com/roberto-grasiano/rabbot-seo/internal/robotsmeta"
)

// status_regression — the page's HTTP status is an error. Severity tiers:
//
//   - Any 5xx (server error) => CRITICAL, regardless of baseline.
//   - 2xx/3xx -> 4xx (a real regression off a prior healthy status) => CRITICAL.
//   - Born-4xx (4xx on the very FIRST crawl, Old.ID == 0) => WARNING. A URL that
//     is 4xx from its first crawl has no 2xx/3xx baseline to regress from, so the
//     critical arm could never fire and the broken page was invisible forever.
//     Mirroring the rich_result first-crawl idiom, we open at WARNING so the issue
//     exists; ProcessFetch's first-crawl guard suppresses the Slack page on crawl 1.
//   - Steady 4xx (Old & New both >= 400) => WARNING, so the issue stays OPEN across
//     rechecks and auto-CLOSES via the engine lifecycle when the page recovers
//     (e.g. 404 -> 200). A 4xx -> 5xx escalates to CRITICAL via the 5xx arm above.
//
// Any non-error status (2xx/3xx) PASSES, closing an open issue on recovery.
type statusRegressionRule struct{}

func (statusRegressionRule) ID() string { return "status_regression" }
func (statusRegressionRule) Eval(ctx EvalContext) Finding {
	s := ctx.New.HTTPStatus
	open := func(sev model.Severity) Finding {
		return Finding{RuleID: "status_regression", Failed: true, Severity: sev,
			Detail: `{"http_status":` + strconv.Itoa(s) + `}`}
	}
	switch {
	case s >= 500:
		// Any server error is critical, regardless of baseline (incl. 4xx -> 5xx).
		return open(model.SeverityCritical)
	case s >= 400:
		switch {
		case ctx.Old.ID != 0 && ctx.Old.HTTPStatus >= 200 && ctx.Old.HTTPStatus < 400:
			// Regression off a prior healthy (2xx/3xx) status.
			return open(model.SeverityCritical)
		case ctx.Old.ID == 0 || ctx.Old.HTTPStatus >= 400:
			// Born-4xx (no baseline) or steady 4xx (both >= 400): keep the issue open
			// at warning so it auto-closes on recovery; never a critical page.
			return open(model.SeverityWarning)
		}
	}
	return Finding{RuleID: "status_regression"}
}

// indexability_flip — page *flipped* from indexable to non-indexable. Critical.
// This is a transition rule, so it requires a prior INDEXABLE baseline: it fires
// only when a real prior snapshot existed (Old.ID != 0), that snapshot was
// indexable, and the new snapshot is not. On a genuine first crawl Old is the
// zero Snapshot (Old.ID == 0), and a page that was already non-indexable on the
// prior crawl has not flipped — both pass here, mirroring brokenLinksSpikeRule's
// Old.ID == 0 guard. A page that is noindex from its very first crawl is a steady
// state, not a regression, and must not open a spurious CRITICAL issue.
type indexabilityFlipRule struct{}

func (indexabilityFlipRule) ID() string { return "indexability_flip" }
func (indexabilityFlipRule) Eval(ctx EvalContext) Finding {
	if ctx.Old.ID != 0 && ctx.Old.Indexable && !ctx.New.Indexable {
		return Finding{RuleID: "indexability_flip", Failed: true, Severity: model.SeverityCritical,
			Detail: `{"reason":` + strconv.Quote(ctx.New.IndexabilityReason) + `}`}
	}
	return Finding{RuleID: "indexability_flip"}
}

// meta_robots_noindex — meta robots / X-Robots-Tag carries a real noindex
// directive. Critical. Only `noindex` (and `none`, which Google defines as
// `noindex, nofollow`) controls indexability; `nofollow` governs link equity, not
// indexability, so `index,nofollow` is NOT a regression here. Detection
// DELEGATES to the shared robotsmeta.IsNoindex parser — the SAME call the
// extractor's indexability verdict uses — so this alert and the verdict can never
// drift on tokenization, the `none` shorthand, or a `googlebot:` user-agent
// prefix. (A substring like "noindex" inside "noindexible" still cannot
// false-match: robotsmeta matches token-exact.)
type metaRobotsNoindexRule struct{}

func (metaRobotsNoindexRule) ID() string { return "meta_robots_noindex" }
func (metaRobotsNoindexRule) Eval(ctx EvalContext) Finding {
	if robotsmeta.IsNoindex(ctx.New.MetaRobots) || robotsmeta.IsNoindex(ctx.New.XRobotsTag) {
		return Finding{RuleID: "meta_robots_noindex", Failed: true, Severity: model.SeverityCritical,
			Detail: `{"directive":"noindex"}`}
	}
	return Finding{RuleID: "meta_robots_noindex"}
}

// canonical_changed — canonical missing (critical) or changed (critical). Off-page
// detection is left to the indexability verdict (extract).
type canonicalChangedRule struct{}

func (canonicalChangedRule) ID() string { return "canonical_changed" }
func (canonicalChangedRule) Eval(ctx EvalContext) Finding {
	if strings.TrimSpace(ctx.New.Canonical) == "" {
		return Finding{RuleID: "canonical_changed", Failed: true, Severity: model.SeverityCritical,
			Detail: `{"canonical":"missing"}`}
	}
	if c, ok := hasChange(ctx.Changes, "canonical"); ok {
		return Finding{RuleID: "canonical_changed", Failed: true, Severity: model.SeverityCritical,
			Detail: `{"old":` + strconv.Quote(c.OldValue) + `,"new":` + strconv.Quote(c.NewValue) + `}`}
	}
	return Finding{RuleID: "canonical_changed"}
}

// title_changed — title missing (warning) or changed (warning). When the field
// is ABSENT the detail carries an explicit "finding":"absent" marker (and a
// human note) so it never reads as a value diff (#86); the RuleID stays
// title_changed so an existing open issue keeps its key and auto-closes when a
// title reappears (no re-baseline).
type titleChangedRule struct{}

func (titleChangedRule) ID() string { return "title_changed" }
func (titleChangedRule) Eval(ctx EvalContext) Finding {
	if strings.TrimSpace(ctx.New.Title) == "" {
		return Finding{RuleID: "title_changed", Failed: true, Severity: model.SeverityWarning,
			Detail: `{"title":"missing","finding":"absent","note":"field is absent, not a change"}`}
	}
	if c, ok := hasChange(ctx.Changes, "title"); ok {
		return Finding{RuleID: "title_changed", Failed: true, Severity: model.SeverityWarning,
			Detail: `{"old":` + strconv.Quote(c.OldValue) + `,"new":` + strconv.Quote(c.NewValue) + `}`}
	}
	return Finding{RuleID: "title_changed"}
}

// meta_description_changed — missing (warning) or changed (warning). When the
// field is ABSENT the detail carries an explicit "finding":"absent" marker (and
// a human note) so it never reads as a value diff (#86); the RuleID stays
// meta_description_changed so an existing open issue keeps its key and
// auto-closes when a description reappears (no re-baseline).
type metaDescriptionChangedRule struct{}

func (metaDescriptionChangedRule) ID() string { return "meta_description_changed" }
func (metaDescriptionChangedRule) Eval(ctx EvalContext) Finding {
	if strings.TrimSpace(ctx.New.MetaDescription) == "" {
		return Finding{RuleID: "meta_description_changed", Failed: true, Severity: model.SeverityWarning,
			Detail: `{"meta_description":"missing","finding":"absent","note":"field is absent, not a change"}`}
	}
	if _, ok := hasChange(ctx.Changes, "meta_description"); ok {
		return Finding{RuleID: "meta_description_changed", Failed: true, Severity: model.SeverityWarning,
			Detail: `{"meta_description":"changed"}`}
	}
	return Finding{RuleID: "meta_description_changed"}
}

// h1_issue — H1 missing (warning), changed (warning), or multiple (info). The
// Headings field is the structured JSON object {"h1":[...],"h2":[...],...} produced
// by extract. We parse it and count the h1 entries: 0 => missing, >1 => multiple,
// 1 => ok. Multiple H1s are not an SEO error under current Google guidance, so the
// "multiple" sub-finding is INFO (recorded in `rabbot issues`, never pages) while a
// missing H1 — a broken-template signal — stays warning. An empty Headings string
// means the page has not been extracted yet, so we emit no finding (avoids false
// positives); invalid JSON likewise yields no finding.
//
// A real heading rewrite (a "headings" diff) WARNS regardless of H1 count: the
// change check sits ABOVE the count switch so a 2+-H1 page's genuine rewrite is
// not silently downgraded to the INFO "multiple" steady-state finding and lost
// (it would never page). The n>1 INFO "multiple" arm therefore reports only the
// steady-state count, when headings did NOT change. A missing H1 (n==0) is the
// strongest signal and still wins above the change check.
type h1IssueRule struct{}

func (h1IssueRule) ID() string { return "h1_issue" }
func (h1IssueRule) Eval(ctx EvalContext) Finding {
	h := strings.TrimSpace(ctx.New.Headings)
	if h == "" {
		return Finding{RuleID: "h1_issue"} // not extracted yet; no finding
	}
	var headings map[string][]string
	if err := json.Unmarshal([]byte(h), &headings); err != nil {
		return Finding{RuleID: "h1_issue"} // unparseable; don't guess
	}
	n := len(headings["h1"])
	if n == 0 {
		return Finding{RuleID: "h1_issue", Failed: true, Severity: model.SeverityWarning,
			Detail: `{"h1":"missing"}`}
	}
	// A genuine heading rewrite WARNS regardless of H1 count — checked BEFORE the
	// count switch so a 2+-H1 page's real change is not shadowed by the INFO
	// "multiple" arm and silently never paged.
	if _, ok := hasChange(ctx.Changes, "headings"); ok {
		return Finding{RuleID: "h1_issue", Failed: true, Severity: model.SeverityWarning,
			Detail: `{"headings":"changed"}`}
	}
	if n > 1 {
		// Multiple H1s are not an SEO error under current Google guidance, so this is
		// INFO tier (recorded, never pages); reached only when headings did NOT change.
		return Finding{RuleID: "h1_issue", Failed: true, Severity: model.SeverityInfo,
			Detail: `{"h1":"multiple","count":` + strconv.Itoa(n) + `}`}
	}
	return Finding{RuleID: "h1_issue"}
}

// broken_links_spike — internal link count dropped sharply (>30%) vs prior. Warning.
type brokenLinksSpikeRule struct{}

func (brokenLinksSpikeRule) ID() string { return "broken_links_spike" }
func (brokenLinksSpikeRule) Eval(ctx EvalContext) Finding {
	if ctx.Old.ID == 0 || ctx.Old.InternalLinkCount == 0 {
		return Finding{RuleID: "broken_links_spike"}
	}
	drop := float64(ctx.Old.InternalLinkCount-ctx.New.InternalLinkCount) / float64(ctx.Old.InternalLinkCount)
	if drop > 0.30 {
		return Finding{RuleID: "broken_links_spike", Failed: true, Severity: model.SeverityWarning,
			Detail: `{"old":` + strconv.Itoa(ctx.Old.InternalLinkCount) + `,"new":` + strconv.Itoa(ctx.New.InternalLinkCount) + `}`}
	}
	return Finding{RuleID: "broken_links_spike"}
}

// hreflang_invalid — the hreflang set changed vs the prior crawl. Warning.
//
// #16: this rule used to ALSO claim a CRITICAL "invalid hreflang" tier, gated on
// the IndexabilityReason containing "hreflang". That path was DEAD — nothing
// writes a "hreflang" token into IndexabilityReason (see internal/extract), and
// hreflang is stored as a raw lang array with no validity check — so the critical
// could never fire. Per the locked decision ("downgrade the doc now, validation
// is fast-follow") the rule is made HONEST: it fires warning-on-change only and
// does not advertise a critical it cannot reach. Real BCP-47 / reciprocity
// validation is a deferred fast-follow that would (re)introduce a higher-severity
// finding on a genuine signal.
type hreflangInvalidRule struct{}

func (hreflangInvalidRule) ID() string { return "hreflang_invalid" }
func (hreflangInvalidRule) Eval(ctx EvalContext) Finding {
	if _, ok := hasChange(ctx.Changes, "hreflang"); ok {
		return Finding{RuleID: "hreflang_invalid", Failed: true, Severity: model.SeverityWarning,
			Detail: `{"hreflang":"changed"}`}
	}
	return Finding{RuleID: "hreflang_invalid"}
}

// needs_rendering — the page's persisted render_mode (A8) says its SEO content is not
// fully recoverable from the server HTML. Warning (owner decision #8). Two failing
// modes, with DISTINCT detail so the operator knows exactly what monitoring lost:
//   - head_only_shell: the SEO head is server-rendered (title/meta still monitored),
//     but the body is an empty framework root with no hydration payload — body
//     content is NOT monitored.
//   - client_shell: an empty framework root with very low visible words and no
//     hydration payload — nothing beyond the fetch status is monitored; the page
//     likely needs JavaScript to render at all.
//
// PASSES (Failed=false) for every recoverable / steady-state mode — server_rendered,
// hydrated, unknown, and "" (pre-A8 rows). A page recovering to server_rendered or
// hydrated therefore CLOSES the open issue via the engine lifecycle. unknown/"" are
// steady states, not findings, so they must never open a spurious issue. This rule
// reads only the persisted RenderMode field (no diff/Changes dependency), so unlike
// the transition rules it has no Old.ID baseline guard — the first-crawl SUPPRESSION
// of its Slack page is handled by ProcessFetch's first-crawl bridge guard, not here.
type needsRenderingRule struct{}

func (needsRenderingRule) ID() string { return "needs_rendering" }
func (needsRenderingRule) Eval(ctx EvalContext) Finding {
	switch ctx.New.RenderMode {
	case model.RenderHeadOnlyShell:
		return Finding{RuleID: "needs_rendering", Failed: true, Severity: model.SeverityWarning,
			Detail: `{"render_mode":"head_only_shell","monitored":"head_only"}`}
	case model.RenderClientShell:
		return Finding{RuleID: "needs_rendering", Failed: true, Severity: model.SeverityWarning,
			Detail: `{"render_mode":"client_shell","monitored":"fetch_status_only"}`}
	}
	return Finding{RuleID: "needs_rendering"}
}
