package rules

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/roberto-grasiano/rabbot-seo/internal/model"
)

// Dormant-signal thresholds (A5). All POLICY constants, hard-coded per the
// broken_links_spike precedent (default_rules.go) — per-rule tunability is the F4
// fast-follow, deliberately not a config surface today.
const (
	// externalLinkSpikeAbsFloor is the minimum absolute jump in external links
	// before external_link_spike considers firing. The floor stops a 1→2 link
	// change from reading as a "2× spike".
	externalLinkSpikeAbsFloor = 10
	// externalLinkSpikeFactor is the multiplicative jump required: new ≥ factor×old.
	// The 2× factor stops routine +10-on-500 editorial churn from paging.
	externalLinkSpikeFactor = 2

	// altMinImageCount is the minimum image count before image_alt_missing fires.
	// Below it, a page's alt coverage is not statistically meaningful (and the
	// gate, checked first, makes the coverage division safe on image-less pages).
	altMinImageCount = 5
	// altCoverageFloor is the alt-coverage ratio below which image_alt_missing
	// fires: (ImageCount − MissingAltCount) / ImageCount. At or above the floor the
	// page passes, so "≥1 missing alt" — true of many real pages — no longer opens
	// an info issue site-wide.
	altCoverageFloor = 0.80
)

// RedirectChainInfo parses a stored redirect chain (a JSON array of hop URLs, as
// the fetcher records it: [requested, hop1, …, final]) and reports its hop depth
// and whether any URL repeats (a within-cap loop).
//
//   - depth is len(chain) − 1, floored at 0 (a single-entry or empty chain is
//     depth 0 — no redirect).
//   - loopURL is the first URL string that appears more than once in the chain,
//     or "" if none repeats.
//   - ok is false when the input is empty or not a JSON array (e.g. "", "null",
//     non-JSON, or a JSON object): callers emit NO finding rather than guess, the
//     same don't-guess stance as h1IssueRule on unparseable headings.
//
// Both internal/rules and internal/scheduler call this helper, so the redirect
// semantics are defined in exactly one place.
func RedirectChainInfo(chainJSON string) (depth int, loopURL string, ok bool) {
	s := strings.TrimSpace(chainJSON)
	// Require a JSON array. This rejects "", "null", non-JSON, and JSON objects in
	// one check, so a nil decode (from "null") is never mistaken for an empty chain.
	if !strings.HasPrefix(s, "[") {
		return 0, "", false
	}
	var chain []string
	if err := json.Unmarshal([]byte(s), &chain); err != nil {
		return 0, "", false
	}
	d := len(chain) - 1
	if d < 0 {
		d = 0
	}
	seen := make(map[string]struct{}, len(chain))
	for _, u := range chain {
		if _, dup := seen[u]; dup {
			return d, u, true
		}
		seen[u] = struct{}{}
	}
	return d, "", true
}

// external_link_spike — external link count jumped sharply (a classic hacked-site
// / injected-link tell). Warning, transition rule. Fires when the absolute jump is
// at least externalLinkSpikeAbsFloor AND the new count is at least
// externalLinkSpikeFactor× the old. Requires a prior snapshot (Old.ID != 0).
type externalLinkSpikeRule struct{}

func (externalLinkSpikeRule) ID() string { return "external_link_spike" }
func (externalLinkSpikeRule) Eval(ctx EvalContext) Finding {
	if ctx.Old.ID == 0 {
		return Finding{RuleID: "external_link_spike"}
	}
	old := ctx.Old.ExternalLinkCount
	nw := ctx.New.ExternalLinkCount
	if nw-old >= externalLinkSpikeAbsFloor && nw >= externalLinkSpikeFactor*old {
		return Finding{RuleID: "external_link_spike", Failed: true, Severity: model.SeverityWarning,
			Detail: oldNewDetailJSON(old, nw)}
	}
	return Finding{RuleID: "external_link_spike"}
}

// image_alt_regression — more images are missing an alt attribute than on the
// prior crawl. Warning, transition rule. Requires a prior snapshot (Old.ID != 0)
// and fires only on an INCREASE, so the one-time MissingAltCount re-baseline from
// the alt="" fix (which can only lower the count) cannot trip it.
type imageAltRegressionRule struct{}

func (imageAltRegressionRule) ID() string { return "image_alt_regression" }
func (imageAltRegressionRule) Eval(ctx EvalContext) Finding {
	if ctx.Old.ID == 0 {
		return Finding{RuleID: "image_alt_regression"}
	}
	old := ctx.Old.MissingAltCount
	nw := ctx.New.MissingAltCount
	if nw > old {
		return Finding{RuleID: "image_alt_regression", Failed: true, Severity: model.SeverityWarning,
			Detail: oldNewDetailJSON(old, nw)}
	}
	return Finding{RuleID: "image_alt_regression"}
}

// image_alt_missing — steady-state alt-coverage hygiene. Info tier (issue-only,
// never paged). Fires when the page has a meaningful number of images
// (ImageCount ≥ altMinImageCount) AND alt coverage is below altCoverageFloor.
// The image-count gate is checked first, so the coverage division is always safe
// (no division by zero on image-less pages) and "≥1 missing alt" on a small page
// does not open an issue site-wide. No first-crawl guard: it is a steady state, so
// it should be visible from the first crawl that observes low coverage.
type imageAltMissingRule struct{}

func (imageAltMissingRule) ID() string { return "image_alt_missing" }
func (imageAltMissingRule) Eval(ctx EvalContext) Finding {
	images := ctx.New.ImageCount
	if images < altMinImageCount {
		return Finding{RuleID: "image_alt_missing"}
	}
	missing := ctx.New.MissingAltCount
	coverage := float64(images-missing) / float64(images)
	if coverage < altCoverageFloor {
		return Finding{RuleID: "image_alt_missing", Failed: true, Severity: model.SeverityInfo,
			Detail: `{"images":` + strconv.Itoa(images) + `,"missing":` + strconv.Itoa(missing) + `}`}
	}
	return Finding{RuleID: "image_alt_missing"}
}

// redirect_chain_growth — the redirect chain got longer (more hops) than the prior
// crawl. Warning, transition rule on parsed hop depth. Requires a prior snapshot
// (Old.ID != 0) and that BOTH chains parse. It yields to redirect_loop: if the new
// chain contains a loop, this rule stays silent so one root cause never
// double-pages (the loop is the critical finding).
type redirectChainGrowthRule struct{}

func (redirectChainGrowthRule) ID() string { return "redirect_chain_growth" }
func (redirectChainGrowthRule) Eval(ctx EvalContext) Finding {
	if ctx.Old.ID == 0 {
		return Finding{RuleID: "redirect_chain_growth"}
	}
	oldDepth, _, oldOK := RedirectChainInfo(ctx.Old.RedirectChain)
	newDepth, newLoop, newOK := RedirectChainInfo(ctx.New.RedirectChain)
	if !oldOK || !newOK {
		return Finding{RuleID: "redirect_chain_growth"} // don't guess on unparseable chains
	}
	// A growing chain that also loops is owned by redirect_loop (critical); stay
	// silent here so the same root cause does not page twice.
	if newLoop != "" {
		return Finding{RuleID: "redirect_chain_growth"}
	}
	if newDepth > oldDepth {
		return Finding{RuleID: "redirect_chain_growth", Failed: true, Severity: model.SeverityWarning,
			Detail: oldNewDetailJSON(oldDepth, newDepth)}
	}
	return Finding{RuleID: "redirect_chain_growth"}
}

// redirect_loop — a URL repeats in the new redirect chain (an A→B→A revisit that
// ultimately resolved within the redirect cap). Critical, steady-state rule on the
// new chain alone (no Old.ID guard). A chain that EXHAUSTS the redirect cap is
// classified FetchUnreachable upstream and never snapshotted, so it already alerts
// operationally as monitoring_unreachable; this rule catches the within-cap loop
// that silently burns crawl budget without tripping the fetch classifier. No
// finding on an empty/unparseable chain (don't guess).
type redirectLoopRule struct{}

func (redirectLoopRule) ID() string { return "redirect_loop" }
func (redirectLoopRule) Eval(ctx EvalContext) Finding {
	depth, loopURL, ok := RedirectChainInfo(ctx.New.RedirectChain)
	if !ok {
		return Finding{RuleID: "redirect_loop"}
	}
	if loopURL != "" {
		return Finding{RuleID: "redirect_loop", Failed: true, Severity: model.SeverityCritical,
			Detail: `{"repeated":` + strconv.Quote(loopURL) + `,"depth":` + strconv.Itoa(depth) + `}`}
	}
	return Finding{RuleID: "redirect_loop"}
}

// oldNewDetailJSON builds the shared {"old":N,"new":M} detail payload used by the
// count/depth transition rules, following the existing detail convention
// (default_rules.go's broken_links_spike).
func oldNewDetailJSON(old, nw int) string {
	return `{"old":` + strconv.Itoa(old) + `,"new":` + strconv.Itoa(nw) + `}`
}
