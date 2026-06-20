package verify

import (
	"context"
	"errors"
	"fmt"
	"net/url"

	"github.com/roberto-grasiano/rabbot-seo/internal/model"
)

// This file holds the verify ORCHESTRATION shared by the CLI verify command and
// the daemon's POST /v1/verify endpoint, extracted from internal/cli/verify.go so
// there is ONE writer-aware core. Begin derives the token + instructions with no
// DB write; Check performs the real proof fetch and persists the proof record via
// the injected ProofStore (the daemon's single DB handle). The token is always
// DERIVED here, never caller-supplied — a hand-edited config or a replayed surface
// can never mint a verified tier.

// ProofStore is the narrow store seam Check needs. *store.DB satisfies it
// structurally; declaring it here (over model.Site, not store types) keeps
// internal/verify free of an import of internal/store (store imports verify, not
// the reverse — see the package doc), so no import cycle is introduced.
type ProofStore interface {
	GetSite(ctx context.Context, id int64) (model.Site, error)
	SaveVerification(ctx context.Context, siteID int64, rec ProofRecord) error
}

// BeginResult is the write-free output of Begin: the derived instance-bound token
// and human-readable placement instructions for the chosen method. It performs NO
// DB write, so a caller (the verify_begin MCP tool) can be ReadOnlyHint:true.
type BeginResult struct {
	SiteID       int64
	Host         string
	Method       Method
	Token        string
	Instructions string
}

// Begin derives the proof token for (host, method) from this instance's secret key
// and returns method-specific placement instructions. It writes nothing. An empty
// key fails closed (mirrors Verify): a zero key can never derive a usable token, so
// rather than hand back a token derived from no secret, Begin errors.
func Begin(siteID int64, host string, method Method, key []byte) (BeginResult, error) {
	if len(key) == 0 {
		return BeginResult{}, errors.New("verify: instance key is empty; cannot derive a proof token")
	}
	token := DeriveToken(key, host)
	return BeginResult{
		SiteID:       siteID,
		Host:         host,
		Method:       method,
		Token:        token,
		Instructions: placementInstructions(method, host, token),
	}, nil
}

// placementInstructions returns the method-specific proof-placement guidance,
// matching cli.printPlacement verbatim (the CLI now renders this same string).
// The DNS branch strips the port via url.URL.Hostname() because VerifyDNS resolves
// a bare hostname — the no-host-port-drift invariant (the wizard regression).
func placementInstructions(method Method, host, token string) string {
	switch method {
	case MethodWellKnown:
		return fmt.Sprintf("Place this file:  https://%s%s\nWith contents:    %s",
			host, wellKnownPath, token)
	case MethodDNS:
		bare := (&url.URL{Host: host}).Hostname()
		return fmt.Sprintf("Add a DNS TXT record on %s:\n  rabbot-verify=%s", bare, token)
	case MethodMeta:
		return fmt.Sprintf("Add to the <head> of https://%s/:\n  <meta name=\"rabbot-verify\" content=\"%s\">",
			host, token)
	default:
		return ""
	}
}

// CheckResult is the output of Check: the persisted proof record (the authoritative
// living state) plus the transient Reason for UX messaging.
type CheckResult struct {
	Record ProofRecord
	Reason Reason
}

// Check performs the real proof fetch for (siteID, host, method), persists the
// resulting ProofRecord through the injected ProofStore, and returns the Outcome.
// It is the DAEMON-OWNED verify path: the daemon holds the single DB handle, so
// running Check inside the daemon (via POST /v1/verify) keeps the writer single.
//
// Behavior preserved verbatim from cli.runVerify's verify-now path (verify.go:111):
//   - The token is DERIVED inside Verify from opts.Key; no caller value is trusted.
//   - On a transport/DNS error, the throttled attempt record is STILL persisted
//     (the attempt happened) and the error is returned — never a verified claim.
//   - On success, the verified record is persisted and the verified tier returned.
//   - An empty opts.Key yields {throttled, unreachable} with no error (fail-safe),
//     and that throttled attempt is recorded too.
//
// SaveVerification is the only write; a save error is surfaced (the caller decides
// HTTP 500 vs CLI hard error).
func Check(ctx context.Context, st ProofStore, siteID int64, host string, method Method, opts Options) (CheckResult, error) {
	out, verr := Verify(ctx, Request{
		SiteID: siteID,
		Host:   host,
		Method: method,
	}, opts)
	// Persist the record regardless of verr: a failed/unreachable attempt is a real
	// throttled record (records LastReverifiedAt), and the success path persists the
	// verified record. SaveVerification is the single write either way.
	if serr := st.SaveVerification(ctx, siteID, out.Record); serr != nil {
		return CheckResult{}, fmt.Errorf("verify: save proof record: %w", serr)
	}
	if verr != nil {
		return CheckResult(out), verr
	}
	return CheckResult(out), nil
}
