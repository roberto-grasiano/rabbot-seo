package alerts

import (
	"context"

	"github.com/roberto-grasiano/rabbot-seo/internal/model"
)

// AccessContext carries the latest fetch classification for one URL (§5A).
type AccessContext struct {
	SiteID     int64
	Site       string
	URL        string
	URLID      int64
	FetchClass model.FetchClass
	Detector   string // WAF/challenge vendor on hard_block
	DeepLink   string
}

// operationalChangeType maps a non-ok fetch class to its operational change type.
func operationalChangeType(fc model.FetchClass) (string, bool) {
	switch fc {
	case model.FetchHardBlock, model.FetchSoftBlock:
		return model.ChangeTypeMonitoringBlocked, true
	case model.FetchUnreachable:
		return model.ChangeTypeMonitoringUnreachable, true
	default:
		return "", false
	}
}

// HandleFetchClass implements the §5A access gate. When the latest fetch class is
// not ok, it SUPPRESSES the supplied SEO events and instead opens/maintains a
// distinct operational incident (monitoring_blocked / monitoring_unreachable),
// returning handled=true so the caller skips normal SEO emission. When the class
// is ok, it auto-closes any open operational incidents for this URL and returns
// handled=false so the caller proceeds with the SEO pipeline normally.
//
// Operational incidents route like criticals but are labeled access problems and
// are NEVER folded into the SEO health score.
func (p *Pipeline) HandleFetchClass(ctx context.Context, ac AccessContext, suppressedSEO []Event) (handled bool, err error) {
	ct, isOperational := operationalChangeType(ac.FetchClass)
	if !isOperational {
		// ok: auto-close any open operational incidents for this URL.
		for _, t := range []string{model.ChangeTypeMonitoringBlocked, model.ChangeTypeMonitoringUnreachable} {
			resolveEv := Event{
				SiteID: ac.SiteID, Site: ac.Site, URL: ac.URL,
				ChangeType: t, Severity: model.SeverityCritical, Operational: true,
			}
			if err := p.Resolve(ctx, resolveEv); err != nil {
				return false, err
			}
		}
		return false, nil // SEO pipeline proceeds
	}

	// Non-ok: suppress SEO (suppressedSEO intentionally dropped) and raise/maintain
	// the operational incident. It routes like a critical.
	opEv := Event{
		SiteID:      ac.SiteID,
		Site:        ac.Site,
		URL:         ac.URL,
		URLID:       ac.URLID,
		ChangeType:  ct,
		Severity:    model.SeverityCritical,
		Before:      "ok",
		After:       string(ac.FetchClass),
		Operational: true,
		DeepLink:    ac.DeepLink,
	}
	if err := p.Ingest(ctx, opEv); err != nil {
		return true, err
	}
	return true, nil
}
