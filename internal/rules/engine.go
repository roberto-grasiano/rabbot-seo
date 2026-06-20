package rules

import (
	"context"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/model"
	"github.com/roberto-grasiano/rabbot-seo/internal/store"
)

// IssueStore is the subset of store.Store the engine needs. store.Store satisfies it.
type IssueStore interface {
	ListIssues(ctx context.Context, f store.IssueFilter) ([]model.Issue, error)
	UpsertIssue(ctx context.Context, iss model.Issue) (int64, error)
	CloseIssue(ctx context.Context, urlID int64, ruleID string, at time.Time) error
}

// Engine evaluates the rule set for one URL and reconciles open/closed issues.
type Engine struct {
	rules []Rule
	store IssueStore
	now   func() time.Time
}

// NewEngine builds an engine. now is injectable for deterministic tests.
func NewEngine(rules []Rule, st IssueStore, now func() time.Time) *Engine {
	if now == nil {
		now = time.Now
	}
	return &Engine{rules: rules, store: st, now: now}
}

// Apply evaluates every rule against ctx and reconciles issues:
//   - failing rule with no open issue     -> open (UpsertIssue, status open)
//   - failing rule with an open issue      -> refresh LastSeenAt (no re-alert)
//   - failing rule with an 'ignored' issue -> left untouched (user silenced it)
//   - passing rule with an open issue       -> close (CloseIssue) => resolved
//   - passing rule with an 'ignored' issue  -> close (CloseIssue) => resolved
//
// The ignored state silences a *failing* rule (no re-alert), but it is not
// permanent: once the underlying problem is actually fixed and the rule passes,
// the issue is closed so the silence clears with the defect. Without this, an
// ignored-then-recovered issue would linger as 'ignored' forever and silently
// suppress a later genuine recurrence (the engine would still see an 'ignored'
// prior and stay silent). CloseIssue must therefore resolve issues from either
// 'open' or 'ignored' for this to take effect end-to-end.
func (e *Engine) Apply(ctx context.Context, ec EvalContext) error {
	now := e.now()

	// Scope to this URL's issues (indexed by UNIQUE(url_id, rule_id)) rather than
	// scanning every issue across all sites on every fetch.
	urlID := ec.URLID
	existing, err := e.store.ListIssues(ctx, store.IssueFilter{URLID: &urlID})
	if err != nil {
		return err
	}
	byRule := make(map[string]model.Issue, len(existing))
	for _, iss := range existing {
		byRule[iss.RuleID] = iss
	}

	for _, r := range e.rules {
		f := r.Eval(ec)
		prior, hasPrior := byRule[r.ID()]
		ignored := hasPrior && prior.Status == model.IssueIgnored

		if f.Failed {
			if ignored {
				continue // user silenced this rule for this URL; no re-alert
			}
			openedAt := now
			if hasPrior && prior.Status == model.IssueOpen {
				openedAt = prior.OpenedAt // keep original open time; no re-alert
			}
			// Re-upserting an already-open issue intentionally refreshes
			// ImpactPoints from the current importance (which can drift as the
			// site's link graph / cold-start heuristic evolves) and advances
			// LastSeenAt to now, while preserving the original OpenedAt above so
			// the issue is not treated as newly re-opened.
			iss := model.Issue{
				URLID:        ec.URLID,
				RuleID:       f.RuleID,
				Status:       model.IssueOpen,
				Severity:     f.Severity,
				ImpactPoints: ImpactPoints(ec.Importance, f.Severity),
				OpenedAt:     openedAt,
				LastSeenAt:   now,
				Detail:       f.Detail,
			}
			if _, err := e.store.UpsertIssue(ctx, iss); err != nil {
				return err
			}
			continue
		}

		// Rule passes: close any prior open OR ignored issue (=> resolved). Closing
		// an ignored issue here clears a user's silence once the defect is fixed, so
		// a later genuine recurrence is not implicitly suppressed.
		if hasPrior && (prior.Status == model.IssueOpen || prior.Status == model.IssueIgnored) {
			if err := e.store.CloseIssue(ctx, ec.URLID, r.ID(), now); err != nil {
				return err
			}
		}
	}
	return nil
}
