package verify

// Reason is a transient, human-facing classification of a verification attempt's
// outcome. Unlike State (verified/attested/throttled), it is NOT persisted — it
// exists so the onboarding-UX layer can show a specific "why it failed" message
// (the onboarding-ux-redesign spec consumes these). Defined here because this
// package owns the verification contract.
type Reason string

const (
	// ReasonVerified: the derived token was found at the surface.
	ReasonVerified Reason = "verified"
	// ReasonNotFound: no rabbot-verify value present at the surface.
	ReasonNotFound Reason = "not_found"
	// ReasonMismatch: a rabbot-verify value was present but did not equal the
	// derived token (e.g. a stale token, or someone else's).
	ReasonMismatch Reason = "mismatch"
	// ReasonRedirected: the surface redirected (off-host OR same-host); a redirect
	// must never satisfy a proof, so this is reported, never counted as verified.
	ReasonRedirected Reason = "redirected"
	// ReasonUnreachable: a transport / DNS error (or a missing instance key) meant
	// the surface could not be read; the attempt is inconclusive, never verified.
	ReasonUnreachable Reason = "unreachable"
)

// Outcome bundles the persisted proof record with the transient Reason. Verify
// returns it so callers get BOTH the authoritative State (for the throttle
// resolver / store) and the Reason (for UX messaging).
type Outcome struct {
	Record ProofRecord
	Reason Reason
}
