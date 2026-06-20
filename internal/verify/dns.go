package verify

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strings"
)

// dnsTXTPrefix is the required TXT record body: rabbot-verify=<token>.
const dnsTXTPrefix = "rabbot-verify="

// LookupTXTFunc resolves the TXT records for a bare hostname (VerifyDNS strips
// any port before calling it). Production uses a wrapper over
// net.DefaultResolver.LookupTXT (no user-controlled resolver — spec §Security);
// tests inject a stub so no real DNS is hit.
type LookupTXTFunc func(ctx context.Context, hostname string) ([]string, error)

// defaultLookupTXT is the production resolver: net.DefaultResolver on the bare
// hostname. There is NO HTTP and NO user-controlled resolver here, so DNS
// verification has no SSRF surface.
func defaultLookupTXT(ctx context.Context, hostname string) ([]string, error) {
	return net.DefaultResolver.LookupTXT(ctx, hostname)
}

// VerifyDNS checks the DNS TXT proof rabbot-verify=<token> on the BARE host.
// A lookup error ⇒ ReasonUnreachable (wrapped); a rabbot-verify= record whose
// value matches ⇒ ReasonVerified; a rabbot-verify= record present but not
// matching ⇒ ReasonMismatch; none present ⇒ ReasonNotFound. DNS has no redirect
// concept and no SSRF surface (net.DefaultResolver, no user-controlled resolver).
//
// The lookup is performed on the BARE hostname — DNS has no concept of ports, so
// an explicit port on the site URL (e.g. example.com:8443) is stripped before
// resolving; only the HTTP verifiers keep the port for their fetch. A nil lookup
// defaults to net.DefaultResolver (production). The match tolerates surrounding
// whitespace, the quotes some resolvers add around TXT chunks, and a trailing dot
// some emit.
func VerifyDNS(ctx context.Context, host, token string, lookup LookupTXTFunc) (Reason, error) {
	// An empty token is never a real proof: subtle.ConstantTimeCompare("","")==1,
	// so without this guard an empty TXT value (rabbot-verify=) would falsely
	// match. Fail closed before any lookup.
	if token == "" {
		return ReasonNotFound, nil
	}
	if lookup == nil {
		lookup = defaultLookupTXT
	}
	// url.URL.Hostname() strips any :port (and IPv6 brackets) without touching a
	// bare hostname — DNS resolves names, not host:port pairs.
	hostname := (&url.URL{Host: host}).Hostname()
	records, err := lookup(ctx, hostname)
	if err != nil {
		return ReasonUnreachable, fmt.Errorf("verify: dns lookup %s: %w", hostname, err)
	}
	want := dnsTXTPrefix + token
	present := false
	for _, rec := range records {
		// net.LookupTXT already joins the per-record string chunks (RFC), so we
		// only normalize the resulting string — never re-implement chunk joining.
		rec = strings.TrimSpace(rec)
		// Strip a single layer of surrounding double quotes some resolvers add,
		// then re-trim any whitespace that sat inside the quotes.
		rec = strings.TrimSpace(strings.Trim(rec, `"`))
		// Tolerate a single trailing dot some resolvers append to the value.
		rec = strings.TrimSuffix(rec, ".")
		if strings.HasPrefix(rec, dnsTXTPrefix) {
			present = true
			if tokenEqual(rec, want) {
				return ReasonVerified, nil
			}
		}
	}
	if present {
		return ReasonMismatch, nil
	}
	return ReasonNotFound, nil
}
