// Package verify implements proof-of-control verification for monitored sites.
//
// An operator proves they control a domain by placing a public, unguessable
// token via one of three methods — a .well-known file, a DNS TXT record, or a
// homepage <meta> tag — which verify then checks. The token is PUBLIC: its
// placement on a surface only the domain owner controls is the proof, so the
// only security requirement on the token is unguessability (>= 160 bits of
// crypto/rand entropy), not secrecy.
//
// The package has NO dependency on internal/store or internal/config: store
// imports verify only for the ProofRecord type (store -> verify and cli ->
// verify are clean one-way edges). It is stdlib + goquery (already a dep), so
// CGO_ENABLED=0 stays intact.
//
// Security (spec §Security): a redirect to an attacker-controlled host must NOT
// satisfy a proof. The HTTP verifiers therefore use an SSRF-guarded client whose
// CheckRedirect returns http.ErrUseLastResponse, so NO redirect (off-host OR
// same-host) is ever followed — the token must sit at the exact path on the
// literal host, returning 200, to count.
package verify

import (
	"context"
	"fmt"
	"time"
)

// State is the lifecycle tier of a site's proof record. It persists verbatim to
// the DB (verification_state) and config, and the Phase 4 throttle resolver
// reads it — these exact strings are load-bearing, never change them silently.
type State string

const (
	// StateVerified: a proof check succeeded; the site runs at full speed.
	StateVerified State = "verified"
	// StateAttested: the operator explicitly skipped verification (after the
	// Step-2 authorization attestation); the site stays throttled until a real
	// verify succeeds, but the skip is a deliberate recorded act.
	StateAttested State = "attested"
	// StateThrottled: the safe default — unverified/legacy sites and failed
	// verifications run at the throttled tier until an explicit verify flips them.
	StateThrottled State = "throttled"
)

// Method identifies which proof surface was used. It persists verbatim to the DB
// (verification_method) and config.
type Method string

const (
	// MethodWellKnown: GET /.well-known/rabbot-verify.txt == token.
	MethodWellKnown Method = "well_known"
	// MethodDNS: a TXT record rabbot-verify=<token> on the literal host.
	MethodDNS Method = "dns"
	// MethodMeta: <meta name="rabbot-verify" content="<token>"> on the homepage.
	MethodMeta Method = "meta"
)

// Options configures a verification run. Now injects the clock for deterministic
// timestamp assertions (mirroring setup.Options.Now). AllowPrivate clears the
// SSRF dial guard so tests can target loopback httptest servers; production
// always passes false. BaseOverride replaces the production https://<host> base
// with a test base (an httptest server's loopback URL) so the same fetch path is
// exercised under test; it is empty in production.
type Options struct {
	Now          time.Time
	AllowPrivate bool
	BaseOverride string
	// Key is the per-instance secret key Verify derives the expected token from.
	// Production loads it via LoadOrCreateInstanceKey; an empty Key fails safe
	// (never verifies).
	Key []byte
}

// ProofRecord is the authoritative, persisted proof-of-control record for a site
// (spec §E/D5). It is NEVER a bare boolean: State is an explicit enum so that
// editing config alone cannot mint a "verified" tier — the Phase 4 daemon
// re-verifies and rewrites the living state.
//
// VerifiedAt is the timestamp of the last SUCCESSFUL verify and is zero for an
// attested-only record (the skip path never proves control). LastReverifiedAt is
// the timestamp of the last (re)verify ATTEMPT outcome that produced this record.
type ProofRecord struct {
	SiteID           int64
	Method           Method
	Token            string
	State            State
	VerifiedAt       time.Time
	LastReverifiedAt time.Time
}

// TokenPrefix is prepended to every derived token. Tokens are PUBLIC — their
// placement is the proof — so this prefix is purely a recognizability marker.
const TokenPrefix = "rab_"

// Request describes one verification attempt. The expected token is DERIVED from
// the per-instance key (Options.Key), never carried here — there is no
// caller-supplied match value. Lookup is only consulted for MethodDNS;
// production passes nil (net.DefaultResolver), tests inject a stub.
type Request struct {
	SiteID int64
	Host   string
	Method Method
	Lookup LookupTXTFunc
}

// Verify is the instance-bound verification entry point: it DERIVES the expected
// token from opts.Key (never trusts a caller-supplied token) and checks the
// method surface, returning an Outcome (State + Reason). An empty key fails safe
// (throttled, ReasonUnreachable) — it can never verify. This is the secure core:
// there is no parameter, flag, config field, or DB column whose value becomes the
// match target.
func Verify(ctx context.Context, req Request, opts Options) (Outcome, error) {
	rec := ProofRecord{
		SiteID:           req.SiteID,
		Method:           req.Method,
		State:            StateThrottled,
		LastReverifiedAt: opts.Now,
	}
	if len(opts.Key) == 0 {
		return Outcome{Record: rec, Reason: ReasonUnreachable}, nil
	}
	token := DeriveToken(opts.Key, req.Host)
	rec.Token = token // derived; stored for display/audit only, never trusted as proof

	var (
		reason Reason
		err    error
	)
	switch req.Method {
	case MethodWellKnown:
		reason, err = VerifyWellKnown(ctx, req.Host, token, opts)
	case MethodMeta:
		reason, err = VerifyMeta(ctx, req.Host, token, opts)
	case MethodDNS:
		reason, err = VerifyDNS(ctx, req.Host, token, req.Lookup)
	default:
		return Outcome{Record: rec, Reason: ReasonUnreachable}, fmt.Errorf("verify: unknown method %q", req.Method)
	}
	if err != nil {
		return Outcome{Record: rec, Reason: ReasonUnreachable}, err
	}
	if reason == ReasonVerified {
		rec.State = StateVerified
		rec.VerifiedAt = opts.Now
	}
	return Outcome{Record: rec, Reason: reason}, nil
}

// Attest records the operator's deliberate skip-to-attested decision (the path
// taken after the Step-2 authorization attestation). The returned record is
// StateAttested with VerifiedAt zero — an attestation never proves control, so
// the site stays throttled until a real Verify succeeds. now is injected for
// deterministic timestamps; it is recorded as LastReverifiedAt (the moment the
// decision was made).
func Attest(siteID int64, method Method, now time.Time) ProofRecord {
	return ProofRecord{
		SiteID:           siteID,
		Method:           method,
		State:            StateAttested,
		LastReverifiedAt: now,
	}
}
