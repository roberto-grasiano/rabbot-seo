package wizard

import "github.com/roberto-grasiano/rabbot-seo/internal/verify"

// VerifyResult is the friendly copy for a verification outcome: a headline, an
// optional detail line, and the retry actions to offer. It maps verify.Reason
// (from the instance-bound engine) onto plain, non-blaming language so the
// place-then-verify screen (§V V4) renders a specific "why it failed" message
// plus the retry actions — never the raw enum.
type VerifyResult struct {
	Headline string
	Detail   string
	Actions  []string // button labels, e.g. "Check again"
}

// VerifyMessage maps a verification Reason to user-facing copy. host and token
// are woven in so a mismatch can show the exact value to fix and a success names
// the site. The mapping is TOTAL over every verify.Reason: the success case
// (ReasonVerified) offers no retry actions; every failure reason offers at least
// one. The default arm both handles ReasonUnreachable and guarantees totality if
// a new Reason is ever added (it still gets a non-empty headline + a retry).
func VerifyMessage(r verify.Reason, host, token string) VerifyResult {
	switch r {
	case verify.ReasonVerified:
		return VerifyResult{Headline: "Verified! " + host + " now runs at full speed. 🎉"}
	case verify.ReasonNotFound:
		return VerifyResult{
			Headline: "We couldn't find your code yet.",
			Detail: "It can take a minute to go live. Make sure it's on " + host +
				" (not another page); the code must match exactly: " + token,
			Actions: []string{"Check again", "Try a different way"},
		}
	case verify.ReasonMismatch:
		return VerifyResult{
			Headline: "We found a code, but it doesn't match.",
			Detail:   "Make sure you pasted exactly: " + token,
			Actions:  []string{"Check again"},
		}
	case verify.ReasonRedirected:
		return VerifyResult{
			Headline: "Your homepage redirected us, so we couldn't confirm.",
			Detail:   "Try the 'upload a file' or 'domain provider' method instead.",
			Actions:  []string{"Try a different way", "Finish later"},
		}
	default: // ReasonUnreachable (and any future Reason — totality guard)
		return VerifyResult{
			Headline: "Couldn't reach " + host + " just now (a network hiccup).",
			Actions:  []string{"Try again", "Finish later"},
		}
	}
}
